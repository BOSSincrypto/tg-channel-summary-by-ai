package summarizer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

func TestCustomProviderUsesOpenAICompatibleEndpoint(t *testing.T) {
	var request chatCompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer unit-provider-value" {
			t.Errorf("authorization = %q, want Bearer unit-provider-value", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Проверка пройдена."}}]}`))
	}))
	defer server.Close()

	provider, err := NewCustomProvider(CustomProviderConfig{
		BaseURL:           server.URL + "/v1",
		APIKey:            "unit-provider-value",
		Model:             "my-model",
		HTTPClient:        server.Client(),
		AllowPrivateHosts: true,
	})
	if err != nil {
		t.Fatalf("create custom provider: %v", err)
	}

	got, err := provider.ChatCompletion(context.Background(), []Message{{Role: "user", Content: "Проверка"}})
	if err != nil {
		t.Fatalf("chat completion: %v", err)
	}
	if got != "Проверка пройдена." {
		t.Fatalf("content = %q, want provider response", got)
	}
	if request.Model != "my-model" {
		t.Errorf("model = %q, want my-model", request.Model)
	}
}

func TestProviderFactoryRoutesPersistedCustomProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer unit-provider-factory" {
			t.Errorf("authorization = %q, want Bearer unit-provider-factory", got)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"factory response"}}]}`))
	}))
	defer server.Close()

	provider, err := NewProviderFromConfigForTesting(model.AIProvider{
		BaseURL:      server.URL,
		APIKey:       "unit-provider-factory",
		DefaultModel: "factory-model",
	}, server.Client())
	if err != nil {
		t.Fatalf("create provider from config: %v", err)
	}
	got, err := provider.(*CustomProvider).ChatCompletion(context.Background(), []Message{{Role: "user", Content: "test"}})
	if err != nil {
		t.Fatalf("factory completion: %v", err)
	}
	if got != "factory response" {
		t.Fatalf("response = %q, want factory response", got)
	}
}

func TestValidateCustomProviderRejectsUnauthorizedEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"invalid api key"}}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	err := ValidateCustomProvider(context.Background(), CustomProviderConfig{
		BaseURL:           server.URL,
		APIKey:            "unit-provider-invalid",
		Model:             "my-model",
		HTTPClient:        server.Client(),
		AllowPrivateHosts: true,
	}, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !containsAny(err.Error(), "HTTP 401", "invalid api key") {
		t.Fatalf("validation error = %q, want upstream status or reason", err)
	}
	if containsAny(err.Error(), "unit-provider-invalid") {
		t.Fatalf("validation error should not expose the API key: %q", err)
	}
}

func TestValidateCustomProviderTimesOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer server.Close()

	err := ValidateCustomProvider(context.Background(), CustomProviderConfig{
		BaseURL:           server.URL,
		APIKey:            "unit-provider-timeout",
		Model:             "my-model",
		HTTPClient:        server.Client(),
		AllowPrivateHosts: true,
	}, 20*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout validation error")
	}
	if !containsAny(err.Error(), "context deadline exceeded", "Client.Timeout exceeded") {
		t.Fatalf("validation error = %q, want timeout reason", err)
	}
	if containsAny(err.Error(), "unit-provider-timeout") {
		t.Fatalf("validation error leaked API key: %q", err)
	}
}

func TestCustomProviderRejectsPrivateEndpointByDefault(t *testing.T) {
	_, err := NewCustomProvider(CustomProviderConfig{
		BaseURL: "http://127.0.0.1:11434/v1",
		APIKey:  "unit-provider-value",
		Model:   "unit-model",
	})
	if err == nil {
		t.Fatal("private provider endpoint should be rejected by default")
	}
	if !containsAny(err.Error(), "private network", "localhost") {
		t.Fatalf("private endpoint error = %q, want private-network reason", err)
	}
}

func containsAny(value string, terms ...string) bool {
	for _, term := range terms {
		if len(term) > 0 && indexOf(value, term) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(value, term string) int {
	for i := 0; i+len(term) <= len(value); i++ {
		if value[i:i+len(term)] == term {
			return i
		}
	}
	return -1
}
