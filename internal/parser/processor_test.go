package parser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

func TestChannelProcessorPersistsParserOutputIntoSQLite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/s/example" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`
			<div class="tgme_widget_message" data-post="example/41">
				<div class="tgme_widget_message_text">A post with <a href="https://example.com/story#part">a link</a></div>
				<time datetime="2026-07-15T18:30:00+00:00"></time>
			</div>
			<div class="tgme_widget_message" data-post="example/40">
				<a class="tgme_widget_message_photo_wrap"></a>
			</div>`))
	}))
	defer server.Close()

	database, cleanup := newStorageTestDB(t)
	defer cleanup()
	channel := &model.Channel{Username: "Example", Enabled: true}
	channelID, err := database.Channels.Insert(channel)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	channel.ID = channelID

	fetcher := NewWithOptions(Options{Client: server.Client(), BaseURL: server.URL})
	processor := NewChannelProcessor(fetcher, NewPostStorage(database.Channels, database.Posts))
	result, err := processor.ProcessChannel(channel)
	if err != nil {
		t.Fatalf("process channel: %v", err)
	}
	if result.ParsedPosts != 1 || result.StoredPosts != 1 || result.MediaOnlySkipped != 1 {
		t.Fatalf("process result = %+v, want one parsed/stored post and one media-only skip", result)
	}

	stored, err := database.Posts.GetByChannelAndMessageID(channelID, 41)
	if err != nil {
		t.Fatalf("get stored post: %v", err)
	}
	if stored.URL != "https://t.me/example/41" {
		t.Fatalf("stored URL = %q, want canonical URL", stored.URL)
	}
	if stored.ContentHash != HashContent("A post with a link") {
		t.Fatalf("content hash = %q, want %q", stored.ContentHash, HashContent("A post with a link"))
	}
	wantLinks := HashLinkURLs([]string{"https://example.com/story#part"})
	if stored.LinkURLsHash == nil || wantLinks == nil || *stored.LinkURLsHash != *wantLinks {
		t.Fatalf("link URL hash = %v, want %v", stored.LinkURLsHash, wantLinks)
	}

	updated, err := database.Channels.GetByID(channelID)
	if err != nil {
		t.Fatalf("get updated channel: %v", err)
	}
	if updated.LastPostID != 41 {
		t.Fatalf("last_post_id = %d, want 41", updated.LastPostID)
	}

	second, err := processor.ProcessChannel(updated)
	if err != nil {
		t.Fatalf("reprocess channel: %v", err)
	}
	if second.StoredPosts != 0 {
		t.Fatalf("reprocess result = %+v, want no newly stored posts", second)
	}
	var count int
	if err := database.Conn().QueryRow("SELECT COUNT(*) FROM posts WHERE channel_id = ?", channelID).Scan(&count); err != nil {
		t.Fatalf("count posts: %v", err)
	}
	if count != 1 {
		t.Fatalf("post count = %d, want 1 after duplicate fetch", count)
	}
}

func TestChannelProcessorContinuesBatchAfterChannelFailure(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()
	first := &model.Channel{Username: "first", Enabled: true}
	firstID, err := database.Channels.Insert(first)
	if err != nil {
		t.Fatalf("insert first channel: %v", err)
	}
	first.ID = firstID
	second := &model.Channel{Username: "second", Enabled: true}
	secondID, err := database.Channels.Insert(second)
	if err != nil {
		t.Fatalf("insert second channel: %v", err)
	}
	second.ID = secondID

	fetcher := &fakeChannelFetcher{posts: map[string][]ParsedPost{
		"first": {{MessageID: 1, Text: "first", PostedAt: "2026-07-15T18:30:00Z"}},
	}, errors: map[string]error{"second": ErrChannelNotFound}}
	processor := NewChannelProcessor(fetcher, NewPostStorage(database.Channels, database.Posts))
	batch, err := processor.ProcessChannels([]model.Channel{*first, *second})
	if err != nil {
		t.Fatalf("process batch: %v", err)
	}
	if len(batch.Results) != 1 || batch.Results[0].StoredPosts != 1 {
		t.Fatalf("batch results = %+v, want first channel stored", batch.Results)
	}
	if len(batch.Failures) != 1 || batch.Failures[0].Channel.Username != "second" {
		t.Fatalf("batch failures = %+v, want second channel failure", batch.Failures)
	}
}

func TestChannelProcessorExposesHTTPAndPopulationMetadata(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()

	populated := &model.Channel{Username: "populated", Enabled: true, LastPostID: 41}
	populatedID, err := database.Channels.Insert(populated)
	if err != nil {
		t.Fatalf("insert populated channel: %v", err)
	}
	populated.ID = populatedID
	newChannel := &model.Channel{Username: "new", Enabled: true}
	newID, err := database.Channels.Insert(newChannel)
	if err != nil {
		t.Fatalf("insert new channel: %v", err)
	}
	newChannel.ID = newID

	fetcher := &fakeStatsChannelFetcher{
		posts: map[string][]ParsedPost{
			"populated": nil,
			"new":       {{MessageID: 1, Text: "new post", PostedAt: "2026-07-15T18:30:00Z"}},
		},
		stats: map[string]ParseStats{
			"populated": {HTTPStatus: http.StatusOK},
			"new":       {HTTPStatus: http.StatusOK},
		},
	}
	processor := NewChannelProcessor(fetcher, NewPostStorage(database.Channels, database.Posts))
	batch, err := processor.ProcessChannels([]model.Channel{*populated, *newChannel})
	if err != nil {
		t.Fatalf("process channels: %v", err)
	}
	if len(batch.Results) != 2 {
		t.Fatalf("results = %+v, want two successful results", batch.Results)
	}
	if got := batch.Results[0]; got.HTTPStatus != http.StatusOK || got.ParsedPosts != 0 || !got.PreviouslyPopulated {
		t.Fatalf("populated result = %+v, want HTTP 200, zero posts, previously populated", got)
	}
	if got := batch.Results[1]; got.HTTPStatus != http.StatusOK || got.ParsedPosts != 1 || got.PreviouslyPopulated {
		t.Fatalf("new result = %+v, want HTTP 200, one post, not previously populated", got)
	}
}

func TestChannelProcessorNotifiesOnceForStructuralChange(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()
	channels := make([]model.Channel, 0, 2)
	for _, username := range []string{"first", "second"} {
		channel := &model.Channel{Username: username, Enabled: true, LastPostID: 10}
		id, err := database.Channels.Insert(channel)
		if err != nil {
			t.Fatalf("insert channel %s: %v", username, err)
		}
		channel.ID = id
		channels = append(channels, *channel)
	}

	notifier := &recordingOwnerNotifier{}
	fetcher := &fakeStatsChannelFetcher{
		posts: map[string][]ParsedPost{"first": nil, "second": nil},
		stats: map[string]ParseStats{
			"first":  {HTTPStatus: http.StatusOK},
			"second": {HTTPStatus: http.StatusOK},
		},
	}
	processor := NewChannelProcessor(fetcher, NewPostStorage(database.Channels, database.Posts), notifier)
	batch, err := processor.ProcessChannels(channels)
	if err != nil {
		t.Fatalf("process channels: %v", err)
	}
	if len(batch.Failures) != 0 {
		t.Fatalf("failures = %+v, want none", batch.Failures)
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("notifications = %d, want one", len(notifier.messages))
	}
	if !strings.Contains(notifier.messages[0], "структуру t.me/s") ||
		!strings.Contains(notifier.messages[0], "Проверьте парсер") {
		t.Fatalf("notification = %q, want actionable Russian structure warning", notifier.messages[0])
	}
}

func TestChannelProcessorSkipsFalseStructuralChangeAlerts(t *testing.T) {
	tests := []struct {
		name     string
		channels []model.Channel
		posts    map[string][]ParsedPost
		stats    map[string]ParseStats
		errors   map[string]error
	}{
		{
			name:     "new empty channels",
			channels: []model.Channel{{Username: "new", Enabled: true}},
			posts:    map[string][]ParsedPost{"new": nil},
			stats:    map[string]ParseStats{"new": {HTTPStatus: http.StatusOK}},
		},
		{
			name: "mixed non-empty results",
			channels: []model.Channel{
				{Username: "populated-empty", Enabled: true, LastPostID: 10},
				{Username: "populated-nonempty", Enabled: true, LastPostID: 10},
			},
			posts: map[string][]ParsedPost{
				"populated-empty":    nil,
				"populated-nonempty": {{MessageID: 11, Text: "post", PostedAt: "2026-07-15T18:30:00Z"}},
			},
			stats: map[string]ParseStats{
				"populated-empty":    {HTTPStatus: http.StatusOK},
				"populated-nonempty": {HTTPStatus: http.StatusOK},
			},
		},
		{
			name: "failed HTTP request",
			channels: []model.Channel{
				{Username: "populated", Enabled: true, LastPostID: 10},
				{Username: "failed", Enabled: true, LastPostID: 10},
			},
			posts: map[string][]ParsedPost{"populated": nil, "failed": nil},
			stats: map[string]ParseStats{
				"populated": {HTTPStatus: http.StatusOK},
				"failed":    {HTTPStatus: http.StatusBadGateway},
			},
			errors: map[string]error{"failed": errors.New("bad gateway")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, cleanup := newStorageTestDB(t)
			defer cleanup()
			for i := range tt.channels {
				id, err := database.Channels.Insert(&tt.channels[i])
				if err != nil {
					t.Fatalf("insert channel: %v", err)
				}
				tt.channels[i].ID = id
			}
			notifier := &recordingOwnerNotifier{}
			processor := NewChannelProcessor(
				&fakeStatsChannelFetcher{posts: tt.posts, stats: tt.stats, errors: tt.errors},
				NewPostStorage(database.Channels, database.Posts),
				notifier,
			)
			if _, err := processor.ProcessChannels(tt.channels); err != nil {
				t.Fatalf("process channels: %v", err)
			}
			if len(notifier.messages) != 0 {
				t.Fatalf("notifications = %q, want none", notifier.messages)
			}
		})
	}
}

type fakeChannelFetcher struct {
	posts  map[string][]ParsedPost
	errors map[string]error
}

type fakeStatsChannelFetcher struct {
	posts  map[string][]ParsedPost
	stats  map[string]ParseStats
	errors map[string]error
}

func (f *fakeStatsChannelFetcher) ParseChannel(username string) ([]ParsedPost, error) {
	posts, _, err := f.ParseChannelWithStats(username)
	return posts, err
}

func (f *fakeStatsChannelFetcher) ParseChannelWithStats(username string) ([]ParsedPost, ParseStats, error) {
	if err := f.errors[username]; err != nil {
		return nil, f.stats[username], err
	}
	return f.posts[username], f.stats[username], nil
}

type recordingOwnerNotifier struct {
	messages []string
}

func (n *recordingOwnerNotifier) NotifyOwner(_ context.Context, text string) error {
	n.messages = append(n.messages, text)
	return nil
}

func (f *fakeChannelFetcher) ParseChannel(username string) ([]ParsedPost, error) {
	if err := f.errors[username]; err != nil {
		return nil, err
	}
	return f.posts[username], nil
}
