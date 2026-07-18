package db

import (
	"errors"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

func TestProviderNamesAreUniqueCaseInsensitively(t *testing.T) {
	store, cleanup := newTestDB(t)
	defer cleanup()

	if _, err := store.Providers.Insert(&model.AIProvider{
		Name: "OpenAI", BaseURL: "https://example.test/v1", APIKey: "key-a", DefaultModel: "model",
	}); err != nil {
		t.Fatalf("insert first provider: %v", err)
	}
	if _, err := store.Providers.Insert(&model.AIProvider{
		Name: "openai", BaseURL: "https://example.test/v1", APIKey: "key-b", DefaultModel: "model",
	}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("case-insensitive duplicate error = %v, want ErrDuplicate", err)
	}
}
