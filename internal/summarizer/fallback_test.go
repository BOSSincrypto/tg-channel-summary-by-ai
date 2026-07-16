package summarizer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

func TestFallbackProviderUsesSecondaryAfterTransientPrimaryFailure(t *testing.T) {
	var primaryCalls, fallbackCalls int
	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls++
		http.Error(w, `{"error":{"message":"temporarily unavailable"}}`, http.StatusBadGateway)
	}))
	defer primaryServer.Close()
	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls++
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"[{\"post_id\":1,\"summary\":\"Резервный провайдер обработал пост.\"}]"}}]}`))
	}))
	defer fallbackServer.Close()

	primary := newTestProvider(t, primaryServer.URL, OpenRouterConfig{
		RetrySleep: func(context.Context, time.Duration) error { return nil },
	})
	fallback := newTestProvider(t, fallbackServer.URL, OpenRouterConfig{})
	var notified error
	provider, err := NewFallbackProvider(primary, fallback, func(err error) {
		notified = err
	})
	if err != nil {
		t.Fatalf("create fallback provider: %v", err)
	}
	summaries, err := provider.Summarize(context.Background(), []Post{{ID: 1, Text: "Пост"}})
	if err != nil {
		t.Fatalf("summarize through fallback: %v", err)
	}
	if len(summaries) != 1 || summaries[0].PostID != 1 {
		t.Fatalf("summaries = %#v, want one fallback summary", summaries)
	}
	if primaryCalls != 4 || fallbackCalls != 1 {
		t.Fatalf("primary calls = %d, fallback calls = %d, want 4 and 1", primaryCalls, fallbackCalls)
	}
	if notified == nil {
		t.Fatal("fallback notification callback was not called")
	}
}

type fallbackGroupConfigSource struct {
	config        *model.GroupAIConfig
	defaultConfig *model.AIProvider
}

func (s fallbackGroupConfigSource) ResolveAIConfig(int64) (*model.GroupAIConfig, error) {
	return s.config, nil
}

func (s fallbackGroupConfigSource) GetDefaultProvider() (*model.AIProvider, error) {
	if s.defaultConfig == nil {
		return nil, db.ErrNotFound
	}
	return s.defaultConfig, nil
}

func (s fallbackGroupConfigSource) GetOpenRouterProvider() (*model.AIProvider, error) {
	return s.GetDefaultProvider()
}

func fallbackSummaryResponse() string {
	return `{"choices":[{"message":{"content":"[{\"post_id\":1,\"summary\":\"Резервный провайдер обработал пост.\"}]"}}]}`
}

func TestNewProviderForGroupWithFallbackUsesOpenRouterForTransientCustomFailure(t *testing.T) {
	var primaryCalls, fallbackCalls, notifications int
	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls++
		http.Error(w, `{"error":{"message":"custom provider unavailable"}}`, http.StatusBadGateway)
	}))
	defer primaryServer.Close()
	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls++
		_, _ = w.Write([]byte(fallbackSummaryResponse()))
	}))
	defer fallbackServer.Close()

	source := fallbackGroupConfigSource{
		config: &model.GroupAIConfig{Provider: model.AIProvider{
			Name: "Custom", BaseURL: primaryServer.URL, APIKey: "custom-key", DefaultModel: "custom-model",
		}, Model: "custom-model"},
		defaultConfig: &model.AIProvider{
			Name: "OpenRouter", BaseURL: fallbackServer.URL, APIKey: "openrouter-key", DefaultModel: "openrouter-model",
		},
	}
	var notified error
	provider, err := NewProviderForGroupWithFallbackForTesting(source, 7, primaryServer.Client(), func(err error) {
		notifications++
		notified = err
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if _, ok := provider.(*FallbackProvider); !ok {
		t.Fatalf("provider type = %T, want fallback provider", provider)
	}

	summaries, err := provider.Summarize(context.Background(), []Post{{ID: 1, Text: "Пост"}})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if len(summaries) != 1 || summaries[0].PostID != 1 {
		t.Fatalf("summaries = %#v, want one fallback summary", summaries)
	}
	if primaryCalls != 4 || fallbackCalls != 1 {
		t.Fatalf("provider calls = primary %d, fallback %d, want 4 and 1", primaryCalls, fallbackCalls)
	}
	if notifications != 1 || notified == nil {
		t.Fatalf("fallback notifications = %d, error = %v, want one notification", notifications, notified)
	}
}

func TestNewProviderForGroupWithFallbackDoesNotFallbackPermanentFailure(t *testing.T) {
	var primaryCalls, fallbackCalls, notifications int
	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls++
		http.Error(w, `{"error":{"message":"invalid API key"}}`, http.StatusUnauthorized)
	}))
	defer primaryServer.Close()
	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls++
		_, _ = w.Write([]byte(fallbackSummaryResponse()))
	}))
	defer fallbackServer.Close()

	source := fallbackGroupConfigSource{
		config: &model.GroupAIConfig{Provider: model.AIProvider{
			Name: "Custom", BaseURL: primaryServer.URL, APIKey: "custom-key", DefaultModel: "custom-model",
		}},
		defaultConfig: &model.AIProvider{
			Name: "OpenRouter", BaseURL: fallbackServer.URL, APIKey: "openrouter-key", DefaultModel: "openrouter-model",
		},
	}
	provider, err := NewProviderForGroupWithFallbackForTesting(source, 8, primaryServer.Client(), func(_ error) {
		notifications++
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}

	if _, err := provider.Summarize(context.Background(), []Post{{ID: 1, Text: "Пост"}}); err == nil {
		t.Fatal("expected permanent provider failure")
	}
	if primaryCalls != 1 || fallbackCalls != 0 || notifications != 0 {
		t.Fatalf("provider calls/notifications = %d/%d/%d, want 1/0/0", primaryCalls, fallbackCalls, notifications)
	}
}

func TestNewProviderForGroupWithFallbackRetriesCustomProviderOnNextCycle(t *testing.T) {
	var primaryCalls, fallbackCalls, notifications int
	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls++
		if primaryCalls <= 4 {
			http.Error(w, `{"error":{"message":"temporary outage"}}`, http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(fallbackSummaryResponse()))
	}))
	defer primaryServer.Close()
	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls++
		_, _ = w.Write([]byte(fallbackSummaryResponse()))
	}))
	defer fallbackServer.Close()

	source := fallbackGroupConfigSource{
		config: &model.GroupAIConfig{Provider: model.AIProvider{
			Name: "Custom", BaseURL: primaryServer.URL, APIKey: "custom-key", DefaultModel: "custom-model",
		}},
		defaultConfig: &model.AIProvider{
			Name: "OpenRouter", BaseURL: fallbackServer.URL, APIKey: "openrouter-key", DefaultModel: "openrouter-model",
		},
	}
	provider, err := NewProviderForGroupWithFallbackForTesting(source, 9, primaryServer.Client(), func(_ error) {
		notifications++
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}

	for cycle := 0; cycle < 2; cycle++ {
		if _, err := provider.Summarize(context.Background(), []Post{{ID: 1, Text: "Пост"}}); err != nil {
			t.Fatalf("cycle %d summarize: %v", cycle, err)
		}
	}
	if primaryCalls != 5 || fallbackCalls != 1 || notifications != 1 {
		t.Fatalf("provider calls/notifications = %d/%d/%d, want 5/1/1", primaryCalls, fallbackCalls, notifications)
	}
}
