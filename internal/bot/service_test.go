package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/mymmrac/telego"
)

type fakeTelegramClient struct {
	me            *telego.User
	meErrors      []error
	getMeCalls    int
	updates       chan telego.Update
	commands      *telego.SetMyCommandsParams
	callbacks     []string
	messages      []*telego.SendMessageParams
	chats         map[int64]*telego.ChatFullInfo
	getChatCalls  int
	topics        []*telego.CreateForumTopicParams
	closedTopics  []*telego.CloseForumTopicParams
	editedTopics  []*telego.EditForumTopicParams
	deletedTopics []*telego.DeleteForumTopicParams
	forumTopic    *telego.ForumTopic
	closeErr      error
	apiErr        error
	pollerErr     error
	deleteErr     error
}

func (f *fakeTelegramClient) GetMe(context.Context) (*telego.User, error) {
	index := f.getMeCalls
	f.getMeCalls++
	if index < len(f.meErrors) && f.meErrors[index] != nil {
		return nil, f.meErrors[index]
	}
	return f.me, nil
}

func (f *fakeTelegramClient) SetMyCommands(_ context.Context, params *telego.SetMyCommandsParams) error {
	f.commands = params
	return f.apiErr
}

func (f *fakeTelegramClient) AnswerCallbackQuery(_ context.Context, params *telego.AnswerCallbackQueryParams) error {
	f.callbacks = append(f.callbacks, params.CallbackQueryID)
	return f.apiErr
}

func (f *fakeTelegramClient) SendMessage(_ context.Context, params *telego.SendMessageParams) (*telego.Message, error) {
	f.messages = append(f.messages, params)
	if f.apiErr != nil {
		return nil, f.apiErr
	}
	return &telego.Message{MessageID: len(f.messages)}, nil
}

func (f *fakeTelegramClient) GetChat(_ context.Context, params *telego.GetChatParams) (*telego.ChatFullInfo, error) {
	f.getChatCalls++
	if f.apiErr != nil {
		return nil, f.apiErr
	}
	return f.chats[params.ChatID.ID], nil
}

func (f *fakeTelegramClient) CreateForumTopic(_ context.Context, params *telego.CreateForumTopicParams) (*telego.ForumTopic, error) {
	f.topics = append(f.topics, params)
	if f.apiErr != nil {
		return nil, f.apiErr
	}
	if f.forumTopic == nil {
		f.forumTopic = &telego.ForumTopic{MessageThreadID: 77, Name: params.Name}
	}
	return f.forumTopic, nil
}

func (f *fakeTelegramClient) CloseForumTopic(_ context.Context, params *telego.CloseForumTopicParams) error {
	f.closedTopics = append(f.closedTopics, params)
	if f.apiErr != nil {
		return f.apiErr
	}
	return f.closeErr
}

func (f *fakeTelegramClient) DeleteForumTopic(_ context.Context, params *telego.DeleteForumTopicParams) error {
	f.deletedTopics = append(f.deletedTopics, params)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return f.apiErr
}

func (f *fakeTelegramClient) EditForumTopic(_ context.Context, params *telego.EditForumTopicParams) error {
	f.editedTopics = append(f.editedTopics, params)
	return f.apiErr
}

func (f *fakeTelegramClient) UpdatesViaLongPolling(context.Context, *telego.GetUpdatesParams, ...telego.LongPollingOption) (<-chan telego.Update, error) {
	return f.updates, f.pollerErr
}

type fakeOwnerNotifier struct {
	messages []string
}

func (f *fakeOwnerNotifier) NotifyOwner(_ context.Context, message string) error {
	f.messages = append(f.messages, message)
	return nil
}

type fakeGroupLifecycle struct {
	removed  []int64
	restored []int64
}

func (f *fakeGroupLifecycle) RemoveGroup(groupID int64) {
	f.removed = append(f.removed, groupID)
}

func (f *fakeGroupLifecycle) RestoreGroup(groupID int64) error {
	f.restored = append(f.restored, groupID)
	return nil
}

func TestParseCommandNormalizesCaseAndBotSuffix(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "bare", text: "/start", want: "start"},
		{name: "case", text: "/SeTtInGs", want: "settings"},
		{name: "suffix", text: "/start@DigestBot parameter", want: "start"},
		{name: "not command", text: "hello", want: ""},
		{name: "wrong bot", text: "/start@otherbot", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command, argument, ok := ParseCommand(test.text, "digestbot")
			if test.want == "" {
				if ok {
					t.Fatalf("ParseCommand(%q) matched command %q", test.text, command)
				}
				return
			}
			if !ok || command != test.want {
				t.Fatalf("ParseCommand(%q) = %q, %v, want %q, true", test.text, command, ok, test.want)
			}
			if test.name == "suffix" && argument != "parameter" {
				t.Fatalf("argument = %q, want parameter", argument)
			}
		})
	}
}

func TestServiceAdminCommandsAuthorizeOwnerAndSendWebAppButton(t *testing.T) {
	api := &fakeTelegramClient{
		me:    &telego.User{ID: 123, Username: "DigestBot"},
		chats: map[int64]*telego.ChatFullInfo{},
	}
	service := newServiceForTest(api, api)
	service.ownerID = "123"
	service.botName = "DigestBot"
	service.webAppURL = "https://example.test/webapp/"
	service.configureAdminCommands()

	message := &telego.Message{
		Chat: telego.Chat{ID: 123},
		From: &telego.User{ID: 123, FirstName: "Owner_[test]"},
	}
	for _, command := range []string{"start", "settings"} {
		service.commands[command](context.Background(), message, "")
	}

	if len(api.messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(api.messages))
	}
	if !strings.Contains(api.messages[0].Text, "@DigestBot") ||
		!strings.Contains(api.messages[0].Text, "daily digests") {
		t.Fatalf("welcome text = %q", api.messages[0].Text)
	}
	for i, sent := range api.messages {
		keyboard, ok := sent.ReplyMarkup.(*telego.InlineKeyboardMarkup)
		if !ok || len(keyboard.InlineKeyboard) != 1 || len(keyboard.InlineKeyboard[0]) != 1 {
			t.Fatalf("message %d keyboard = %#v, want one inline button", i, sent.ReplyMarkup)
		}
		button := keyboard.InlineKeyboard[0][0]
		if button.Text != "Open Settings" || button.CallbackData != "" ||
			button.WebApp == nil || button.WebApp.URL != service.webAppURL {
			t.Fatalf("message %d button = %#v, want WebApp settings button", i, button)
		}
	}
	if !strings.Contains(api.messages[0].Text, `\_`) ||
		!strings.Contains(api.messages[0].Text, `\[`) {
		t.Fatalf("welcome text did not escape MarkdownV2 dynamic content: %q", api.messages[0].Text)
	}
	if api.messages[0].ParseMode != "MarkdownV2" {
		t.Fatalf("welcome parse mode = %q, want MarkdownV2", api.messages[0].ParseMode)
	}
}

func TestServiceAdminCommandsDenyNonOwnerWithoutMarkup(t *testing.T) {
	api := &fakeTelegramClient{
		me:    &telego.User{ID: 123, Username: "DigestBot"},
		chats: map[int64]*telego.ChatFullInfo{},
	}
	service := newServiceForTest(api, api)
	service.ownerID = "123"
	service.webAppURL = "https://example.test/webapp/"
	service.configureAdminCommands()

	message := &telego.Message{
		Chat: telego.Chat{ID: 999},
		From: &telego.User{ID: 999},
	}
	for _, command := range []string{"start", "settings"} {
		if err := service.commands[command](context.Background(), message, ""); err != nil {
			t.Fatalf("%s error = %v", command, err)
		}
	}

	if len(api.messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(api.messages))
	}
	for _, sent := range api.messages {
		if sent.ReplyMarkup != nil {
			t.Fatalf("denial message contains reply markup: %#v", sent.ReplyMarkup)
		}
		if !strings.Contains(strings.ToLower(sent.Text), "access denied") {
			t.Fatalf("denial text = %q", sent.Text)
		}
		if sent.ParseMode != "" {
			t.Fatalf("denial parse mode = %q, want plain text", sent.ParseMode)
		}
	}
}

func TestServiceAdminCommandsIgnoreMalformedSender(t *testing.T) {
	api := &fakeTelegramClient{
		me:    &telego.User{ID: 123, Username: "DigestBot"},
		chats: map[int64]*telego.ChatFullInfo{},
	}
	service := newServiceForTest(api, api)
	service.ownerID = "123"
	service.webAppURL = "https://example.test/webapp/"
	service.configureAdminCommands()

	if err := service.HandleUpdate(context.Background(), &telego.Update{
		Message: &telego.Message{Chat: telego.Chat{ID: 123}, From: nil, Text: "/start"},
	}); err != nil {
		t.Fatalf("malformed update error = %v", err)
	}
	if len(api.messages) != 0 {
		t.Fatalf("malformed sender produced messages = %#v", api.messages)
	}
}

func TestServiceCallbackActionsRequireOwner(t *testing.T) {
	api := &fakeTelegramClient{
		me:    &telego.User{ID: 123, Username: "DigestBot"},
		chats: map[int64]*telego.ChatFullInfo{},
	}
	service := newServiceForTest(api, api)
	service.ownerID = "123"
	service.SetCommandHandler("admin_action", func(context.Context, *telego.Message, string) error {
		t.Fatal("non-owner callback handler must not run")
		return nil
	})

	err := service.HandleUpdate(context.Background(), &telego.Update{
		CallbackQuery: &telego.CallbackQuery{
			ID:      "callback-1",
			From:    telego.User{ID: 999},
			Data:    "admin_action",
			Message: &telego.Message{Chat: telego.Chat{ID: 999}},
		},
	})
	if err != nil {
		t.Fatalf("callback handling error = %v", err)
	}
	if len(api.callbacks) != 1 || len(api.messages) != 1 {
		t.Fatalf("callback responses = callbacks:%d messages:%d, want one each", len(api.callbacks), len(api.messages))
	}
	if !strings.Contains(strings.ToLower(api.messages[0].Text), "access denied") {
		t.Fatalf("callback denial text = %q", api.messages[0].Text)
	}
}

func TestServiceCallbackActionsRunForOwner(t *testing.T) {
	api := &fakeTelegramClient{
		me:    &telego.User{ID: 123, Username: "DigestBot"},
		chats: map[int64]*telego.ChatFullInfo{},
	}
	service := newServiceForTest(api, api)
	service.ownerID = "123"
	service.SetCommandHandler("admin_action", func(_ context.Context, message *telego.Message, _ string) error {
		if message.From == nil || message.From.ID != 123 {
			t.Fatalf("callback handler sender = %#v, want owner", message.From)
		}
		return nil
	})

	err := service.HandleUpdate(context.Background(), &telego.Update{
		CallbackQuery: &telego.CallbackQuery{
			ID:      "callback-owner",
			From:    telego.User{ID: 123},
			Data:    "admin_action",
			Message: &telego.Message{Chat: telego.Chat{ID: 123}, From: &telego.User{ID: 999}},
		},
	})
	if err != nil {
		t.Fatalf("callback handling error = %v", err)
	}
	if len(api.callbacks) != 1 {
		t.Fatalf("callback answers = %d, want 1", len(api.callbacks))
	}
}

func TestServiceStartVerifiesIdentityRegistersPrivateCommandsAndAnswersCallbacks(t *testing.T) {
	api := &fakeTelegramClient{
		me:      &telego.User{ID: 123, Username: "DigestBot"},
		updates: make(chan telego.Update, 2),
		chats:   map[int64]*telego.ChatFullInfo{},
	}
	api.updates <- telego.Update{CallbackQuery: &telego.CallbackQuery{ID: "callback-1", Data: "unknown"}}
	close(api.updates)

	service := newServiceForTest(api, api)
	if err := service.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if api.commands == nil {
		t.Fatal("expected setMyCommands call")
	}
	if api.commands.Scope == nil || api.commands.Scope.ScopeType() != "all_private_chats" {
		t.Fatalf("command scope = %#v, want all_private_chats", api.commands.Scope)
	}
	if len(api.commands.Commands) != 2 {
		t.Fatalf("registered commands = %#v, want start and settings", api.commands.Commands)
	}
	if api.commands.Commands[0].Command != "start" || api.commands.Commands[1].Command != "settings" {
		t.Fatalf("registered commands = %#v", api.commands.Commands)
	}
	if len(api.callbacks) != 1 || api.callbacks[0] != "callback-1" {
		t.Fatalf("callback answers = %#v, want callback-1", api.callbacks)
	}
}

func TestServiceHandlesWebAppDataValidation(t *testing.T) {
	api := &fakeTelegramClient{
		me:      &telego.User{ID: 123, Username: "DigestBot"},
		updates: make(chan telego.Update, 2),
		chats:   map[int64]*telego.ChatFullInfo{},
	}
	api.updates <- telego.Update{Message: &telego.Message{
		Chat:       telego.Chat{ID: 123},
		From:       &telego.User{ID: 123},
		WebAppData: &telego.WebAppData{Data: `{"digest_time":"bad"}`},
	}}
	api.updates <- telego.Update{Message: &telego.Message{
		Chat:       telego.Chat{ID: 123},
		From:       &telego.User{ID: 123},
		WebAppData: &telego.WebAppData{Data: `{"digest_time":"21:00","channels":[]}`},
	}}
	close(api.updates)

	service := newServiceForTest(api, api)
	service.ownerID = "123"
	if err := service.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if len(api.messages) != 2 {
		t.Fatalf("messages = %d, want validation error and success", len(api.messages))
	}
	if !strings.Contains(api.messages[0].Text, "digest_time") {
		t.Fatalf("validation message = %q", api.messages[0].Text)
	}
	if api.messages[1].Text != "Settings updated successfully." {
		t.Fatalf("success message = %q", api.messages[1].Text)
	}
}

func TestServiceHandlesMembershipLifecycleAndForumEligibility(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -1001, Title: "Forum"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	api := &fakeTelegramClient{
		me:      &telego.User{ID: 123, Username: "DigestBot"},
		updates: make(chan telego.Update, 2),
		chats: map[int64]*telego.ChatFullInfo{
			-1002: {ID: -1002, Type: "supergroup", Title: "Regular", IsForum: false},
		},
	}
	api.updates <- telego.Update{MyChatMember: &telego.ChatMemberUpdated{
		Chat:          telego.Chat{ID: -1002, Title: "Regular", Type: "supergroup"},
		NewChatMember: &telego.ChatMemberMember{Status: "member", User: telego.User{ID: 123}},
	}}
	api.updates <- telego.Update{MyChatMember: &telego.ChatMemberUpdated{
		Chat:          telego.Chat{ID: -1001, Title: "Forum", Type: "supergroup"},
		NewChatMember: &telego.ChatMemberLeft{Status: "left", User: telego.User{ID: 123}},
	}}
	close(api.updates)
	notifier := &fakeOwnerNotifier{}
	lifecycle := &fakeGroupLifecycle{}

	service := newServiceForTest(api, api)
	service.groups = store.Groups
	service.notifier = notifier
	service.lifecycle = lifecycle
	if err := service.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	ineligible, err := store.Groups.GetByChatID(-1002)
	if err != nil {
		t.Fatalf("get ineligible group: %v", err)
	}
	if ineligible.Status != model.GroupStatusIneligible {
		t.Fatalf("ineligible status = %q", ineligible.Status)
	}
	removed, err := store.Groups.GetByID(groupID)
	if err != nil {
		t.Fatalf("get removed group: %v", err)
	}
	if removed.Status != model.GroupStatusInactive {
		t.Fatalf("removed status = %q", removed.Status)
	}
	if len(api.messages) != 1 || !strings.Contains(api.messages[0].Text, "forum") {
		t.Fatalf("forum warning messages = %#v", api.messages)
	}
	if len(notifier.messages) != 1 || !strings.Contains(notifier.messages[0], "-1001") {
		t.Fatalf("owner notifications = %#v", notifier.messages)
	}
	if len(lifecycle.removed) != 2 {
		t.Fatalf("removed scheduler groups = %#v", lifecycle.removed)
	}
}

func TestServiceIgnoresPrivateMembershipLifecycle(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	api := &fakeTelegramClient{
		me:    &telego.User{ID: 123, Username: "DigestBot"},
		chats: map[int64]*telego.ChatFullInfo{},
	}
	lifecycle := &fakeGroupLifecycle{}
	service := newServiceForTest(api, api)
	service.groups = store.Groups
	service.lifecycle = lifecycle

	for _, status := range []string{"member", "left", "kicked"} {
		err := service.HandleUpdate(context.Background(), &telego.Update{
			MyChatMember: &telego.ChatMemberUpdated{
				Chat:          telego.Chat{ID: 123, Type: "private", Title: "Owner"},
				NewChatMember: &telego.ChatMemberMember{Status: status, User: telego.User{ID: 123}},
			},
		})
		if err != nil {
			t.Fatalf("private status %q error = %v", status, err)
		}
	}

	groups, err := store.Groups.List()
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("private membership inserted groups = %#v", groups)
	}
	if api.getChatCalls != 0 || len(api.messages) != 0 {
		t.Fatalf("private membership Telegram side effects: getChat=%d messages=%d", api.getChatCalls, len(api.messages))
	}
	if len(lifecycle.removed) != 0 || len(lifecycle.restored) != 0 {
		t.Fatalf("private membership scheduler side effects: removed=%v restored=%v", lifecycle.removed, lifecycle.restored)
	}
}

func TestServiceReaddRestoresExistingForumGroupScheduler(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -1009,
		Title:          "Forum",
		Status:         model.GroupStatusInactive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	api := &fakeTelegramClient{
		me: &telego.User{ID: 123, Username: "DigestBot"},
		chats: map[int64]*telego.ChatFullInfo{
			-1009: {ID: -1009, Type: "supergroup", Title: "Forum", IsForum: true},
		},
	}
	lifecycle := &fakeGroupLifecycle{}
	service := newServiceForTest(api, api)
	service.groups = store.Groups
	service.lifecycle = lifecycle

	err = service.HandleUpdate(context.Background(), &telego.Update{
		MyChatMember: &telego.ChatMemberUpdated{
			Chat:          telego.Chat{ID: -1009, Type: "supergroup", Title: "Forum"},
			NewChatMember: &telego.ChatMemberMember{Status: "member", User: telego.User{ID: 123}},
		},
	})
	if err != nil {
		t.Fatalf("forum re-add error = %v", err)
	}
	group, err := store.Groups.GetByID(groupID)
	if err != nil {
		t.Fatalf("get restored group: %v", err)
	}
	if group.Status != model.GroupStatusActive {
		t.Fatalf("restored group status = %q, want active", group.Status)
	}
	if len(lifecycle.restored) != 1 || lifecycle.restored[0] != groupID {
		t.Fatalf("restored scheduler groups = %v, want [%d]", lifecycle.restored, groupID)
	}
}

func TestServiceClassifiesPostStartUnauthorizedPollingAsRevocation(t *testing.T) {
	api := &fakeTelegramClient{
		me:       &telego.User{ID: 123, Username: "DigestBot"},
		meErrors: []error{nil, errors.New("401 Unauthorized")},
		updates:  make(chan telego.Update),
		chats:    map[int64]*telego.ChatFullInfo{},
	}
	close(api.updates)

	service := newServiceForTest(api, api)
	logger := &recordingLogger{}
	service.logger = logger
	err := service.Start()
	if !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("Start() error = %v, want ErrTokenRevoked", err)
	}
	if api.getMeCalls != 2 {
		t.Fatalf("getMe calls = %d, want startup verification plus post-start check", api.getMeCalls)
	}
	if len(logger.messages) == 0 || !strings.Contains(logger.messages[len(logger.messages)-1], "FATAL") ||
		!strings.Contains(logger.messages[len(logger.messages)-1], "token has been revoked") {
		t.Fatalf("revocation logs = %#v, want fatal revocation log", logger.messages)
	}
}

func TestServiceTopicLifecyclePersistsThreadAndAvoidsDuplicates(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -1007, Title: "Forum"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	channelID, err := store.Channels.Insert(&model.Channel{Username: "news", Title: "News", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	if err := store.Groups.AssignChannel(groupID, channelID, nil); err != nil {
		t.Fatalf("assign channel: %v", err)
	}
	api := &fakeTelegramClient{
		me:         &telego.User{ID: 123, Username: "DigestBot"},
		chats:      map[int64]*telego.ChatFullInfo{},
		forumTopic: &telego.ForumTopic{MessageThreadID: 77, Name: "News"},
	}
	service := newServiceForTest(api, api)
	service.groups = store.Groups
	service.channels = store.Channels

	if err := service.CreateChannelTopic(context.Background(), groupID, channelID); err != nil {
		t.Fatalf("create topic: %v", err)
	}
	if err := service.CreateChannelTopic(context.Background(), groupID, channelID); err != nil {
		t.Fatalf("idempotent create topic: %v", err)
	}
	if len(api.topics) != 1 {
		t.Fatalf("topic create calls = %d, want 1", len(api.topics))
	}
	assignment, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("get assignment: %v", err)
	}
	if assignment[0].TopicThreadID == nil || *assignment[0].TopicThreadID != 77 {
		t.Fatalf("stored topic assignment = %#v", assignment)
	}
	if err := service.RenameChannelTopic(context.Background(), groupID, channelID, "Breaking News"); err != nil {
		t.Fatalf("rename topic: %v", err)
	}
	if err := service.RemoveChannelTopic(context.Background(), groupID, channelID); err != nil {
		t.Fatalf("remove topic: %v", err)
	}
	if len(api.editedTopics) != 1 || len(api.closedTopics) != 1 {
		t.Fatalf("topic lifecycle calls: edited=%d closed=%d", len(api.editedTopics), len(api.closedTopics))
	}
	assignments, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("get assignments after removal: %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("assignments after removal = %#v, want none", assignments)
	}
}

func TestServiceTopicCreationDeletesTelegramTopicWhenPersistenceFails(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -1011, Title: "Forum"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	channelID, err := store.Channels.Insert(&model.Channel{Username: "rollback_news", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	if err := store.Groups.AssignChannel(groupID, channelID, nil); err != nil {
		t.Fatalf("assign channel: %v", err)
	}
	if _, err := store.Conn().Exec(`
		CREATE TRIGGER reject_topic_persistence
		BEFORE UPDATE OF topic_thread_id ON group_channels
		BEGIN
			SELECT RAISE(ABORT, 'persist failure');
		END`); err != nil {
		t.Fatalf("create persistence trigger: %v", err)
	}
	api := &fakeTelegramClient{
		me:         &telego.User{ID: 123, Username: "DigestBot"},
		forumTopic: &telego.ForumTopic{MessageThreadID: 88, Name: "rollback_news"},
	}
	service := newServiceForTest(api, api)
	service.groups = store.Groups
	service.channels = store.Channels

	if err := service.CreateChannelTopic(context.Background(), groupID, channelID); err == nil {
		t.Fatal("CreateChannelTopic() succeeded despite persistence failure")
	}
	if len(api.deletedTopics) != 1 || api.deletedTopics[0].MessageThreadID != 88 {
		t.Fatalf("delete compensation = %#v, want thread 88", api.deletedTopics)
	}
	assignments, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("load assignments: %v", err)
	}
	if len(assignments) != 1 || assignments[0].TopicThreadID != nil {
		t.Fatalf("assignment after failed persistence = %#v, want unassigned topic", assignments)
	}
}

func TestServiceTopicRemovalRestoresAssignmentWhenCloseFails(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -1012, Title: "Forum"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	channelID, err := store.Channels.Insert(&model.Channel{Username: "close_fail_news", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	threadID := int64(89)
	if err := store.Groups.AssignChannel(groupID, channelID, &threadID); err != nil {
		t.Fatalf("assign channel: %v", err)
	}
	api := &fakeTelegramClient{
		me:       &telego.User{ID: 123, Username: "DigestBot"},
		closeErr: errors.New("close failed"),
	}
	service := newServiceForTest(api, api)
	service.groups = store.Groups

	if err := service.RemoveChannelTopic(context.Background(), groupID, channelID); err == nil {
		t.Fatal("RemoveChannelTopic() succeeded despite close failure")
	}
	assignments, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("load assignments: %v", err)
	}
	if len(assignments) != 1 || assignments[0].TopicThreadID == nil || *assignments[0].TopicThreadID != threadID {
		t.Fatalf("assignment after failed close = %#v, want restored thread 89", assignments)
	}
}

func TestServiceTopicRemovalKeepsSharedTopicOpen(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -1018, Title: "Forum"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	firstChannelID, err := store.Channels.Insert(&model.Channel{Username: "shared_first", Enabled: true})
	if err != nil {
		t.Fatalf("insert first channel: %v", err)
	}
	secondChannelID, err := store.Channels.Insert(&model.Channel{Username: "shared_second", Enabled: true})
	if err != nil {
		t.Fatalf("insert second channel: %v", err)
	}
	threadID := int64(91)
	if err := store.Groups.AssignChannel(groupID, firstChannelID, &threadID); err != nil {
		t.Fatalf("assign first channel: %v", err)
	}
	if err := store.Groups.AssignChannel(groupID, secondChannelID, &threadID); err != nil {
		t.Fatalf("assign second channel: %v", err)
	}
	api := &fakeTelegramClient{me: &telego.User{ID: 123, Username: "DigestBot"}}
	service := newServiceForTest(api, api)
	service.groups = store.Groups

	if err := service.RemoveChannelTopic(context.Background(), groupID, firstChannelID); err != nil {
		t.Fatalf("remove shared topic assignment: %v", err)
	}
	if len(api.closedTopics) != 0 {
		t.Fatalf("closed shared topic = %#v, want no close", api.closedTopics)
	}
	assignments, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("load shared assignments: %v", err)
	}
	if len(assignments) != 1 || assignments[0].ChannelID != secondChannelID ||
		assignments[0].TopicThreadID == nil || *assignments[0].TopicThreadID != threadID {
		t.Fatalf("shared assignments after removal = %#v", assignments)
	}
}

func newServiceForTest(api telegramClient, poller updatePoller) *Service {
	return &Service{
		api:    api,
		poller: poller,
		logger: testLogger{},
	}
}

type testLogger struct{}

func (testLogger) Printf(string, ...any) {}

type recordingLogger struct {
	messages []string
}

func (l *recordingLogger) Printf(format string, args ...any) {
	l.messages = append(l.messages, fmt.Sprintf(format, args...))
}
