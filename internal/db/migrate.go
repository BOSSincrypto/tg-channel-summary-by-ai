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
	if err := ensureGroupStatusColumn(conn); err != nil {
		return fmt.Errorf("migrate group status: %w", err)
	}
	if err := ensureGroupSettingsEmptyDigestColumn(conn); err != nil {
		return fmt.Errorf("migrate empty digest behavior: %w", err)
	}
	if err := ensureDigestStatusColumn(conn); err != nil {
		return fmt.Errorf("migrate digest status: %w", err)
	}
	if err := ensureDigestMessageTextColumn(conn); err != nil {
		return fmt.Errorf("migrate digest message text: %w", err)
	}
	if err := ensureVersionColumns(conn); err != nil {
		return fmt.Errorf("migrate optimistic locking versions: %w", err)
	}
	if err := ensureProviderNameUniqueness(conn); err != nil {
		return fmt.Errorf("migrate provider name uniqueness: %w", err)
	}
	if err := ensureForumTopicClosePendingColumn(conn); err != nil {
		return fmt.Errorf("migrate forum topic recovery state: %w", err)
	}
	if err := ensureForumTopicCreationIntentTable(conn); err != nil {
		return fmt.Errorf("migrate forum topic creation intents: %w", err)
	}
	if err := ensureForumTopicRecoveryIdentity(conn); err != nil {
		return fmt.Errorf("migrate forum topic recovery identity: %w", err)
	}
	return nil
}

func ensureGroupSettingsEmptyDigestColumn(conn *sql.DB) error {
	rows, err := conn.Query("PRAGMA table_info(group_settings)")
	if err != nil {
		return fmt.Errorf("inspect group_settings table: %w", err)
	}
	defer rows.Close()

	hasColumn := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("scan group_settings column: %w", err)
		}
		if name == "empty_digest_behavior" {
			hasColumn = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate group_settings columns: %w", err)
	}
	if !hasColumn {
		if _, err := conn.Exec(`ALTER TABLE group_settings ADD COLUMN empty_digest_behavior TEXT NOT NULL DEFAULT 'send_message'`); err != nil {
			return fmt.Errorf("add empty_digest_behavior: %w", err)
		}
	}
	return nil
}

func ensureDigestStatusColumn(conn *sql.DB) error {
	rows, err := conn.Query("PRAGMA table_info(digests)")
	if err != nil {
		return fmt.Errorf("inspect digests table: %w", err)
	}
	defer rows.Close()

	hasColumn := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("scan digests column: %w", err)
		}
		if name == "status" {
			hasColumn = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate digests columns: %w", err)
	}
	if !hasColumn {
		if _, err := conn.Exec(`ALTER TABLE digests ADD COLUMN status TEXT NOT NULL DEFAULT 'sent'`); err != nil {
			return fmt.Errorf("add digests status: %w", err)
		}
	}
	return nil
}

func ensureDigestMessageTextColumn(conn *sql.DB) error {
	rows, err := conn.Query("PRAGMA table_info(digests)")
	if err != nil {
		return fmt.Errorf("inspect digests table: %w", err)
	}
	defer rows.Close()

	hasColumn := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("scan digests column: %w", err)
		}
		if name == "message_text" {
			hasColumn = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate digests columns: %w", err)
	}
	if !hasColumn {
		if _, err := conn.Exec(`ALTER TABLE digests ADD COLUMN message_text TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add digests message text: %w", err)
		}
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

func ensureGroupStatusColumn(conn *sql.DB) error {
	rows, err := conn.Query("PRAGMA table_info(groups)")
	if err != nil {
		return fmt.Errorf("inspect groups table: %w", err)
	}
	defer rows.Close()

	hasStatus := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("scan groups column: %w", err)
		}
		if name == "status" {
			hasStatus = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate groups columns: %w", err)
	}
	if !hasStatus {
		if _, err := conn.Exec(`ALTER TABLE groups ADD COLUMN status TEXT NOT NULL DEFAULT 'active'`); err != nil {
			return fmt.Errorf("add groups status: %w", err)
		}
	}
	return nil
}

func ensureVersionColumns(conn *sql.DB) error {
	tables := []string{"channels", "groups", "ai_providers", "config"}
	for _, table := range tables {
		rows, err := conn.Query("PRAGMA table_info(" + table + ")")
		if err != nil {
			return fmt.Errorf("inspect %s table: %w", table, err)
		}
		hasVersion := false
		for rows.Next() {
			var cid, notNull, primaryKey int
			var name, columnType string
			var defaultValue sql.NullString
			if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
				rows.Close()
				return fmt.Errorf("scan %s column: %w", table, err)
			}
			if name == "version" {
				hasVersion = true
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate %s columns: %w", table, err)
		}
		rows.Close()
		if !hasVersion {
			if _, err := conn.Exec("ALTER TABLE " + table + " ADD COLUMN version INTEGER NOT NULL DEFAULT 1"); err != nil {
				return fmt.Errorf("add %s.version: %w", table, err)
			}
		}
	}
	return nil
}

func ensureProviderNameUniqueness(conn *sql.DB) error {
	if _, err := conn.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_ai_providers_name_nocase ON ai_providers(name COLLATE NOCASE)`); err != nil {
		return fmt.Errorf("create case-insensitive provider name index: %w", err)
	}
	return nil
}

func ensureForumTopicClosePendingColumn(conn *sql.DB) error {
	rows, err := conn.Query("PRAGMA table_info(forum_topics)")
	if err != nil {
		return fmt.Errorf("inspect forum_topics table: %w", err)
	}
	defer rows.Close()

	hasClosePending := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("scan forum_topics column: %w", err)
		}
		if name == "close_pending" {
			hasClosePending = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate forum_topics columns: %w", err)
	}
	if !hasClosePending {
		if _, err := conn.Exec(`ALTER TABLE forum_topics ADD COLUMN close_pending INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add forum_topics.close_pending: %w", err)
		}
	}
	return nil
}

func ensureForumTopicCreationIntentTable(conn *sql.DB) error {
	_, err := conn.Exec(`
		CREATE TABLE IF NOT EXISTS forum_topic_creation_intents (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id INTEGER NOT NULL,
			channel_id INTEGER NOT NULL,
			chat_id INTEGER NOT NULL,
			message_thread_id INTEGER NOT NULL DEFAULT 0 CHECK (message_thread_id >= 0),
			expected_version INTEGER NOT NULL,
			name TEXT NOT NULL,
			state TEXT NOT NULL DEFAULT 'creating',
			created_at TEXT DEFAULT (datetime('now')),
			updated_at TEXT DEFAULT (datetime('now')),
			UNIQUE(group_id, channel_id, state)
		)`)
	if err != nil {
		return fmt.Errorf("create forum topic creation intents: %w", err)
	}
	_, err = conn.Exec(`
		CREATE INDEX IF NOT EXISTS idx_forum_topic_creation_intents_pending
		ON forum_topic_creation_intents(state, chat_id, message_thread_id)`)
	if err != nil {
		return fmt.Errorf("index forum topic creation intents: %w", err)
	}
	_, err = conn.Exec(`
		CREATE TABLE IF NOT EXISTS forum_topic_tombstones (
			chat_id INTEGER NOT NULL,
			message_thread_id INTEGER NOT NULL CHECK (message_thread_id > 0),
			group_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			created_at TEXT DEFAULT (datetime('now')),
			PRIMARY KEY(chat_id, message_thread_id)
		)`)
	if err != nil {
		return fmt.Errorf("create forum topic tombstones: %w", err)
	}
	return nil
}

func ensureForumTopicRecoveryIdentity(conn *sql.DB) (err error) {
	rows, err := conn.Query(`PRAGMA foreign_key_list(forum_topic_creation_recovery)`)
	if err != nil {
		return fmt.Errorf("inspect topic recovery foreign keys: %w", err)
	}
	hasGroupForeignKey := false
	for rows.Next() {
		var id, seq int
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			rows.Close()
			return fmt.Errorf("scan topic recovery foreign key: %w", err)
		}
		if table == "groups" && from == "group_id" {
			hasGroupForeignKey = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate topic recovery foreign keys: %w", err)
	}
	rows.Close()
	if !hasGroupForeignKey {
		return nil
	}
	if _, err := conn.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for topic recovery migration: %w", err)
	}
	defer func() {
		if _, restoreErr := conn.Exec(`PRAGMA foreign_keys = ON`); restoreErr != nil && err == nil {
			err = fmt.Errorf("restore foreign keys after topic recovery migration: %w", restoreErr)
		}
	}()
	if _, err := conn.Exec(`DROP INDEX IF EXISTS idx_forum_topic_creation_recovery_group`); err != nil {
		return fmt.Errorf("drop topic recovery index: %w", err)
	}
	if _, err := conn.Exec(`
		ALTER TABLE forum_topic_creation_recovery
		RENAME TO forum_topic_creation_recovery_legacy`); err != nil {
		return fmt.Errorf("rename legacy topic recovery: %w", err)
	}
	if _, err := conn.Exec(`
		CREATE TABLE forum_topic_creation_recovery (
			group_id INTEGER NOT NULL,
			message_thread_id INTEGER NOT NULL CHECK (message_thread_id > 0),
			chat_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			created_at TEXT DEFAULT (datetime('now')),
			PRIMARY KEY(group_id, message_thread_id)
		)`); err != nil {
		return fmt.Errorf("create durable topic recovery table: %w", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO forum_topic_creation_recovery
			(group_id, message_thread_id, chat_id, name, created_at)
		SELECT group_id, message_thread_id, chat_id, name, created_at
		FROM forum_topic_creation_recovery_legacy`); err != nil {
		return fmt.Errorf("copy legacy topic recoveries: %w", err)
	}
	if _, err := conn.Exec(`DROP TABLE forum_topic_creation_recovery_legacy`); err != nil {
		return fmt.Errorf("drop legacy topic recovery: %w", err)
	}
	if _, err := conn.Exec(`
		CREATE INDEX IF NOT EXISTS idx_forum_topic_creation_recovery_group
		ON forum_topic_creation_recovery(group_id, message_thread_id)`); err != nil {
		return fmt.Errorf("recreate topic recovery index: %w", err)
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
		version INTEGER NOT NULL DEFAULT 1,
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
		version INTEGER NOT NULL DEFAULT 1,
		telegram_chat_id INTEGER NOT NULL UNIQUE,
		title TEXT DEFAULT '',
		status TEXT NOT NULL DEFAULT 'active',
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
	// 4. forum_topics (durable observed Telegram topic registry)
	// --------------------------------------------------
	`CREATE TABLE IF NOT EXISTS forum_topics (
		group_id INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
		message_thread_id INTEGER NOT NULL CHECK (message_thread_id > 0),
		name TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'observed',
		lifecycle_owned INTEGER NOT NULL DEFAULT 0,
		closed INTEGER NOT NULL DEFAULT 0,
		close_pending INTEGER NOT NULL DEFAULT 0,
		created_at TEXT DEFAULT (datetime('now')),
		updated_at TEXT DEFAULT (datetime('now')),
		PRIMARY KEY (group_id, message_thread_id)
	)`,

	// --------------------------------------------------
	// 4. ai_providers
	// --------------------------------------------------
	`CREATE TABLE IF NOT EXISTS ai_providers (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		version INTEGER NOT NULL DEFAULT 1,
		name TEXT NOT NULL UNIQUE COLLATE NOCASE,
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
		timezone TEXT DEFAULT 'Europe/Moscow',
		empty_digest_behavior TEXT NOT NULL DEFAULT 'send_message'
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
		post_count INTEGER DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'sent',
		message_text TEXT NOT NULL DEFAULT ''
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
		value TEXT NOT NULL DEFAULT '',
		version INTEGER NOT NULL DEFAULT 1
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
	`CREATE INDEX IF NOT EXISTS idx_forum_topics_group_open ON forum_topics(group_id, closed, message_thread_id)`,

	// --------------------------------------------------
	// 11. forum topic creation recovery
	// --------------------------------------------------
	`CREATE TABLE IF NOT EXISTS forum_topic_creation_recovery (
		group_id INTEGER NOT NULL,
		message_thread_id INTEGER NOT NULL CHECK (message_thread_id > 0),
		chat_id INTEGER NOT NULL,
		name TEXT NOT NULL,
		created_at TEXT DEFAULT (datetime('now')),
		PRIMARY KEY (group_id, message_thread_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_forum_topic_creation_recovery_group
		ON forum_topic_creation_recovery(group_id, message_thread_id)`,
}
