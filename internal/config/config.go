// Package config loads and validates configuration from environment
// variables (.env file). It provides a typed Config struct used across
// all components via dependency injection.
package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Config holds all configuration values for the application.
type Config struct {
	BotToken          string
	OwnerTelegramID   string
	OpenRouterKey     string
	ProviderKey       string
	CustomProviders   string
	DigestTime        string
	Timezone          string
	Port              string
	DBPath            string
	LogLevel          string
	FetchDelayMs      int
	MaxRetries        int
	MaxPostsPerChan   int
	PostRetentionDays int
}

// Load reads configuration from the .env file in the current directory,
// if present, and applies environment variable overrides. Missing .env is
// tolerated so production can start from environment-only Fly secrets.
func Load() (*Config, error) {
	f, err := os.Open(".env")
	if err != nil {
		if os.IsNotExist(err) {
			return Parse(strings.NewReader(""))
		}
		return nil, fmt.Errorf("open .env: %w", err)
	}
	defer f.Close()
	return Parse(f)
}

// Parse reads configuration from an io.Reader in KEY=VALUE format.
// Environment variables override values from the input.
// Required fields: BOT_TOKEN, OWNER_TELEGRAM_ID, OPENROUTER_API_KEY.
func Parse(r io.Reader) (*Config, error) {
	values, err := readValues(r)
	if err != nil {
		return nil, err
	}

	// Environment variable overrides
	for _, key := range allKeys {
		if envVal, ok := os.LookupEnv(key); ok && envVal != "" {
			values[key] = envVal
		}
	}

	cfg := &Config{}

	// Required fields
	cfg.BotToken = values["BOT_TOKEN"]
	if cfg.BotToken == "" {
		return nil, fmt.Errorf("BOT_TOKEN is required but not set")
	}

	cfg.OwnerTelegramID = values["OWNER_TELEGRAM_ID"]
	if cfg.OwnerTelegramID == "" {
		return nil, fmt.Errorf("OWNER_TELEGRAM_ID is required but not set")
	}

	cfg.OpenRouterKey = values["OPENROUTER_API_KEY"]
	if cfg.OpenRouterKey == "" {
		return nil, fmt.Errorf("OPENROUTER_API_KEY is required but not set")
	}

	// Optional fields with defaults
	cfg.DigestTime = stringDefault(values, "DIGEST_TIME", "21:00")
	cfg.Timezone = stringDefault(values, "TIMEZONE", "Europe/Moscow")
	cfg.Port = stringDefault(values, "PORT", "8080")
	cfg.DBPath = stringDefault(values, "DB_PATH", "bot.db")
	cfg.LogLevel = stringDefault(values, "LOG_LEVEL", "info")
	cfg.CustomProviders = values["CUSTOM_PROVIDERS"] // may be empty
	cfg.ProviderKey = stringDefault(values, "PROVIDER_ENCRYPTION_KEY", cfg.BotToken)

	cfg.FetchDelayMs = intDefault(values, "FETCH_DELAY_MS", 2500)
	cfg.MaxRetries = intDefault(values, "MAX_RETRIES", 3)
	cfg.MaxPostsPerChan = intDefault(values, "MAX_POSTS_PER_CHANNEL", 50)
	cfg.PostRetentionDays = positiveIntDefault(values, "POST_RETENTION_DAYS", 90)

	return cfg, nil
}

// allKeys lists every config key known to the application.
var allKeys = []string{
	"BOT_TOKEN",
	"OWNER_TELEGRAM_ID",
	"OPENROUTER_API_KEY",
	"PROVIDER_ENCRYPTION_KEY",
	"CUSTOM_PROVIDERS",
	"DIGEST_TIME",
	"TIMEZONE",
	"PORT",
	"DB_PATH",
	"LOG_LEVEL",
	"FETCH_DELAY_MS",
	"MAX_RETRIES",
	"MAX_POSTS_PER_CHANNEL",
	"POST_RETENTION_DAYS",
}

// readValues parses KEY=VALUE lines from an io.Reader.
// It handles:
//   - Comments (lines starting with #)
//   - Empty lines
//   - Inline comments (# after value but before it, preceded by space)
//   - Quoted values (strips surrounding double quotes)
//   - Whitespace around the = sign
//   - CRLF line endings
func readValues(r io.Reader) (map[string]string, error) {
	values := make(map[string]string)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and full-line comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Split on first = sign
		eqIdx := strings.Index(line, "=")
		if eqIdx < 0 {
			continue // malformed line, skip
		}

		key := strings.TrimSpace(line[:eqIdx])
		if key == "" {
			continue
		}

		val := strings.TrimSpace(line[eqIdx+1:])

		// Strip inline comments (a # preceded by a space)
		if hashIdx := strings.Index(val, " #"); hashIdx >= 0 {
			// Only strip if the # has whitespace before it (to allow # in values)
			val = strings.TrimSpace(val[:hashIdx])
		} else if hashIdx := strings.Index(val, "\t#"); hashIdx >= 0 {
			val = strings.TrimSpace(val[:hashIdx])
		}

		// Strip surrounding double quotes
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}

		values[key] = val
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read env: %w", err)
	}

	return values, nil
}

// stringDefault returns the value for key, or def if empty/missing.
func stringDefault(values map[string]string, key, def string) string {
	if v, ok := values[key]; ok && v != "" {
		return v
	}
	return def
}

// intDefault parses the value for key as an int, returning def on parse failure
// or if the key is missing/empty.
func intDefault(values map[string]string, key string, def int) int {
	v, ok := values[key]
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// positiveIntDefault parses a positive integer value for key, returning def on
// parse failure, zero, negative values, or if the key is missing/empty.
func positiveIntDefault(values map[string]string, key string, def int) int {
	n := intDefault(values, key, def)
	if n <= 0 {
		return def
	}
	return n
}
