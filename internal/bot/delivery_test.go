package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/digest"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
)

func TestDeliverUsesConfiguredGroupTopicAndReturnsMessageMetadata(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	channelID, err := store.Channels.Insert(&model.Channel{Username: "delivery_channel", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -100123, Title: "Delivery"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	threadID := int64(42)
	if err := store.Groups.AssignChannel(groupID, channelID, &threadID); err != nil {
		t.Fatalf("assign channel: %v", err)
	}

	api := &fakeTelegramClient{}
	service := New()
	service.api = api
	service.groups = store.Groups
	receipt, err := service.Deliver(context.Background(), groupID, &digest.Digest{Text: "📋 test digest"})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if receipt.MessageID != 1 {
		t.Fatalf("message ID = %d, want 1", receipt.MessageID)
	}
	if len(api.messages) != 1 {
		t.Fatalf("messages = %d, want one", len(api.messages))
	}
	sent := api.messages[0]
	if sent.ChatID.ID != -100123 || sent.MessageThreadID != 42 || sent.Text != "📋 test digest" {
		t.Fatalf("send params = %+v, want group/thread/text", sent)
	}
}

func TestDeliverSplitsLongMarkdownDigestAndSetsParseMode(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -100124, Title: "Long digest"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	api := &fakeTelegramClient{}
	service := New()
	service.api = api
	service.groups = store.Groups
	longText := "📋 *Long digest*\n\n*Channel*\n• [summary](https://t.me/channel/1)\n" +
		strings.Repeat("• [длинная сводка](https://t.me/channel/2)\n", 150)

	receipt, err := service.Deliver(context.Background(), groupID, &digest.Digest{Text: longText})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if len(api.messages) < 2 {
		t.Fatalf("messages = %d, want split digest", len(api.messages))
	}
	for index, message := range api.messages {
		if message.ParseMode != "MarkdownV2" {
			t.Fatalf("message %d parse mode = %q, want MarkdownV2", index+1, message.ParseMode)
		}
		if len([]rune(message.Text)) > 4096 {
			t.Fatalf("message %d has %d runes, exceeds Telegram limit", index+1, len([]rune(message.Text)))
		}
		if !strings.Contains(message.Text, "Часть ") {
			t.Fatalf("message %d missing split marker: %s", index+1, message.Text[:min(len(message.Text), 80)])
		}
	}
	if receipt.MessageID != int64(len(api.messages)) {
		t.Fatalf("receipt message ID = %d, want last message %d", receipt.MessageID, len(api.messages))
	}
}

type scriptedDeliveryClient struct {
	*fakeTelegramClient
	errors   []error
	attempts int
}

func (c *scriptedDeliveryClient) SendMessage(_ context.Context, params *telego.SendMessageParams) (*telego.Message, error) {
	c.attempts++
	c.messages = append(c.messages, params)
	if len(c.errors) > 0 {
		err := c.errors[0]
		c.errors = c.errors[1:]
		if err != nil {
			return nil, err
		}
	}
	return &telego.Message{MessageID: c.attempts}, nil
}

type recordingDeliveryNotifier struct {
	messages []string
}

func (n *recordingDeliveryNotifier) NotifyOwner(_ context.Context, message string) error {
	n.messages = append(n.messages, message)
	return nil
}

func newDeliveryService(t *testing.T, api telegramClient, settings model.GroupSettings) (*Service, int64, *db.GroupRepository, *db.ChannelRepository) {
	t.Helper()
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -100125, Title: "Delivery"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if settings.GroupID == 0 {
		settings.GroupID = groupID
	}
	if err := store.Groups.UpdateGroupSettings(&settings); err != nil {
		t.Fatalf("update group settings: %v", err)
	}
	service := New()
	service.api = api
	service.groups = store.Groups
	return service, groupID, store.Groups, store.Channels
}

func TestDeliverRetriesTransientErrorsWithExponentialBackoff(t *testing.T) {
	api := &scriptedDeliveryClient{fakeTelegramClient: &fakeTelegramClient{}, errors: []error{
		&telegoapi.Error{ErrorCode: 502, Description: "Bad Gateway"},
		&telegoapi.Error{ErrorCode: 503, Description: "Service Unavailable"},
		nil,
	}}
	service, groupID, _, _ := newDeliveryService(t, api, model.GroupSettings{})
	var sleeps []time.Duration
	service.deliverySleeper = func(_ context.Context, delay time.Duration) error {
		sleeps = append(sleeps, delay)
		return nil
	}

	receipt, err := service.Deliver(context.Background(), groupID, &digest.Digest{Text: "digest"})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if receipt.MessageID != 3 || api.attempts != 3 {
		t.Fatalf("receipt/attempts = %d/%d, want 3/3", receipt.MessageID, api.attempts)
	}
	if want := []time.Duration{time.Second, 2 * time.Second}; !equalDurations(sleeps, want) {
		t.Fatalf("sleeps = %v, want %v", sleeps, want)
	}
}

func TestDeliverHonorsTelegramRetryAfter(t *testing.T) {
	api := &scriptedDeliveryClient{fakeTelegramClient: &fakeTelegramClient{}, errors: []error{
		&telegoapi.Error{
			ErrorCode:   429,
			Description: "Too Many Requests",
			Parameters:  &telegoapi.ResponseParameters{RetryAfter: 7},
		},
		nil,
	}}
	service, groupID, _, _ := newDeliveryService(t, api, model.GroupSettings{})
	var sleeps []time.Duration
	service.deliverySleeper = func(_ context.Context, delay time.Duration) error {
		sleeps = append(sleeps, delay)
		return nil
	}

	if _, err := service.Deliver(context.Background(), groupID, &digest.Digest{Text: "digest"}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if want := []time.Duration{7 * time.Second}; !equalDurations(sleeps, want) {
		t.Fatalf("sleeps = %v, want %v", sleeps, want)
	}
}

func TestDeliverSetsDisableNotificationFromGroupSettings(t *testing.T) {
	api := &scriptedDeliveryClient{fakeTelegramClient: &fakeTelegramClient{}}
	service, groupID, _, _ := newDeliveryService(t, api, model.GroupSettings{SilentDigest: true})

	if _, err := service.Deliver(context.Background(), groupID, &digest.Digest{Text: "digest"}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if len(api.messages) != 1 || !api.messages[0].DisableNotification {
		t.Fatalf("send params = %+v, want disable_notification=true", api.messages)
	}
}

func TestDeliverClosedTopicNotifiesOwnerWithoutRetrying(t *testing.T) {
	api := &scriptedDeliveryClient{
		fakeTelegramClient: &fakeTelegramClient{},
		errors:             []error{&telegoapi.Error{ErrorCode: 400, Description: "Bad Request: TOPIC_CLOSED"}},
	}
	notifier := &recordingDeliveryNotifier{}
	service, groupID, store, channels := newDeliveryService(t, api, model.GroupSettings{})
	service.notifier = notifier
	channelID, err := channels.Insert(&model.Channel{Username: "delivery_channel", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	threadID := int64(43)
	if err := store.AssignChannel(groupID, channelID, &threadID); err != nil {
		t.Fatalf("assign channel: %v", err)
	}

	if _, err := service.Deliver(context.Background(), groupID, &digest.Digest{Text: "digest"}); err == nil {
		t.Fatal("deliver succeeded for closed topic")
	}
	if api.attempts != 1 {
		t.Fatalf("attempts = %d, want one closed-topic attempt", api.attempts)
	}
	if len(notifier.messages) != 1 || !strings.Contains(notifier.messages[0], "топик") {
		t.Fatalf("notifications = %v, want actionable topic notification", notifier.messages)
	}
}

func TestDeliverDeletedTopicVariantsNotifyOwnerWithoutRetrying(t *testing.T) {
	for _, description := range []string{
		"Bad Request: message thread not found",
		"Bad Request: topic not found",
		"Bad Request: topic was deleted",
		"Bad Request: already deleted",
	} {
		t.Run(description, func(t *testing.T) {
			api := &scriptedDeliveryClient{
				fakeTelegramClient: &fakeTelegramClient{},
				errors:             []error{&telegoapi.Error{ErrorCode: 400, Description: description}},
			}
			notifier := &recordingDeliveryNotifier{}
			service, groupID, store, channels := newDeliveryService(t, api, model.GroupSettings{})
			service.notifier = notifier
			channelID, err := channels.Insert(&model.Channel{Username: "deleted_topic_channel"})
			if err != nil {
				t.Fatalf("insert channel: %v", err)
			}
			threadID := int64(44)
			if err := store.AssignChannel(groupID, channelID, &threadID); err != nil {
				t.Fatalf("assign channel: %v", err)
			}

			if _, err := service.Deliver(context.Background(), groupID, &digest.Digest{Text: "digest"}); err == nil {
				t.Fatal("deliver succeeded for deleted topic")
			}
			if api.attempts != 1 {
				t.Fatalf("attempts = %d, want one deleted-topic attempt", api.attempts)
			}
			if len(notifier.messages) != 1 || !strings.Contains(notifier.messages[0], "топик") {
				t.Fatalf("notifications = %v, want actionable topic notification", notifier.messages)
			}
		})
	}
}

func equalDurations(got, want []time.Duration) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
