package summarizer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenRouterProviderChatCompletion(t *testing.T) {
	var gotRequest chatCompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/v1/chat/completions" {
			t.Errorf("path = %s, want /api/v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization = %q, want Bearer test-key", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q, want application/json", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotRequest); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"completion-1","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"Готово."},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	provider, err := NewOpenRouterWithConfig(OpenRouterConfig{
		BaseURL:           server.URL + "/api/v1",
		APIKey:            "test-key",
		Model:             "test-model",
		HTTPClient:        server.Client(),
		AllowPrivateHosts: true,
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}

	got, err := provider.ChatCompletion(context.Background(), []Message{
		{Role: "user", Content: "Привет"},
	})
	if err != nil {
		t.Fatalf("chat completion: %v", err)
	}
	if got != "Готово." {
		t.Fatalf("content = %q, want %q", got, "Готово.")
	}
	if gotRequest.Model != "test-model" {
		t.Errorf("model = %q, want test-model", gotRequest.Model)
	}
	if len(gotRequest.Messages) != 1 || gotRequest.Messages[0].Role != "user" {
		t.Errorf("messages = %#v, want one user message", gotRequest.Messages)
	}
}

func TestOpenRouterProviderSummarizeParsesBatchResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"[{\"post_id\":1,\"summary\":\"Первое сообщение.\"},{\"post_id\":2,\"summary\":\"Второе сообщение.\"}]"}}]}`))
	}))
	defer server.Close()

	provider, err := NewOpenRouterWithConfig(OpenRouterConfig{
		BaseURL:           server.URL,
		APIKey:            "test-key",
		Model:             "test-model",
		HTTPClient:        server.Client(),
		AllowPrivateHosts: true,
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}

	summaries, err := provider.Summarize(context.Background(), []Post{
		{ID: 1, Text: "Первый пост"},
		{ID: 2, Text: "Второй пост"},
	})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summaries = %#v, want two summaries", summaries)
	}
	if summaries[0].PostID != 1 || summaries[0].Text != "Первое сообщение." {
		t.Errorf("first summary = %#v", summaries[0])
	}
	if summaries[1].PostID != 2 || summaries[1].Text != "Второе сообщение." {
		t.Errorf("second summary = %#v", summaries[1])
	}
}

func TestOpenRouterProviderRejectsNonSuccessResponseWithoutLeakingKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"upstream failed"}}`, http.StatusBadGateway)
	}))
	defer server.Close()

	provider, err := NewOpenRouterWithConfig(OpenRouterConfig{
		BaseURL:    server.URL,
		APIKey:     "secret-key",
		Model:      "test-model",
		HTTPClient: server.Client(),
		RetrySleep: func(context.Context, time.Duration) error {
			return nil
		},
		AllowPrivateHosts: true,
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}

	_, err = provider.ChatCompletion(context.Background(), []Message{{Role: "user", Content: "test"}})
	if err == nil {
		t.Fatal("expected upstream error")
	}
	if !strings.Contains(err.Error(), "HTTP 502") {
		t.Errorf("error = %q, want HTTP status", err)
	}
	if strings.Contains(err.Error(), "secret-key") {
		t.Errorf("error leaked API key: %q", err)
	}
}

func TestOpenRouterProviderRedactsKeyFromProviderResponseError(t *testing.T) {
	const apiKey = "provider-response-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"upstream rejected Authorization Bearer provider-response-secret; retry request"}}`, http.StatusBadGateway)
	}))
	defer server.Close()

	provider, err := NewOpenRouterWithConfig(OpenRouterConfig{
		BaseURL:    server.URL,
		APIKey:     apiKey,
		Model:      "test-model",
		HTTPClient: server.Client(),
		RetrySleep: func(context.Context, time.Duration) error {
			return nil
		},
		AllowPrivateHosts: true,
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}

	_, err = provider.ChatCompletion(context.Background(), []Message{{Role: "user", Content: "test"}})
	if err == nil {
		t.Fatal("expected upstream error")
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Fatalf("provider error leaked configured API key: %q", err)
	}
	for _, want := range []string{"HTTP 502", "[redacted]", "retry request"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("provider error %q does not retain safe context %q", err, want)
		}
	}
}

func TestOpenRouterProviderRedactsKeyFromTransportError(t *testing.T) {
	const apiKey = "transport-provider-secret"
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("transport unavailable for Authorization Bearer %s", apiKey)
		}),
	}
	provider, err := NewOpenRouterWithConfig(OpenRouterConfig{
		BaseURL:    "https://provider.invalid/v1",
		APIKey:     apiKey,
		Model:      "test-model",
		HTTPClient: client,
		RetrySleep: func(context.Context, time.Duration) error {
			return nil
		},
		AllowPrivateHosts: true,
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}

	_, err = provider.ChatCompletion(context.Background(), []Message{{Role: "user", Content: "test"}})
	if err == nil {
		t.Fatal("expected transport error")
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Fatalf("transport error leaked configured API key: %q", err)
	}
	if !strings.Contains(err.Error(), "transport unavailable") {
		t.Fatalf("transport error lost safe context: %q", err)
	}
}

func TestOpenRouterProviderHonorsContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	provider, err := NewOpenRouterWithConfig(OpenRouterConfig{
		BaseURL:           server.URL,
		APIKey:            "test-key",
		Model:             "test-model",
		HTTPClient:        server.Client(),
		AllowPrivateHosts: true,
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = provider.ChatCompletion(ctx, []Message{{Role: "user", Content: "test"}})
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
