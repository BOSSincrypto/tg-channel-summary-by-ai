package parser

import (
	"net/http"
	"net/http/httptest"
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

type fakeChannelFetcher struct {
	posts  map[string][]ParsedPost
	errors map[string]error
}

func (f *fakeChannelFetcher) ParseChannel(username string) ([]ParsedPost, error) {
	if err := f.errors[username]; err != nil {
		return nil, err
	}
	return f.posts[username], nil
}
