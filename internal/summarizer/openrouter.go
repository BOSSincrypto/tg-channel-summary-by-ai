package summarizer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultOpenRouterBaseURL is the OpenAI-compatible OpenRouter API base URL.
	DefaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"
	// DefaultOpenRouterModel is the model selected for the default provider.
	DefaultOpenRouterModel = "openai/gpt-oss-120b"
)

// OpenRouterConfig configures an OpenRouterProvider.
type OpenRouterConfig struct {
	BaseURL     string
	APIKey      string
	Model       string
	HTTPClient  *http.Client
	HTTPReferer string
	AppTitle    string
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
	baseURL     string
	apiKey      string
	model       string
	httpClient  *http.Client
	httpReferer string
	appTitle    string
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
		baseURL:    DefaultOpenRouterBaseURL,
		apiKey:     apiKey,
		model:      model,
		httpClient: &http.Client{Timeout: 60 * time.Second},
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
		baseURL:     baseURL,
		apiKey:      config.APIKey,
		model:       model,
		httpClient:  client,
		httpReferer: strings.TrimSpace(config.HTTPReferer),
		appTitle:    strings.TrimSpace(config.AppTitle),
	}, nil
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
	requestBody := chatCompletionRequest{
		Model:       p.model,
		Messages:    messages,
		Temperature: 0.3,
		Stream:      false,
	}
	payload, err := json.Marshal(requestBody)
	if err != nil {
		return "", fmt.Errorf("marshal OpenRouter request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("create OpenRouter request: %w", err)
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
		return "", fmt.Errorf("OpenRouter request: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read OpenRouter response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var apiError chatCompletionResponse
		if json.Unmarshal(body, &apiError) == nil && apiError.Error != nil && apiError.Error.Message != "" {
			return "", fmt.Errorf("OpenRouter chat completion: HTTP %d: %s", response.StatusCode, apiError.Error.Message)
		}
		return "", fmt.Errorf("OpenRouter chat completion: HTTP %d", response.StatusCode)
	}

	var decoded chatCompletionResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", fmt.Errorf("decode OpenRouter response: %w", err)
	}
	if decoded.Error != nil && decoded.Error.Message != "" {
		return "", fmt.Errorf("OpenRouter chat completion: %s", decoded.Error.Message)
	}
	if len(decoded.Choices) == 0 {
		return "", errors.New("OpenRouter response contained no choices")
	}
	content := strings.TrimSpace(decoded.Choices[0].Message.Content)
	if content == "" {
		return "", errors.New("OpenRouter response contained empty content")
	}
	return content, nil
}

// Summarize implements Provider using one batch chat completion request.
func (p *OpenRouterProvider) Summarize(ctx context.Context, posts []Post) ([]Summary, error) {
	if len(posts) == 0 {
		return []Summary{}, nil
	}
	var prompt strings.Builder
	prompt.WriteString("Прочитай посты из Telegram и подготовь для каждого ровно одно предложение на русском языке. ")
	prompt.WriteString("Верни только JSON-массив объектов с полями post_id и summary, сохраняя идентификаторы постов.\n\n")
	for _, post := range posts {
		fmt.Fprintf(&prompt, "--- ПОСТ %d ---\n%s\n\n", post.ID, post.Text)
	}
	content, err := p.ChatCompletion(ctx, []Message{
		{Role: "system", Content: "Ты — точный редактор русскоязычных Telegram-дайджестов."},
		{Role: "user", Content: prompt.String()},
	})
	if err != nil {
		return nil, fmt.Errorf("summarize posts: %w", err)
	}
	parsed, err := parseBatchSummaries(content)
	if err != nil {
		return nil, fmt.Errorf("parse summaries: %w", err)
	}
	summaries := make([]Summary, 0, len(parsed))
	for _, item := range parsed {
		if strings.TrimSpace(item.Summary) == "" {
			return nil, fmt.Errorf("summary for post %d is empty", item.PostID)
		}
		summaries = append(summaries, Summary{PostID: item.PostID, Text: strings.TrimSpace(item.Summary)})
	}
	return summaries, nil
}

func parseBatchSummaries(content string) ([]batchSummary, error) {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(strings.TrimSpace(content), "```")

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
