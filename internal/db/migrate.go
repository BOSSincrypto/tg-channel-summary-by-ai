package db

import (
	"database/sql"
	"fmt"
)

// runMigrations creates all tables and indexes if they don't exist.
// This is idempotent: each CREATE uses IF NOT EXISTS.
func runMigrations(conn *sql.DB) error {
	for i, migration := range migrations {
		if _, err := conn.Exec(migration); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	if err := ensureChannelFetchErrorColumns(conn); err != nil {
		return fmt.Errorf("migrate channel fetch error state: %w", err)
	}
	return nil
}

func ensureChannelFetchErrorColumns(conn *sql.DB) error {
	rows, err := conn.Query("PRAGMA table_info(channels)")
	if err != nil {
		return fmt.Errorf("inspect channels table: %w", err)
	}
	defer rows.Close()

	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("scan channels column: %w", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate channels columns: %w", err)
	}

	for _, column := range []struct {
		name string
		ddl  string
	}{
		{name: "fetch_error_kind", ddl: "ALTER TABLE channels ADD COLUMN fetch_error_kind TEXT DEFAULT ''"},
		{name: "fetch_error_message", ddl: "ALTER TABLE channels ADD COLUMN fetch_error_message TEXT DEFAULT ''"},
		{name: "fetch_error_at", ddl: "ALTER TABLE channels ADD COLUMN fetch_error_at TEXT"},
	} {
		if columns[column.name] {
			continue
		}
		if _, err := conn.Exec(column.ddl); err != nil {
			return fmt.Errorf("add %s: %w", column.name, err)
		}
	}
	return nil
}

// migrations is an ordered list of DDL statements that define the database schema.
// They must be applied in order.
var migrations = []string{
	// --------------------------------------------------
	// 1. channels
	// --------------------------------------------------
	`CREATE TABLE IF NOT EXISTS channels (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL UNIQUE,
		title TEXT DEFAULT '',
		enabled INTEGER DEFAULT 1,
		last_post_id INTEGER DEFAULT 0,
		fetch_error_kind TEXT DEFAULT '',
		fetch_error_message TEXT DEFAULT '',
		fetch_error_at TEXT,
		created_at TEXT DEFAULT (datetime('now'))
	)`,

	// --------------------------------------------------
	// 2. groups
	// --------------------------------------------------
	`CREATE TABLE IF NOT EXISTS groups (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		telegram_chat_id INTEGER NOT NULL UNIQUE,
		title TEXT DEFAULT '',
		created_at TEXT DEFAULT (datetime('now'))
	)`,

	// --------------------------------------------------
	// 3. group_channels (many-to-many with topic support)
	// --------------------------------------------------
	`CREATE TABLE IF NOT EXISTS group_channels (
		group_id INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
		channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
		topic_thread_id INTEGER,
		PRIMARY KEY (group_id, channel_id)
	)`,

	// --------------------------------------------------
	// 4. ai_providers
	// --------------------------------------------------
	`CREATE TABLE IF NOT EXISTS ai_providers (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		base_url TEXT NOT NULL,
		api_key TEXT NOT NULL,
		default_model TEXT DEFAULT '',
		is_default INTEGER DEFAULT 0,
		created_at TEXT DEFAULT (datetime('now'))
	)`,

	// --------------------------------------------------
	// 5. group_settings (per-group AI + scheduling config)
	// --------------------------------------------------
	`CREATE TABLE IF NOT EXISTS group_settings (
		group_id INTEGER PRIMARY KEY REFERENCES groups(id) ON DELETE CASCADE,
		provider_id INTEGER REFERENCES ai_providers(id) ON DELETE SET NULL,
		model TEXT,
		digest_time TEXT DEFAULT '21:00',
		timezone TEXT DEFAULT 'Europe/Moscow'
	)`,

	// --------------------------------------------------
	// 6. posts
	// --------------------------------------------------
	`CREATE TABLE IF NOT EXISTS posts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
		message_id INTEGER NOT NULL,
		text TEXT DEFAULT '',
		summary TEXT,
		posted_at TEXT NOT NULL,
		url TEXT NOT NULL DEFAULT '',
		content_hash TEXT NOT NULL,
		link_urls_hash TEXT,
		created_at TEXT DEFAULT (datetime('now')),
		UNIQUE(channel_id, message_id)
	)`,

	// --------------------------------------------------
	// 7. digests
	// --------------------------------------------------
	`CREATE TABLE IF NOT EXISTS digests (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		group_id INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
		sent_at TEXT DEFAULT (datetime('now')),
		message_id INTEGER,
		post_count INTEGER DEFAULT 0
	)`,

	// --------------------------------------------------
	// 8. digest_posts (many-to-many)
	// --------------------------------------------------
	`CREATE TABLE IF NOT EXISTS digest_posts (
		digest_id INTEGER NOT NULL REFERENCES digests(id) ON DELETE CASCADE,
		post_id INTEGER NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
		PRIMARY KEY (digest_id, post_id)
	)`,

	// --------------------------------------------------
	// 9. config (key-value store)
	// --------------------------------------------------
	`CREATE TABLE IF NOT EXISTS config (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL DEFAULT ''
	)`,

	// --------------------------------------------------
	// 10. Indexes for common queries
	// --------------------------------------------------
	`CREATE INDEX IF NOT EXISTS idx_channels_username ON channels(username)`,
	`CREATE INDEX IF NOT EXISTS idx_channels_enabled ON channels(enabled)`,
	`CREATE INDEX IF NOT EXISTS idx_posts_channel_id ON posts(channel_id)`,
	`CREATE INDEX IF NOT EXISTS idx_posts_posted_at ON posts(posted_at)`,
	`CREATE INDEX IF NOT EXISTS idx_posts_content_hash ON posts(content_hash)`,
	`CREATE INDEX IF NOT EXISTS idx_digests_group_id ON digests(group_id)`,
	`CREATE INDEX IF NOT EXISTS idx_group_channels_group ON group_channels(group_id)`,
	`CREATE INDEX IF NOT EXISTS idx_group_channels_channel ON group_channels(channel_id)`,
}
