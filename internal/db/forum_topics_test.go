package db

import (
	"errors"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

func TestForumTopicRegistryPersistsObservedAndOwnedState(t *testing.T) {
	store, cleanup := newTestDB(t)
	defer cleanup()
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -100200,
		Title:          "Forum",
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}

	if err := store.ForumTopics.Observe(groupID, 901, "Existing topic"); err != nil {
		t.Fatalf("observe topic: %v", err)
	}
	topics, err := store.ForumTopics.ListOpen(groupID)
	if err != nil {
		t.Fatalf("list observed topic: %v", err)
	}
	if len(topics) != 1 || topics[0].MessageThreadID != 901 ||
		topics[0].Name != "Existing topic" ||
		topics[0].Status != model.ForumTopicStatusObserved ||
		topics[0].LifecycleOwned {
		t.Fatalf("observed topics = %#v", topics)
	}

	if err := store.ForumTopics.PersistOwned(groupID, 902, "Bot topic"); err != nil {
		t.Fatalf("persist owned topic: %v", err)
	}
	owned, err := store.ForumTopics.Get(groupID, 902)
	if err != nil {
		t.Fatalf("get owned topic: %v", err)
	}
	if !owned.LifecycleOwned || owned.Status != model.ForumTopicStatusPersisted {
		t.Fatalf("owned topic = %#v", owned)
	}

	if err := store.ForumTopics.MarkClosed(groupID, 901); err != nil {
		t.Fatalf("close observed topic: %v", err)
	}
	topics, err = store.ForumTopics.ListOpen(groupID)
	if err != nil {
		t.Fatalf("list after close: %v", err)
	}
	if len(topics) != 1 || topics[0].MessageThreadID != 902 {
		t.Fatalf("open topics after close = %#v", topics)
	}
	if err := store.ForumTopics.MarkReopened(groupID, 901); err != nil {
		t.Fatalf("reopen observed topic: %v", err)
	}
	if err := store.ForumTopics.MarkEdited(groupID, 901, "Renamed topic"); err != nil {
		t.Fatalf("edit observed topic: %v", err)
	}
	renamed, err := store.ForumTopics.Get(groupID, 901)
	if err != nil {
		t.Fatalf("get renamed topic: %v", err)
	}
	if renamed.Name != "Renamed topic" || renamed.Closed {
		t.Fatalf("renamed topic = %#v", renamed)
	}
}

func TestForumTopicRegistryDeletesOnlyLifecycleOwnedTopics(t *testing.T) {
	store, cleanup := newTestDB(t)
	defer cleanup()
	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -100201, Status: model.GroupStatusActive})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := store.ForumTopics.Observe(groupID, 903, "Observed"); err != nil {
		t.Fatalf("observe topic: %v", err)
	}
	if err := store.ForumTopics.DeleteOwned(groupID, 903); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete observed topic = %v, want ErrNotFound", err)
	}
	if _, err := store.ForumTopics.Get(groupID, 903); err != nil {
		t.Fatalf("observed topic disappeared: %v", err)
	}
	if err := store.ForumTopics.PersistOwned(groupID, 904, "Owned"); err != nil {
		t.Fatalf("persist owned topic: %v", err)
	}
	if err := store.ForumTopics.DeleteOwned(groupID, 904); err != nil {
		t.Fatalf("delete owned topic: %v", err)
	}
	if _, err := store.ForumTopics.Get(groupID, 904); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owned topic after delete = %v, want ErrNotFound", err)
	}
}
