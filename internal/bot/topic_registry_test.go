package bot

import (
	"context"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/mymmrac/telego"
)

func TestProductionTelegramTopicUpdatePersistsUnassignedObservedTopic(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	if _, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -100301,
		Title:          "Observed Forum",
		Status:         model.GroupStatusActive,
	}); err != nil {
		t.Fatalf("insert group: %v", err)
	}
	service := newServiceForTest(&fakeTelegramClient{}, nil)
	service.groups = store.Groups
	service.SetForumTopicRegistry(store.ForumTopics)

	update := telego.Update{Message: &telego.Message{
		MessageID:       1701,
		MessageThreadID: 1701,
		Chat:            telego.Chat{ID: -100301, Type: telego.ChatTypeSupergroup},
		ForumTopicCreated: &telego.ForumTopicCreated{
			Name: "Existing unassigned",
		},
	}}
	if err := service.HandleUpdate(context.Background(), &update); err != nil {
		t.Fatalf("handle topic update: %v", err)
	}
	group, err := store.Groups.GetByChatID(-100301)
	if err != nil {
		t.Fatalf("load group: %v", err)
	}
	topic, err := store.ForumTopics.Get(group.ID, 1701)
	if err != nil {
		t.Fatalf("load observed topic: %v", err)
	}
	if topic.Name != "Existing unassigned" || topic.LifecycleOwned {
		t.Fatalf("observed topic = %#v", topic)
	}
	if assignments, err := store.Groups.GetChannelAssignments(group.ID); err != nil {
		t.Fatalf("load assignments: %v", err)
	} else if len(assignments) != 0 {
		t.Fatalf("topic update created assignment rows = %#v", assignments)
	}
}

func TestOnlyLifecycleOwnedTopicIsClosedOnUnassignment(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -100302,
		Title:          "Forum",
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	observedChannelID, err := store.Channels.Insert(&model.Channel{Username: "observed"})
	if err != nil {
		t.Fatalf("insert observed channel: %v", err)
	}
	ownedChannelID, err := store.Channels.Insert(&model.Channel{Username: "owned"})
	if err != nil {
		t.Fatalf("insert owned channel: %v", err)
	}
	observedThread := int64(1801)
	ownedThread := int64(1802)
	if err := store.Groups.AssignChannel(groupID, observedChannelID, &observedThread); err != nil {
		t.Fatalf("assign observed channel: %v", err)
	}
	if err := store.ForumTopics.Observe(groupID, observedThread, "Observed"); err != nil {
		t.Fatalf("observe topic: %v", err)
	}
	if err := store.Groups.AssignChannel(groupID, ownedChannelID, &ownedThread); err != nil {
		t.Fatalf("assign owned channel: %v", err)
	}
	if err := store.ForumTopics.PersistOwned(groupID, ownedThread, "Owned"); err != nil {
		t.Fatalf("persist owned topic: %v", err)
	}

	api := &fakeTelegramClient{}
	service := newServiceForTest(api, nil)
	service.groups = store.Groups
	service.SetForumTopicRegistry(store.ForumTopics)
	if err := service.RemoveChannelTopic(context.Background(), groupID, observedChannelID); err != nil {
		t.Fatalf("remove observed assignment: %v", err)
	}
	if len(api.closedTopics) != 0 {
		t.Fatalf("observed topic close calls = %#v, want none", api.closedTopics)
	}
	if err := service.RemoveChannelTopic(context.Background(), groupID, ownedChannelID); err != nil {
		t.Fatalf("remove owned assignment: %v", err)
	}
	if len(api.closedTopics) != 1 || api.closedTopics[0].MessageThreadID != int(ownedThread) {
		t.Fatalf("owned topic close calls = %#v", api.closedTopics)
	}
	topic, err := store.ForumTopics.Get(groupID, ownedThread)
	if err != nil {
		t.Fatalf("load closed owned topic: %v", err)
	}
	if !topic.Closed {
		t.Fatalf("owned topic state = %#v, want closed", topic)
	}
}

func TestCreateChannelTopicPersistsLifecycleOwnership(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -100303,
		Title:          "Forum",
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	channelID, err := store.Channels.Insert(&model.Channel{Username: "owned_topic", Title: "Owned topic"})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	if err := store.Groups.AssignChannel(groupID, channelID, nil); err != nil {
		t.Fatalf("assign channel: %v", err)
	}
	api := &fakeTelegramClient{forumTopic: &telego.ForumTopic{MessageThreadID: 1901, Name: "Owned topic"}}
	service := newServiceForTest(api, nil)
	service.groups = store.Groups
	service.channels = store.Channels
	service.SetForumTopicRegistry(store.ForumTopics)
	if err := service.CreateChannelTopic(context.Background(), groupID, channelID); err != nil {
		t.Fatalf("create topic: %v", err)
	}
	topic, err := store.ForumTopics.Get(groupID, 1901)
	if err != nil {
		t.Fatalf("load persisted topic: %v", err)
	}
	if !topic.LifecycleOwned || topic.Status != model.ForumTopicStatusPersisted {
		t.Fatalf("persisted topic = %#v", topic)
	}
}
