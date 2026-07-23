package db

import (
	"strings"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

func TestProviderEncryptionSurvivesDatabaseReopen(t *testing.T) {
	path := t.TempDir() + "\\providers.db"
	const encryptionMaterial = "stable-unit-material"

	store, err := OpenWithEncryptionKey(path, encryptionMaterial)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	id, err := store.Providers.Insert(&model.AIProvider{
		Name: "Stable Provider", BaseURL: "https://provider.invalid/v1",
		APIKey: "value-for-reopen-test", DefaultModel: "unit-model",
	})
	if err != nil {
		store.Close()
		t.Fatalf("insert provider: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	reopened, err := OpenWithEncryptionKey(path, encryptionMaterial)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer reopened.Close()
	provider, err := reopened.Providers.GetByID(id)
	if err != nil {
		t.Fatalf("read provider after reopen: %v", err)
	}
	if provider.APIKey != "value-for-reopen-test" {
		t.Fatalf("decrypted API key = %q, want original value", provider.APIKey)
	}
}

func TestPlaintextProviderKeyIsMigratedOnOpen(t *testing.T) {
	path := t.TempDir() + "\\legacy-providers.db"
	store, err := OpenWithEncryptionKey(path, "legacy-unit-material")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	id, err := store.Providers.Insert(&model.AIProvider{
		Name: "Legacy Provider", BaseURL: "https://provider.invalid/v1",
		APIKey: "value-for-migration-test", DefaultModel: "unit-model",
	})
	if err != nil {
		store.Close()
		t.Fatalf("insert provider: %v", err)
	}
	if _, err := store.Conn().Exec("UPDATE ai_providers SET api_key = ? WHERE id = ?", "value-for-migration-test", id); err != nil {
		store.Close()
		t.Fatalf("seed legacy provider row: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	reopened, err := OpenWithEncryptionKey(path, "legacy-unit-material")
	if err != nil {
		t.Fatalf("reopen legacy database: %v", err)
	}
	defer reopened.Close()

	var stored string
	if err := reopened.Conn().QueryRow("SELECT api_key FROM ai_providers WHERE id = ?", id).Scan(&stored); err != nil {
		t.Fatalf("read migrated provider row: %v", err)
	}
	if !strings.HasPrefix(stored, encryptedAPIKeyPrefix) {
		t.Fatalf("provider key remained unencrypted: %q", stored)
	}
	provider, err := reopened.Providers.GetByID(id)
	if err != nil {
		t.Fatalf("read migrated provider: %v", err)
	}
	if provider.APIKey != "value-for-migration-test" {
		t.Fatalf("migrated API key = %q, want original value", provider.APIKey)
	}
}

func TestLegacyBotTokenProviderKeysMigrateToExplicitKey(t *testing.T) {
	path := t.TempDir() + "\\legacy-token-providers.db"
	const legacyToken = "legacy-bot-token"
	const explicitKey = "stable-provider-encryption-key"

	t.Setenv("PROVIDER_ENCRYPTION_KEY", "")
	t.Setenv("BOT_TOKEN", legacyToken)
	store, err := OpenWithEncryptionKey(path, legacyToken)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	id, err := store.Providers.Insert(&model.AIProvider{
		Name: "Migrated Provider", BaseURL: "https://provider.invalid/v1",
		APIKey: "value-for-key-rotation", DefaultModel: "unit-model",
	})
	if err != nil {
		store.Close()
		t.Fatalf("insert provider: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	t.Setenv("PROVIDER_ENCRYPTION_KEY", explicitKey)
	t.Setenv("BOT_TOKEN", legacyToken)
	migrated, err := OpenWithEncryptionKey(path, explicitKey)
	if err != nil {
		t.Fatalf("migrate legacy provider key: %v", err)
	}
	defer migrated.Close()

	provider, err := migrated.Providers.GetByID(id)
	if err != nil {
		t.Fatalf("read migrated provider: %v", err)
	}
	if provider.APIKey != "value-for-key-rotation" {
		t.Fatalf("migrated API key = %q, want original value", provider.APIKey)
	}
	var stored string
	if err := migrated.Conn().QueryRow("SELECT api_key FROM ai_providers WHERE id = ?", id).Scan(&stored); err != nil {
		t.Fatalf("read migrated ciphertext: %v", err)
	}
	if !strings.HasPrefix(stored, encryptedAPIKeyPrefix) {
		t.Fatalf("migrated provider key is not encrypted: %q", stored)
	}
	if _, err := migrated.Providers.GetByID(id); err != nil {
		t.Fatalf("read provider with explicit key after migration: %v", err)
	}
}

func TestProviderKeyMigrationFailsClosedForWrongKey(t *testing.T) {
	path := t.TempDir() + "\\wrong-key-providers.db"
	const legacyKey = "legacy-provider-key"

	t.Setenv("PROVIDER_ENCRYPTION_KEY", "")
	t.Setenv("BOT_TOKEN", "")
	store, err := OpenWithEncryptionKey(path, legacyKey)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	id, err := store.Providers.Insert(&model.AIProvider{
		Name: "Wrong Key Provider", BaseURL: "https://provider.invalid/v1",
		APIKey: "value-that-must-remain", DefaultModel: "unit-model",
	})
	if err != nil {
		store.Close()
		t.Fatalf("insert provider: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	if _, err := OpenWithEncryptionKey(path, "wrong-provider-key"); err == nil {
		t.Fatal("opening with wrong provider key succeeded")
	} else if !strings.Contains(err.Error(), "PROVIDER_ENCRYPTION_KEY_PREVIOUS") {
		t.Fatalf("wrong-key error = %v, want actionable migration guidance", err)
	}

	reopened, err := OpenWithEncryptionKey(path, legacyKey)
	if err != nil {
		t.Fatalf("reopen with legacy key after failed migration: %v", err)
	}
	defer reopened.Close()
	provider, err := reopened.Providers.GetByID(id)
	if err != nil {
		t.Fatalf("read provider after failed migration: %v", err)
	}
	if provider.APIKey != "value-that-must-remain" {
		t.Fatalf("provider key after failed migration = %q, want original value", provider.APIKey)
	}
}

func TestProviderKeyMigrationSurvivesBotTokenRotation(t *testing.T) {
	path := t.TempDir() + "\\rotated-token-providers.db"
	const legacyToken = "legacy-token-before-rotation"
	const explicitKey = "stable-key-after-rotation"

	t.Setenv("PROVIDER_ENCRYPTION_KEY", "")
	t.Setenv("BOT_TOKEN", legacyToken)
	store, err := OpenWithEncryptionKey(path, legacyToken)
	if err != nil {
		t.Fatalf("open pre-rotation database: %v", err)
	}
	if _, err := store.Providers.Insert(&model.AIProvider{
		Name: "Rotated Token Provider", BaseURL: "https://provider.invalid/v1",
		APIKey: "value-surviving-token-rotation", DefaultModel: "unit-model",
	}); err != nil {
		store.Close()
		t.Fatalf("insert provider: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close pre-rotation database: %v", err)
	}

	t.Setenv("PROVIDER_ENCRYPTION_KEY", explicitKey)
	t.Setenv("BOT_TOKEN", legacyToken)
	migrated, err := OpenWithEncryptionKey(path, explicitKey)
	if err != nil {
		t.Fatalf("open during key migration: %v", err)
	}
	if err := migrated.Close(); err != nil {
		t.Fatalf("close migrated database: %v", err)
	}

	t.Setenv("BOT_TOKEN", "rotated-bot-token")
	rotated, err := OpenWithEncryptionKey(path, explicitKey)
	if err != nil {
		t.Fatalf("open after bot token rotation: %v", err)
	}
	defer rotated.Close()
	providers, err := rotated.Providers.List()
	if err != nil {
		t.Fatalf("list providers after bot token rotation: %v", err)
	}
	if len(providers) != 1 || providers[0].APIKey != "value-surviving-token-rotation" {
		t.Fatalf("providers after bot token rotation = %+v", providers)
	}
}
