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
