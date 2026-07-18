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
