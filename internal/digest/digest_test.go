package digest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/parser"
	"github.com/boss/tg-channel-summary-by-ai/internal/summarizer"
)

type recordingDigestNotifier struct {
	mu       sync.Mutex
	messages []string
}

func (n *recordingDigestNotifier) NotifyOwner(_ context.Context, text string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.messages = append(n.messages, text)
	return nil
}

func (n *recordingDigestNotifier) snapshot() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, len(n.messages))
	copy(out, n.messages)
	return out
}

// failingThenSucceedingNotifier fails the first N deliveries, then succeeds.
type failingThenSucceedingNotifier struct {
	mu           sync.Mutex
	failuresLeft int
	messages     []string
	attempts     int
}

func (n *failingThenSucceedingNotifier) NotifyOwner(_ context.Context, text string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.attempts++
	if n.failuresLeft > 0 {
		n.failuresLeft--
		return errors.New("telegram sendMessage temporarily unavailable")
	}
	n.messages = append(n.messages, text)
	return nil
}

// concurrentGateNotifier serializes concurrent NotifyOwner calls and records
// how many deliveries overlapped. The first caller blocks until release is closed.
type concurrentGateNotifier struct {
	started   chan struct{}
	release   chan struct{}
	mu        sync.Mutex
	messages  []string
	active    int32
	maxActive int32
	calls     int32
}

func (n *concurrentGateNotifier) NotifyOwner(_ context.Context, text string) error {
	atomic.AddInt32(&n.calls, 1)
	current := atomic.AddInt32(&n.active, 1)
	for {
		max := atomic.LoadInt32(&n.maxActive)
		if current <= max || atomic.CompareAndSwapInt32(&n.maxActive, max, current) {
			break
		}
	}
	select {
	case <-n.started:
	default:
		close(n.started)
	}
	<-n.release
	n.mu.Lock()
	n.messages = append(n.messages, text)
	n.mu.Unlock()
	atomic.AddInt32(&n.active, -1)
	return nil
}

// blockingFailingNotifier keeps the first delivery in flight, then fails it.
// A waiting retry should take over after that failure releases the claim.
type blockingFailingNotifier struct {
	started  chan struct{}
	release  chan struct{}
	mu       sync.Mutex
	messages []string
	attempts int32
}

func (n *blockingFailingNotifier) NotifyOwner(_ context.Context, text string) error {
	attempt := atomic.AddInt32(&n.attempts, 1)
	if attempt == 1 {
		close(n.started)
		<-n.release
		return errors.New("telegram sendMessage temporarily unavailable")
	}
	n.mu.Lock()
	n.messages = append(n.messages, text)
	n.mu.Unlock()
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

func TestGenerateAIParseFailureNotifiesOnceAndPreservesPosts(t *testing.T) {
	database, groupID, postID := newDigestFailureFixture(t)
	notifier := &recordingDigestNotifier{}
	service := NewWithProcessor(database, nil)
	service.aiConfigSource = database.Groups
	service.notifier = notifier
	service.providerFactory = func(summarizer.GroupAIConfigSource, int64, *http.Client, func(error)) (summarizer.Provider, error) {
		return failingDigestProvider{err: errors.New("parse summaries: expected JSON summary array")}, nil
	}

	err := service.summarize(groupID, []model.Post{{ID: postID}})
	if err == nil {
		t.Fatal("expected parse failure")
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("notifications = %d, want one: %v", len(notifier.messages), notifier.messages)
	}
	if !strings.Contains(notifier.messages[0], "разобрать") || !strings.Contains(notifier.messages[0], "Failure group") {
		t.Fatalf("notification = %q, want actionable parse context", notifier.messages[0])
	}
	post, err := database.Posts.GetByID(postID)
	if err != nil {
		t.Fatalf("get post: %v", err)
	}
	if post.Summary != nil {
		t.Fatalf("post summary = %v, want unsummarized post preserved", post.Summary)
	}
}

func TestSummarizeRejectsIncompleteResultBeforePersistence(t *testing.T) {
	database, groupID, postIDs := newDigestPostsFixture(t, 2)
	notifier := &recordingDigestNotifier{}
	service := NewWithProcessor(database, nil)
	service.aiConfigSource = database.Groups
	service.notifier = notifier
	service.providerFactory = func(summarizer.GroupAIConfigSource, int64, *http.Client, func(error)) (summarizer.Provider, error) {
		return staticDigestProvider{summaries: []summarizer.Summary{
			{PostID: postIDs[0], Text: "Первый пост обработан."},
		}}, nil
	}

	err := service.summarize(groupID, []model.Post{
		{ID: postIDs[0]},
		{ID: postIDs[1]},
	})
	if err == nil {
		t.Fatal("expected incomplete summary result to fail")
	}
	if !strings.Contains(err.Error(), "summary") {
		t.Fatalf("error = %q, want summary validation context", err)
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("notifications = %d, want one: %v", len(notifier.messages), notifier.messages)
	}
	for _, postID := range postIDs {
		post, getErr := database.Posts.GetByID(postID)
		if getErr != nil {
			t.Fatalf("get post %d: %v", postID, getErr)
		}
		if post.Summary != nil {
			t.Fatalf("post %d summary = %v, want unsummarized post preserved", postID, post.Summary)
		}
	}
}

func TestSummarizeRejectsOverCompleteResultAndDoesNotPersistAnySummary(t *testing.T) {
	database, groupID, postIDs := newDigestPostsFixture(t, 2)
	service := NewWithProcessor(database, nil)
	service.aiConfigSource = database.Groups
	service.providerFactory = func(summarizer.GroupAIConfigSource, int64, *http.Client, func(error)) (summarizer.Provider, error) {
		return staticDigestProvider{summaries: []summarizer.Summary{
			{PostID: postIDs[0], Text: "Первый пост обработан."},
			{PostID: postIDs[1], Text: "Второй пост обработан."},
			{PostID: 999999, Text: "Выдуманный пост обработан."},
		}}, nil
	}

	err := service.summarize(groupID, []model.Post{
		{ID: postIDs[0]},
		{ID: postIDs[1]},
	})
	if err == nil {
		t.Fatal("expected over-complete summary result to fail")
	}
	for _, postID := range postIDs {
		post, getErr := database.Posts.GetByID(postID)
		if getErr != nil {
			t.Fatalf("get post %d: %v", postID, getErr)
		}
		if post.Summary != nil {
			t.Fatalf("post %d summary = %v, want no partial persistence", postID, post.Summary)
		}
	}
}

func TestGeneratePermanentAIFailureNotifiesOnceWithoutRetry(t *testing.T) {
	database, groupID, postID := newDigestFailureFixture(t)
	notifier := &recordingDigestNotifier{}
	providerCalls := 0
	service := NewWithProcessor(database, nil)
	service.aiConfigSource = database.Groups
	service.notifier = notifier
	service.providerFactory = func(summarizer.GroupAIConfigSource, int64, *http.Client, func(error)) (summarizer.Provider, error) {
		return failingDigestProvider{
			err: &summarizer.ProviderError{
				StatusCode: http.StatusUnauthorized,
				Message:    "invalid API key",
				Provider:   "OpenRouter",
			},
			calls: &providerCalls,
		}, nil
	}

	err := service.summarize(groupID, []model.Post{{ID: postID}})
	if err == nil {
		t.Fatal("expected permanent provider failure")
	}
	if providerCalls != 1 {
		t.Fatalf("provider calls = %d, want one because permanent errors are not retried", providerCalls)
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("notifications = %d, want one: %v", len(notifier.messages), notifier.messages)
	}
	message := notifier.messages[0]
	if !strings.Contains(message, "401") || !strings.Contains(message, "Failure group") || !strings.Contains(message, "ключ") {
		t.Fatalf("notification = %q, want status, group, and key action", message)
	}
}

func TestGenerateExhaustedTransientFailureNotifiesOnce(t *testing.T) {
	database, groupID, postID := newDigestFailureFixture(t)
	notifier := &recordingDigestNotifier{}
	service := NewWithProcessor(database, nil)
	service.aiConfigSource = database.Groups
	service.notifier = notifier
	service.providerFactory = func(summarizer.GroupAIConfigSource, int64, *http.Client, func(error)) (summarizer.Provider, error) {
		return failingDigestProvider{err: &summarizer.ProviderError{
			StatusCode: http.StatusBadGateway,
			Message:    "temporary outage",
			Provider:   "OpenRouter",
		}}, nil
	}

	err := service.summarize(groupID, []model.Post{{ID: postID}})
	if err == nil {
		t.Fatal("expected exhausted transient failure")
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("notifications = %d, want one: %v", len(notifier.messages), notifier.messages)
	}
	message := notifier.messages[0]
	if !strings.Contains(message, "OpenRouter") || !strings.Contains(message, "5xx") ||
		!strings.Contains(message, "повтор") {
		t.Fatalf("notification = %q, want provider, retry outcome, and next action", message)
	}
}

func TestGenerateSuccessfulRetryDoesNotNotifyFailure(t *testing.T) {
	database, groupID, postID := newDigestFailureFixture(t)
	notifier := &recordingDigestNotifier{}
	service := NewWithProcessor(database, nil)
	service.aiConfigSource = database.Groups
	service.notifier = notifier
	service.providerFactory = func(summarizer.GroupAIConfigSource, int64, *http.Client, func(error)) (summarizer.Provider, error) {
		return successfulDigestProvider{}, nil
	}

	if err := service.summarize(groupID, []model.Post{{ID: postID}}); err != nil {
		t.Fatalf("summarize after successful retry: %v", err)
	}
	if len(notifier.messages) != 0 {
		t.Fatalf("notifications = %v, want none after recovery", notifier.messages)
	}
}

func TestGenerateAndManualDigestEachNotifyOnePermanentAIFailure(t *testing.T) {
	parserServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<div class="tgme_widget_message" data-post="cycle_channel/1"><div class="tgme_widget_message_text">пост цикла</div><time datetime="2099-07-15T18:30:00Z"></time></div>`))
	}))
	defer parserServer.Close()
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"invalid key"}}`, http.StatusForbidden)
	}))
	defer providerServer.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()
	providerID, err := database.Providers.Insert(&model.AIProvider{
		Name: "OpenRouter", BaseURL: providerServer.URL, APIKey: "cycle-key", DefaultModel: "cycle-model", IsDefault: true,
	})
	if err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	channelID, err := database.Channels.Insert(&model.Channel{Username: "cycle_channel", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	groupID, err := database.Groups.Insert(&model.Group{TelegramChatID: -10021, Title: "Cycle group"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := database.Groups.AssignChannel(groupID, channelID, nil); err != nil {
		t.Fatalf("assign channel: %v", err)
	}
	if err := database.Groups.UpdateGroupSettings(&model.GroupSettings{
		GroupID: groupID, ProviderID: &providerID, DigestTime: "21:00", Timezone: "UTC",
	}); err != nil {
		t.Fatalf("update group settings: %v", err)
	}

	fetcher := parser.NewWithOptions(parser.Options{Client: parserServer.Client(), BaseURL: parserServer.URL})
	processor := parser.NewChannelProcessor(fetcher, parser.NewPostStorage(database.Channels, database.Posts))
	notifier := &recordingDigestNotifier{}
	service := NewWithProcessorAndAIForTesting(database, processor, database.Groups, providerServer.Client(), notifier)

	if _, err := service.GenerateWithWindow(groupID, "scheduled-cycle-window"); err == nil {
		t.Fatal("scheduled digest should fail cleanly on permanent AI error")
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("scheduled notifications = %d, want one: %v", len(notifier.messages), notifier.messages)
	}
	if _, err := service.GenerateManualWithWindow(groupID, "manual-cycle-window"); err == nil {
		t.Fatal("manual digest should fail cleanly on permanent AI error")
	}
	if len(notifier.messages) != 2 {
		t.Fatalf("scheduled and manual notifications = %d, want one per cycle: %v", len(notifier.messages), notifier.messages)
	}
	if !strings.Contains(notifier.messages[0], "scheduled-cycle-window") ||
		!strings.Contains(notifier.messages[1], "manual-cycle-window") {
		t.Fatalf("cycle notifications = %v, want scheduled and manual window IDs", notifier.messages)
	}
	posts, err := database.Posts.ListUnsummarized(groupID, 24)
	if err != nil {
		t.Fatalf("list unsummarized posts: %v", err)
	}
	if len(posts) != 1 {
		t.Fatalf("unsummarized posts = %d, want one preserved post", len(posts))
	}
}

func TestGenerateFallbackOpenRouterOutageNotifiesOnlyTerminalFailure(t *testing.T) {
	database, groupID, postID := newDigestFailureFixture(t)
	notifier := &recordingDigestNotifier{}
	service := NewWithProcessor(database, nil)
	service.aiConfigSource = database.Groups
	service.notifier = notifier
	service.providerFactory = func(_ summarizer.GroupAIConfigSource, _ int64, _ *http.Client, onFallback func(error)) (summarizer.Provider, error) {
		onFallback(errors.New("custom provider unavailable"))
		return failingDigestProvider{err: &summarizer.ProviderError{
			StatusCode: http.StatusBadGateway, Provider: "OpenRouter",
		}}, nil
	}

	if err := service.summarize(groupID, []model.Post{{ID: postID}}); err == nil {
		t.Fatal("expected fallback outage")
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("notifications = %v, want one terminal failure notification", notifier.messages)
	}
	if !strings.Contains(notifier.messages[0], "OpenRouter недоступен") ||
		strings.Contains(notifier.messages[0], "использован OpenRouter") {
		t.Fatalf("notification = %q, want only outage context", notifier.messages[0])
	}
}

func TestOpenRouterOutageNotificationIsDeduplicatedAcrossGroups(t *testing.T) {
	notifier := &recordingDigestNotifier{}
	service := &Service{notifier: notifier}
	err := &summarizer.ProviderError{
		StatusCode: http.StatusBadGateway,
		Provider:   "OpenRouter",
	}

	service.notifyAIFailureForWindow("shared-window", 101, err)
	service.notifyAIFailureForWindow("shared-window", 202, err)

	if len(notifier.messages) != 1 {
		t.Fatalf("notifications = %d, want one outage notification across groups: %v", len(notifier.messages), notifier.messages)
	}
	for _, want := range []string{"OpenRouter недоступен", "Провайдер: OpenRouter", "Ошибка: HTTP 502 (5xx)"} {
		if !strings.Contains(notifier.messages[0], want) {
			t.Fatalf("notification = %q, want outage context %q", notifier.messages[0], want)
		}
	}
}

func TestOpenRouterOutageNotificationUsesExplicitWindowIDs(t *testing.T) {
	notifier := &recordingDigestNotifier{}
	service := &Service{notifier: notifier}
	err := &summarizer.ProviderError{
		StatusCode: http.StatusBadGateway,
		Provider:   "OpenRouter",
	}

	service.notifyAIFailureForWindow("scheduled-window-a", 101, err)
	service.notifyAIFailureForWindow("scheduled-window-a", 202, err)
	service.notifyAIFailureForWindow("scheduled-window-b", 303, err)

	messages := notifier.snapshot()
	if len(messages) != 2 {
		t.Fatalf("notifications = %d, want one per explicit window: %v", len(messages), messages)
	}
	for _, want := range []string{"scheduled-window-a", "scheduled-window-b"} {
		found := false
		for _, message := range messages {
			if strings.Contains(message, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("notifications = %v, want window ID %q", messages, want)
		}
	}
}

func TestOpenRouterOutageNotificationRetriesAfterDeliveryFailure(t *testing.T) {
	notifier := &failingThenSucceedingNotifier{failuresLeft: 1}
	service := &Service{notifier: notifier}
	err := &summarizer.ProviderError{
		StatusCode: http.StatusBadGateway,
		Provider:   "OpenRouter",
		Message:    "temporary outage",
	}

	service.notifyAIFailureForWindow("window-retry", 101, err)
	if len(notifier.messages) != 0 {
		t.Fatalf("messages after failed delivery = %v, want none", notifier.messages)
	}
	if notifier.attempts != 1 {
		t.Fatalf("attempts after first failure = %d, want 1", notifier.attempts)
	}

	// A later affected group must be able to retry the same window/category.
	service.notifyAIFailureForWindow("window-retry", 202, err)
	if notifier.attempts != 2 {
		t.Fatalf("attempts after retry = %d, want 2", notifier.attempts)
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("messages after successful retry = %v, want one delivered alert", notifier.messages)
	}
	if !strings.Contains(notifier.messages[0], "window-retry") ||
		!strings.Contains(notifier.messages[0], "OpenRouter недоступен") {
		t.Fatalf("delivered message = %q, want sanitized outage diagnostics with window ID", notifier.messages[0])
	}

	// Successful delivery permanently suppresses later duplicates.
	service.notifyAIFailureForWindow("window-retry", 303, err)
	if notifier.attempts != 2 {
		t.Fatalf("attempts after success suppression = %d, want 2", notifier.attempts)
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("messages after success suppression = %v, want one", notifier.messages)
	}
}

func TestOpenRouterOutageNotificationGeneratesWindowForEmptyIDs(t *testing.T) {
	notifier := &recordingDigestNotifier{}
	service := &Service{notifier: notifier}
	err := &summarizer.ProviderError{
		StatusCode: http.StatusBadGateway,
		Provider:   "OpenRouter",
	}

	service.notifyAIFailureForWindow("", 101, err)
	service.notifyAIFailureForWindow("", 202, err)

	messages := notifier.snapshot()
	if len(messages) != 2 {
		t.Fatalf("notifications = %d, want one per generated window: %v", len(messages), messages)
	}
	for _, message := range messages {
		if !strings.Contains(message, "Окно дайджеста: scheduled-") {
			t.Fatalf("notification = %q, want generated scheduled window ID", message)
		}
	}
	if messages[0] == messages[1] {
		t.Fatalf("generated notifications unexpectedly identical: %v", messages)
	}
}

func TestOpenRouterOutageNotificationWaitingClaimRetriesAfterFailure(t *testing.T) {
	notifier := &blockingFailingNotifier{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	waitObserved := make(chan struct{})
	service := &Service{
		notifier:                 notifier,
		notificationWaitObserved: waitObserved,
	}
	err := &summarizer.ProviderError{
		StatusCode: http.StatusBadGateway,
		Provider:   "OpenRouter",
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		service.notifyAIFailureForWindow("window-waiting-retry", 101, err)
	}()
	<-notifier.started
	go func() {
		defer wg.Done()
		service.notifyAIFailureForWindow("window-waiting-retry", 202, err)
	}()

	<-waitObserved
	close(notifier.release)
	wg.Wait()

	if got := atomic.LoadInt32(&notifier.attempts); got != 2 {
		t.Fatalf("NotifyOwner attempts = %d, want failed owner plus waiting retry", got)
	}
	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if len(notifier.messages) != 1 || !strings.Contains(notifier.messages[0], "window-waiting-retry") {
		t.Fatalf("delivered messages = %v, want one retry notification", notifier.messages)
	}
}

func TestOpenRouterOutageNotificationRepeatedFailuresRemainRetryable(t *testing.T) {
	notifier := &failingThenSucceedingNotifier{failuresLeft: 3}
	service := &Service{notifier: notifier}
	err := &summarizer.ProviderError{
		StatusCode: http.StatusServiceUnavailable,
		Provider:   "OpenRouter",
	}

	for groupID := int64(1); groupID <= 3; groupID++ {
		service.notifyAIFailureForWindow("window-repeat-fail", groupID, err)
	}
	if notifier.attempts != 3 {
		t.Fatalf("attempts = %d, want three failed deliveries", notifier.attempts)
	}
	if len(notifier.messages) != 0 {
		t.Fatalf("messages = %v, want none while deliveries keep failing", notifier.messages)
	}

	service.notifyAIFailureForWindow("window-repeat-fail", 4, err)
	if notifier.attempts != 4 || len(notifier.messages) != 1 {
		t.Fatalf("after recovery: attempts=%d messages=%v, want one success", notifier.attempts, notifier.messages)
	}
}

func TestOpenRouterOutageNotificationSerializesConcurrentAttempts(t *testing.T) {
	notifier := &concurrentGateNotifier{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	waitObserved := make(chan struct{})
	service := &Service{
		notifier:                 notifier,
		notificationWaitObserved: waitObserved,
	}
	err := &summarizer.ProviderError{
		StatusCode: http.StatusBadGateway,
		Provider:   "OpenRouter",
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		service.notifyAIFailureForWindow("window-concurrent", 101, err)
	}()
	<-notifier.started
	go func() {
		defer wg.Done()
		service.notifyAIFailureForWindow("window-concurrent", 202, err)
	}()
	<-waitObserved
	close(notifier.release)
	wg.Wait()

	if got := atomic.LoadInt32(&notifier.calls); got != 1 {
		t.Fatalf("NotifyOwner calls = %d, want 1 concurrent delivery", got)
	}
	if got := atomic.LoadInt32(&notifier.maxActive); got != 1 {
		t.Fatalf("max concurrent deliveries = %d, want 1", got)
	}
	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if len(notifier.messages) != 1 {
		t.Fatalf("messages = %v, want exactly one successful alert", notifier.messages)
	}
}

func TestGeneratePropagatesExplicitWindowIDAndManualCallsCreateDistinctWindows(t *testing.T) {
	database, groupID, postID := newDigestFailureFixture(t)
	notifier := &recordingDigestNotifier{}
	service := NewWithProcessor(database, nil)
	service.aiConfigSource = database.Groups
	service.notifier = notifier
	service.providerFactory = func(summarizer.GroupAIConfigSource, int64, *http.Client, func(error)) (summarizer.Provider, error) {
		return failingDigestProvider{err: &summarizer.ProviderError{
			StatusCode: http.StatusBadGateway,
			Provider:   "OpenRouter",
		}}, nil
	}

	err := service.summarizeWithWindow(groupID, []model.Post{{ID: postID}}, "scheduled-window-boundary")
	if err == nil {
		t.Fatal("expected scheduled outage")
	}
	if len(notifier.messages) != 1 || !strings.Contains(notifier.messages[0], "scheduled-window-boundary") {
		t.Fatalf("scheduled notifications = %v, want explicit window ID", notifier.messages)
	}

	// The post remains unsummarized, so a manual retry is still eligible and
	// receives a new logical window instead of inheriting the scheduled one.
	if err := service.summarizeWithWindow(groupID, []model.Post{{ID: postID}}, NewWindowID("manual")); err == nil {
		t.Fatal("expected manual outage")
	}
	if len(notifier.messages) != 2 {
		t.Fatalf("notifications after manual retry = %v, want one per window", notifier.messages)
	}
	if strings.Contains(notifier.messages[1], "scheduled-window-boundary") {
		t.Fatalf("manual notification reused scheduled window ID: %q", notifier.messages[1])
	}
	post, err := database.Posts.GetByID(postID)
	if err != nil {
		t.Fatalf("get post: %v", err)
	}
	if post.Summary != nil {
		t.Fatalf("post summary = %v, want retry-eligible unsummarized post", post.Summary)
	}
}

func TestClassifyOpenRouterTransportFailureAsOutage(t *testing.T) {
	err := &summarizer.ProviderError{
		Message:  `OpenRouter request: dial tcp: connection refused`,
		Provider: "OpenRouter",
	}

	if got := classifyAIFailure(err); got != aiFailureOpenRouterOutage {
		t.Fatalf("failure class = %q, want %q", got, aiFailureOpenRouterOutage)
	}
}

func newDigestFailureFixture(t *testing.T) (*db.DB, int64, int64) {
	t.Helper()
	database, groupID, postIDs := newDigestPostsFixture(t, 1)
	return database, groupID, postIDs[0]
}

func newDigestPostsFixture(t *testing.T, count int) (*db.DB, int64, []int64) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	channelID, err := database.Channels.Insert(&model.Channel{Username: "failure_channel", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	groupID, err := database.Groups.Insert(&model.Group{TelegramChatID: -10020, Title: "Failure group"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := database.Groups.AssignChannel(groupID, channelID, nil); err != nil {
		t.Fatalf("assign channel: %v", err)
	}
	postIDs := make([]int64, 0, count)
	for messageID := 1; messageID <= count; messageID++ {
		postID, insertErr := database.Posts.Insert(&model.Post{
			ChannelID: channelID, MessageID: int64(messageID), Text: "тестовый пост",
			PostedAt:    time.Now().UTC().Format(time.RFC3339),
			URL:         "https://t.me/failure_channel/" + fmt.Sprint(messageID),
			ContentHash: "failure-post-" + fmt.Sprint(messageID),
		})
		if insertErr != nil {
			t.Fatalf("insert post %d: %v", messageID, insertErr)
		}
		postIDs = append(postIDs, postID)
	}
	return database, groupID, postIDs
}

type failingDigestProvider struct {
	err   error
	calls *int
}

func (p failingDigestProvider) Summarize(context.Context, []summarizer.Post) ([]summarizer.Summary, error) {
	if p.calls != nil {
		(*p.calls)++
	}
	return nil, p.err
}

type successfulDigestProvider struct{}

func (successfulDigestProvider) Summarize(_ context.Context, posts []summarizer.Post) ([]summarizer.Summary, error) {
	summaries := make([]summarizer.Summary, 0, len(posts))
	for _, post := range posts {
		summaries = append(summaries, summarizer.Summary{PostID: post.ID, Text: "Пост обработан."})
	}
	return summaries, nil
}

type staticDigestProvider struct {
	summaries []summarizer.Summary
}

func (p staticDigestProvider) Summarize(context.Context, []summarizer.Post) ([]summarizer.Summary, error) {
	return p.summaries, nil
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

func TestGenerateCapsPostsPerChannelBeforeSummarizationAndDefersExcess(t *testing.T) {
	parserServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<div class="tgme_channel_info"></div>`))
	}))
	defer parserServer.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	channelA, err := database.Channels.Insert(&model.Channel{Username: "capped_a", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel A: %v", err)
	}
	channelB, err := database.Channels.Insert(&model.Channel{Username: "capped_b", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel B: %v", err)
	}
	groupID, err := database.Groups.Insert(&model.Group{TelegramChatID: -1011, Title: "Capped"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	for _, channelID := range []int64{channelA, channelB} {
		if err := database.Groups.AssignChannel(groupID, channelID, nil); err != nil {
			t.Fatalf("assign channel %d: %v", channelID, err)
		}
	}

	postedAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	for _, item := range []struct {
		channelID int64
		messageID int64
	}{
		{channelA, 1}, {channelA, 2}, {channelA, 3},
		{channelB, 1}, {channelB, 2}, {channelB, 3},
	} {
		if _, err := database.Posts.Insert(&model.Post{
			ChannelID: item.channelID, MessageID: item.messageID,
			Text:        "post to cap",
			PostedAt:    postedAt,
			URL:         "https://t.me/capped/" + string(rune('0'+item.messageID)),
			ContentHash: "unique-" + string(rune('0'+item.channelID)) + "-" + string(rune('0'+item.messageID)),
		}); err != nil {
			t.Fatalf("insert post %d/%d: %v", item.channelID, item.messageID, err)
		}
	}

	fetcher := parser.NewWithOptions(parser.Options{Client: parserServer.Client(), BaseURL: parserServer.URL})
	processor := parser.NewChannelProcessor(fetcher, parser.NewPostStorage(database.Channels, database.Posts))
	provider := &recordingDigestProvider{}
	service := NewWithProcessor(database, processor, WithMaxPostsPerChannel(2))
	service.aiConfigSource = database.Groups
	service.providerFactory = func(summarizer.GroupAIConfigSource, int64, *http.Client, func(error)) (summarizer.Provider, error) {
		return provider, nil
	}

	first, err := service.Generate(groupID)
	if err != nil {
		t.Fatalf("first generate: %v", err)
	}
	if first.PostCount != 4 {
		t.Fatalf("first digest post count = %d, want 4", first.PostCount)
	}
	if len(provider.calls) != 1 || len(provider.calls[0]) != 4 {
		t.Fatalf("first provider calls = %#v, want one call with four capped posts", provider.calls)
	}
	counts := map[int64]int{}
	for _, post := range provider.calls[0] {
		counts[post.ID]++
	}
	if len(counts) != 4 {
		t.Fatalf("first provider post IDs = %#v, want four unique posts", counts)
	}

	remaining, err := database.Posts.ListUnsummarized(groupID, 24)
	if err != nil {
		t.Fatalf("list deferred posts: %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("deferred posts = %d, want 2", len(remaining))
	}

	second, err := service.GenerateManual(groupID)
	if err != nil {
		t.Fatalf("second generate: %v", err)
	}
	if second.PostCount != 2 {
		t.Fatalf("second digest post count = %d, want 2 deferred posts", second.PostCount)
	}
	if len(provider.calls) != 2 || len(provider.calls[1]) != 2 {
		t.Fatalf("second provider calls = %#v, want deferred posts only", provider.calls)
	}
	remaining, err = database.Posts.ListUnsummarized(groupID, 24)
	if err != nil {
		t.Fatalf("list posts after second cycle: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("posts remaining after second cycle = %d, want 0", len(remaining))
	}
}

type recordingDigestProvider struct {
	calls [][]summarizer.Post
}

func TestCapPostsPerChannelDefaultsToFiftyAndKeepsChannelsIndependent(t *testing.T) {
	posts := make([]model.Post, 0, 102)
	for channelID := int64(1); channelID <= 2; channelID++ {
		for messageID := int64(1); messageID <= 51; messageID++ {
			posts = append(posts, model.Post{ChannelID: channelID, MessageID: messageID})
		}
	}

	selected := capPostsPerChannel(posts, 0)
	if len(selected) != 100 {
		t.Fatalf("selected posts = %d, want 100 with default limit 50 per channel", len(selected))
	}
	counts := map[int64]int{}
	for _, post := range selected {
		counts[post.ChannelID]++
	}
	if counts[1] != 50 || counts[2] != 50 {
		t.Fatalf("selected counts by channel = %#v, want 50 each", counts)
	}
}

type outcomeFetcher struct {
	posts map[string][]parser.ParsedPost
	errs  map[string]error
}

func (f outcomeFetcher) ParseChannel(username string) ([]parser.ParsedPost, error) {
	if err := f.errs[username]; err != nil {
		return nil, err
	}
	return f.posts[username], nil
}

type outcomeDelivery struct {
	err error
}

func (d outcomeDelivery) Deliver(context.Context, int64, *Digest) (DeliveryReceipt, error) {
	if d.err != nil {
		return DeliveryReceipt{}, d.err
	}
	return DeliveryReceipt{MessageID: 777, MessageURL: "https://t.me/c/777"}, nil
}

func TestGenerateManualResultExposesAllTerminalOutcomes(t *testing.T) {
	tests := []struct {
		name            string
		channels        []string
		posts           map[string][]parser.ParsedPost
		errs            map[string]error
		provider        summarizer.Provider
		delivery        Delivery
		wantOutcome     string
		wantPostCount   int
		wantFailedCount int
		wantSent        bool
		wantSaved       bool
	}{
		{
			name: "succeeded", channels: []string{"ok"},
			posts:    map[string][]parser.ParsedPost{"ok": {{MessageID: 1, Text: "пост", PostedAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)}}},
			provider: successfulDigestProvider{}, delivery: outcomeDelivery{},
			wantOutcome: OutcomeSucceeded, wantPostCount: 1, wantSent: true, wantSaved: true,
		},
		{
			name: "no posts", channels: []string{"empty"},
			posts:    map[string][]parser.ParsedPost{"empty": {}},
			provider: successfulDigestProvider{}, delivery: outcomeDelivery{},
			wantOutcome: OutcomeNoPosts,
		},
		{
			name: "partial", channels: []string{"ok", "broken"},
			posts:    map[string][]parser.ParsedPost{"ok": {{MessageID: 2, Text: "пост", PostedAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)}}},
			errs:     map[string]error{"broken": errors.New("channel unavailable")},
			provider: successfulDigestProvider{}, delivery: outcomeDelivery{},
			wantOutcome: OutcomePartial, wantPostCount: 1, wantFailedCount: 1, wantSent: true, wantSaved: true,
		},
		{
			name: "all channels failed", channels: []string{"broken"},
			errs:     map[string]error{"broken": errors.New("channel unavailable")},
			provider: successfulDigestProvider{}, delivery: outcomeDelivery{},
			wantOutcome: OutcomeAllChannelsFailed, wantFailedCount: 1,
		},
		{
			name: "ai failed", channels: []string{"ok"},
			posts:    map[string][]parser.ParsedPost{"ok": {{MessageID: 3, Text: "пост", PostedAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)}}},
			provider: failingDigestProvider{err: errors.New("AI timeout")}, delivery: outcomeDelivery{},
			wantOutcome: OutcomeAIFailed, wantPostCount: 1,
		},
		{
			name: "delivery failed", channels: []string{"ok"},
			posts:    map[string][]parser.ParsedPost{"ok": {{MessageID: 4, Text: "пост", PostedAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)}}},
			provider: successfulDigestProvider{}, delivery: outcomeDelivery{err: errors.New("Telegram sendMessage failed")},
			wantOutcome: OutcomeDeliveryFailed, wantPostCount: 1, wantSaved: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database, err := db.Open(":memory:")
			if err != nil {
				t.Fatalf("open database: %v", err)
			}
			defer database.Close()
			groupID, err := database.Groups.Insert(&model.Group{TelegramChatID: -10077, Title: "Outcome"})
			if err != nil {
				t.Fatalf("insert group: %v", err)
			}
			for _, username := range test.channels {
				channelID, insertErr := database.Channels.Insert(&model.Channel{Username: username, Enabled: true})
				if insertErr != nil {
					t.Fatalf("insert channel: %v", insertErr)
				}
				if assignErr := database.Groups.AssignChannel(groupID, channelID, nil); assignErr != nil {
					t.Fatalf("assign channel: %v", assignErr)
				}
			}
			processor := parser.NewChannelProcessor(
				outcomeFetcher{posts: test.posts, errs: test.errs},
				parser.NewPostStorage(database.Channels, database.Posts),
			)
			service := NewWithProcessor(database, processor)
			service.aiConfigSource = database.Groups
			service.providerFactory = func(summarizer.GroupAIConfigSource, int64, *http.Client, func(error)) (summarizer.Provider, error) {
				return test.provider, nil
			}
			service.delivery = test.delivery

			result, err := service.GenerateManualResult(groupID)
			if err != nil {
				t.Fatalf("GenerateManualResult: %v", err)
			}
			if result.Outcome != test.wantOutcome {
				t.Fatalf("outcome = %q, want %q", result.Outcome, test.wantOutcome)
			}
			if result.PostCount != test.wantPostCount || len(result.FailedChannels) != test.wantFailedCount {
				t.Fatalf("counts = posts:%d failed:%v, want posts:%d failed:%d", result.PostCount, result.FailedChannels, test.wantPostCount, test.wantFailedCount)
			}
			if result.Delivered != test.wantSent || result.SummariesSaved != test.wantSaved {
				t.Fatalf("delivery state = delivered:%v saved:%v, want delivered:%v saved:%v", result.Delivered, result.SummariesSaved, test.wantSent, test.wantSaved)
			}
			if result.Outcome == OutcomeNoPosts || result.Outcome == OutcomeAllChannelsFailed || result.Outcome == OutcomeAIFailed {
				if result.Delivered {
					t.Fatal("terminal non-delivery outcome reported delivered")
				}
			}
			if result.Outcome == OutcomeDeliveryFailed && !result.SummariesSaved {
				t.Fatal("delivery failure must preserve saved summaries")
			}
		})
	}
}

func (p *recordingDigestProvider) Summarize(_ context.Context, posts []summarizer.Post) ([]summarizer.Summary, error) {
	copied := append([]summarizer.Post(nil), posts...)
	p.calls = append(p.calls, copied)
	summaries := make([]summarizer.Summary, 0, len(posts))
	for _, post := range posts {
		summaries = append(summaries, summarizer.Summary{PostID: post.ID, Text: "Пост обработан."})
	}
	return summaries, nil
}
