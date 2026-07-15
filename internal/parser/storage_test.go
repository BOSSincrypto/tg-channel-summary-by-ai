package parser

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

func TestHashContentUsesFirst500Runes(t *testing.T) {
	text := "начало" + string(make([]rune, 0))
	for len([]rune(text)) < 501 {
		text += "я"
	}

	got := HashContent(text)
	want := HashContent(string([]rune(text)[:500]))
	if got != want {
		t.Fatalf("HashContent() = %q, want first 500 runes hash %q", got, want)
	}
	if got != "" && got == HashContent(text[:500]) {
		t.Fatal("HashContent should count Unicode runes, not byte offsets")
	}
}

func TestHashLinkURLsIsOrderIndependentAndSkipsDuplicates(t *testing.T) {
	first := HashLinkURLs([]string{"https://example.com/a", "https://example.com/b", "https://example.com/a"})
	second := HashLinkURLs([]string{"https://example.com/b", "https://example.com/a"})
	if first == nil || second == nil || *first != *second {
		t.Fatalf("HashLinkURLs should be stable for URL order and duplicates: %v, %v", first, second)
	}
	if HashLinkURLs(nil) != nil {
		t.Fatal("HashLinkURLs(nil) should return nil")
	}
}

func TestPostStorageStoresHashesSkipsOldAndTracksLastPostID(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()

	channel := &model.Channel{Username: "example", Enabled: true, LastPostID: 10}
	channelID, err := database.Channels.Insert(channel)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	channel.ID = channelID

	storage := NewPostStorage(database.Channels, database.Posts)
	stored, err := storage.Store(channel, []ParsedPost{
		{MessageID: 10, Text: "old post", PostedAt: time.Now().UTC().Format(time.RFC3339)},
		{MessageID: 11, Text: "new post", PostedAt: time.Now().UTC().Format(time.RFC3339), LinkURLs: []string{"https://example.com/a", "https://example.com/b"}},
		{MessageID: 13, Text: "newer post", PostedAt: time.Now().UTC().Format(time.RFC3339)},
	})
	if err != nil {
		t.Fatalf("store posts: %v", err)
	}
	if len(stored) != 2 || stored[0].MessageID != 11 || stored[1].MessageID != 13 {
		t.Fatalf("stored posts = %+v, want message IDs [11 13]", stored)
	}

	post, err := database.Posts.GetByChannelAndMessageID(channelID, 11)
	if err != nil {
		t.Fatalf("get stored post: %v", err)
	}
	if post.ContentHash != HashContent("new post") {
		t.Fatalf("content_hash = %q, want %q", post.ContentHash, HashContent("new post"))
	}
	wantLinks := HashLinkURLs([]string{"https://example.com/a", "https://example.com/b"})
	if !reflect.DeepEqual(post.LinkURLsHash, wantLinks) {
		t.Fatalf("link_urls_hash = %v, want %v", post.LinkURLsHash, wantLinks)
	}

	updated, err := database.Channels.GetByID(channelID)
	if err != nil {
		t.Fatalf("get updated channel: %v", err)
	}
	if updated.LastPostID != 13 {
		t.Fatalf("last_post_id = %d, want 13", updated.LastPostID)
	}
}

func TestPostStorageSkipsExistingDuplicateAndAdvancesCursor(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()

	channel := &model.Channel{Username: "example", Enabled: true}
	channelID, err := database.Channels.Insert(channel)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	channel.ID = channelID
	original := &model.Post{
		ChannelID:   channelID,
		MessageID:   20,
		Text:        "already stored",
		PostedAt:    time.Now().UTC().Format(time.RFC3339),
		URL:         "https://t.me/example/20",
		ContentHash: HashContent("already stored"),
	}
	if _, err := database.Posts.Insert(original); err != nil {
		t.Fatalf("insert existing post: %v", err)
	}

	stored, err := NewPostStorage(database.Channels, database.Posts).Store(channel, []ParsedPost{
		{MessageID: 20, Text: "already stored", PostedAt: original.PostedAt},
		{MessageID: 21, Text: "fresh", PostedAt: original.PostedAt},
	})
	if err != nil {
		t.Fatalf("store duplicate batch: %v", err)
	}
	if len(stored) != 1 || stored[0].MessageID != 21 {
		t.Fatalf("stored posts = %+v, want only message ID 21", stored)
	}

	var count int
	if err := database.Conn().QueryRow("SELECT COUNT(*) FROM posts WHERE channel_id = ?", channelID).Scan(&count); err != nil {
		t.Fatalf("count posts: %v", err)
	}
	if count != 2 {
		t.Fatalf("post count = %d, want 2", count)
	}
	updated, err := database.Channels.GetByID(channelID)
	if err != nil {
		t.Fatalf("get updated channel: %v", err)
	}
	if updated.LastPostID != 21 {
		t.Fatalf("last_post_id = %d, want 21", updated.LastPostID)
	}
}

func TestPostStorageReturnsCursorUpdateError(t *testing.T) {
	wantErr := errors.New("cursor unavailable")
	channels := &fakeStorageChannels{channel: &model.Channel{ID: 1, Username: "example"}, updateErr: wantErr}
	posts := &fakeStoragePosts{}

	_, err := NewPostStorage(channels, posts).Store(channels.channel, []ParsedPost{{
		MessageID: 1,
		Text:      "post",
		PostedAt:  time.Now().UTC().Format(time.RFC3339),
	}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("store error = %v, want cursor error %v", err, wantErr)
	}
	if len(posts.posts) != 1 {
		t.Fatalf("inserted posts = %d, want 1", len(posts.posts))
	}
}

type fakeStorageChannels struct {
	channel   *model.Channel
	updateErr error
}

func (f *fakeStorageChannels) UpdateLastPostID(int64, int64) error { return f.updateErr }

type fakeStoragePosts struct {
	posts []model.Post
}

func (f *fakeStoragePosts) Insert(post *model.Post) (int64, error) {
	f.posts = append(f.posts, *post)
	return int64(len(f.posts)), nil
}

func newStorageTestDB(t *testing.T) (*db.DB, func()) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	return database, func() { _ = database.Close() }
}
