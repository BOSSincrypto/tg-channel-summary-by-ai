package summarizer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/boss/tg-channel-summary-by-ai/internal/security"
)

const (
	// DefaultOpenRouterBaseURL is the OpenAI-compatible OpenRouter API base URL.
	DefaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"
	// DefaultOpenRouterModel is the model selected for the default provider.
	DefaultOpenRouterModel  = "openai/gpt-oss-120b"
	defaultMaxPostsPerBatch = 50
	defaultMaxPostChars     = 2000
	fallbackSummary         = "[Не удалось создать краткое содержание]"
	maxRetryDelay           = 30 * time.Second
)

// OpenRouterConfig configures an OpenRouterProvider.
type OpenRouterConfig struct {
	BaseURL      string
	APIKey       string
	Model        string
	ProviderName string
	HTTPClient   *http.Client
	HTTPReferer  string
	AppTitle     string
	// RetrySleep overrides the retry delay, primarily for deterministic tests.
	RetrySleep func(context.Context, time.Duration) error
	// AllowPrivateHosts is intended only for trusted local test endpoints.
	// Production provider configuration must leave it false.
	AllowPrivateHosts bool
}

// Message is an OpenAI-compatible chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// OpenRouterProvider implements Provider through OpenRouter's
// OpenAI-compatible chat completions endpoint.
type OpenRouterProvider struct {
	baseURL      string
	apiKey       string
	model        string
	httpClient   *http.Client
	httpReferer  string
	appTitle     string
	retrySleep   func(context.Context, time.Duration) error
	redactor     *security.Redactor
	providerName string
}

var _ Provider = (*OpenRouterProvider)(nil)

type chatCompletionRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	Stream      bool      `json:"stream"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type batchSummary struct {
	PostID  int64  `json:"post_id"`
	Summary string `json:"summary"`
}

// NewOpenRouter creates a provider using the default OpenRouter endpoint.
// NewOpenRouterWithConfig should be used when the HTTP client or endpoint
// needs to be customized, such as in tests.
func NewOpenRouter(apiKey, model string) *OpenRouterProvider {
	if model == "" {
		model = DefaultOpenRouterModel
	}
	return &OpenRouterProvider{
		baseURL:      DefaultOpenRouterBaseURL,
		apiKey:       apiKey,
		model:        model,
		httpClient:   &http.Client{Timeout: 60 * time.Second},
		retrySleep:   sleepWithContext,
		redactor:     security.NewRedactor(apiKey),
		providerName: "OpenRouter",
	}
}

// NewOpenRouterWithConfig creates a configured OpenRouter provider.
func NewOpenRouterWithConfig(config OpenRouterConfig) (*OpenRouterProvider, error) {
	baseURL := strings.TrimSpace(config.BaseURL)
	if baseURL == "" {
		baseURL = DefaultOpenRouterBaseURL
	}
	baseURL, err := validateProviderBaseURL(baseURL, config.AllowPrivateHosts)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("OpenRouter API key is required")
	}
	model := strings.TrimSpace(config.Model)
	if model == "" {
		model = DefaultOpenRouterModel
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	client = secureProviderHTTPClient(client, config.AllowPrivateHosts)
	return &OpenRouterProvider{
		baseURL:      baseURL,
		apiKey:       config.APIKey,
		model:        model,
		httpClient:   client,
		httpReferer:  strings.TrimSpace(config.HTTPReferer),
		appTitle:     strings.TrimSpace(config.AppTitle),
		retrySleep:   retrySleep(config.RetrySleep),
		redactor:     security.NewRedactor(config.APIKey),
		providerName: providerName(config.ProviderName),
	}, nil
}

func providerName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "OpenRouter"
	}
	return name
}

// ChatCompletion sends messages to /chat/completions and returns the first
// assistant message. It uses the standard OpenAI-compatible JSON contract.
func (p *OpenRouterProvider) ChatCompletion(ctx context.Context, messages []Message) (string, error) {
	if p == nil {
		return "", errors.New("OpenRouter provider is nil")
	}
	if len(messages) == 0 {
		return "", errors.New("chat completion requires at least one message")
	}
	var lastErr error
	for attempt := 0; attempt <= 3; attempt++ {
		content, retryAfter, err := p.chatCompletionAttempt(ctx, messages)
		if err == nil {
			return content, nil
		}
		lastErr = err
		if !isTransientProviderError(ctx, err) || attempt == 3 {
			break
		}
		delay := retryDelay(attempt, retryAfter)
		if err := p.retrySleep(ctx, delay); err != nil {
			return "", fmt.Errorf("wait before OpenRouter retry: %w", err)
		}
	}
	return "", p.sanitizeError(lastErr)
}

func (p *OpenRouterProvider) sanitizeError(err error) error {
	if err == nil {
		return nil
	}
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		copy := *providerErr
		copy.Provider = p.providerName
		err = &copy
	} else {
		err = &ProviderError{
			Message:  err.Error(),
			Provider: p.providerName,
			Cause:    err,
		}
	}
	return p.redactor.Wrap("", err)
}

func (p *OpenRouterProvider) chatCompletionAttempt(ctx context.Context, messages []Message) (string, time.Duration, error) {
	requestBody := chatCompletionRequest{
		Model:       p.model,
		Messages:    messages,
		Temperature: 0.3,
		Stream:      false,
	}
	payload, err := json.Marshal(requestBody)
	if err != nil {
		return "", 0, fmt.Errorf("marshal OpenRouter request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", 0, fmt.Errorf("create OpenRouter request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+p.apiKey)
	request.Header.Set("Content-Type", "application/json")
	if p.httpReferer != "" {
		request.Header.Set("HTTP-Referer", p.httpReferer)
	}
	if p.appTitle != "" {
		request.Header.Set("X-OpenRouter-Title", p.appTitle)
	}

	response, err := p.httpClient.Do(request)
	if err != nil {
		return "", 0, fmt.Errorf("OpenRouter request: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", 0, fmt.Errorf("read OpenRouter response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var apiError chatCompletionResponse
		if json.Unmarshal(body, &apiError) == nil && apiError.Error != nil && apiError.Error.Message != "" {
			return "", retryAfter(response), &ProviderError{
				StatusCode: response.StatusCode,
				Message:    apiError.Error.Message,
			}
		}
		return "", retryAfter(response), &ProviderError{StatusCode: response.StatusCode}
	}

	var decoded chatCompletionResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", 0, fmt.Errorf("decode OpenRouter response: %w", err)
	}
	if decoded.Error != nil && decoded.Error.Message != "" {
		return "", 0, &ProviderError{StatusCode: response.StatusCode, Message: decoded.Error.Message}
	}
	if len(decoded.Choices) == 0 {
		return "", 0, errors.New("OpenRouter response contained no choices")
	}
	content := strings.TrimSpace(decoded.Choices[0].Message.Content)
	if content == "" {
		return "", 0, errors.New("OpenRouter response contained empty content")
	}
	return content, 0, nil
}

// Summarize implements Provider using one batch chat completion request.
func (p *OpenRouterProvider) Summarize(ctx context.Context, posts []Post) ([]Summary, error) {
	if len(posts) == 0 {
		return []Summary{}, nil
	}
	summaries := make([]Summary, 0, len(posts))
	for start := 0; start < len(posts); start += defaultMaxPostsPerBatch {
		end := start + defaultMaxPostsPerBatch
		if end > len(posts) {
			end = len(posts)
		}
		batch, err := p.summarizeBatch(ctx, posts[start:end])
		if err != nil {
			return nil, fmt.Errorf("summarize posts %d-%d: %w", start+1, end, err)
		}
		summaries = append(summaries, batch...)
	}
	return summaries, nil
}

func (p *OpenRouterProvider) summarizeBatch(ctx context.Context, posts []Post) ([]Summary, error) {
	var prompt strings.Builder
	prompt.WriteString("Прочитай посты из Telegram и подготовь для каждого ровно одно короткое предложение на русском языке. ")
	prompt.WriteString("Не добавляй факты от себя и не объединяй посты. Верни только JSON-массив объектов с полями post_id и summary, сохраняя идентификаторы постов.\n\n")
	for _, post := range posts {
		fmt.Fprintf(&prompt, "--- ПОСТ %d ---\n%s\n\n", post.ID, truncatePostText(post.Text, defaultMaxPostChars))
	}
	content, err := p.ChatCompletion(ctx, []Message{
		{Role: "system", Content: "Ты — точный редактор русскоязычных Telegram-дайджестов. Отвечай только на русском языке, готовь ровно одно предложение для каждого поста."},
		{Role: "user", Content: prompt.String()},
	})
	if err != nil {
		return nil, fmt.Errorf("summarize posts: %w", err)
	}
	parsed, err := parseBatchSummaries(content)
	if err != nil {
		log.Printf("AI response unparseable: %v", err)
		return nil, fmt.Errorf("parse summaries: %w", err)
	}

	expected := make(map[int64]struct{}, len(posts))
	for _, post := range posts {
		expected[post.ID] = struct{}{}
	}
	byID := make(map[int64]string, len(parsed))
	for _, item := range parsed {
		text := strings.TrimSpace(item.Summary)
		if _, ok := expected[item.PostID]; !ok {
			log.Printf("AI returned hallucinated summary for unknown post %d; discarding", item.PostID)
			continue
		}
		if _, duplicate := byID[item.PostID]; duplicate {
			log.Printf("AI returned duplicate summary for post %d; discarding", item.PostID)
			continue
		}
		if err := validateSummaryText(text); err != nil {
			return nil, fmt.Errorf("summary for post %d: %w", item.PostID, err)
		}
		byID[item.PostID] = text
	}
	if len(byID) != len(posts) {
		log.Printf("AI returned %d summaries for %d posts: WARNING: %d posts not summarized", len(byID), len(posts), len(posts)-len(byID))
	} else {
		log.Printf("AI returned %d summaries for %d posts - OK", len(byID), len(posts))
	}
	summaries := make([]Summary, 0, len(posts))
	for _, post := range posts {
		text := byID[post.ID]
		if text == "" {
			text = fallbackSummary
		}
		summaries = append(summaries, Summary{PostID: post.ID, Text: text})
	}
	return summaries, nil
}

func validateSummaryText(text string) error {
	if text == "" {
		return errors.New("summary is empty")
	}
	var cyrillic, letters int
	for _, r := range text {
		if unicode.IsLetter(r) {
			letters++
			if unicode.In(r, unicode.Cyrillic) {
				cyrillic++
			}
		}
	}
	if cyrillic == 0 || (letters > 8 && float64(cyrillic)/float64(letters) < 0.25) {
		return errors.New("summary is not in Russian")
	}
	if sentenceCount(text) > 1 {
		return errors.New("summary must contain exactly one sentence")
	}
	return nil
}

func sentenceCount(text string) int {
	runes := []rune(strings.TrimSpace(text))
	count := 0
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		if r == '.' {
			if i+1 < len(runes) && unicode.IsDigit(runes[i+1]) {
				continue
			}
			next := i + 1
			for next < len(runes) && unicode.IsSpace(runes[next]) {
				next++
			}
			if next < len(runes) && unicode.IsLower(runes[next]) {
				continue
			}
		}
		count++
		for i+1 < len(runes) && (runes[i+1] == '.' || runes[i+1] == '!' || runes[i+1] == '?') {
			i++
		}
	}
	if count == 0 {
		return 1
	}
	return count
}

func truncatePostText(text string, maxChars int) string {
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	if maxChars <= 3 {
		return string(runes[:maxChars])
	}
	return string(runes[:maxChars-3]) + "..."
}

func parseBatchSummaries(content string) ([]batchSummary, error) {
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "```") {
		content = strings.TrimSpace(strings.TrimPrefix(content, "```json"))
		content = strings.TrimSpace(strings.TrimPrefix(content, "```"))
		content = strings.TrimSpace(strings.TrimSuffix(content, "```"))
	}

	var summaries []batchSummary
	if err := json.Unmarshal([]byte(content), &summaries); err == nil {
		return summaries, nil
	}
	var envelope struct {
		Summaries []batchSummary `json:"summaries"`
	}
	if err := json.Unmarshal([]byte(content), &envelope); err != nil {
		return nil, fmt.Errorf("expected JSON summary array: %w", err)
	}
	if envelope.Summaries == nil {
		return nil, errors.New("summary response did not contain summaries")
	}
	return envelope.Summaries, nil
}

// ProviderError describes an HTTP or transport failure returned by an AI
// provider.
type ProviderError struct {
	StatusCode int
	Message    string
	Provider   string
	Cause      error
}

func (e *ProviderError) Error() string {
	if e == nil {
		return "AI provider error"
	}
	providerName := e.Provider
	if providerName == "" {
		providerName = "OpenRouter"
	}
	if e.StatusCode <= 0 {
		if e.Message == "" {
			return fmt.Sprintf("%s chat completion transport failure", providerName)
		}
		return fmt.Sprintf("%s chat completion transport failure: %s", providerName, e.Message)
	}
	if e.Message == "" {
		return fmt.Sprintf("%s chat completion: HTTP %d", providerName, e.StatusCode)
	}
	return fmt.Sprintf("%s chat completion: HTTP %d: %s", providerName, e.StatusCode, e.Message)
}

// Unwrap keeps the underlying transport error available for retry and
// classification while the provider identity remains attached to the error.
func (e *ProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func isTransientProviderError(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		if providerErr.StatusCode == http.StatusRequestTimeout ||
			providerErr.StatusCode == http.StatusTooManyRequests ||
			providerErr.StatusCode >= http.StatusInternalServerError {
			return true
		}
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && (networkErr.Timeout() || networkErr.Temporary()) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var operationErr *net.OpError
	return errors.As(err, &operationErr)
}

func retryAfter(response *http.Response) time.Duration {
	value := strings.TrimSpace(response.Header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func retryDelay(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > maxRetryDelay {
			return maxRetryDelay
		}
		return retryAfter
	}
	delay := time.Second * time.Duration(1<<attempt)
	if delay > maxRetryDelay {
		return maxRetryDelay
	}
	return delay
}

func retrySleep(sleep func(context.Context, time.Duration) error) func(context.Context, time.Duration) error {
	if sleep == nil {
		return sleepWithContext
	}
	return sleep
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
