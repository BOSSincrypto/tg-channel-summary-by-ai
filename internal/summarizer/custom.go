package summarizer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

// CustomProviderConfig configures an OpenAI-compatible custom provider.
// Custom endpoints use the same /chat/completions contract as OpenRouter.
type CustomProviderConfig = OpenRouterConfig

// CustomProvider is an OpenAI-compatible provider configured by an admin.
type CustomProvider = OpenRouterProvider

// GroupAIConfigSource supplies the effective provider and model assignment for
// a group. The database repository implements this interface.
type GroupAIConfigSource interface {
	ResolveAIConfig(groupID int64) (*model.GroupAIConfig, error)
}

// NewCustomProvider creates a provider for a custom OpenAI-compatible endpoint.
func NewCustomProvider(config CustomProviderConfig) (*CustomProvider, error) {
	if config.Model == "" {
		return nil, errors.New("custom provider model is required")
	}
	return NewOpenRouterWithConfig(OpenRouterConfig(config))
}

// ValidateCustomProvider performs a bounded test completion before a provider
// is persisted. The API key is never included in the returned error.
func ValidateCustomProvider(ctx context.Context, config CustomProviderConfig, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	provider, err := NewCustomProvider(config)
	if err != nil {
		return fmt.Errorf("custom provider configuration: %w", err)
	}
	testCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err = provider.ChatCompletion(testCtx, []Message{
		{Role: "user", Content: "Ответь одним словом: OK"},
	})
	if err != nil {
		return fmt.Errorf("custom provider test request failed: %w", sanitizeProviderError(err, config.APIKey))
	}
	return nil
}

func sanitizeProviderError(err error, apiKey string) error {
	if err == nil || apiKey == "" {
		return err
	}
	return errors.New(replaceSecret(err.Error(), apiKey))
}

func replaceSecret(value, secret string) string {
	if secret == "" {
		return value
	}
	result := value
	for {
		next := replaceFirst(result, secret, "[redacted]")
		if next == result {
			return result
		}
		result = next
	}
}

func replaceFirst(value, old, new string) string {
	for i := 0; i+len(old) <= len(value); i++ {
		if value[i:i+len(old)] == old {
			return value[:i] + new + value[i+len(old):]
		}
	}
	return value
}

// Ensure the custom provider remains usable anywhere a Provider is expected.
var _ Provider = (*CustomProvider)(nil)

// NewProviderFromConfig builds the correct OpenAI-compatible implementation
// for a persisted provider, allowing digest execution to route per group.
func NewProviderFromConfig(config model.AIProvider, client *http.Client) (Provider, error) {
	return newProviderFromConfig(config, "", client, false)
}

// NewProviderFromConfigForTesting permits loopback httptest endpoints without
// weakening the production provider factory's outbound network policy.
func NewProviderFromConfigForTesting(config model.AIProvider, client *http.Client) (Provider, error) {
	return newProviderFromConfig(config, "", client, true)
}

// NewProviderForGroup resolves a group's provider assignment and creates the
// provider with that group's model override when one is configured.
func NewProviderForGroup(source GroupAIConfigSource, groupID int64, client *http.Client) (Provider, error) {
	return newProviderForGroup(source, groupID, client, false)
}

// NewProviderForGroupForTesting permits loopback httptest endpoints while
// testing group routing without weakening production network restrictions.
func NewProviderForGroupForTesting(source GroupAIConfigSource, groupID int64, client *http.Client) (Provider, error) {
	return newProviderForGroup(source, groupID, client, true)
}

func newProviderForGroup(source GroupAIConfigSource, groupID int64, client *http.Client, allowPrivateHosts bool) (Provider, error) {
	if source == nil {
		return nil, errors.New("group AI config source is required")
	}
	config, err := source.ResolveAIConfig(groupID)
	if err != nil {
		return nil, fmt.Errorf("resolve AI provider for group %d: %w", groupID, err)
	}
	if config == nil {
		return nil, fmt.Errorf("resolve AI provider for group %d: configuration is nil", groupID)
	}
	return newProviderFromConfig(config.Provider, config.Model, client, allowPrivateHosts)
}

func newProviderFromConfig(config model.AIProvider, modelOverride string, client *http.Client, allowPrivateHosts bool) (Provider, error) {
	effectiveModel := strings.TrimSpace(modelOverride)
	if effectiveModel == "" {
		effectiveModel = config.DefaultModel
	}
	client = boundedProviderHTTPClient(client)
	if strings.TrimRight(config.BaseURL, "/") == DefaultOpenRouterBaseURL {
		return NewOpenRouterWithConfig(OpenRouterConfig{
			BaseURL: config.BaseURL, APIKey: config.APIKey, Model: effectiveModel,
			HTTPClient: client, AllowPrivateHosts: allowPrivateHosts,
		})
	}
	return NewCustomProvider(CustomProviderConfig{
		BaseURL: config.BaseURL, APIKey: config.APIKey, Model: effectiveModel,
		HTTPClient: client, AllowPrivateHosts: allowPrivateHosts,
	})
}

func boundedProviderHTTPClient(client *http.Client) *http.Client {
	if client == nil || client.Timeout > 0 {
		return client
	}
	clone := *client
	clone.Timeout = 60 * time.Second
	return &clone
}
