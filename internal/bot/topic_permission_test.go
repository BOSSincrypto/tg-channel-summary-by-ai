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

func TestTopicPermissionAcceptsCreatorAndTopicManager(t *testing.T) {
	tests := []struct {
		name   string
		member telego.ChatMember
	}{
		{
			name:   "creator",
			member: &telego.ChatMemberOwner{Status: telego.MemberStatusCreator, User: telego.User{ID: 123}},
		},
		{
			name: "administrator with can_manage_topics",
			member: &telego.ChatMemberAdministrator{
				Status:          telego.MemberStatusAdministrator,
				User:            telego.User{ID: 123},
				CanManageTopics: true,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &fakeTelegramClient{
				me:                   &telego.User{ID: 123, Username: "DigestBot"},
				chatMember:           test.member,
				permissionConfigured: true,
			}
			service := newServiceForTest(api, nil)
			if err := service.ensureTopicPermission(context.Background(), -100501); err != nil {
				t.Fatalf("ensureTopicPermission() error = %v", err)
			}
			if api.getMeCalls != 1 || api.getChatMemberCalls != 1 {
				t.Fatalf("permission calls = getMe:%d getChatMember:%d, want one each", api.getMeCalls, api.getChatMemberCalls)
			}
		})
	}
}

func TestTopicMutationsFailClosedBeforeTelegramOrSQLiteSideEffects(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, *Service, int64, int64) error
	}{
		{
			name: "create",
			call: func(ctx context.Context, service *Service, groupID, channelID int64) error {
				return service.CreateChannelTopic(ctx, groupID, channelID)
			},
		},
		{
			name: "rename",
			call: func(ctx context.Context, service *Service, groupID, channelID int64) error {
				return service.RenameChannelTopic(ctx, groupID, channelID, "Renamed")
			},
		},
		{
			name: "remove",
			call: func(ctx context.Context, service *Service, groupID, channelID int64) error {
				return service.RemoveChannelTopic(ctx, groupID, channelID)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, groupID, channelID := newPermissionTestStore(t, test.name != "create")
			defer store.Close()
			api := &fakeTelegramClient{
				me:                   &telego.User{ID: 123, Username: "DigestBot"},
				chatMember:           &telego.ChatMemberAdministrator{Status: telego.MemberStatusAdministrator, User: telego.User{ID: 123}},
				permissionConfigured: true,
			}
			service := newServiceForTest(api, nil)
			service.groups = store.Groups
			service.channels = store.Channels

			if err := test.call(context.Background(), service, groupID, channelID); !errors.Is(err, ErrTopicPermissionDenied) {
				t.Fatalf("mutation error = %v, want ErrTopicPermissionDenied", err)
			}
			if len(api.topics) != 0 || len(api.editedTopics) != 0 ||
				len(api.closedTopics) != 0 || len(api.deletedTopics) != 0 {
				t.Fatalf("Telegram topic calls after denied guard: create=%d edit=%d close=%d delete=%d",
					len(api.topics), len(api.editedTopics), len(api.closedTopics), len(api.deletedTopics))
			}
			assignments, err := store.Groups.GetChannelAssignments(groupID)
			if err != nil {
				t.Fatalf("load assignments: %v", err)
			}
			if len(assignments) != 1 {
				t.Fatalf("assignments = %#v, want unchanged row", assignments)
			}
			if test.name == "create" && assignments[0].TopicThreadID != nil {
				t.Fatalf("create assignment = %#v, want no topic mutation", assignments[0])
			}
			if test.name != "create" && (assignments[0].TopicThreadID == nil || *assignments[0].TopicThreadID != 501) {
				t.Fatalf("existing assignment = %#v, want thread 501", assignments[0])
			}
		})
	}
}

func TestTopicMutationPermissionFailureIsFailClosedForUnknownAndTelegramErrors(t *testing.T) {
	tests := []struct {
		name       string
		member     telego.ChatMember
		meErrors   []error
		memberErrs []error
	}{
		{name: "unknown member"},
		{name: "identity lookup failure", meErrors: []error{errors.New("identity lookup failed")}},
		{name: "lookup failure", memberErrs: []error{errors.New("permission lookup failed")}},
		{
			name: "regular member",
			member: &telego.ChatMemberMember{
				Status: telego.MemberStatusMember,
				User:   telego.User{ID: 123},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, groupID, channelID := newPermissionTestStore(t, false)
			defer store.Close()
			api := &fakeTelegramClient{
				me:                   &telego.User{ID: 123, Username: "DigestBot"},
				meErrors:             test.meErrors,
				chatMember:           test.member,
				chatMemberErrs:       test.memberErrs,
				permissionConfigured: true,
			}
			service := newServiceForTest(api, nil)
			service.groups = store.Groups
			service.channels = store.Channels

			err := service.RenameChannelTopic(context.Background(), groupID, channelID, "Renamed")
			if err == nil {
				t.Fatal("rename succeeded despite failed permission check")
			}
			if len(api.editedTopics) != 0 {
				t.Fatalf("rename calls = %#v, want none", api.editedTopics)
			}
		})
	}
}

func TestTopicDeleteCompensationChecksPermissionBeforeDelete(t *testing.T) {
	store, groupID, channelID := newPermissionTestStore(t, false)
	defer store.Close()
	if _, err := store.Conn().Exec(`
		CREATE TRIGGER reject_topic_persistence
		BEFORE UPDATE OF topic_thread_id ON group_channels
		BEGIN
			SELECT RAISE(ABORT, 'persist failure');
		END`); err != nil {
		t.Fatalf("create persistence trigger: %v", err)
	}
	api := &fakeTelegramClient{
		me: &telego.User{ID: 123, Username: "DigestBot"},
		chatMembers: []telego.ChatMember{
			&telego.ChatMemberAdministrator{Status: telego.MemberStatusAdministrator, User: telego.User{ID: 123}, CanManageTopics: true},
			&telego.ChatMemberAdministrator{Status: telego.MemberStatusAdministrator, User: telego.User{ID: 123}, CanManageTopics: false},
		},
		permissionConfigured: true,
		forumTopic:           &telego.ForumTopic{MessageThreadID: 1501, Name: "Compensation"},
	}
	service := newServiceForTest(api, nil)
	service.groups = store.Groups
	service.channels = store.Channels

	if err := service.CreateChannelTopic(context.Background(), groupID, channelID); err == nil {
		t.Fatal("create succeeded despite persistence failure")
	}
	if len(api.topics) != 1 {
		t.Fatalf("create calls = %d, want one", len(api.topics))
	}
	if len(api.deletedTopics) != 0 {
		t.Fatalf("delete compensation calls = %#v, want none after denied guard", api.deletedTopics)
	}
	assignments, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("load assignments: %v", err)
	}
	if len(assignments) != 1 || assignments[0].TopicThreadID != nil {
		t.Fatalf("assignment after denied compensation = %#v, want unchanged", assignments)
	}
}

func TestPendingTopicReconciliationFailsClosedWithoutCloseOrStateMutation(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -100502,
		Title:          "Pending forum",
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	threadID := int64(1502)
	if err := store.ForumTopics.PersistOwned(groupID, threadID, "Pending"); err != nil {
		t.Fatalf("persist topic: %v", err)
	}
	if _, err := store.Conn().Exec(`
		UPDATE forum_topics SET close_pending = 1
		WHERE group_id = ? AND message_thread_id = ?`, groupID, threadID); err != nil {
		t.Fatalf("seed pending topic: %v", err)
	}
	api := &fakeTelegramClient{
		me:                   &telego.User{ID: 123, Username: "DigestBot"},
		chatMember:           &telego.ChatMemberAdministrator{Status: telego.MemberStatusAdministrator, User: telego.User{ID: 123}},
		permissionConfigured: true,
	}
	service := newServiceForTest(api, nil)
	service.groups = store.Groups
	service.SetForumTopicRegistry(store.ForumTopics)

	if err := service.ReconcilePendingTopicClosures(context.Background()); err == nil {
		t.Fatal("reconciliation succeeded despite denied permission")
	}
	if len(api.closedTopics) != 0 {
		t.Fatalf("reconciliation close calls = %#v, want none", api.closedTopics)
	}
	topic, err := store.ForumTopics.Get(groupID, threadID)
	if err != nil {
		t.Fatalf("load pending topic: %v", err)
	}
	if !topic.ClosePending || topic.Closed {
		t.Fatalf("pending topic after denied reconciliation = %#v, want unchanged", topic)
	}
}

func TestProductionWebAppTopicCreationChecksPermissionBeforeAssignment(t *testing.T) {
	store, groupID, channelID := newPermissionTestStore(t, false)
	defer store.Close()
	if err := store.Groups.UnassignChannel(groupID, channelID); err != nil {
		t.Fatalf("remove seed assignment: %v", err)
	}
	api := &fakeTelegramClient{
		me:                   &telego.User{ID: 123, Username: "DigestBot"},
		chatMember:           &telego.ChatMemberAdministrator{Status: telego.MemberStatusAdministrator, User: telego.User{ID: 123}},
		permissionConfigured: true,
	}
	service := newServiceForTest(api, nil)
	service.groups = store.Groups
	service.channels = store.Channels
	server := webapp.NewWithProvidersForTesting(store, 0, http.DefaultClient)
	server.SetTopicLifecycle(service)

	request := httptest.NewRequest(http.MethodPost,
		"/api/groups/"+strconv.FormatInt(groupID, 10)+"/channels",
		strings.NewReader(`{"channel_id":"`+strconv.FormatInt(channelID, 10)+`","version":1}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("WebApp response = %d, body=%s, want permission failure", response.Code, response.Body.String())
	}
	if len(api.topics) != 0 {
		t.Fatalf("WebApp create calls = %#v, want none", api.topics)
	}
	assignments, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("load assignments: %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("WebApp assignments after denied guard = %#v, want none", assignments)
	}
}

func newPermissionTestStore(t *testing.T, withTopic bool) (*db.DB, int64, int64) {
	t.Helper()
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -100501,
		Title:          "Permission forum",
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		store.Close()
		t.Fatalf("insert group: %v", err)
	}
	channelID, err := store.Channels.Insert(&model.Channel{Username: "permission_test", Title: "Permission test", Enabled: true})
	if err != nil {
		store.Close()
		t.Fatalf("insert channel: %v", err)
	}
	var threadID *int64
	if withTopic {
		value := int64(501)
		threadID = &value
	}
	if err := store.Groups.AssignChannel(groupID, channelID, threadID); err != nil {
		store.Close()
		t.Fatalf("assign channel: %v", err)
	}
	return store, groupID, channelID
}
