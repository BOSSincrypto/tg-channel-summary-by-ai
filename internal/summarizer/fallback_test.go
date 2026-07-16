package summarizer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
