package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/parser"
	"github.com/go-chi/chi/v5"
)

var channelUsernamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{5,32}$`)

// ChannelVerifier checks that a username resolves through Telegram's public
// t.me/s endpoint before it is persisted.
type ChannelVerifier interface {
	Verify(context.Context, string) (string, error)
}

type parserChannelVerifier struct {
	parser *parser.Parser
}

func (v parserChannelVerifier) Verify(_ context.Context, username string) (string, error) {
	_, stats, err := v.parser.ParseChannelWithStats(username)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stats.ChannelTitle), nil
}

// ChannelService owns channel validation and persistence.
type ChannelService struct {
	repository dbChannelRepository
	verifier   ChannelVerifier
	maxRetries int
	sleep      func(context.Context, time.Duration) error
}

type dbChannelRepository interface {
	Insert(*model.Channel) (int64, error)
	GetByID(int64) (*model.Channel, error)
	List() ([]model.Channel, error)
	UpdateEnabledOptimistic(int64, bool, int64) error
	DeleteOptimistic(int64, int64) error
}

func NewChannelService(repository dbChannelRepository, verifier ChannelVerifier) *ChannelService {
	return &ChannelService{
		repository: repository,
		verifier:   verifier,
		maxRetries: 3,
		sleep:      sleepWithContext,
	}
}

// SetVerificationRetry configures the bounded verification attempts and
// injectable backoff used by channel creation.
func (s *ChannelService) SetVerificationRetry(maxRetries int, sleep func(context.Context, time.Duration) error) {
	if maxRetries < 1 {
		maxRetries = 1
	}
	s.maxRetries = maxRetries
	if sleep == nil {
		sleep = sleepWithContext
	}
	s.sleep = sleep
}

type channelInput struct {
	Username string `json:"username"`
	Enabled  *bool  `json:"enabled"`
	Version  int64  `json:"version"`
}

type versionInput struct {
	Version int64 `json:"version"`
}

func (s *ChannelService) Create(ctx context.Context, username string) (*model.Channel, error) {
	username = strings.TrimSpace(username)
	if strings.HasPrefix(username, "@") {
		username = strings.TrimPrefix(username, "@")
	}
	if !channelUsernamePattern.MatchString(username) || strings.Contains(username, "@") {
		return nil, errors.New("Неверный формат username")
	}
	username = strings.ToLower(username)
	if lookup, ok := s.repository.(interface {
		GetByUsername(string) (*model.Channel, error)
	}); ok {
		existing, err := lookup.GetByUsername(username)
		if err == nil && existing != nil {
			return nil, db.ErrDuplicate
		}
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, err
		}
	}
	if s.verifier != nil {
		title, err := s.verifyWithRetry(ctx, username)
		if err != nil {
			return nil, classifyChannelVerificationError(err)
		}
		ch := &model.Channel{Username: username, Title: strings.TrimSpace(title), Enabled: true}
		id, err := s.repository.Insert(ch)
		if err != nil {
			return nil, err
		}
		return s.repository.GetByID(id)
	}
	ch := &model.Channel{Username: username, Enabled: true}
	id, err := s.repository.Insert(ch)
	if err != nil {
		return nil, err
	}
	return s.repository.GetByID(id)
}

func (s *ChannelService) verifyWithRetry(ctx context.Context, username string) (string, error) {
	var lastErr error
	attempts := s.maxRetries
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		title, err := s.verifier.Verify(ctx, username)
		if err == nil {
			return title, nil
		}
		lastErr = err
		if !isTransientVerificationError(err) || attempt == attempts-1 {
			return "", err
		}
		delay := verificationBackoff(attempt)
		if err := s.sleep(ctx, delay); err != nil {
			return "", fmt.Errorf("channel verification backoff: %w", err)
		}
	}
	return "", lastErr
}

func isTransientVerificationError(err error) bool {
	if err == nil || errors.Is(err, parser.ErrChannelNotFound) ||
		errors.Is(err, parser.ErrChannelPrivate) ||
		errors.Is(err, parser.ErrChannelUnavailable) {
		return false
	}
	var rateLimitErr *parser.RateLimitError
	if errors.As(err, &rateLimitErr) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		var nestedNetworkErr net.Error
		return errors.As(urlErr.Err, &nestedNetworkErr)
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"status 408", "status 429", "status 500", "status 501", "status 502", "status 503", "status 504",
		"http 408", "http 429", "http 500", "http 501", "http 502", "http 503", "http 504",
		" 408 ", " 429 ", " 500 ", " 501 ", " 502 ", " 503 ", " 504 ",
		"rate limited",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

const maxVerificationBackoff = 2 * time.Second

func verificationBackoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := 100 * time.Millisecond * time.Duration(1<<min(attempt, 4))
	if delay > maxVerificationBackoff {
		return maxVerificationBackoff
	}
	return delay
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func classifyChannelVerificationError(err error) error {
	switch {
	case errors.Is(err, parser.ErrChannelNotFound):
		return fmt.Errorf("channel verification not found: %w", err)
	case errors.Is(err, parser.ErrChannelPrivate), errors.Is(err, parser.ErrChannelUnavailable):
		return fmt.Errorf("channel verification private: %w", err)
	default:
		return fmt.Errorf("channel verification unavailable: %w", err)
	}
}

func channelJSON(channel model.Channel) map[string]any {
	result := map[string]any{
		"id":           channel.ID,
		"version":      channel.Version,
		"username":     channel.Username,
		"title":        channel.Title,
		"enabled":      channel.Enabled,
		"last_post_id": channel.LastPostID,
	}
	if channel.FetchErrorMessage != "" {
		result["fetch_error_message"] = channel.FetchErrorMessage
		result["fetch_error_kind"] = channel.FetchErrorKind
		result["fetch_error_at"] = channel.FetchErrorAt
	}
	return result
}

func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	if s.channelService == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "channel service is not configured"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		channels, err := s.channelService.repository.List()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Не удалось загрузить каналы"})
			return
		}
		body := make([]map[string]any, 0, len(channels))
		for _, channel := range channels {
			body = append(body, channelJSON(channel))
		}
		writeJSON(w, http.StatusOK, body)
	case http.MethodPost:
		var input channelInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Некорректный JSON"})
			return
		}
		channel, err := s.channelService.Create(r.Context(), input.Username)
		if err != nil {
			writeChannelError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, channelJSON(*channel))
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleChannelByID(w http.ResponseWriter, r *http.Request) {
	if s.channelService == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "channel service is not configured"})
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Некорректный ID канала"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		channel, err := s.channelService.repository.GetByID(id)
		if err != nil {
			writeChannelError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, channelJSON(*channel))
	case http.MethodPut, http.MethodPatch:
		var input channelInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil || input.Enabled == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enabled должен быть boolean"})
			return
		}
		if input.Version <= 0 {
			writeChannelError(w, db.ErrConflict)
			return
		}
		err := s.channelService.repository.UpdateEnabledOptimistic(id, *input.Enabled, input.Version)
		if err != nil {
			writeChannelError(w, err)
			return
		}
		channel, err := s.channelService.repository.GetByID(id)
		if err != nil {
			writeChannelError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, channelJSON(*channel))
	case http.MethodDelete:
		var input versionInput
		if err := decodeJSON(r, w, &input); err != nil {
			return
		}
		if input.Version <= 0 {
			writeChannelError(w, db.ErrConflict)
			return
		}
		if err := s.channelService.repository.DeleteOptimistic(id, input.Version); err != nil {
			writeChannelError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PUT, PATCH, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func writeChannelError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "Не удалось обработать канал"
	switch {
	case errors.Is(err, db.ErrNotFound):
		status, message = http.StatusNotFound, "Канал был удалён в другой сессии."
	case errors.Is(err, db.ErrConflict):
		status, message = http.StatusConflict, "Данные были изменены в другой сессии. Обновите страницу."
	case errors.Is(err, db.ErrDuplicate):
		status, message = http.StatusConflict, "Канал уже добавлен"
	case errors.Is(err, parser.ErrChannelNotFound):
		status, message = http.StatusUnprocessableEntity, "Канал не найден"
	case errors.Is(err, parser.ErrChannelPrivate), errors.Is(err, parser.ErrChannelUnavailable):
		status, message = http.StatusUnprocessableEntity, "Канал приватный или недоступен"
	case strings.Contains(strings.ToLower(err.Error()), "verification unavailable"):
		status, message = http.StatusBadGateway, "Не удалось проверить канал, попробуйте позже"
	case strings.Contains(strings.ToLower(err.Error()), "username"):
		status, message = http.StatusBadRequest, "Неверный формат username"
	}
	writeJSON(w, status, map[string]string{"error": message})
}
