package main

import (
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
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
