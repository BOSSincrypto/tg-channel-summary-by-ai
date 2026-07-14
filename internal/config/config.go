// Package config loads and validates configuration from environment
// variables (.env file). It provides a typed Config struct used across
// all components via dependency injection.
package config

// Config holds all configuration values for the application.
type Config struct {
	BotToken        string
	OwnerTelegramID string
	OpenRouterKey   string
	CustomProviders string
	DigestTime      string
	Timezone        string
	Port            string
	DBPath          string
	LogLevel        string
	FetchDelayMs    int
	MaxRetries      int
	MaxPostsPerChan int
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	// TODO: parse .env file, validate required fields
	return &Config{}, nil
}
