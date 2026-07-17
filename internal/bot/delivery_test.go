package bot

import (
	"context"
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
