package bot

import (
	"context"
	"strings"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/digest"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
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

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
