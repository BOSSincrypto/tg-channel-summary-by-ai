package digest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/parser"
)

type recordingDigestNotifier struct {
	messages []string
}

func (n *recordingDigestNotifier) NotifyOwner(_ context.Context, text string) error {
	n.messages = append(n.messages, text)
	return nil
}

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

func TestGenerateRedactsProviderKeyFromProductionError(t *testing.T) {
	const apiKey = "digest-provider-secret"
	parserServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<div class="tgme_widget_message" data-post="redaction_channel/9"><div class="tgme_widget_message_text">пост для проверки ошибки</div><time datetime="2099-07-15T18:30:00Z"></time></div>`))
	}))
	defer parserServer.Close()

	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"provider request failed with key digest-provider-secret; retry later"}}`, http.StatusBadGateway)
	}))
	defer providerServer.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	providerID, err := database.Providers.Insert(&model.AIProvider{
		Name:         "Redaction provider",
		BaseURL:      providerServer.URL,
		APIKey:       apiKey,
		DefaultModel: "redaction-model",
		IsDefault:    true,
	})
	if err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	channelID, err := database.Channels.Insert(&model.Channel{Username: "redaction_channel", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	groupID, err := database.Groups.Insert(&model.Group{TelegramChatID: -1005, Title: "Redaction group"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := database.Groups.AssignChannel(groupID, channelID, nil); err != nil {
		t.Fatalf("assign channel: %v", err)
	}
	if err := database.Groups.UpdateGroupSettings(&model.GroupSettings{
		GroupID:    groupID,
		ProviderID: &providerID,
		DigestTime: "21:00",
		Timezone:   "UTC",
	}); err != nil {
		t.Fatalf("update group settings: %v", err)
	}

	fetcher := parser.NewWithOptions(parser.Options{Client: parserServer.Client(), BaseURL: parserServer.URL})
	processor := parser.NewChannelProcessor(fetcher, parser.NewPostStorage(database.Channels, database.Posts))
	service := NewWithProcessorAndAIForTesting(database, processor, database.Groups, providerServer.Client())

	_, err = service.Generate(groupID)
	if err == nil {
		t.Fatal("expected digest provider error")
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Fatalf("digest error leaked configured API key: %q", err)
	}
	for _, want := range []string{"summarize group", "Redaction provider", "HTTP 502", "[redacted]"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("digest error %q does not retain safe context %q", err, want)
		}
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

func TestScheduledAndManualDigestUseConfiguredProviderFallback(t *testing.T) {
	var parserCycle int
	parserServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parserCycle++
		if parserCycle == 1 {
			_, _ = w.Write([]byte(`<div class="tgme_widget_message" data-post="failover_channel/1"><div class="tgme_widget_message_text">первый пост</div><time datetime="2099-07-15T18:30:00Z"></time></div>`))
			return
		}
		_, _ = w.Write([]byte(`<div class="tgme_widget_message" data-post="failover_channel/2"><div class="tgme_widget_message_text">второй пост</div><time datetime="2099-07-15T18:31:00Z"></time></div>`))
	}))
	defer parserServer.Close()

	var customCalls, openRouterCalls int
	customServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		customCalls++
		if customCalls <= 4 {
			http.Error(w, `{"error":{"message":"custom provider timed out"}}`, http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"[{\"post_id\":2,\"summary\":\"Второй пост обработан кастомным провайдером.\"}]"}}]}`))
	}))
	defer customServer.Close()
	openRouterServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openRouterCalls++
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"[{\"post_id\":1,\"summary\":\"Первый пост обработан резервным провайдером.\"}]"}}]}`))
	}))
	defer openRouterServer.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()
	customID, err := database.Providers.Insert(&model.AIProvider{
		Name: "Custom provider", BaseURL: customServer.URL, APIKey: "custom-key", DefaultModel: "custom-model",
	})
	if err != nil {
		t.Fatalf("insert custom provider: %v", err)
	}
	if _, err := database.Providers.Insert(&model.AIProvider{
		Name: "OpenRouter", BaseURL: openRouterServer.URL, APIKey: "openrouter-key", DefaultModel: "openrouter-model", IsDefault: true,
	}); err != nil {
		t.Fatalf("insert OpenRouter provider: %v", err)
	}
	channelID, err := database.Channels.Insert(&model.Channel{Username: "failover_channel", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	groupID, err := database.Groups.Insert(&model.Group{TelegramChatID: -1010, Title: "Failover"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := database.Groups.AssignChannel(groupID, channelID, nil); err != nil {
		t.Fatalf("assign channel: %v", err)
	}
	if err := database.Groups.UpdateGroupSettings(&model.GroupSettings{
		GroupID: groupID, ProviderID: &customID, DigestTime: "21:00", Timezone: "UTC",
	}); err != nil {
		t.Fatalf("update group settings: %v", err)
	}

	fetcher := parser.NewWithOptions(parser.Options{Client: parserServer.Client(), BaseURL: parserServer.URL})
	processor := parser.NewChannelProcessor(fetcher, parser.NewPostStorage(database.Channels, database.Posts))
	notifier := &recordingDigestNotifier{}
	service := NewWithProcessorAndAIForTesting(database, processor, database.Groups, customServer.Client(), notifier)

	if _, err := service.Generate(groupID); err != nil {
		t.Fatalf("scheduled digest generation: %v", err)
	}
	if customCalls != 4 || openRouterCalls != 1 {
		t.Fatalf("scheduled provider calls = custom %d, OpenRouter %d, want 4 and 1", customCalls, openRouterCalls)
	}
	if len(notifier.messages) != 1 || !strings.Contains(notifier.messages[0], "OpenRouter") {
		t.Fatalf("fallback notifications = %v, want one OpenRouter notification", notifier.messages)
	}

	if _, err := service.GenerateManual(groupID); err != nil {
		t.Fatalf("manual digest generation: %v", err)
	}
	if customCalls != 5 || openRouterCalls != 1 {
		t.Fatalf("manual provider calls = custom %d, OpenRouter %d, want 5 and 1", customCalls, openRouterCalls)
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("notifications after successful retry = %d, want 1", len(notifier.messages))
	}
	post, err := database.Posts.GetByChannelAndMessageID(channelID, 2)
	if err != nil {
		t.Fatalf("get manually summarized post: %v", err)
	}
	if post.Summary == nil || !strings.Contains(*post.Summary, "кастомным") {
		t.Fatalf("manual post summary = %v, want custom-provider summary", post.Summary)
	}
}
