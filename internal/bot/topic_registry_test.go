package bot

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/webapp"
	"github.com/mymmrac/telego"
)

type markClosedFailureRegistry struct {
	*db.ForumTopicRepository
	failMarkClosed bool
}

func (r *markClosedFailureRegistry) MarkClosed(groupID, threadID int64) error {
	if r.failMarkClosed {
		return errors.New("injected MarkClosed failure")
	}
	return r.ForumTopicRepository.MarkClosed(groupID, threadID)
}

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

func TestProductionTopicCloseFailureLeavesDurablePendingRecovery(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -100304,
		Title:          "Forum",
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	channelID, err := store.Channels.Insert(&model.Channel{Username: "pending_close"})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	threadID := int64(1902)
	if err := store.Groups.AssignChannel(groupID, channelID, &threadID); err != nil {
		t.Fatalf("assign channel: %v", err)
	}
	if err := store.ForumTopics.PersistOwned(groupID, threadID, "Pending close"); err != nil {
		t.Fatalf("persist topic: %v", err)
	}

	api := &fakeTelegramClient{}
	registry := &markClosedFailureRegistry{
		ForumTopicRepository: store.ForumTopics,
		failMarkClosed:       true,
	}
	service := newServiceForTest(api, nil)
	service.groups = store.Groups
	service.SetForumTopicRegistry(registry)

	if err := service.RemoveChannelTopic(context.Background(), groupID, channelID); err == nil {
		t.Fatal("remove topic succeeded despite injected MarkClosed failure")
	}
	if len(api.closedTopics) != 1 || api.closedTopics[0].MessageThreadID != int(threadID) {
		t.Fatalf("close calls = %#v, want one successful close", api.closedTopics)
	}
	assignments, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("load assignments after close: %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("assignments after close failure = %#v, want removed", assignments)
	}
	topic, err := store.ForumTopics.Get(groupID, threadID)
	if err != nil {
		t.Fatalf("load pending topic: %v", err)
	}
	if !topic.ClosePending || topic.Closed {
		t.Fatalf("topic after MarkClosed failure = %#v, want durable pending state", topic)
	}
	openTopics, err := store.ForumTopics.ListOpen(groupID)
	if err != nil {
		t.Fatalf("list open topics: %v", err)
	}
	if len(openTopics) != 0 {
		t.Fatalf("open topics after MarkClosed failure = %#v, want none", openTopics)
	}

	registry.failMarkClosed = false
	if err := service.ReconcilePendingTopicClosures(context.Background()); err != nil {
		t.Fatalf("reconcile pending close: %v", err)
	}
	if len(api.closedTopics) != 2 {
		t.Fatalf("reconciliation close calls = %d, want idempotent retry", len(api.closedTopics))
	}
	topic, err = store.ForumTopics.Get(groupID, threadID)
	if err != nil {
		t.Fatalf("load reconciled topic: %v", err)
	}
	if !topic.Closed || topic.ClosePending {
		t.Fatalf("topic after reconciliation = %#v, want closed and finalized", topic)
	}
	if err := service.ReconcilePendingTopicClosures(context.Background()); err != nil {
		t.Fatalf("repeat reconciliation: %v", err)
	}
	if len(api.closedTopics) != 2 {
		t.Fatalf("repeat reconciliation close calls = %d, want idempotent no-op", len(api.closedTopics))
	}
}

func TestPendingCloseReconciliationCancelsWhenAssignmentPersistenceFails(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -100305,
		Title:          "Forum",
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	channelID, err := store.Channels.Insert(&model.Channel{Username: "assignment_failure"})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	threadID := int64(1903)
	if err := store.Groups.AssignChannel(groupID, channelID, &threadID); err != nil {
		t.Fatalf("assign channel: %v", err)
	}
	if err := store.ForumTopics.PersistOwned(groupID, threadID, "Assignment failure"); err != nil {
		t.Fatalf("persist topic: %v", err)
	}
	if _, err := store.Conn().Exec(`
		CREATE TRIGGER reject_topic_unassignment
		BEFORE DELETE ON group_channels
		BEGIN
			SELECT RAISE(ABORT, 'unassignment persistence failure');
		END`); err != nil {
		t.Fatalf("create unassignment trigger: %v", err)
	}

	api := &fakeTelegramClient{}
	service := newServiceForTest(api, nil)
	service.groups = store.Groups
	service.SetForumTopicRegistry(store.ForumTopics)
	if err := service.RemoveChannelTopic(context.Background(), groupID, channelID); err == nil {
		t.Fatal("remove topic succeeded despite assignment persistence failure")
	}
	if len(api.closedTopics) != 0 {
		t.Fatalf("close calls after assignment failure = %#v, want none", api.closedTopics)
	}
	topic, err := store.ForumTopics.Get(groupID, threadID)
	if err != nil {
		t.Fatalf("load pending topic: %v", err)
	}
	if topic.ClosePending || topic.Closed {
		t.Fatalf("topic after assignment failure = %#v, want open and not pending", topic)
	}
	assignments, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("load surviving assignment: %v", err)
	}
	if len(assignments) != 1 || assignments[0].TopicThreadID == nil ||
		*assignments[0].TopicThreadID != threadID {
		t.Fatalf("surviving assignment = %#v", assignments)
	}

	if _, err := store.Conn().Exec(`
		UPDATE forum_topics
		SET close_pending = 1
		WHERE group_id = ? AND message_thread_id = ?`, groupID, threadID); err != nil {
		t.Fatalf("seed legacy pending close: %v", err)
	}
	if err := service.ReconcilePendingTopicClosures(context.Background()); err != nil {
		t.Fatalf("reconcile surviving assignment: %v", err)
	}
	if len(api.closedTopics) != 0 {
		t.Fatalf("close calls during assignment reconciliation = %#v, want none", api.closedTopics)
	}
	topic, err = store.ForumTopics.Get(groupID, threadID)
	if err != nil {
		t.Fatalf("load reopened pending topic: %v", err)
	}
	if topic.ClosePending || topic.Closed {
		t.Fatalf("topic after assignment reconciliation = %#v, want open and not pending", topic)
	}
	if _, err := store.Conn().Exec(`DROP TRIGGER reject_topic_unassignment`); err != nil {
		t.Fatalf("drop unassignment trigger: %v", err)
	}
	if err := service.RemoveChannelTopic(context.Background(), groupID, channelID); err != nil {
		t.Fatalf("remove topic after persistence recovery: %v", err)
	}
	if len(api.closedTopics) != 1 {
		t.Fatalf("close calls after persistence recovery = %d, want one", len(api.closedTopics))
	}
}

func TestProductionWebAppUsesRealBotTopicRemovalBoundary(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -100306,
		Title:          "WebApp forum",
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	channelID, err := store.Channels.Insert(&model.Channel{
		Username: "webapp_real_boundary",
		Title:    "WebApp real boundary",
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	threadID := int64(1904)
	if err := store.ForumTopics.PersistOwned(groupID, threadID, "WebApp real boundary"); err != nil {
		t.Fatalf("persist topic: %v", err)
	}
	api := &fakeTelegramClient{}
	registry := &markClosedFailureRegistry{
		ForumTopicRepository: store.ForumTopics,
		failMarkClosed:       true,
	}
	service := newServiceForTest(api, nil)
	service.groups = store.Groups
	service.SetForumTopicRegistry(registry)
	server := webapp.NewWithProvidersForTesting(store, 0, http.DefaultClient)
	server.SetTopicLifecycle(service)

	assignment := httptest.NewRequest(http.MethodPost,
		"/api/groups/"+strconv.FormatInt(groupID, 10)+"/channels",
		strings.NewReader(`{"channel_id":"`+strconv.FormatInt(channelID, 10)+`","topic_thread_id":`+
			strconv.FormatInt(threadID, 10)+`,"version":1}`))
	assignment.Header.Set("Content-Type", "application/json")
	assignmentResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(assignmentResponse, assignment)
	if assignmentResponse.Code != http.StatusCreated {
		t.Fatalf("assignment response = %d, body=%s", assignmentResponse.Code, assignmentResponse.Body.String())
	}

	removal := httptest.NewRequest(http.MethodDelete,
		"/api/groups/"+strconv.FormatInt(groupID, 10)+"/channels/"+strconv.FormatInt(channelID, 10),
		strings.NewReader(`{"version":2}`))
	removal.Header.Set("Content-Type", "application/json")
	removalResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(removalResponse, removal)
	if removalResponse.Code != http.StatusBadGateway {
		t.Fatalf("removal response = %d, body=%s", removalResponse.Code, removalResponse.Body.String())
	}
	if len(api.closedTopics) != 1 || api.closedTopics[0].MessageThreadID != int(threadID) {
		t.Fatalf("real boundary close calls = %#v, want one close", api.closedTopics)
	}
	assignments, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("load assignments after WebApp removal: %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("WebApp assignments after removal = %#v, want none", assignments)
	}
	topic, err := store.ForumTopics.Get(groupID, threadID)
	if err != nil {
		t.Fatalf("load pending WebApp topic: %v", err)
	}
	if !topic.ClosePending || topic.Closed {
		t.Fatalf("WebApp topic after MarkClosed failure = %#v, want pending", topic)
	}

	registry.failMarkClosed = false
	if err := service.ReconcilePendingTopicClosures(context.Background()); err != nil {
		t.Fatalf("reconcile WebApp topic: %v", err)
	}
	topic, err = store.ForumTopics.Get(groupID, threadID)
	if err != nil {
		t.Fatalf("load reconciled WebApp topic: %v", err)
	}
	if !topic.Closed || topic.ClosePending {
		t.Fatalf("WebApp topic after reconciliation = %#v, want closed", topic)
	}
	if err := service.ReconcilePendingTopicClosures(context.Background()); err != nil {
		t.Fatalf("repeat reconcile WebApp topic: %v", err)
	}
	if len(api.closedTopics) != 2 {
		t.Fatalf("repeat WebApp close calls = %d, want idempotent recovery", len(api.closedTopics))
	}
}
