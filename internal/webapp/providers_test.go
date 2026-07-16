package webapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

func TestProviderServiceCreateMasksAPIKeyAndPersistsEncryptedSecret(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	service := NewProviderServiceForTesting(store.Providers, server.Client())
	provider, err := service.Create(context.Background(), ProviderInput{
		Name:         "Local vLLM",
		BaseURL:      server.URL + "/v1",
		APIKey:       "unit-provider-create",
		DefaultModel: "my-model",
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if provider.APIKey != "********" {
		t.Fatalf("returned API key = %q, want masked value", provider.APIKey)
	}

	var stored string
	if err := store.Conn().QueryRow("SELECT api_key FROM ai_providers WHERE id = ?", provider.ID).Scan(&stored); err != nil {
		t.Fatalf("read stored provider: %v", err)
	}
	if stored == "unit-provider-create" || !strings.HasPrefix(stored, "enc:v1:") {
		t.Fatalf("stored API key is not encrypted: %q", stored)
	}
	raw, err := store.Providers.GetByID(provider.ID)
	if err != nil {
		t.Fatalf("read provider: %v", err)
	}
	if raw.APIKey != "unit-provider-create" {
		t.Fatalf("decrypted API key = %q, want original secret", raw.APIKey)
	}
}

func TestProviderServiceUpdatePreservesKeyWhenMaskedOrOmitted(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	service := NewProviderServiceForTesting(store.Providers, server.Client())
	created, err := service.Create(context.Background(), ProviderInput{
		Name:         "Local",
		BaseURL:      server.URL,
		APIKey:       "unit-provider-original",
		DefaultModel: "model-a",
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}

	updated, err := service.Update(context.Background(), created.ID, ProviderInput{
		Name:         "Renamed",
		BaseURL:      server.URL,
		APIKey:       "********",
		DefaultModel: "model-b",
	})
	if err != nil {
		t.Fatalf("update provider: %v", err)
	}
	if updated.APIKey != "********" {
		t.Fatalf("updated API key = %q, want masked value", updated.APIKey)
	}
	raw, err := store.Providers.GetByID(created.ID)
	if err != nil {
		t.Fatalf("read updated provider: %v", err)
	}
	if raw.APIKey != "unit-provider-original" {
		t.Fatalf("stored secret changed during masked update: %q", raw.APIKey)
	}
}

func TestProviderServiceUpdateDoesNotPersistWhenValidationFails(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	working := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer working.Close()
	service := NewProviderServiceForTesting(store.Providers, working.Client())
	created, err := service.Create(context.Background(), ProviderInput{
		Name: "Stable", BaseURL: working.URL, APIKey: "unit-provider-stable", DefaultModel: "model-a",
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"invalid"}}`, http.StatusUnauthorized)
	}))
	defer failing.Close()
	_, err = service.Update(context.Background(), created.ID, ProviderInput{
		Name: "Should Not Persist", BaseURL: failing.URL, APIKey: "unit-provider-new", DefaultModel: "model-b",
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	current, err := store.Providers.GetByID(created.ID)
	if err != nil {
		t.Fatalf("read provider after failed update: %v", err)
	}
	if current.Name != "Stable" || current.DefaultModel != "model-a" || current.APIKey != "unit-provider-stable" {
		t.Fatalf("failed update changed provider: %#v", current)
	}
}

func TestProviderHTTPCreateReturnsValidationAndTimeoutErrors(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
	}))
	defer unauthorized.Close()

	server := NewWithProvidersForTesting(store, 50*time.Millisecond, unauthorized.Client())
	requestBody := `{"name":"Broken","base_url":"` + unauthorized.URL + `","api_key":"unit-provider-bad","default_model":"model"}`
	req := httptest.NewRequest(http.MethodPost, "/api/providers", strings.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "unit-provider-bad") {
		t.Fatalf("response leaked API key: %s", recorder.Body.String())
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer slow.Close()
	server = NewWithProvidersForTesting(store, 20*time.Millisecond, slow.Client())
	requestBody = `{"name":"Slow","base_url":"` + slow.URL + `","api_key":"unit-provider-slow","default_model":"model"}`
	req = httptest.NewRequest(http.MethodPost, "/api/providers", strings.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout status = %d, want 504", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "unit-provider-slow") {
		t.Fatalf("timeout response leaked API key: %s", recorder.Body.String())
	}

	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode timeout response: %v", err)
	}
	if body["error"] == "" {
		t.Fatal("timeout response missing error")
	}
}

func TestProviderServiceListMasksAPIKeys(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	if _, err := store.Providers.Insert(&model.AIProvider{
		Name:         "Provider",
		BaseURL:      "https://example.test/v1",
		APIKey:       "unit-provider-list",
		DefaultModel: "model",
	}); err != nil {
		t.Fatalf("insert provider: %v", err)
	}

	service := NewProviderServiceForTesting(store.Providers, http.DefaultClient)
	providers, err := service.List()
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	if len(providers) != 1 || providers[0].APIKey != "********" {
		t.Fatalf("providers = %#v, want one masked provider", providers)
	}
}
