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

func TestForumTopicRegistryPersistsPendingCloseAndHidesItFromCatalog(t *testing.T) {
	store, cleanup := newTestDB(t)
	defer cleanup()
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -100205,
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := store.ForumTopics.PersistOwned(groupID, 905, "Pending"); err != nil {
		t.Fatalf("persist owned topic: %v", err)
	}

	if err := store.ForumTopics.BeginClose(groupID, 905); err != nil {
		t.Fatalf("begin close: %v", err)
	}
	topic, err := store.ForumTopics.Get(groupID, 905)
	if err != nil {
		t.Fatalf("get pending topic: %v", err)
	}
	if !topic.ClosePending || topic.Closed {
		t.Fatalf("pending topic = %#v, want pending and open", topic)
	}
	topics, err := store.ForumTopics.ListOpen(groupID)
	if err != nil {
		t.Fatalf("list open topics: %v", err)
	}
	if len(topics) != 0 {
		t.Fatalf("open topics = %#v, want pending topic hidden", topics)
	}

	if err := store.ForumTopics.MarkClosed(groupID, 905); err != nil {
		t.Fatalf("mark closed: %v", err)
	}
	topic, err = store.ForumTopics.Get(groupID, 905)
	if err != nil {
		t.Fatalf("get closed topic: %v", err)
	}
	if !topic.Closed || topic.ClosePending {
		t.Fatalf("closed topic = %#v, want finalized", topic)
	}
}

func TestForumTopicBeginCloseRejectsSurvivingAssignment(t *testing.T) {
	store, cleanup := newTestDB(t)
	defer cleanup()
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -100206,
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	channelID, err := store.Channels.Insert(&model.Channel{Username: "surviving_assignment"})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	threadID := int64(906)
	if err := store.ForumTopics.PersistOwned(groupID, threadID, "Surviving assignment"); err != nil {
		t.Fatalf("persist owned topic: %v", err)
	}
	if err := store.Groups.AssignChannel(groupID, channelID, &threadID); err != nil {
		t.Fatalf("assign topic: %v", err)
	}

	if err := store.ForumTopics.BeginClose(groupID, threadID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("begin close with surviving assignment = %v, want ErrNotFound", err)
	}
	topic, err := store.ForumTopics.Get(groupID, threadID)
	if err != nil {
		t.Fatalf("get topic after rejected close: %v", err)
	}
	if topic.ClosePending || topic.Closed {
		t.Fatalf("topic after rejected close = %#v, want open and not pending", topic)
	}
}

func TestAssignChannelRejectsPendingOrClosedTopic(t *testing.T) {
	store, cleanup := newTestDB(t)
	defer cleanup()
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -100207,
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	channelID, err := store.Channels.Insert(&model.Channel{Username: "blocked_topic"})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	threadID := int64(907)
	if err := store.ForumTopics.PersistOwned(groupID, threadID, "Blocked topic"); err != nil {
		t.Fatalf("persist topic: %v", err)
	}
	for _, state := range []string{"pending", "closed"} {
		t.Run(state, func(t *testing.T) {
			if state == "pending" {
				if err := store.ForumTopics.BeginClose(groupID, threadID); err != nil {
					t.Fatalf("begin close: %v", err)
				}
			} else {
				if err := store.ForumTopics.MarkClosed(groupID, threadID); err != nil {
					t.Fatalf("mark closed: %v", err)
				}
			}
			if err := store.Groups.AssignChannel(groupID, channelID, &threadID); !errors.Is(err, ErrConflict) {
				t.Fatalf("assign %s topic = %v, want ErrConflict", state, err)
			}
			if err := store.ForumTopics.MarkReopened(groupID, threadID); err != nil {
				t.Fatalf("reset topic: %v", err)
			}
		})
	}
}
