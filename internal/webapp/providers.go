package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/summarizer"
	"github.com/go-chi/chi/v5"
)

const maskedAPIKey = "********"

// ProviderInput is the JSON representation accepted by provider CRUD APIs.
// APIKey may be omitted on update to preserve the existing secret.
type ProviderInput struct {
	Name         string `json:"name"`
	BaseURL      string `json:"base_url"`
	APIKey       string `json:"api_key"`
	DefaultModel string `json:"default_model"`
	Model        string `json:"model"`
	IsDefault    bool   `json:"is_default"`
	Version      int64  `json:"version"`
}

// ProviderService validates custom endpoints before storing providers.
type ProviderService struct {
	repository        providerRepository
	httpClient        *http.Client
	validationTimeout time.Duration
	allowPrivateHosts bool
}

type providerRepository interface {
	Insert(*model.AIProvider) (int64, error)
	GetByID(int64) (*model.AIProvider, error)
	List() ([]model.AIProvider, error)
	Update(*model.AIProvider) error
	Delete(int64) error
}

type optimisticProviderRepository interface {
	UpdateOptimistic(*model.AIProvider, int64) error
}

type strictProviderRepository interface {
	DeleteOptimistic(int64, int64) error
}

// NewProviderService creates a provider service using a bounded test request.
func NewProviderService(repository providerRepository, client *http.Client) *ProviderService {
	if client == nil {
		client = &http.Client{}
	}
	return &ProviderService{
		repository:        repository,
		httpClient:        client,
		validationTimeout: 10 * time.Second,
	}
}

// NewProviderServiceForTesting permits loopback httptest endpoints. Production
// handlers must use NewProviderService so provider validation remains SSRF-safe.
func NewProviderServiceForTesting(repository providerRepository, client *http.Client) *ProviderService {
	service := NewProviderService(repository, client)
	service.allowPrivateHosts = true
	return service
}

func (s *ProviderService) Create(ctx context.Context, input ProviderInput) (*model.AIProvider, error) {
	if s == nil || s.repository == nil {
		return nil, errors.New("provider service is not configured")
	}
	if err := validateProviderInput(input); err != nil {
		return nil, err
	}
	if err := s.ensureUniqueName(input.Name, 0); err != nil {
		return nil, err
	}
	modelValue := providerModel(input)
	if err := summarizer.ValidateCustomProvider(ctx, summarizer.CustomProviderConfig{
		BaseURL: input.BaseURL, APIKey: input.APIKey, Model: modelValue, HTTPClient: s.httpClient,
		AllowPrivateHosts: s.allowPrivateHosts,
	}, s.validationTimeout); err != nil {
		return nil, err
	}
	provider := &model.AIProvider{
		Name: input.Name, BaseURL: input.BaseURL, APIKey: input.APIKey,
		DefaultModel: modelValue, IsDefault: input.IsDefault,
	}
	id, err := s.repository.Insert(provider)
	if err != nil {
		return nil, fmt.Errorf("create provider: %w", err)
	}
	return s.getMasked(id)
}

func (s *ProviderService) Update(ctx context.Context, id int64, input ProviderInput) (*model.AIProvider, error) {
	if s == nil || s.repository == nil {
		return nil, errors.New("provider service is not configured")
	}
	if input.Version <= 0 {
		return nil, fmt.Errorf("provider version: %w", db.ErrConflict)
	}
	if err := validateProviderInput(input); err != nil {
		return nil, err
	}
	existing, err := s.repository.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("load provider: %w", err)
	}
	if existing.Version != input.Version {
		return nil, fmt.Errorf("provider version: %w", db.ErrConflict)
	}
	apiKey := strings.TrimSpace(input.APIKey)
	if apiKey == "" || apiKey == maskedAPIKey {
		apiKey = existing.APIKey
	}
	if apiKey == "" {
		return nil, errors.New("provider API key is required")
	}
	if err := s.ensureUniqueName(input.Name, id); err != nil {
		return nil, err
	}
	modelValue := providerModel(input)
	if err := summarizer.ValidateCustomProvider(ctx, summarizer.CustomProviderConfig{
		BaseURL: input.BaseURL, APIKey: apiKey, Model: modelValue, HTTPClient: s.httpClient,
		AllowPrivateHosts: s.allowPrivateHosts,
	}, s.validationTimeout); err != nil {
		return nil, err
	}
	provider := &model.AIProvider{
		ID: id, Name: input.Name, BaseURL: input.BaseURL, APIKey: apiKey,
		DefaultModel: modelValue, IsDefault: input.IsDefault,
	}
	if optimistic, ok := s.repository.(optimisticProviderRepository); ok {
		if err := optimistic.UpdateOptimistic(provider, input.Version); err != nil {
			return nil, fmt.Errorf("update provider: %w", err)
		}
	} else {
		return nil, fmt.Errorf("update provider: %w", db.ErrConflict)
	}
	return s.getMasked(id)
}

func (s *ProviderService) List() ([]model.AIProvider, error) {
	if s == nil || s.repository == nil {
		return nil, errors.New("provider service is not configured")
	}
	providers, err := s.repository.List()
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	for i := range providers {
		providers[i].APIKey = maskAPIKey(providers[i].APIKey)
	}
	return providers, nil
}

func (s *ProviderService) Delete(id, version int64) error {
	if s == nil || s.repository == nil {
		return errors.New("provider service is not configured")
	}
	if version <= 0 {
		return fmt.Errorf("provider version: %w", db.ErrConflict)
	}
	provider, err := s.repository.GetByID(id)
	if err != nil {
		return fmt.Errorf("load provider: %w", err)
	}
	if provider.Version != version {
		return fmt.Errorf("provider version: %w", db.ErrConflict)
	}
	if provider.IsDefault {
		return errors.New("default provider cannot be deleted")
	}
	strict, ok := s.repository.(strictProviderRepository)
	if !ok {
		return fmt.Errorf("delete provider: %w", db.ErrConflict)
	}
	if err := strict.DeleteOptimistic(id, version); err != nil {
		return fmt.Errorf("delete provider: %w", err)
	}
	return nil
}

func (s *ProviderService) ensureUniqueName(name string, currentID int64) error {
	if s == nil || s.repository == nil {
		return errors.New("provider service is not configured")
	}
	providers, err := s.repository.List()
	if err != nil {
		return fmt.Errorf("check provider name: %w", err)
	}
	for _, provider := range providers {
		if provider.ID != currentID && strings.EqualFold(strings.TrimSpace(provider.Name), strings.TrimSpace(name)) {
			return errors.New("provider with this name already exists")
		}
	}
	return nil
}

func (s *ProviderService) getMasked(id int64) (*model.AIProvider, error) {
	if s == nil || s.repository == nil {
		return nil, errors.New("provider service is not configured")
	}
	provider, err := s.repository.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("load provider: %w", err)
	}
	provider.APIKey = maskAPIKey(provider.APIKey)
	return provider, nil
}

func providerJSON(provider model.AIProvider) map[string]any {
	return map[string]any{
		"id":            provider.ID,
		"version":       provider.Version,
		"name":          provider.Name,
		"base_url":      provider.BaseURL,
		"has_key":       strings.TrimSpace(provider.APIKey) != "",
		"default_model": provider.DefaultModel,
		"is_default":    provider.IsDefault,
		"created_at":    provider.CreatedAt,
	}
}

func validateProviderInput(input ProviderInput) error {
	if strings.TrimSpace(input.Name) == "" {
		return errors.New("provider name is required")
	}
	if len(input.Name) > 100 {
		return errors.New("provider name is too long")
	}
	parsed, err := url.Parse(strings.TrimSpace(input.BaseURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("provider base_url must be a valid http or https URL")
	}
	if providerModel(input) == "" {
		return errors.New("provider model is required")
	}
	return nil
}

func providerModel(input ProviderInput) string {
	if strings.TrimSpace(input.DefaultModel) != "" {
		return strings.TrimSpace(input.DefaultModel)
	}
	return strings.TrimSpace(input.Model)
}

func maskAPIKey(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return maskedAPIKey
}

func writeProviderError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	message := err.Error()
	field := providerErrorField(message)
	lower := strings.ToLower(message)
	if strings.Contains(lower, "deadline") || strings.Contains(lower, "timeout") {
		status = http.StatusGatewayTimeout
		message = "Таймаут при проверке провайдера. Проверьте base_url."
	} else if strings.Contains(lower, "custom provider test request failed") {
		message = "Не удалось подключиться к провайдеру: " + message
	} else if strings.Contains(lower, "not found") {
		status = http.StatusNotFound
	} else if errors.Is(err, db.ErrDuplicate) {
		status = http.StatusConflict
		message = "Провайдер с таким именем уже существует"
		field = "name"
	} else if strings.Contains(lower, "name already exists") {
		status = http.StatusConflict
		message = "Провайдер с таким именем уже существует"
	} else if strings.Contains(lower, "default provider cannot be deleted") {
		status = http.StatusConflict
		message = "Нельзя удалить провайдера по умолчанию"
	} else if errors.Is(err, db.ErrConflict) {
		status = http.StatusConflict
		message = "Данные были изменены в другой сессии. Обновите страницу."
	} else if strings.Contains(lower, "unique constraint") || strings.Contains(lower, "already exists") {
		status = http.StatusConflict
	}
	response := map[string]string{"error": message}
	if field != "" {
		response["field"] = field
	}
	writeJSON(w, status, response)
}

func providerErrorField(message string) string {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "name") && (strings.Contains(lower, "exist") || strings.Contains(lower, "required") || strings.Contains(lower, "duplicate")):
		return "name"
	case strings.Contains(lower, "base url") || strings.Contains(lower, "base_url") || strings.Contains(lower, "endpoint") || strings.Contains(lower, "url"):
		return "base_url"
	case strings.Contains(lower, "api key") || strings.Contains(lower, "api_key") || strings.Contains(lower, "authorization"):
		return "api_key"
	case strings.Contains(lower, "model"):
		return "default_model"
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline") ||
		strings.Contains(lower, "connection") || strings.Contains(lower, "transport") ||
		strings.Contains(lower, "custom provider test request failed"):
		if strings.Contains(lower, "401") || strings.Contains(lower, "403") ||
			strings.Contains(lower, "unauthorized") || strings.Contains(lower, "forbidden") {
			return "api_key"
		}
		return "base_url"
	default:
		return ""
	}
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	if s.providerService == nil {
		http.Error(w, "provider service is not configured", http.StatusNotImplemented)
		return
	}
	switch r.Method {
	case http.MethodGet:
		providers, err := s.providerService.List()
		if err != nil {
			writeProviderError(w, err)
			return
		}
		body := make([]map[string]any, 0, len(providers))
		for _, provider := range providers {
			body = append(body, providerJSON(provider))
		}
		writeJSON(w, http.StatusOK, body)
	case http.MethodPost:
		var input ProviderInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid provider JSON"})
			return
		}
		provider, err := s.providerService.Create(r.Context(), input)
		if err != nil {
			writeProviderError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, providerJSON(*provider))
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleProviderByID(w http.ResponseWriter, r *http.Request) {
	if s.providerService == nil {
		http.Error(w, "provider service is not configured", http.StatusNotImplemented)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid provider id"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		provider, err := s.providerService.getMasked(id)
		if err != nil {
			writeProviderError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, providerJSON(*provider))
	case http.MethodPut, http.MethodPatch:
		var input ProviderInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid provider JSON"})
			return
		}
		provider, err := s.providerService.Update(r.Context(), id, input)
		if err != nil {
			writeProviderError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, providerJSON(*provider))
	case http.MethodDelete:
		var input versionInput
		if err := decodeJSON(r, w, &input); err != nil {
			return
		}
		if err := s.providerService.Delete(id, input.Version); err != nil {
			writeProviderError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "PUT, PATCH, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return
	}
}

var _ providerRepository = (*db.ProviderRepository)(nil)
