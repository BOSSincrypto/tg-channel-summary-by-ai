package main

import (
	"context"
	"strings"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/bot"
	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/digest"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/scheduler"
	"github.com/boss/tg-channel-summary-by-ai/internal/summarizer"
)

func TestEnsureDefaultAIProviderSeedsOpenRouter(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	if err := ensureDefaultAIProvider(store, "startup-key"); err != nil {
		t.Fatalf("ensure default provider: %v", err)
	}
	provider, err := store.Providers.GetDefault()
	if err != nil {
		t.Fatalf("get default provider: %v", err)
	}
	if provider.Name != "OpenRouter" || provider.BaseURL != summarizer.DefaultOpenRouterBaseURL {
		t.Fatalf("default provider = %#v, want OpenRouter", provider)
	}
	if provider.APIKey != "startup-key" {
		t.Fatalf("default provider API key = %q, want startup key", provider.APIKey)
	}
	if provider.DefaultModel != summarizer.DefaultOpenRouterModel {
		t.Fatalf("default model = %q, want %q", provider.DefaultModel, summarizer.DefaultOpenRouterModel)
	}
}

func TestEnsureDefaultAIProviderDoesNotReplaceCustomDefault(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	if _, err := store.Providers.Insert(&model.AIProvider{
		Name:         "Custom",
		BaseURL:      "https://custom.example/v1",
		APIKey:       "custom-key",
		DefaultModel: "custom-model",
		IsDefault:    true,
	}); err != nil {
		t.Fatalf("insert custom provider: %v", err)
	}

	if err := ensureDefaultAIProvider(store, "startup-key"); err != nil {
		t.Fatalf("ensure default provider: %v", err)
	}
	provider, err := store.Providers.GetDefault()
	if err != nil {
		t.Fatalf("get default provider: %v", err)
	}
	if provider.Name != "Custom" || provider.APIKey != "custom-key" || provider.DefaultModel != "custom-model" {
		t.Fatalf("custom default was replaced: %#v", provider)
	}
}

func TestApplyProductionSettingsPersistsAndRefreshesScheduler(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -1001,
		Title:          "Production group",
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := store.Groups.UpdateGroupSettings(&model.GroupSettings{
		GroupID:    groupID,
		DigestTime: "21:00",
		Timezone:   "UTC",
	}); err != nil {
		t.Fatalf("update group settings: %v", err)
	}

	runner := &testDigestRunner{}
	sched := scheduler.New(runner, scheduler.WithGroupSource(store.Groups))
	if err := sched.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer sched.Stop()

	settings := bot.BotSettings{DigestTime: "09:30", Channels: []string{"@news"}}
	if err := applyProductionSettings(context.Background(), store, sched, settings); err != nil {
		t.Fatalf("apply production settings: %v", err)
	}

	groupSettings, err := store.Groups.GetGroupSettings(groupID)
	if err != nil {
		t.Fatalf("load group settings: %v", err)
	}
	if groupSettings.DigestTime != "09:30" || groupSettings.Timezone != "UTC" {
		t.Fatalf("group settings = %+v, want digest time 09:30 and preserved UTC", groupSettings)
	}
	value, err := store.Config.Get("webapp_settings")
	if err != nil {
		t.Fatalf("load persisted WebApp settings: %v", err)
	}
	if value == "" || !containsJSONValue(value, "09:30") || !containsJSONValue(value, "@news") {
		t.Fatalf("persisted WebApp settings = %q", value)
	}
	if got, ok := sched.ScheduleForGroup(groupID); !ok || got != "CRON_TZ=UTC 30 9 * * *" {
		t.Fatalf("scheduler schedule = %q, registered=%v", got, ok)
	}
}

type testDigestRunner struct{}

func (*testDigestRunner) Generate(int64) (*digest.Digest, error) {
	return &digest.Digest{}, nil
}

func containsJSONValue(value, expected string) bool {
	return strings.Contains(value, expected)
}
