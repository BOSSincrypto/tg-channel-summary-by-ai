package db

import (
	"errors"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

func TestResolveAIConfigUsesDefaultProviderAndProviderModel(t *testing.T) {
	store, cleanup := newTestDB(t)
	defer cleanup()

	providerID, err := store.Providers.Insert(&model.AIProvider{
		Name:         "Default",
		BaseURL:      "https://example.test/v1",
		APIKey:       "default-key",
		DefaultModel: "default-model",
		IsDefault:    true,
	})
	if err != nil {
		t.Fatalf("insert default provider: %v", err)
	}
	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -1001, Title: "Default group"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}

	config, err := store.Groups.ResolveAIConfig(groupID)
	if err != nil {
		t.Fatalf("resolve group AI config: %v", err)
	}
	if config.Provider.ID != providerID {
		t.Fatalf("provider ID = %d, want %d", config.Provider.ID, providerID)
	}
	if config.Model != "default-model" {
		t.Fatalf("model = %q, want provider default model", config.Model)
	}
}

func TestResolveAIConfigUsesAssignedProviderAndGroupModelOverride(t *testing.T) {
	store, cleanup := newTestDB(t)
	defer cleanup()

	if _, err := store.Providers.Insert(&model.AIProvider{
		Name:         "Default",
		BaseURL:      "https://default.example/v1",
		APIKey:       "default-key",
		DefaultModel: "default-model",
		IsDefault:    true,
	}); err != nil {
		t.Fatalf("insert default provider: %v", err)
	}
	assignedID, err := store.Providers.Insert(&model.AIProvider{
		Name:         "Assigned",
		BaseURL:      "https://assigned.example/v1",
		APIKey:       "assigned-key",
		DefaultModel: "assigned-model",
	})
	if err != nil {
		t.Fatalf("insert assigned provider: %v", err)
	}
	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -1002, Title: "Assigned group"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	override := "group-model"
	if err := store.Groups.UpdateGroupSettings(&model.GroupSettings{
		GroupID:    groupID,
		ProviderID: &assignedID,
		Model:      &override,
		DigestTime: "21:00",
		Timezone:   "Europe/Moscow",
	}); err != nil {
		t.Fatalf("update group settings: %v", err)
	}

	config, err := store.Groups.ResolveAIConfig(groupID)
	if err != nil {
		t.Fatalf("resolve group AI config: %v", err)
	}
	if config.Provider.ID != assignedID {
		t.Fatalf("provider ID = %d, want assigned provider %d", config.Provider.ID, assignedID)
	}
	if config.Model != override {
		t.Fatalf("model = %q, want group override %q", config.Model, override)
	}
}

func TestResolveAIConfigRejectsGroupWithoutDefaultProvider(t *testing.T) {
	store, cleanup := newTestDB(t)
	defer cleanup()

	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -1003, Title: "Unconfigured group"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}

	_, err = store.Groups.ResolveAIConfig(groupID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve error = %v, want ErrNotFound", err)
	}
}
