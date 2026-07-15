package digest

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/parser"
)

func TestGenerateFetchesStoresAndSelectsFreshPosts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<div class="tgme_widget_message" data-post="digest_channel/7"><div class="tgme_widget_message_text">fresh digest post</div><time datetime="2099-07-15T18:30:00Z"></time></div>`))
	}))
	defer server.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()
	channel := &model.Channel{Username: "digest_channel", Enabled: true}
	channelID, err := database.Channels.Insert(channel)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	groupID, err := database.Groups.Insert(&model.Group{TelegramChatID: -1001, Title: "Digest"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := database.Groups.AssignChannel(groupID, channelID, nil); err != nil {
		t.Fatalf("assign channel: %v", err)
	}

	fetcher := parser.NewWithOptions(parser.Options{Client: server.Client(), BaseURL: server.URL})
	processor := parser.NewChannelProcessor(fetcher, parser.NewPostStorage(database.Channels, database.Posts))
	service := NewWithProcessor(database, processor)
	result, err := service.Generate(groupID)
	if err != nil {
		t.Fatalf("generate digest: %v", err)
	}
	if result.PostCount != 1 {
		t.Fatalf("digest post count = %d, want 1", result.PostCount)
	}

	storedChannel, err := database.Channels.GetByID(channelID)
	if err != nil {
		t.Fatalf("get channel: %v", err)
	}
	if storedChannel.LastPostID != 7 {
		t.Fatalf("last_post_id = %d, want 7", storedChannel.LastPostID)
	}
	storedPost, err := database.Posts.GetByChannelAndMessageID(channelID, 7)
	if err != nil {
		t.Fatalf("get post: %v", err)
	}
	if storedPost.URL != "https://t.me/digest_channel/7" {
		t.Fatalf("stored URL = %q, want canonical URL", storedPost.URL)
	}
}
