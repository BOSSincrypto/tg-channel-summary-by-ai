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

func TestProductionTelegramEditedUnknownTopicSeedsObservedRegistry(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -100307,
		Title:          "Edited topic forum",
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	api := &fakeTelegramClient{}
	service := newServiceForTest(api, nil)
	service.groups = store.Groups
	service.SetForumTopicRegistry(store.ForumTopics)

	update := telego.Update{Message: &telego.Message{
		MessageID:        1702,
		MessageThreadID:  1702,
		Chat:             telego.Chat{ID: -100307, Type: telego.ChatTypeSupergroup},
		ForumTopicEdited: &telego.ForumTopicEdited{Name: "Edited unknown"},
	}}
	if err := service.HandleUpdate(context.Background(), &update); err != nil {
		t.Fatalf("handle edited topic update: %v", err)
	}
	if err := service.HandleUpdate(context.Background(), &update); err != nil {
		t.Fatalf("handle duplicate edited topic update: %v", err)
	}

	topic, err := store.ForumTopics.Get(groupID, 1702)
	if err != nil {
		t.Fatalf("load observed edited topic: %v", err)
	}
	if topic.Name != "Edited unknown" || topic.Status != model.ForumTopicStatusObserved ||
		topic.LifecycleOwned || topic.Closed || topic.ClosePending {
		t.Fatalf("observed edited topic = %#v", topic)
	}
	var count int
	if err := store.Conn().QueryRow(
		`SELECT COUNT(*) FROM forum_topics WHERE group_id = ? AND message_thread_id = ?`,
		groupID, 1702,
	).Scan(&count); err != nil {
		t.Fatalf("count observed edited topics: %v", err)
	}
	if count != 1 {
		t.Fatalf("observed edited topic count = %d, want one", count)
	}
	catalog := webapp.NewWithProvidersForTesting(store, 0, http.DefaultClient)
	catalogRequest := httptest.NewRequest(http.MethodGet,
		"/api/groups/"+strconv.FormatInt(groupID, 10)+"/topics", nil)
	catalogResponse := httptest.NewRecorder()
	catalog.Handler().ServeHTTP(catalogResponse, catalogRequest)
	if catalogResponse.Code != http.StatusOK {
		t.Fatalf("topic catalog status = %d, body=%s", catalogResponse.Code, catalogResponse.Body.String())
	}
	if !strings.Contains(catalogResponse.Body.String(), `"message_thread_id":1702`) ||
		!strings.Contains(catalogResponse.Body.String(), `"name":"Edited unknown"`) {
		t.Fatalf("topic catalog = %s, want observed edited topic", catalogResponse.Body.String())
	}
	if len(api.topics) != 0 || len(api.closedTopics) != 0 || len(api.deletedTopics) != 0 {
		t.Fatalf("edited topic lifecycle calls = created:%d closed:%d deleted:%d, want none",
			len(api.topics), len(api.closedTopics), len(api.deletedTopics))
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

func TestVersionedTopicAssignmentCompensatesAfterDurableFinalizeFailure(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -100308,
		Title:          "Transactional forum",
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	channelID, err := store.Channels.Insert(&model.Channel{Username: "transactional", Title: "Transactional"})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	api := &fakeTelegramClient{
		forumTopic: &telego.ForumTopic{MessageThreadID: 1951, Name: "Transactional"},
	}
	service := newServiceForTest(api, nil)
	service.groups = store.Groups
	service.channels = store.Channels
	service.SetForumTopicRegistry(store.ForumTopics)
	if _, err := store.Conn().Exec(`
		CREATE TRIGGER reject_owned_topic
		BEFORE INSERT ON forum_topics
		WHEN NEW.lifecycle_owned = 1
		BEGIN
			SELECT RAISE(ABORT, 'injected registry persistence failure');
		END`); err != nil {
		t.Fatalf("create registry failure trigger: %v", err)
	}

	if _, err := service.AssignChannelTopicWithVersion(
		context.Background(), groupID, channelID, nil, 1,
	); err == nil {
		t.Fatal("versioned assignment succeeded despite durable finalize failure")
	}
	group, err := store.Groups.GetByID(groupID)
	if err != nil {
		t.Fatalf("load group after compensation: %v", err)
	}
	if group.Version != 1 {
		t.Fatalf("group version after compensation = %d, want 1", group.Version)
	}
	assignments, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("load assignments after compensation: %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("assignments after compensation = %#v, want none", assignments)
	}
	if len(api.topics) != 1 || len(api.deletedTopics) != 1 {
		t.Fatalf("topic lifecycle calls = created:%d deleted:%d, want one each", len(api.topics), len(api.deletedTopics))
	}
	recovery, err := store.Groups.ListPendingTopicCreationRecoveries()
	if err != nil {
		t.Fatalf("list topic recovery: %v", err)
	}
	if len(recovery) != 0 {
		t.Fatalf("successful compensation left recovery rows = %#v", recovery)
	}
}

func TestVersionedTopicAssignmentRecordsRetryableCleanupAndReconciles(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -100309,
		Title:          "Recovery forum",
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	channelID, err := store.Channels.Insert(&model.Channel{Username: "recovery", Title: "Recovery"})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	api := &fakeTelegramClient{
		forumTopic: &telego.ForumTopic{MessageThreadID: 1952, Name: "Recovery"},
		deleteErr:  errors.New("external cleanup unavailable"),
	}
	service := newServiceForTest(api, nil)
	service.groups = store.Groups
	service.channels = store.Channels
	service.SetForumTopicRegistry(store.ForumTopics)
	if _, err := store.Conn().Exec(`
		CREATE TRIGGER reject_owned_topic
		BEFORE INSERT ON forum_topics
		WHEN NEW.lifecycle_owned = 1
		BEGIN
			SELECT RAISE(ABORT, 'injected registry persistence failure');
		END`); err != nil {
		t.Fatalf("create registry failure trigger: %v", err)
	}

	if _, err := service.AssignChannelTopicWithVersion(
		context.Background(), groupID, channelID, nil, 1,
	); err == nil {
		t.Fatal("versioned assignment succeeded despite durable finalize failure")
	}
	recovery, err := store.Groups.ListPendingTopicCreationRecoveries()
	if err != nil {
		t.Fatalf("list durable cleanup: %v", err)
	}
	if len(recovery) != 1 || recovery[0].MessageThreadID != 1952 {
		t.Fatalf("durable cleanup rows = %#v, want topic 1952", recovery)
	}
	group, err := store.Groups.GetByID(groupID)
	if err != nil {
		t.Fatalf("load group after failed cleanup: %v", err)
	}
	if group.Version != 1 {
		t.Fatalf("group version after failed cleanup = %d, want 1", group.Version)
	}
	api.deleteErr = nil
	if err := service.ReconcilePendingTopicCreations(context.Background()); err != nil {
		t.Fatalf("reconcile durable cleanup: %v", err)
	}
	recovery, err = store.Groups.ListPendingTopicCreationRecoveries()
	if err != nil {
		t.Fatalf("list cleanup after reconciliation: %v", err)
	}
	if len(recovery) != 0 {
		t.Fatalf("cleanup after reconciliation = %#v, want none", recovery)
	}
	if err := service.ReconcilePendingTopicCreations(context.Background()); err != nil {
		t.Fatalf("repeat reconciliation: %v", err)
	}
	if len(api.deletedTopics) != 2 {
		t.Fatalf("delete calls after retry = %d, want failed attempt plus retry", len(api.deletedTopics))
	}
}

func TestProductionWebAppConcurrentTopicAssignmentsSerializeByGroupVersion(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -100310,
		Title:          "Concurrent forum",
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	channelIDs := make([]int64, 2)
	for i, username := range []string{"concurrent_one", "concurrent_two"} {
		channelIDs[i], err = store.Channels.Insert(&model.Channel{
			Username: username,
			Title:    username,
			Enabled:  true,
		})
		if err != nil {
			t.Fatalf("insert channel %d: %v", i, err)
		}
	}
	api := &fakeTelegramClient{
		forumTopic: &telego.ForumTopic{MessageThreadID: 1960, Name: "Concurrent"},
	}
	service := newServiceForTest(api, nil)
	service.groups = store.Groups
	service.channels = store.Channels
	service.SetForumTopicRegistry(store.ForumTopics)
	server := webapp.NewWithProvidersForTesting(store, 0, http.DefaultClient)
	server.SetTopicLifecycle(service)

	type result struct {
		status int
		body   string
	}
	results := make(chan result, len(channelIDs))
	for _, channelID := range channelIDs {
		go func(channelID int64) {
			request := httptest.NewRequest(http.MethodPost,
				"/api/groups/"+strconv.FormatInt(groupID, 10)+"/channels",
				strings.NewReader(`{"channel_id":"`+strconv.FormatInt(channelID, 10)+`","version":1}`))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			results <- result{status: response.Code, body: response.Body.String()}
		}(channelID)
	}
	var created, conflicts int
	for range channelIDs {
		response := <-results
		switch response.status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("concurrent assignment status = %d, body=%s", response.status, response.body)
		}
	}
	if created != 1 || conflicts != 1 {
		t.Fatalf("concurrent assignment outcomes = created:%d conflicts:%d, want one each", created, conflicts)
	}
	if len(api.topics) != 1 {
		t.Fatalf("created Telegram topics = %d, want one", len(api.topics))
	}
	assignments, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("load concurrent assignments: %v", err)
	}
	if len(assignments) != 1 || assignments[0].TopicThreadID == nil ||
		*assignments[0].TopicThreadID != 1960 {
		t.Fatalf("concurrent assignments = %#v, want one valid topic assignment", assignments)
	}
	topics, err := store.ForumTopics.ListOpen(groupID)
	if err != nil {
		t.Fatalf("load concurrent topic registry: %v", err)
	}
	if len(topics) != 1 || !topics[0].LifecycleOwned || topics[0].MessageThreadID != 1960 {
		t.Fatalf("concurrent topic registry = %#v, want one owned topic", topics)
	}
	group, err := store.Groups.GetByID(groupID)
	if err != nil {
		t.Fatalf("load concurrent group: %v", err)
	}
	if group.Version != 2 {
		t.Fatalf("concurrent group version = %d, want 2", group.Version)
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

type creationIntentBarrierClient struct {
	*fakeTelegramClient
	entered chan struct{}
	release chan struct{}
}

func (c *creationIntentBarrierClient) CreateForumTopic(ctx context.Context, params *telego.CreateForumTopicParams) (*telego.ForumTopic, error) {
	close(c.entered)
	<-c.release
	return c.fakeTelegramClient.CreateForumTopic(ctx, params)
}

func TestTopicCreationIntentIsDurableBeforeTelegramCall(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -100320, Status: model.GroupStatusActive})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	channelID, err := store.Channels.Insert(&model.Channel{Username: "intent", Title: "Intent"})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	api := &creationIntentBarrierClient{
		fakeTelegramClient: &fakeTelegramClient{
			me:         &telego.User{ID: 123, Username: "DigestBot"},
			chatMember: &telego.ChatMemberAdministrator{Status: telego.MemberStatusAdministrator, CanManageTopics: true},
			forumTopic: &telego.ForumTopic{MessageThreadID: 3201, Name: "Intent"},
		},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	service := newServiceForTest(api, nil)
	service.groups = store.Groups
	service.channels = store.Channels
	service.SetForumTopicRegistry(store.ForumTopics)
	result := make(chan error, 1)
	go func() {
		_, assignErr := service.AssignChannelTopicWithVersion(context.Background(), groupID, channelID, nil, 1)
		result <- assignErr
	}()
	<-api.entered
	var count int
	if err := store.Conn().QueryRow(`
		SELECT COUNT(*) FROM forum_topic_creation_intents
		WHERE group_id = ? AND channel_id = ? AND message_thread_id = 0`,
		groupID, channelID).Scan(&count); err != nil {
		t.Fatalf("inspect pre-create intent: %v", err)
	}
	if count != 1 {
		t.Fatalf("pre-create intents = %d, want one", count)
	}
	close(api.release)
	if err := <-result; err != nil {
		t.Fatalf("assignment: %v", err)
	}
	if err := store.Conn().QueryRow(`
		SELECT COUNT(*) FROM forum_topic_creation_intents
		WHERE group_id = ? AND channel_id = ?`,
		groupID, channelID).Scan(&count); err != nil {
		t.Fatalf("inspect finalized intent: %v", err)
	}
	if count != 0 {
		t.Fatalf("finalized intents = %d, want none", count)
	}
}

func TestTopicCreationRecoverySurvivesGroupDeletionAndUsesChatIdentity(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -100321, Status: model.GroupStatusActive})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := store.Groups.RecordTopicCreationRecovery(groupID, 3202, -100321, "Deleted group topic"); err != nil {
		t.Fatalf("record recovery: %v", err)
	}
	if err := store.Groups.Delete(groupID); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	recoveries, err := store.Groups.ListPendingTopicCreationRecoveries()
	if err != nil {
		t.Fatalf("list recovery after group deletion: %v", err)
	}
	if len(recoveries) != 1 || recoveries[0].ChatID != -100321 {
		t.Fatalf("recoveries after group deletion = %#v", recoveries)
	}
	api := &fakeTelegramClient{}
	service := newServiceForTest(api, nil)
	service.groups = store.Groups
	service.SetForumTopicRegistry(store.ForumTopics)
	if err := service.ReconcilePendingTopicCreations(context.Background()); err != nil {
		t.Fatalf("reconcile deleted group recovery: %v", err)
	}
	if len(api.deletedTopics) != 1 || api.deletedTopics[0].ChatID.ID != -100321 {
		t.Fatalf("deleted topic calls = %#v", api.deletedTopics)
	}
	recoveries, err = store.Groups.ListPendingTopicCreationRecoveries()
	if err != nil {
		t.Fatalf("list converged recovery: %v", err)
	}
	if len(recoveries) != 0 {
		t.Fatalf("converged recoveries = %#v", recoveries)
	}
}

func TestAlreadyDeletedTopicRecoveryLeavesClosedTombstoneAndIgnoresLateObservation(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -100322, Status: model.GroupStatusActive})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := store.Groups.RecordTopicCreationRecovery(groupID, 3203, -100322, "Already deleted"); err != nil {
		t.Fatalf("record recovery: %v", err)
	}
	api := &fakeTelegramClient{deleteErr: errors.New("Bad Request: message thread not found")}
	service := newServiceForTest(api, nil)
	service.groups = store.Groups
	service.SetForumTopicRegistry(store.ForumTopics)
	if err := service.ReconcilePendingTopicCreations(context.Background()); err != nil {
		t.Fatalf("reconcile already deleted topic: %v", err)
	}
	topic, err := store.ForumTopics.Get(groupID, 3203)
	if err != nil {
		t.Fatalf("load compensation tombstone: %v", err)
	}
	if !topic.LifecycleOwned || !topic.Closed {
		t.Fatalf("compensation topic = %#v, want closed owned tombstone", topic)
	}
	update := telego.Update{Message: &telego.Message{
		MessageID: 3203, MessageThreadID: 3203,
		Chat:              telego.Chat{ID: -100322, Type: telego.ChatTypeSupergroup},
		ForumTopicCreated: &telego.ForumTopicCreated{Name: "Late observation"},
	}}
	if err := service.HandleUpdate(context.Background(), &update); err != nil {
		t.Fatalf("late observation: %v", err)
	}
	openTopics, err := store.ForumTopics.ListOpen(groupID)
	if err != nil {
		t.Fatalf("list topics after late observation: %v", err)
	}
	if len(openTopics) != 0 {
		t.Fatalf("late observation reopened tombstone: %#v", openTopics)
	}
}
