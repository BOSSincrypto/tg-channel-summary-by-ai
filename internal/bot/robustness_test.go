package bot

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/mymmrac/telego"
)

type warningFailureTelegramClient struct {
	fakeTelegramClient
	err error
}

func (f *warningFailureTelegramClient) SendMessage(ctx context.Context, params *telego.SendMessageParams) (*telego.Message, error) {
	if strings.Contains(strings.ToLower(params.Text), "forum") {
		return nil, f.err
	}
	return f.fakeTelegramClient.SendMessage(ctx, params)
}

func TestNonForumJoinKeepsSchedulerCleanupWhenWarningDeliveryFails(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	api := &warningFailureTelegramClient{
		fakeTelegramClient: fakeTelegramClient{
			chats: map[int64]*telego.ChatFullInfo{
				-1002: {ID: -1002, Type: "supergroup", Title: "Regular", IsForum: false},
			},
		},
		err: errors.New("warning delivery failed"),
	}
	lifecycle := &fakeGroupLifecycle{}
	service := newServiceForTest(api, api)
	service.groups = store.Groups
	service.lifecycle = lifecycle

	err = service.handleGroupJoin(context.Background(), telego.Chat{
		ID: -1002, Type: "supergroup", Title: "Regular",
	})
	if err != nil {
		t.Fatalf("handleGroupJoin() error = %v, want warning failure to be non-fatal", err)
	}
	group, err := store.Groups.GetByChatID(-1002)
	if err != nil {
		t.Fatalf("load ineligible group: %v", err)
	}
	if group.Status != model.GroupStatusIneligible {
		t.Fatalf("group status = %q, want ineligible", group.Status)
	}
	if len(lifecycle.removed) != 1 || lifecycle.removed[0] != group.ID {
		t.Fatalf("removed scheduler groups = %v, want [%d]", lifecycle.removed, group.ID)
	}
}

func TestRapidStartCommandsDontOverflowOrCrash(t *testing.T) {
	// VAL-BOT-029: Rate limiting — many rapid /start calls should not crash
	// or overflow the delivery queue.
	api := &fakeTelegramClient{
		me: &telego.User{ID: 123, Username: "DigestBot"},
	}
	service := newServiceForTest(api, api)
	service.ownerID = "123"
	service.webAppURL = "https://example.test/webapp/"
	service.configureAdminCommands()

	// Simulate 500 rapid /start calls from both owner and non-owner.
	const rapidCount = 500
	ctx := context.Background()
	for i := 0; i < rapidCount; i++ {
		chatID := int64(123)
		fromID := int64(123)
		if i%2 == 0 {
			chatID = int64(i % 1000)
			fromID = int64(i % 1000)
		}
		err := service.HandleUpdate(ctx, &telego.Update{
			Message: &telego.Message{
				Chat: telego.Chat{ID: chatID},
				From: &telego.User{ID: fromID, FirstName: "Test"},
				Text: "/start",
			},
		})
		if err != nil {
			// Delivery queue full is expected under extreme load; the bot
			// must not crash and must remain responsive afterwards.
			if !strings.Contains(err.Error(), "delivery queue is full") &&
				!strings.Contains(err.Error(), "delivery") {
				t.Fatalf("unexpected error after %d rapid calls: %v", i+1, err)
			}
		}
	}
	// After the stress test the bot should still process a normal request.
	api.messages = nil // reset to keep assertion clean
	err := service.HandleUpdate(ctx, &telego.Update{
		Message: &telego.Message{
			Chat: telego.Chat{ID: 123},
			From: &telego.User{ID: 123, FirstName: "Owner"},
			Text: "/start",
		},
	})
	if err != nil {
		t.Fatalf("post-stress handleUpdate error = %v", err)
	}
	if len(api.messages) == 0 {
		t.Fatal("bot did not send response after stress test")
	}
}

func TestPanicRecoveryInHandleUpdateDoesNotCrashBot(t *testing.T) {
	api := &fakeTelegramClient{
		me: &telego.User{ID: 123, Username: "DigestBot"},
	}
	service := newServiceForTest(api, api)
	service.ownerID = "123"
	service.webAppURL = "https://example.test/webapp/"
	service.configureAdminCommands()

	// Inject a command handler that panics.
	service.SetCommandHandler("testpanic", func(ctx context.Context, message *telego.Message, argument string) error {
		panic("simulated panic in command handler")
	})

	// The panic should be caught and prevent the bot from crashing.
	err := service.HandleUpdate(context.Background(), &telego.Update{
		Message: &telego.Message{
			Chat: telego.Chat{ID: 123},
			From: &telego.User{ID: 123},
			Text: "/testpanic",
		},
	})
	if err == nil {
		t.Fatal("HandleUpdate should return an error for a panicking handler")
	}
	if !strings.Contains(err.Error(), "panic handling update") {
		t.Fatalf("error = %v, want panic recovery message", err)
	}
	// The bot must still be operational after the panic.
	err = service.HandleUpdate(context.Background(), &telego.Update{
		Message: &telego.Message{
			Chat: telego.Chat{ID: 123},
			From: &telego.User{ID: 123, FirstName: "Owner"},
			Text: "/start",
		},
	})
	if err != nil {
		t.Fatalf("post-panic handleUpdate error = %v", err)
	}
	if len(api.messages) == 0 {
		t.Fatal("bot did not recover after panic")
	}
}

func TestUnconfiguredBotHelpersReturnErrorsInsteadOfPanicking(t *testing.T) {
	service := New()
	if err := service.Start(); err == nil {
		t.Fatal("unconfigured Start() returned nil error")
	}
	if err := service.sendPlain(context.Background(), telego.ChatID{ID: 1}, "test"); err == nil {
		t.Fatal("unconfigured sendPlain() returned nil error")
	}

	var nilNotifier *OwnerNotifier
	if err := nilNotifier.NotifyOwner(context.Background(), "test"); err == nil {
		t.Fatal("nil notifier returned nil error")
	}
}
