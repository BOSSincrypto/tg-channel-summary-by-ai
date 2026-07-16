package digest

import (
	"encoding/json"
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

func TestGenerateUsesEffectiveGroupAIProviderAndModel(t *testing.T) {
	parserServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<div class="tgme_widget_message" data-post="ai_channel/8"><div class="tgme_widget_message_text">новый пост для группы</div><time datetime="2099-07-15T18:30:00Z"></time></div>`))
	}))
	defer parserServer.Close()

	var request struct {
		Model string `json:"model"`
	}
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode provider request: %v", err)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"[{\"post_id\":1,\"summary\":\"Группа получила новый пост.\"}]"}}]}`))
	}))
	defer providerServer.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()
	providerID, err := database.Providers.Insert(&model.AIProvider{
		Name:         "Group provider",
		BaseURL:      providerServer.URL,
		APIKey:       "group-provider-key",
		DefaultModel: "provider-model",
		IsDefault:    true,
	})
	if err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	channelID, err := database.Channels.Insert(&model.Channel{Username: "ai_channel", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	groupID, err := database.Groups.Insert(&model.Group{TelegramChatID: -1004, Title: "AI group"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := database.Groups.AssignChannel(groupID, channelID, nil); err != nil {
		t.Fatalf("assign channel: %v", err)
	}
	override := "group-model"
	if err := database.Groups.UpdateGroupSettings(&model.GroupSettings{
		GroupID:    groupID,
		ProviderID: &providerID,
		Model:      &override,
		DigestTime: "21:00",
		Timezone:   "Europe/Moscow",
	}); err != nil {
		t.Fatalf("update group settings: %v", err)
	}

	fetcher := parser.NewWithOptions(parser.Options{Client: parserServer.Client(), BaseURL: parserServer.URL})
	processor := parser.NewChannelProcessor(fetcher, parser.NewPostStorage(database.Channels, database.Posts))
	service := NewWithProcessorAndAIForTesting(database, processor, database.Groups, providerServer.Client())
	if _, err := service.Generate(groupID); err != nil {
		t.Fatalf("generate digest: %v", err)
	}

	if request.Model != override {
		t.Fatalf("provider model = %q, want group override %q", request.Model, override)
	}
	post, err := database.Posts.GetByChannelAndMessageID(channelID, 8)
	if err != nil {
		t.Fatalf("get stored post: %v", err)
	}
	if post.Summary == nil || *post.Summary != "Группа получила новый пост." {
		t.Fatalf("post summary = %v, want AI summary", post.Summary)
	}
}
