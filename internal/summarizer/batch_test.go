package summarizer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOpenRouterSummarizeBuildsRussianBatchPrompt(t *testing.T) {
	var requests []chatCompletionRequest
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request chatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"[{\"post_id\":11,\"summary\":\"Компания представила новый продукт.\"},{\"post_id\":12,\"summary\":\"Регулятор опубликовал обновлённые правила.\"}]"}}]}`))
	}))
	defer server.Close()

	provider := newTestProvider(t, server.URL, OpenRouterConfig{})
	summaries, err := provider.Summarize(context.Background(), []Post{
		{ID: 11, Text: "Компания представила новый продукт"},
		{ID: 12, Text: "Регулятор опубликовал обновлённые правила"},
	})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summary count = %d, want 2", len(summaries))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	if requests[0].Messages[0].Role != "system" ||
		!strings.Contains(requests[0].Messages[0].Content, "рус") ||
		!strings.Contains(requests[0].Messages[0].Content, "одно предложение") {
		t.Errorf("system prompt does not require Russian one-sentence output: %q", requests[0].Messages[0].Content)
	}
	if !strings.Contains(requests[0].Messages[1].Content, "Компания представила новый продукт") ||
		!strings.Contains(requests[0].Messages[1].Content, "Регулятор опубликовал обновлённые правила") {
		t.Errorf("user prompt omitted post text: %q", requests[0].Messages[1].Content)
	}
}

func TestOpenRouterSummarizeUsesFallbackForMissingAndDropsExtraSummaries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"[{\"post_id\":1,\"summary\":\"Первый пост обработан.\"},{\"post_id\":999,\"summary\":\"Лишний выдуманный пост.\"}]"}}]}`))
	}))
	defer server.Close()

	provider := newTestProvider(t, server.URL, OpenRouterConfig{})
	summaries, err := provider.Summarize(context.Background(), []Post{
		{ID: 1, Text: "Первый пост"},
		{ID: 2, Text: "Второй пост"},
	})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summary count = %d, want 2", len(summaries))
	}
	if summaries[0].PostID != 1 || summaries[0].Text != "Первый пост обработан." {
		t.Errorf("first summary = %#v", summaries[0])
	}
	if summaries[1].PostID != 2 || summaries[1].Text != fallbackSummary {
		t.Errorf("missing summary fallback = %#v, want %q", summaries[1], fallbackSummary)
	}
}

func TestOpenRouterSummarizeRejectsNonRussianOrMultiSentenceOutput(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "non Russian", content: `[{"post_id":1,"summary":"The company launched a product."}]`},
		{name: "multiple sentences", content: `[{"post_id":1,"summary":"Компания выпустила продукт. Рынок отреагировал."}]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				response, _ := json.Marshal(map[string]any{"choices": []map[string]any{
					{"message": map[string]string{"content": test.content}},
				}})
				_, _ = w.Write(response)
			}))
			defer server.Close()

			provider := newTestProvider(t, server.URL, OpenRouterConfig{})
			if _, err := provider.Summarize(context.Background(), []Post{{ID: 1, Text: "Пост"}}); err == nil {
				t.Fatal("expected output validation error")
			}
		})
	}
}

func TestOpenRouterChatCompletionRetriesTransientErrors(t *testing.T) {
	var calls int
	var sleeps []time.Duration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			http.Error(w, `{"error":{"message":"temporary"}}`, http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Готово."}}]}`))
	}))
	defer server.Close()

	provider := newTestProvider(t, server.URL, OpenRouterConfig{
		RetrySleep: func(ctx context.Context, delay time.Duration) error {
			sleeps = append(sleeps, delay)
			return nil
		},
	})
	content, err := provider.ChatCompletion(context.Background(), []Message{{Role: "user", Content: "Проверка"}})
	if err != nil {
		t.Fatalf("chat completion: %v", err)
	}
	if content != "Готово." || calls != 3 {
		t.Fatalf("content = %q, calls = %d, want success after 3 calls", content, calls)
	}
	if len(sleeps) != 2 || sleeps[0] != time.Second || sleeps[1] != 2*time.Second {
		t.Fatalf("retry delays = %v, want [1s 2s]", sleeps)
	}
}

func TestOpenRouterChatCompletionDoesNotRetryPermanentErrors(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, `{"error":{"message":"invalid key"}}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	provider := newTestProvider(t, server.URL, OpenRouterConfig{
		RetrySleep: func(context.Context, time.Duration) error {
			t.Fatal("permanent error must not sleep before retry")
			return nil
		},
	})
	if _, err := provider.ChatCompletion(context.Background(), []Message{{Role: "user", Content: "Проверка"}}); err == nil {
		t.Fatal("expected permanent provider error")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 for permanent error", calls)
	}
}

func TestOpenRouterSummarizeSplitsLargeInputAndTruncatesLongPosts(t *testing.T) {
	var requests []chatCompletionRequest
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request chatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()

		var content strings.Builder
		content.WriteString("[")
		for id := int64(len(requests)-1)*defaultMaxPostsPerBatch + 1; id <= int64(len(requests))*defaultMaxPostsPerBatch && id <= 51; id++ {
			if content.Len() > 1 {
				content.WriteString(",")
			}
			content.WriteString(`{"post_id":`)
			content.WriteString(fmt.Sprintf("%d", id))
			content.WriteString(`,"summary":"Пост обработан."}`)
		}
		content.WriteString("]")
		response, _ := json.Marshal(map[string]any{"choices": []map[string]any{
			{"message": map[string]string{"content": content.String()}},
		}})
		_, _ = w.Write(response)
	}))
	defer server.Close()

	provider := newTestProvider(t, server.URL, OpenRouterConfig{})
	posts := make([]Post, 51)
	for i := range posts {
		posts[i] = Post{ID: int64(i + 1), Text: strings.Repeat("длинный текст ", 500)}
	}
	summaries, err := provider.Summarize(context.Background(), posts)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if len(summaries) != len(posts) {
		t.Fatalf("summary count = %d, want %d", len(summaries), len(posts))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	for _, request := range requests {
		if len(request.Messages) != 2 {
			t.Fatalf("message count = %d, want system and user", len(request.Messages))
		}
		if len([]rune(request.Messages[1].Content)) > defaultMaxPostChars*defaultMaxPostsPerBatch+5000 {
			t.Fatalf("batch prompt was not bounded: %d runes", len([]rune(request.Messages[1].Content)))
		}
	}
}

func newTestProvider(t *testing.T, baseURL string, config OpenRouterConfig) *OpenRouterProvider {
	t.Helper()
	config.BaseURL = baseURL
	config.APIKey = "test-key"
	config.Model = "test-model"
	config.AllowPrivateHosts = true
	config.HTTPClient = http.DefaultClient
	provider, err := NewOpenRouterWithConfig(config)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return provider
}
