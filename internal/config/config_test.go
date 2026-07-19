package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func chdirToTempDir(t *testing.T) {
	t.Helper()

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir temp dir: %v", err)
	}

	t.Cleanup(func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
}

func TestLoad_UsesDotEnvWhenPresent(t *testing.T) {
	chdirToTempDir(t)

	if err := os.WriteFile(".env", []byte("BOT_TOKEN=file-token\nOWNER_TELEGRAM_ID=111\nOPENROUTER_API_KEY=file-openrouter\nDB_PATH=/data/bot.db\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	t.Setenv("BOT_TOKEN", "")
	t.Setenv("OWNER_TELEGRAM_ID", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("DB_PATH", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.BotToken != "file-token" {
		t.Errorf("BotToken = %q, want %q", cfg.BotToken, "file-token")
	}
	if cfg.OwnerTelegramID != "111" {
		t.Errorf("OwnerTelegramID = %q, want %q", cfg.OwnerTelegramID, "111")
	}
	if cfg.OpenRouterKey != "file-openrouter" {
		t.Errorf("OpenRouterKey = %q, want %q", cfg.OpenRouterKey, "file-openrouter")
	}
	if cfg.DBPath != "/data/bot.db" {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, "/data/bot.db")
	}
}

func TestLoad_MissingDotEnvUsesEnvironmentValues(t *testing.T) {
	chdirToTempDir(t)

	t.Setenv("BOT_TOKEN", "env-token")
	t.Setenv("OWNER_TELEGRAM_ID", "222")
	t.Setenv("OPENROUTER_API_KEY", "env-openrouter")
	t.Setenv("DB_PATH", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.BotToken != "env-token" {
		t.Errorf("BotToken = %q, want %q", cfg.BotToken, "env-token")
	}
	if cfg.OwnerTelegramID != "222" {
		t.Errorf("OwnerTelegramID = %q, want %q", cfg.OwnerTelegramID, "222")
	}
	if cfg.OpenRouterKey != "env-openrouter" {
		t.Errorf("OpenRouterKey = %q, want %q", cfg.OpenRouterKey, "env-openrouter")
	}
	if cfg.DBPath != "bot.db" {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, "bot.db")
	}
}

func TestLoad_MissingDotEnvUsesProvidedDBPath(t *testing.T) {
	chdirToTempDir(t)

	t.Setenv("BOT_TOKEN", "env-token")
	t.Setenv("OWNER_TELEGRAM_ID", "222")
	t.Setenv("OPENROUTER_API_KEY", "env-openrouter")
	t.Setenv("DB_PATH", "/data/bot.db")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.DBPath != "/data/bot.db" {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, "/data/bot.db")
	}
}

func TestParse_AllRequiredFields(t *testing.T) {
	t.Setenv("WEBAPP_URL", "")
	input := strings.NewReader(`
BOT_TOKEN=test_bot_token_123
OWNER_TELEGRAM_ID=123456789
OPENROUTER_API_KEY=test-openrouter-placeholder
`)
	cfg, err := Parse(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BotToken != "test_bot_token_123" {
		t.Errorf("BotToken = %q, want %q", cfg.BotToken, "test_bot_token_123")
	}
	if cfg.OwnerTelegramID != "123456789" {
		t.Errorf("OwnerTelegramID = %q, want %q", cfg.OwnerTelegramID, "123456789")
	}
	if cfg.OpenRouterKey != "test-openrouter-placeholder" {
		t.Errorf("OpenRouterKey = %q, want %q", cfg.OpenRouterKey, "test-openrouter-placeholder")
	}
	if cfg.WebAppURL != "https://tg-channel-summary.fly.dev/webapp/" {
		t.Errorf("WebAppURL = %q, want default HTTPS URL", cfg.WebAppURL)
	}
}

func TestParse_WebAppURLCanBeConfigured(t *testing.T) {
	t.Setenv("WEBAPP_URL", "")
	input := strings.NewReader(`
BOT_TOKEN=test
OWNER_TELEGRAM_ID=123
OPENROUTER_API_KEY=test
WEBAPP_URL=https://settings.example.test/
`)
	cfg, err := Parse(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.WebAppURL != "https://settings.example.test/" {
		t.Fatalf("WebAppURL = %q, want configured URL", cfg.WebAppURL)
	}
}

func TestParse_MissingBotToken(t *testing.T) {
	input := strings.NewReader(`
OWNER_TELEGRAM_ID=123456789
OPENROUTER_API_KEY=test
`)
	_, err := Parse(input)
	if err == nil {
		t.Fatal("expected error for missing BOT_TOKEN, got nil")
	}
	if !strings.Contains(err.Error(), "BOT_TOKEN") {
		t.Errorf("error should mention BOT_TOKEN, got: %v", err)
	}
}

func TestParse_MissingOwnerTelegramID(t *testing.T) {
	input := strings.NewReader(`
BOT_TOKEN=test_token
OPENROUTER_API_KEY=test
`)
	_, err := Parse(input)
	if err == nil {
		t.Fatal("expected error for missing OWNER_TELEGRAM_ID, got nil")
	}
	if !strings.Contains(err.Error(), "OWNER_TELEGRAM_ID") {
		t.Errorf("error should mention OWNER_TELEGRAM_ID, got: %v", err)
	}
}

func TestParse_MissingOpenRouterKey(t *testing.T) {
	input := strings.NewReader(`
BOT_TOKEN=test_token
OWNER_TELEGRAM_ID=123
`)
	_, err := Parse(input)
	if err == nil {
		t.Fatal("expected error for missing OPENROUTER_API_KEY, got nil")
	}
	if !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Errorf("error should mention OPENROUTER_API_KEY, got: %v", err)
	}
}

func TestParse_EmptyBotToken(t *testing.T) {
	input := strings.NewReader(`
BOT_TOKEN=
OWNER_TELEGRAM_ID=123
OPENROUTER_API_KEY=test
`)
	_, err := Parse(input)
	if err == nil {
		t.Fatal("expected error for empty BOT_TOKEN, got nil")
	}
}

func TestParse_EmptyOwnerTelegramID(t *testing.T) {
	input := strings.NewReader(`
BOT_TOKEN=test
OWNER_TELEGRAM_ID=
OPENROUTER_API_KEY=test
`)
	_, err := Parse(input)
	if err == nil {
		t.Fatal("expected error for empty OWNER_TELEGRAM_ID, got nil")
	}
}

func TestParse_InvalidOwnerTelegramID(t *testing.T) {
	input := strings.NewReader(`
BOT_TOKEN=test
OWNER_TELEGRAM_ID=not-a-number
OPENROUTER_API_KEY=test
`)
	_, err := Parse(input)
	if err == nil || !strings.Contains(err.Error(), "OWNER_TELEGRAM_ID") {
		t.Fatalf("error = %v, want invalid OWNER_TELEGRAM_ID error", err)
	}
}

func TestParse_EmptyOpenRouterKey(t *testing.T) {
	input := strings.NewReader(`
BOT_TOKEN=test
OWNER_TELEGRAM_ID=123
OPENROUTER_API_KEY=
`)
	_, err := Parse(input)
	if err == nil {
		t.Fatal("expected error for empty OPENROUTER_API_KEY, got nil")
	}
}

func TestParse_DefaultValues(t *testing.T) {
	input := strings.NewReader(`
BOT_TOKEN=test
OWNER_TELEGRAM_ID=123
OPENROUTER_API_KEY=test
`)
	cfg, err := Parse(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DigestTime != "21:00" {
		t.Errorf("DigestTime = %q, want %q", cfg.DigestTime, "21:00")
	}
	if cfg.Timezone != "Europe/Moscow" {
		t.Errorf("Timezone = %q, want %q", cfg.Timezone, "Europe/Moscow")
	}
	if cfg.WebAppURL != "https://tg-channel-summary.fly.dev/webapp/" {
		t.Errorf("WebAppURL = %q, want default HTTPS URL", cfg.WebAppURL)
	}
	if cfg.Port != "8080" {
		t.Errorf("Port = %q, want %q", cfg.Port, "8080")
	}
	if cfg.DBPath != "bot.db" {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, "bot.db")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
	if cfg.FetchDelayMs != 2500 {
		t.Errorf("FetchDelayMs = %d, want %d", cfg.FetchDelayMs, 2500)
	}
	if cfg.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d, want %d", cfg.MaxRetries, 3)
	}
	if cfg.MaxPostsPerChan != 50 {
		t.Errorf("MaxPostsPerChan = %d, want %d", cfg.MaxPostsPerChan, 50)
	}
	if cfg.PostRetentionDays != 90 {
		t.Errorf("PostRetentionDays = %d, want %d", cfg.PostRetentionDays, 90)
	}
}

func TestParse_AllOptionalFields(t *testing.T) {
	input := strings.NewReader(`
BOT_TOKEN=test
OWNER_TELEGRAM_ID=123
OPENROUTER_API_KEY=test
DIGEST_TIME=18:00
TIMEZONE=Asia/Tokyo
PORT=9090
DB_PATH=/data/mybot.db
LOG_LEVEL=debug
FETCH_DELAY_MS=5000
MAX_RETRIES=10
MAX_POSTS_PER_CHANNEL=200
POST_RETENTION_DAYS=45
CUSTOM_PROVIDERS=[{"name":"p1"}]
`)
	cfg, err := Parse(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DigestTime != "18:00" {
		t.Errorf("DigestTime = %q, want %q", cfg.DigestTime, "18:00")
	}
	if cfg.Timezone != "Asia/Tokyo" {
		t.Errorf("Timezone = %q, want %q", cfg.Timezone, "Asia/Tokyo")
	}
	if cfg.Port != "9090" {
		t.Errorf("Port = %q, want %q", cfg.Port, "9090")
	}
	if cfg.DBPath != "/data/mybot.db" {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, "/data/mybot.db")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
	}
	if cfg.FetchDelayMs != 5000 {
		t.Errorf("FetchDelayMs = %d, want %d", cfg.FetchDelayMs, 5000)
	}
	if cfg.MaxRetries != 10 {
		t.Errorf("MaxRetries = %d, want %d", cfg.MaxRetries, 10)
	}
	if cfg.MaxPostsPerChan != 200 {
		t.Errorf("MaxPostsPerChan = %d, want %d", cfg.MaxPostsPerChan, 200)
	}
	if cfg.PostRetentionDays != 45 {
		t.Errorf("PostRetentionDays = %d, want %d", cfg.PostRetentionDays, 45)
	}
	if cfg.CustomProviders != `[{"name":"p1"}]` {
		t.Errorf("CustomProviders = %q, want %q", cfg.CustomProviders, `[{"name":"p1"}]`)
	}
}

func TestParse_CommentsAndEmptyLines(t *testing.T) {
	input := strings.NewReader(`
# This is a comment
BOT_TOKEN=test_token

# Another comment
OWNER_TELEGRAM_ID=123456

OPENROUTER_API_KEY=test_key
# trailing comment line
`)
	cfg, err := Parse(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BotToken != "test_token" {
		t.Errorf("BotToken = %q, want %q", cfg.BotToken, "test_token")
	}
	if cfg.OwnerTelegramID != "123456" {
		t.Errorf("OwnerTelegramID = %q, want %q", cfg.OwnerTelegramID, "123456")
	}
}

func TestParse_WhitespaceAroundEquals(t *testing.T) {
	input := strings.NewReader(`
BOT_TOKEN = my_token
OWNER_TELEGRAM_ID = 999
OPENROUTER_API_KEY = test-key-whitespace
`)
	cfg, err := Parse(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BotToken != "my_token" {
		t.Errorf("BotToken = %q, want %q", cfg.BotToken, "my_token")
	}
	if cfg.OwnerTelegramID != "999" {
		t.Errorf("OwnerTelegramID = %q, want %q", cfg.OwnerTelegramID, "999")
	}
}

func TestParse_InlineComments(t *testing.T) {
	input := strings.NewReader(`
BOT_TOKEN=my_token   # my bot token
OWNER_TELEGRAM_ID=123456
OPENROUTER_API_KEY=test-key-inline # the key
`)
	cfg, err := Parse(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BotToken != "my_token" {
		t.Errorf("BotToken = %q, want %q", cfg.BotToken, "my_token")
	}
	if cfg.OpenRouterKey != "test-key-inline" {
		t.Errorf("OpenRouterKey = %q, want %q", cfg.OpenRouterKey, "test-key-inline")
	}
}

func TestParse_InvalidNumericValue(t *testing.T) {
	input := strings.NewReader(`
BOT_TOKEN=test
OWNER_TELEGRAM_ID=123
OPENROUTER_API_KEY=test
FETCH_DELAY_MS=notanumber
POST_RETENTION_DAYS=0
`)
	cfg, err := Parse(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Invalid numeric values should fall back to defaults.
	if cfg.FetchDelayMs != 2500 {
		t.Errorf("FetchDelayMs = %d, want %d (default)", cfg.FetchDelayMs, 2500)
	}
	if cfg.PostRetentionDays != 90 {
		t.Errorf("PostRetentionDays = %d, want %d (default)", cfg.PostRetentionDays, 90)
	}
}

func TestParse_EmptyInput(t *testing.T) {
	input := strings.NewReader("")
	_, err := Parse(input)
	if err == nil {
		t.Fatal("expected error for empty input, got nil")
	}
}

func TestParse_QuotedValues(t *testing.T) {
	input := strings.NewReader(`
BOT_TOKEN="quoted_token"
OWNER_TELEGRAM_ID=123
OPENROUTER_API_KEY="test-quoted-value"
`)
	cfg, err := Parse(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BotToken != "quoted_token" {
		t.Errorf("BotToken = %q, want %q", cfg.BotToken, "quoted_token")
	}
	if cfg.OpenRouterKey != "test-quoted-value" {
		t.Errorf("OpenRouterKey = %q, want %q", cfg.OpenRouterKey, "test-quoted-value")
	}
}

func TestParse_ValuesWithEquals(t *testing.T) {
	input := strings.NewReader(`
BOT_TOKEN=abc=def=ghi
OWNER_TELEGRAM_ID=123
OPENROUTER_API_KEY=key=with=equals
`)
	cfg, err := Parse(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BotToken != "abc=def=ghi" {
		t.Errorf("BotToken = %q, want %q", cfg.BotToken, "abc=def=ghi")
	}
	if cfg.OpenRouterKey != "key=with=equals" {
		t.Errorf("OpenRouterKey = %q, want %q", cfg.OpenRouterKey, "key=with=equals")
	}
}

func TestParse_CRLFLineEndings(t *testing.T) {
	// Simulate Windows-style line endings
	input := strings.NewReader("BOT_TOKEN=test\r\nOWNER_TELEGRAM_ID=123\r\nOPENROUTER_API_KEY=test\r\n")
	cfg, err := Parse(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BotToken != "test" {
		t.Errorf("BotToken = %q, want %q", cfg.BotToken, "test")
	}
}

func TestLoadValidator_RequiresExplicitFakeCredentialsAndTempDatabase(t *testing.T) {
	chdirToTempDir(t)
	t.Setenv("VALIDATOR_HTTP_ONLY", "1")
	t.Setenv("BOT_TOKEN", "validator:fake")
	t.Setenv("OWNER_TELEGRAM_ID", "715602446")
	t.Setenv("OPENROUTER_API_KEY", "validator-openrouter-key")
	t.Setenv("PROVIDER_ENCRYPTION_KEY", "")
	t.Setenv("CUSTOM_PROVIDERS", "")
	dbPath := filepath.Join(os.TempDir(), "tg-channel-summary-validator-test.sqlite")
	t.Setenv("DB_PATH", dbPath)
	t.Setenv("PORT", "9999")

	if err := os.WriteFile(".env", []byte("BOT_TOKEN=production-token\nOPENROUTER_API_KEY=production-key\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	cfg, err := LoadValidator()
	if err != nil {
		t.Fatalf("load validator config: %v", err)
	}
	if cfg.BotToken != "validator:fake" || cfg.OpenRouterKey != "validator-openrouter-key" {
		t.Fatalf("validator credentials came from an unsafe source: bot=%q openrouter=%q", cfg.BotToken, cfg.OpenRouterKey)
	}
	if cfg.Port != "8080" {
		t.Fatalf("validator port = %q, want 8080", cfg.Port)
	}
	if cfg.DBPath != dbPath {
		t.Fatalf("validator DB path = %q, want %q", cfg.DBPath, dbPath)
	}
	if cfg.WebAppURL != "http://localhost:8080/webapp/" {
		t.Fatalf("validator WebApp URL = %q, want local HTTP origin", cfg.WebAppURL)
	}
	if cfg.ProviderKey != cfg.BotToken {
		t.Fatalf("validator provider encryption key was not replaced with fake bot credential")
	}
}

func TestLoadValidatorRejectsProductionCredentialShape(t *testing.T) {
	chdirToTempDir(t)
	t.Setenv("VALIDATOR_HTTP_ONLY", "1")
	t.Setenv("BOT_TOKEN", "123456:production-token")
	t.Setenv("OWNER_TELEGRAM_ID", "715602446")
	t.Setenv("OPENROUTER_API_KEY", "sk-or-v1-production-key")
	t.Setenv("DB_PATH", filepath.Join(os.TempDir(), "tg-channel-summary-validator-test.sqlite"))

	if _, err := LoadValidator(); err == nil {
		t.Fatal("expected validator mode to reject production-shaped credentials")
	}
}

func TestLoadValidatorRequiresExplicitOptIn(t *testing.T) {
	t.Setenv("VALIDATOR_HTTP_ONLY", "")
	if _, err := LoadValidator(); err == nil {
		t.Fatal("expected validator config to require VALIDATOR_HTTP_ONLY=1")
	}
}
