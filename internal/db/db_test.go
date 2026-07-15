package db

import (
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

func ptrString(s string) *string {
	return &s
}

func offsetRFC3339(t time.Time, offsetHours int) string {
	return t.In(time.FixedZone("offset", offsetHours*3600)).Format(time.RFC3339)
}

// TestCleanupPosts verifies post cleanup preserves rows referenced by recent digests.
func TestCleanupPosts(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	now := time.Now().UTC()

	ch := &model.Channel{Username: "cleanup-chan", Enabled: true}
	chID, err := db.Channels.Insert(ch)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}

	group := &model.Group{TelegramChatID: -1005555, Title: "Cleanup Group"}
	groupID, err := db.Groups.Insert(group)
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := db.Groups.AssignChannel(groupID, chID, nil); err != nil {
		t.Fatalf("assign channel: %v", err)
	}

	referencedPost := &model.Post{
		ChannelID:   chID,
		MessageID:   1,
		Text:        "Referenced old post",
		PostedAt:    offsetRFC3339(now.Add(-72*time.Hour), 14),
		URL:         "https://t.me/cleanup-chan/1",
		ContentHash: "referenced-old",
	}
	referencedID, err := db.Posts.Insert(referencedPost)
	if err != nil {
		t.Fatalf("insert referenced post: %v", err)
	}

	unreferencedPost := &model.Post{
		ChannelID:   chID,
		MessageID:   2,
		Text:        "Unreferenced old post",
		PostedAt:    offsetRFC3339(now.Add(-72*time.Hour), 14),
		URL:         "https://t.me/cleanup-chan/2",
		ContentHash: "unreferenced-old",
	}
	unreferencedID, err := db.Posts.Insert(unreferencedPost)
	if err != nil {
		t.Fatalf("insert unreferenced post: %v", err)
	}

	recentPost := &model.Post{
		ChannelID:   chID,
		MessageID:   3,
		Text:        "Recent post",
		PostedAt:    now.Add(-2 * time.Hour).Format(time.RFC3339),
		URL:         "https://t.me/cleanup-chan/3",
		ContentHash: "recenthash",
	}
	recentID, err := db.Posts.Insert(recentPost)
	if err != nil {
		t.Fatalf("insert recent post: %v", err)
	}

	digest := &model.Digest{GroupID: groupID, PostCount: 2}
	digestID, err := db.Digests.Insert(digest)
	if err != nil {
		t.Fatalf("insert digest: %v", err)
	}
	if err := db.Digests.AddPost(digestID, referencedID); err != nil {
		t.Fatalf("link referenced post to digest: %v", err)
	}
	if err := db.Digests.AddPost(digestID, recentID); err != nil {
		t.Fatalf("link recent post to digest: %v", err)
	}

	staleDigest := now.Add(-2 * time.Hour).Format(time.RFC3339)
	if _, err := db.Conn().Exec(`UPDATE digests SET sent_at = ? WHERE id = ?`, staleDigest, digestID); err != nil {
		t.Fatalf("age digest: %v", err)
	}

	deleted, err := db.CleanupPosts(1)
	if err != nil {
		t.Fatalf("cleanup posts: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected exactly 1 old post deleted, got %d", deleted)
	}
	if _, err := db.Posts.GetByID(referencedID); err != nil {
		t.Fatalf("expected referenced post to remain, got %v", err)
	}
	if _, err := db.Posts.GetByID(unreferencedID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected unreferenced post to be deleted, got %v", err)
	}
	if _, err := db.Posts.GetByID(recentID); err != nil {
		t.Fatalf("expected recent post to remain, got %v", err)
	}
}

// TestPostRepositoryDedupAndWindow verifies cross-channel dedup and normalized time windows.
func TestPostRepositoryDedupAndWindow(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	now := time.Now().UTC()

	ch1 := &model.Channel{Username: "chan1", Enabled: true}
	ch1ID, err := db.Channels.Insert(ch1)
	if err != nil {
		t.Fatalf("insert channel 1: %v", err)
	}
	ch2 := &model.Channel{Username: "chan2", Enabled: true}
	ch2ID, err := db.Channels.Insert(ch2)
	if err != nil {
		t.Fatalf("insert channel 2: %v", err)
	}
	group := &model.Group{TelegramChatID: -1006666, Title: "Dedup Group"}
	groupID, err := db.Groups.Insert(group)
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := db.Groups.AssignChannel(groupID, ch1ID, nil); err != nil {
		t.Fatalf("assign channel 1: %v", err)
	}
	if err := db.Groups.AssignChannel(groupID, ch2ID, nil); err != nil {
		t.Fatalf("assign channel 2: %v", err)
	}

	sharedContentHash := "shared-content"
	sharedLinkHash := ptrString("shared-links")
	postedAtSharedOld := offsetRFC3339(now.Add(-23*time.Hour), 14)
	postedAtRecent := now.Add(-2 * time.Hour).Format(time.RFC3339)
	postedAtTooOld := offsetRFC3339(now.Add(-25*time.Hour), 14)

	post1 := &model.Post{ChannelID: ch1ID, MessageID: 1, Text: "same text", PostedAt: postedAtSharedOld, URL: "https://t.me/chan1/1", ContentHash: sharedContentHash, LinkURLsHash: sharedLinkHash}
	post2 := &model.Post{ChannelID: ch2ID, MessageID: 1, Text: "same text", PostedAt: postedAtRecent, URL: "https://t.me/chan2/1", ContentHash: sharedContentHash, LinkURLsHash: sharedLinkHash}
	post3 := &model.Post{ChannelID: ch1ID, MessageID: 2, Text: "unique text", PostedAt: postedAtTooOld, URL: "https://t.me/chan1/2", ContentHash: "unique-old", LinkURLsHash: nil}
	post4 := &model.Post{ChannelID: ch2ID, MessageID: 2, Text: "unique text", PostedAt: postedAtRecent, URL: "https://t.me/chan2/2", ContentHash: "unique-recent", LinkURLsHash: nil}

	if _, err := db.Posts.Insert(post1); err != nil {
		t.Fatalf("insert post1: %v", err)
	}
	if _, err := db.Posts.Insert(post2); err != nil {
		t.Fatalf("insert post2: %v", err)
	}
	if _, err := db.Posts.Insert(post3); err != nil {
		t.Fatalf("insert post3: %v", err)
	}
	if _, err := db.Posts.Insert(post4); err != nil {
		t.Fatalf("insert post4: %v", err)
	}

	unsummarized, err := db.Posts.ListUnsummarized(groupID, 24)
	if err != nil {
		t.Fatalf("list unsummarized: %v", err)
	}
	if len(unsummarized) != 2 {
		t.Fatalf("expected 2 unsummarized posts in last 24h after dedup, got %d", len(unsummarized))
	}
	for _, p := range unsummarized {
		if p.ContentHash == "unique-old" {
			t.Fatal("expected old unique post to be excluded by normalized posted_at window")
		}
	}

	exists, err := db.Posts.ExistsByContentHash(groupID, sharedContentHash)
	if err != nil {
		t.Fatalf("exists by content hash: %v", err)
	}
	if !exists {
		t.Fatal("expected shared content hash to be found across channels")
	}

	exists, err = db.Posts.ExistsByContentHash(groupID, "missing")
	if err != nil {
		t.Fatalf("exists by missing hash: %v", err)
	}
	if exists {
		t.Fatal("expected missing content hash to not exist")
	}
}

// newTestDB opens an in-memory SQLite database for testing.
// The returned cleanup function must be called by the test to close the DB.
func newTestDB(t *testing.T) (*DB, func()) {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	return db, func() { db.Close() }
}

// TestOpenWALMode verifies that SQLite WAL mode is enabled on the database.
// Note: in-memory databases report "memory" for journal_mode even with WAL
// enabled in the DSN, which is expected behavior. The WAL flag in the DSN
// still takes effect for file-based databases, tested in TestFileDatabase.
func TestOpenWALMode(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	var journalMode string
	err := db.Conn().QueryRow("PRAGMA journal_mode").Scan(&journalMode)
	if err != nil {
		t.Fatalf("failed to check journal_mode: %v", err)
	}
	// In-memory DBs report "memory"; file DBs report "wal". Both are acceptable.
	if journalMode != "wal" && journalMode != "memory" {
		t.Errorf("expected journal_mode 'wal' or 'memory', got '%s'", journalMode)
	}
}

// TestOpenForeignKeys verifies that foreign keys are enabled.
func TestOpenForeignKeys(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	var fkEnabled int
	err := db.Conn().QueryRow("PRAGMA foreign_keys").Scan(&fkEnabled)
	if err != nil {
		t.Fatalf("failed to check foreign_keys: %v", err)
	}
	if fkEnabled != 1 {
		t.Errorf("expected foreign_keys=1, got %d", fkEnabled)
	}
}

// TestIntegrityCheck verifies that PRAGMA integrity_check passes on a fresh DB.
func TestIntegrityCheck(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	err := integrityCheck(db.Conn())
	if err != nil {
		t.Fatalf("integrity_check failed on fresh DB: %v", err)
	}
}

// TestSchemaExists verifies that all expected tables exist after migrations.
func TestSchemaExists(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	expectedTables := []string{
		"channels", "groups", "group_channels", "ai_providers",
		"group_settings", "posts", "digests", "digest_posts", "config",
	}

	rows, err := db.Conn().Query(
		"SELECT name FROM sqlite_master WHERE type='table' ORDER BY name",
	)
	if err != nil {
		t.Fatalf("failed to list tables: %v", err)
	}
	defer rows.Close()

	tables := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables[name] = true
	}

	for _, expected := range expectedTables {
		if !tables[expected] {
			t.Errorf("expected table '%s' not found", expected)
		}
	}
}

// TestSchemaColumns verifies each table has the expected columns.
func TestSchemaColumns(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	tests := []struct {
		table   string
		columns []string
	}{
		{"channels", []string{"id", "username", "title", "enabled", "last_post_id", "created_at"}},
		{"groups", []string{"id", "telegram_chat_id", "title", "created_at"}},
		{"group_channels", []string{"group_id", "channel_id", "topic_thread_id"}},
		{"ai_providers", []string{"id", "name", "base_url", "api_key", "default_model", "is_default", "created_at"}},
		{"group_settings", []string{"group_id", "provider_id", "model", "digest_time", "timezone"}},
		{"posts", []string{"id", "channel_id", "message_id", "text", "summary", "posted_at", "url", "content_hash", "link_urls_hash", "created_at"}},
		{"digests", []string{"id", "group_id", "sent_at", "message_id", "post_count"}},
		{"digest_posts", []string{"digest_id", "post_id"}},
		{"config", []string{"key", "value"}},
	}

	for _, tc := range tests {
		t.Run(tc.table, func(t *testing.T) {
			rows, err := db.Conn().Query("PRAGMA table_info(" + tc.table + ")")
			if err != nil {
				t.Fatalf("pragma table_info(%s): %v", tc.table, err)
			}
			defer rows.Close()

			columns := make(map[string]bool)
			for rows.Next() {
				var cid int
				var name, colType string
				var notNull, pk int
				var dflt sql.NullString
				if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
					t.Fatalf("scan column: %v", err)
				}
				columns[name] = true
			}

			for _, expected := range tc.columns {
				if !columns[expected] {
					t.Errorf("table %s: expected column '%s' not found", tc.table, expected)
				}
			}
		})
	}
}

// TestChannelRepository tests full CRUD for channels.
func TestChannelRepository(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	// Insert
	ch := &model.Channel{Username: "testchannel", Title: "Test Channel", Enabled: true}
	id, err := db.Channels.Insert(ch)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero id after insert")
	}
	ch.ID = id

	// GetByID
	got, err := db.Channels.GetByID(id)
	if err != nil {
		t.Fatalf("get channel by id: %v", err)
	}
	if got.Username != "testchannel" {
		t.Errorf("expected username 'testchannel', got '%s'", got.Username)
	}
	if !got.Enabled {
		t.Error("expected channel to be enabled")
	}

	// GetByUsername (case-insensitive)
	got2, err := db.Channels.GetByUsername("TESTCHANNEL")
	if err != nil {
		t.Fatalf("get channel by username (case-insensitive): %v", err)
	}
	if got2.Username != "testchannel" {
		t.Errorf("expected 'testchannel', got '%s'", got2.Username)
	}

	// ExistsByUsername
	exists, err := db.Channels.ExistsByUsername("TestChannel")
	if err != nil {
		t.Fatalf("check exists: %v", err)
	}
	if !exists {
		t.Error("expected channel to exist")
	}

	exists, err = db.Channels.ExistsByUsername("nonexistent")
	if err != nil {
		t.Fatalf("check exists (nonexistent): %v", err)
	}
	if exists {
		t.Error("expected nonexistent channel to not exist")
	}

	// List
	ch2 := &model.Channel{Username: "secondchannel", Title: "Second", Enabled: false}
	_, err = db.Channels.Insert(ch2)
	if err != nil {
		t.Fatalf("insert second channel: %v", err)
	}

	all, err := db.Channels.List()
	if err != nil {
		t.Fatalf("list channels: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 channels, got %d", len(all))
	}

	// ListEnabled
	enabled, err := db.Channels.ListEnabled()
	if err != nil {
		t.Fatalf("list enabled: %v", err)
	}
	if len(enabled) != 1 {
		t.Errorf("expected 1 enabled channel, got %d", len(enabled))
	}
	if enabled[0].Username != "testchannel" {
		t.Errorf("expected 'testchannel', got '%s'", enabled[0].Username)
	}

	// Update
	ch.Enabled = false
	ch.Title = "Updated Title"
	if err := db.Channels.Update(ch); err != nil {
		t.Fatalf("update channel: %v", err)
	}
	updated, _ := db.Channels.GetByID(id)
	if updated.Enabled {
		t.Error("expected channel to be disabled after update")
	}
	if updated.Title != "Updated Title" {
		t.Errorf("expected 'Updated Title', got '%s'", updated.Title)
	}

	// UpdateLastPostID
	if err := db.Channels.UpdateLastPostID(id, 42); err != nil {
		t.Fatalf("update last post id: %v", err)
	}
	updated2, _ := db.Channels.GetByID(id)
	if updated2.LastPostID != 42 {
		t.Errorf("expected last_post_id=42, got %d", updated2.LastPostID)
	}

	// Delete
	if err := db.Channels.Delete(id); err != nil {
		t.Fatalf("delete channel: %v", err)
	}
	_, err = db.Channels.GetByID(id)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}

// TestGroupRepository tests full CRUD for groups including channel assignments.
func TestGroupRepository(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	// Insert
	g := &model.Group{TelegramChatID: -1001234567890, Title: "Test Group"}
	id, err := db.Groups.Insert(g)
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero id")
	}

	// GetByID
	got, err := db.Groups.GetByID(id)
	if err != nil {
		t.Fatalf("get group by id: %v", err)
	}
	if got.TelegramChatID != -1001234567890 {
		t.Errorf("expected chat_id -1001234567890, got %d", got.TelegramChatID)
	}

	// GetByChatID
	got2, err := db.Groups.GetByChatID(-1001234567890)
	if err != nil {
		t.Fatalf("get group by chat id: %v", err)
	}
	if got2.Title != "Test Group" {
		t.Errorf("expected 'Test Group', got '%s'", got2.Title)
	}

	// Check default settings created
	gs, err := db.Groups.GetGroupSettings(id)
	if err != nil {
		t.Fatalf("get group settings: %v", err)
	}
	if gs.DigestTime != "21:00" {
		t.Errorf("expected default digest_time '21:00', got '%s'", gs.DigestTime)
	}
	if gs.Timezone != "Europe/Moscow" {
		t.Errorf("expected default timezone 'Europe/Moscow', got '%s'", gs.Timezone)
	}

	// List
	g2 := &model.Group{TelegramChatID: -1009876543210, Title: "Second Group"}
	_, err = db.Groups.Insert(g2)
	if err != nil {
		t.Fatalf("insert second group: %v", err)
	}
	all, err := db.Groups.List()
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 groups, got %d", len(all))
	}

	// Update
	g.Title = "Updated Group"
	if err := db.Groups.Update(g); err != nil {
		t.Fatalf("update group: %v", err)
	}

	// Assign channels
	ch := &model.Channel{Username: "ch1", Enabled: true}
	chID, _ := db.Channels.Insert(ch)
	ch2 := &model.Channel{Username: "ch2", Enabled: true}
	ch2ID, _ := db.Channels.Insert(ch2)

	topicID := int64(555)
	if err := db.Groups.AssignChannel(id, chID, &topicID); err != nil {
		t.Fatalf("assign channel 1: %v", err)
	}
	if err := db.Groups.AssignChannel(id, ch2ID, nil); err != nil {
		t.Fatalf("assign channel 2: %v", err)
	}

	assignments, err := db.Groups.GetChannelAssignments(id)
	if err != nil {
		t.Fatalf("get assignments: %v", err)
	}
	if len(assignments) != 2 {
		t.Errorf("expected 2 assignments, got %d", len(assignments))
	}

	// GetChannelsForGroup
	channels, err := db.Groups.GetChannelsForGroup(id)
	if err != nil {
		t.Fatalf("get channels for group: %v", err)
	}
	if len(channels) != 2 {
		t.Errorf("expected 2 channels for group, got %d", len(channels))
	}

	// CountChannelsForGroup
	count, err := db.Groups.CountChannelsForGroup(id)
	if err != nil {
		t.Fatalf("count channels: %v", err)
	}
	if count != 2 {
		t.Errorf("expected count 2, got %d", count)
	}

	// UpdateGroupSettings
	settings := &model.GroupSettings{
		GroupID:    id,
		DigestTime: "09:00",
		Timezone:   "Europe/Berlin",
	}
	if err := db.Groups.UpdateGroupSettings(settings); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	gs2, _ := db.Groups.GetGroupSettings(id)
	if gs2.DigestTime != "09:00" {
		t.Errorf("expected '09:00', got '%s'", gs2.DigestTime)
	}

	// Unassign channel
	if err := db.Groups.UnassignChannel(id, chID); err != nil {
		t.Fatalf("unassign channel: %v", err)
	}
	assignments2, _ := db.Groups.GetChannelAssignments(id)
	if len(assignments2) != 1 {
		t.Errorf("expected 1 assignment after unassign, got %d", len(assignments2))
	}

	// Delete
	if err := db.Groups.Delete(id); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	_, err = db.Groups.GetByID(id)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}

// TestPostRepository tests full CRUD for posts including dedup.
func TestPostRepository(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	now := time.Now().UTC()

	// Create a channel first
	ch := &model.Channel{Username: "source", Enabled: true}
	chID, err := db.Channels.Insert(ch)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}

	// Insert post
	summary := "test summary"
	p := &model.Post{
		ChannelID:   chID,
		MessageID:   100,
		Text:        "Hello world post",
		Summary:     &summary,
		PostedAt:    now.Add(-3 * time.Hour).Format(time.RFC3339),
		URL:         "https://t.me/source/100",
		ContentHash: "abc123hash",
	}
	id, err := db.Posts.Insert(p)
	if err != nil {
		t.Fatalf("insert post: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero id")
	}

	// GetByID
	got, err := db.Posts.GetByID(id)
	if err != nil {
		t.Fatalf("get post by id: %v", err)
	}
	if got.ChannelID != chID || got.MessageID != 100 {
		t.Errorf("unexpected post data: ch=%d msg=%d", got.ChannelID, got.MessageID)
	}

	// GetByChannelAndMessageID
	got2, err := db.Posts.GetByChannelAndMessageID(chID, 100)
	if err != nil {
		t.Fatalf("get by channel+message: %v", err)
	}
	if got2.Text != "Hello world post" {
		t.Errorf("expected 'Hello world post', got '%s'", got2.Text)
	}

	// Insert duplicate (same channel, same message_id)
	p2 := &model.Post{
		ChannelID:   chID,
		MessageID:   100,
		Text:        "Duplicate",
		PostedAt:    now.Add(-3 * time.Hour).Format(time.RFC3339),
		URL:         "https://t.me/source/100",
		ContentHash: "different",
	}
	id2, err := db.Posts.Insert(p2)
	if err == nil {
		t.Error("expected error for duplicate insert")
	}
	if !errors.Is(err, ErrDuplicate) {
		t.Errorf("expected ErrDuplicate, got %v", err)
	}
	if id2 != id {
		t.Errorf("duplicate insert should return existing id %d, got %d", id, id2)
	}

	// UpdateSummary
	if err := db.Posts.UpdateSummary(id, "new summary"); err != nil {
		t.Fatalf("update summary: %v", err)
	}
	updated, _ := db.Posts.GetByID(id)
	if updated.Summary == nil || *updated.Summary != "new summary" {
		t.Errorf("expected summary 'new summary', got %v", updated.Summary)
	}

	// Insert unsummarized posts
	p3 := &model.Post{
		ChannelID:   chID,
		MessageID:   200,
		Text:        "Unsummarized post",
		PostedAt:    now.Add(-2 * time.Hour).Format(time.RFC3339),
		URL:         "https://t.me/source/200",
		ContentHash: "hash200",
	}
	db.Posts.Insert(p3)

	// Create group and assign channel
	g := &model.Group{TelegramChatID: -1001111, Title: "Digest Group"}
	gID, _ := db.Groups.Insert(g)
	db.Groups.AssignChannel(gID, chID, nil)

	// ListUnsummarized should return p3
	unsummarized, err := db.Posts.ListUnsummarized(gID, 24)
	if err != nil {
		t.Fatalf("list unsummarized: %v", err)
	}
	if len(unsummarized) != 1 {
		t.Errorf("expected 1 unsummarized post, got %d", len(unsummarized))
	}

	// ExistsByContentHash
	exists, err := db.Posts.ExistsByContentHash(gID, "hash200")
	if err != nil {
		t.Fatalf("check content hash: %v", err)
	}
	if !exists {
		t.Error("expected content hash to exist")
	}

	exists, err = db.Posts.ExistsByContentHash(gID, "nonexistent")
	if err != nil {
		t.Fatalf("check content hash (nonexistent): %v", err)
	}
	if exists {
		t.Error("expected content hash to not exist")
	}

	// DeleteOlderThan
	deleted, err := db.Posts.DeleteOlderThan(365)
	if err != nil {
		t.Fatalf("delete older than: %v", err)
	}
	// Should delete nothing for recent posts
	if deleted != 0 {
		t.Errorf("expected 0 deleted, got %d", deleted)
	}

	// Non-existent post
	_, err = db.Posts.GetByID(99999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestDigestRepository tests full CRUD for digests.
func TestDigestRepository(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	// Create group first
	g := &model.Group{TelegramChatID: -1002222, Title: "Digest Target"}
	gID, err := db.Groups.Insert(g)
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}

	// Insert digest
	d := &model.Digest{GroupID: gID, PostCount: 5}
	id, err := db.Digests.Insert(d)
	if err != nil {
		t.Fatalf("insert digest: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero id")
	}

	// GetByID
	got, err := db.Digests.GetByID(id)
	if err != nil {
		t.Fatalf("get digest: %v", err)
	}
	if got.PostCount != 5 {
		t.Errorf("expected post_count 5, got %d", got.PostCount)
	}

	// UpdateMessageID
	teleMsgID := int64(7777)
	if err := db.Digests.UpdateMessageID(id, teleMsgID); err != nil {
		t.Fatalf("update message id: %v", err)
	}
	got2, _ := db.Digests.GetByID(id)
	if got2.MessageID == nil || *got2.MessageID != 7777 {
		t.Errorf("expected message_id 7777, got %v", got2.MessageID)
	}

	// Create channel + post for linking
	ch := &model.Channel{Username: "digestchan", Enabled: true}
	chID, _ := db.Channels.Insert(ch)
	p := &model.Post{
		ChannelID:   chID,
		MessageID:   1,
		Text:        "Post for digest",
		PostedAt:    time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339),
		URL:         "https://t.me/digestchan/1",
		ContentHash: "digesthash1",
	}
	postID, _ := db.Posts.Insert(p)

	// AddPost
	if err := db.Digests.AddPost(id, postID); err != nil {
		t.Fatalf("add post to digest: %v", err)
	}

	// GetPostsForDigest
	posts, err := db.Digests.GetPostsForDigest(id)
	if err != nil {
		t.Fatalf("get posts for digest: %v", err)
	}
	if len(posts) != 1 {
		t.Errorf("expected 1 post in digest, got %d", len(posts))
	}

	// ListByGroup
	digests, err := db.Digests.ListByGroup(gID, 10)
	if err != nil {
		t.Fatalf("list digests: %v", err)
	}
	if len(digests) != 1 {
		t.Errorf("expected 1 digest, got %d", len(digests))
	}

	// DeleteOlderThan
	deleted, err := db.Digests.DeleteOlderThan(365)
	if err != nil {
		t.Fatalf("delete older than: %v", err)
	}
	if deleted != 0 {
		t.Errorf("expected 0 deleted, got %d", deleted)
	}
}

// TestProviderRepository tests full CRUD for AI providers.
func TestProviderRepository(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	// Insert default provider
	ap := &model.AIProvider{
		Name:         "OpenRouter",
		BaseURL:      "https://openrouter.ai/api/v1",
		APIKey:       "sk-test-key",
		DefaultModel: "openai/gpt-oss-120b",
		IsDefault:    true,
	}
	id, err := db.Providers.Insert(ap)
	if err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero id")
	}

	// GetByID
	got, err := db.Providers.GetByID(id)
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if got.Name != "OpenRouter" {
		t.Errorf("expected 'OpenRouter', got '%s'", got.Name)
	}
	if !got.IsDefault {
		t.Error("expected provider to be default")
	}

	// GetDefault
	def, err := db.Providers.GetDefault()
	if err != nil {
		t.Fatalf("get default: %v", err)
	}
	if def.Name != "OpenRouter" {
		t.Errorf("expected default 'OpenRouter', got '%s'", def.Name)
	}

	// Insert another default (should clear previous)
	ap2 := &model.AIProvider{
		Name:         "Custom",
		BaseURL:      "https://custom.ai/v1",
		APIKey:       "sk-custom",
		DefaultModel: "gpt-4",
		IsDefault:    true,
	}
	id2, _ := db.Providers.Insert(ap2)

	// Old default should be cleared
	old, _ := db.Providers.GetByID(id)
	if old.IsDefault {
		t.Error("old provider should no longer be default")
	}

	newDef, _ := db.Providers.GetDefault()
	if newDef.ID != id2 {
		t.Errorf("expected new default id %d, got %d", id2, newDef.ID)
	}

	// GetByName
	byName, err := db.Providers.GetByName("Custom")
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}
	if byName.BaseURL != "https://custom.ai/v1" {
		t.Errorf("unexpected base_url: %s", byName.BaseURL)
	}

	// List
	all, err := db.Providers.List()
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 providers, got %d", len(all))
	}

	// Update
	ap.Name = "Updated OpenRouter"
	ap.IsDefault = true
	ap.ID = id
	if err := db.Providers.Update(ap); err != nil {
		t.Fatalf("update provider: %v", err)
	}
	// Should have made it default and cleared newDef
	updated, _ := db.Providers.GetByID(id)
	if !updated.IsDefault {
		t.Error("updated provider should be default")
	}

	newDef2, _ := db.Providers.GetByID(id2)
	if newDef2.IsDefault {
		t.Error("other provider should no longer be default")
	}

	// Delete
	if err := db.Providers.Delete(id); err != nil {
		t.Fatalf("delete provider: %v", err)
	}
	_, err = db.Providers.GetByID(id)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestConfigRepository tests key-value config operations.
func TestConfigRepository(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	// Set
	if err := db.Config.Set("last_migration", "v1"); err != nil {
		t.Fatalf("set config: %v", err)
	}
	if err := db.Config.Set("schema_version", "1"); err != nil {
		t.Fatalf("set config: %v", err)
	}

	// Get
	v, err := db.Config.Get("last_migration")
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if v != "v1" {
		t.Errorf("expected 'v1', got '%s'", v)
	}

	// Get missing
	_, err = db.Config.Get("nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}

	// Set overwrite
	if err := db.Config.Set("last_migration", "v2"); err != nil {
		t.Fatalf("set config overwrite: %v", err)
	}
	v2, _ := db.Config.Get("last_migration")
	if v2 != "v2" {
		t.Errorf("expected 'v2', got '%s'", v2)
	}

	// GetAll
	all, err := db.Config.GetAll()
	if err != nil {
		t.Fatalf("get all config: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 entries, got %d", len(all))
	}

	// Delete
	if err := db.Config.Delete("schema_version"); err != nil {
		t.Fatalf("delete config: %v", err)
	}
	all2, _ := db.Config.GetAll()
	if len(all2) != 1 {
		t.Errorf("expected 1 entry after delete, got %d", len(all2))
	}
}

// TestCascadeDelete tests ON DELETE CASCADE behavior.
func TestCascadeDelete(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	ch := &model.Channel{Username: "cascadechan", Enabled: true}
	chID, _ := db.Channels.Insert(ch)

	g := &model.Group{TelegramChatID: -1003333, Title: "Cascade Group"}
	gID, _ := db.Groups.Insert(g)

	db.Groups.AssignChannel(gID, chID, nil)

	// Verify assignment exists
	assignments, _ := db.Groups.GetChannelAssignments(gID)
	if len(assignments) != 1 {
		t.Fatal("assignment should exist")
	}

	// Delete channel — cascade should remove group_channels entry
	if err := db.Channels.Delete(chID); err != nil {
		t.Fatalf("delete channel: %v", err)
	}

	assignments2, _ := db.Groups.GetChannelAssignments(gID)
	if len(assignments2) != 0 {
		t.Errorf("expected 0 assignments after cascade, got %d", len(assignments2))
	}

	// Insert post with this channel (should fail because channel was deleted)
	// Actually we need a new channel for this test
	ch2 := &model.Channel{Username: "cascadechan2", Enabled: true}
	ch2ID, _ := db.Channels.Insert(ch2)
	p := &model.Post{
		ChannelID:   ch2ID,
		MessageID:   1,
		Text:        "Test",
		PostedAt:    time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339),
		URL:         "https://t.me/c/1",
		ContentHash: "cascadehash",
	}
	postID, _ := db.Posts.Insert(p)

	// Create digest with that post
	d := &model.Digest{GroupID: gID, PostCount: 1}
	digID, _ := db.Digests.Insert(d)
	db.Digests.AddPost(digID, postID)

	// Delete channel — cascade should remove the post
	db.Channels.Delete(ch2ID)
	_, err := db.Posts.GetByID(postID)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("post should be cascade-deleted, got %v", err)
	}
}

func TestOpenMalformedDatabaseFailsClosedWithRepairGuidance(t *testing.T) {
	path := t.TempDir() + "\\malformed.db"
	if err := os.WriteFile(path, []byte("this is not a SQLite database"), 0o600); err != nil {
		t.Fatalf("write malformed database: %v", err)
	}

	_, err := Open(path)
	if err == nil {
		t.Fatal("expected malformed database to fail startup")
	}

	message := err.Error()
	for _, want := range []string{
		path,
		"driver",
		"Database corruption detected at " + path + ". Restore from backup or manually repair.",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("startup error %q does not contain %q", message, want)
		}
	}
}

func TestOpenUnreadableDatabaseFailsClosedWithPathAndRepairGuidance(t *testing.T) {
	path := t.TempDir()

	_, err := Open(path)
	if err == nil {
		t.Fatal("expected database directory to fail startup")
	}

	message := err.Error()
	for _, want := range []string{
		path,
		"driver",
		"Database corruption detected at " + path + ". Restore from backup or manually repair.",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("startup error %q does not contain %q", message, want)
		}
	}
}

func TestDatabaseFullStartupErrorIsExplicitAndFailsClosed(t *testing.T) {
	path := "/data/bot.db"
	err := startupError(path, "integrity check", errors.New("database or disk is full (13)"))
	message := err.Error()

	for _, want := range []string{path, "database full", "database or disk is full (13)"} {
		if !strings.Contains(message, want) {
			t.Errorf("startup error %q does not contain %q", message, want)
		}
	}
	if strings.Contains(message, "Database corruption detected") {
		t.Errorf("database-full error was mislabeled as corruption: %q", message)
	}
}

// TestFileDatabase tests that the DB can be opened with a file path (not just :memory:).
func TestFileDatabase(t *testing.T) {
	path := "test_integrity_check.db"
	defer os.Remove(path)
	defer os.Remove(path + "-wal")
	defer os.Remove(path + "-shm")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("open file database: %v", err)
	}
	defer db.Close()

	// Verify WAL mode
	var mode string
	db.Conn().QueryRow("PRAGMA journal_mode").Scan(&mode)
	if mode != "wal" {
		t.Errorf("expected 'wal', got '%s'", mode)
	}

	// Insert and read back
	ch := &model.Channel{Username: "filetest", Enabled: true}
	id, _ := db.Channels.Insert(ch)
	got, _ := db.Channels.GetByID(id)
	if got.Username != "filetest" {
		t.Errorf("expected 'filetest', got '%s'", got.Username)
	}

	db.Close()

	// Reopen and verify data persisted
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen file database: %v", err)
	}
	defer db2.Close()

	got2, err := db2.Channels.GetByID(id)
	if err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	if got2.Username != "filetest" {
		t.Errorf("expected 'filetest' after reopen, got '%s'", got2.Username)
	}
}

// TestGroupProviderIntegration tests updating group settings with a provider reference.
func TestGroupProviderIntegration(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	// Create a provider
	ap := &model.AIProvider{
		Name:         "TestProvider",
		BaseURL:      "https://test.ai/v1",
		APIKey:       "sk-test",
		DefaultModel: "test-model",
	}
	provID, err := db.Providers.Insert(ap)
	if err != nil {
		t.Fatalf("insert provider: %v", err)
	}

	// Create a group
	g := &model.Group{TelegramChatID: -1004444, Title: "Provider Group"}
	gID, err := db.Groups.Insert(g)
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}

	// Set group settings with provider
	modelRef := "custom-model-v2"
	settings := &model.GroupSettings{
		GroupID:    gID,
		ProviderID: &provID,
		Model:      &modelRef,
		DigestTime: "18:00",
		Timezone:   "Asia/Tokyo",
	}
	if err := db.Groups.UpdateGroupSettings(settings); err != nil {
		t.Fatalf("update settings with provider: %v", err)
	}

	// Verify
	gs, err := db.Groups.GetGroupSettings(gID)
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	if gs.ProviderID == nil || *gs.ProviderID != provID {
		t.Errorf("expected provider_id %d, got %v", provID, gs.ProviderID)
	}
	if gs.Model == nil || *gs.Model != "custom-model-v2" {
		t.Errorf("expected model 'custom-model-v2', got %v", gs.Model)
	}
	if gs.DigestTime != "18:00" {
		t.Errorf("expected '18:00', got '%s'", gs.DigestTime)
	}

	// Delete provider - group settings provider_id should be nullified
	db.Providers.Delete(provID)
	gs2, _ := db.Groups.GetGroupSettings(gID)
	if gs2.ProviderID != nil {
		t.Error("expected provider_id to be NULL after provider delete")
	}
}
