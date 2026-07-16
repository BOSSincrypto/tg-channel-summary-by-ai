package summarizer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

type fakeGroupAIConfigSource struct {
	config *model.GroupAIConfig
	err    error
}

func (f fakeGroupAIConfigSource) ResolveAIConfig(int64) (*model.GroupAIConfig, error) {
	return f.config, f.err
}

func TestNewProviderForGroupUsesModelOverride(t *testing.T) {
	var request chatCompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Готово."}}]}`))
	}))
	defer server.Close()

	provider, err := NewProviderForGroupForTesting(fakeGroupAIConfigSource{
		config: &model.GroupAIConfig{
			Provider: model.AIProvider{
				BaseURL:      server.URL,
				APIKey:       "group-provider-key",
				DefaultModel: "provider-default",
			},
			Model: "group-override",
		},
	}, 42, server.Client())
	if err != nil {
		t.Fatalf("create group provider: %v", err)
	}
	if provider.(*CustomProvider).httpClient.Timeout <= 0 {
		t.Fatal("group provider HTTP client must have a finite timeout")
	}

	if _, err := provider.(*CustomProvider).ChatCompletion(context.Background(), []Message{
		{Role: "user", Content: "Проверка"},
	}); err != nil {
		t.Fatalf("chat completion: %v", err)
	}
	if request.Model != "group-override" {
		t.Fatalf("request model = %q, want group override", request.Model)
	}
}

func TestNewProviderForGroupUsesProviderDefaultModelWithoutOverride(t *testing.T) {
	var request chatCompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Готово."}}]}`))
	}))
	defer server.Close()

	provider, err := NewProviderForGroupForTesting(fakeGroupAIConfigSource{
		config: &model.GroupAIConfig{
			Provider: model.AIProvider{
				BaseURL:      server.URL,
				APIKey:       "group-provider-key",
				DefaultModel: "provider-default",
			},
		},
	}, 43, server.Client())
	if err != nil {
		t.Fatalf("create group provider: %v", err)
	}

	if _, err := provider.(*CustomProvider).ChatCompletion(context.Background(), []Message{
		{Role: "user", Content: "Проверка"},
	}); err != nil {
		t.Fatalf("chat completion: %v", err)
	}
	if request.Model != "provider-default" {
		t.Fatalf("request model = %q, want provider default", request.Model)
	}
}
