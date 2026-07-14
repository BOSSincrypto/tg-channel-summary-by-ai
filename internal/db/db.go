// Package db provides the SQLite database layer using the repository pattern.
// It manages schema creation, migrations, and provides repository
// implementations for all entities: channels, groups, posts, digests, providers.
package db

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// DB wraps the SQLite database connection and provides access to all
// entity repositories.
type DB struct {
	conn     *sql.DB
	Channels *ChannelRepository
	Groups   *GroupRepository
	Posts    *PostRepository
	Digests  *DigestRepository
	Providers *ProviderRepository
	Config   *ConfigRepository
}

// Open opens a SQLite database at the given path with WAL mode enabled,
// runs migrations, performs integrity check, and returns an initialized DB.
func Open(path string) (*DB, error) {
	// Use WAL mode by default via pragma in the DSN
	dsn := path + "?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Verify connection
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	// Enable WAL mode explicitly (belt and suspenders)
	if _, err := conn.Exec("PRAGMA journal_mode=WAL"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}

	// Enable foreign keys
	if _, err := conn.Exec("PRAGMA foreign_keys=ON"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	// Run integrity check before migrations
	if err := integrityCheck(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("integrity check failed: %w", err)
	}

	// Run migrations
	if err := runMigrations(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	// Verify schema after migrations
	if err := integrityCheck(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("post-migration integrity check failed: %w", err)
	}

	db := &DB{conn: conn}
	db.Channels = &ChannelRepository{db: db}
	db.Groups = &GroupRepository{db: db}
	db.Posts = &PostRepository{db: db}
	db.Digests = &DigestRepository{db: db}
	db.Providers = &ProviderRepository{db: db}
	db.Config = &ConfigRepository{db: db}

	return db, nil
}

// Close closes the database connection.
func (d *DB) Close() error {
	return d.conn.Close()
}

// Conn returns the underlying sql.DB for direct access when needed.
func (d *DB) Conn() *sql.DB {
	return d.conn
}

// integrityCheck runs PRAGMA integrity_check and returns an error if it fails.
func integrityCheck(conn *sql.DB) error {
	rows, err := conn.Query("PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("run integrity_check: %w", err)
	}
	defer rows.Close()

	var result string
	for rows.Next() {
		if err := rows.Scan(&result); err != nil {
			return fmt.Errorf("scan integrity_check: %w", err)
		}
		if result != "ok" {
			return fmt.Errorf("database corruption detected: %s", result)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows iteration: %w", err)
	}
	return nil
}

// CleanupPosts deletes posts older than retentionDays and runs VACUUM
// to reclaim disk space. Returns the number of deleted rows.
func (d *DB) CleanupPosts(retentionDays int) (int64, error) {
	result, err := d.conn.Exec(
		"DELETE FROM posts WHERE posted_at < datetime('now', ? || ' days')",
		fmt.Sprintf("-%d", retentionDays),
	)
	if err != nil {
		return 0, fmt.Errorf("cleanup posts: %w", err)
	}
	deleted, _ := result.RowsAffected()

	// Reclaim disk space
	if _, err := d.conn.Exec("PRAGMA optimize"); err != nil {
		return deleted, fmt.Errorf("pragma optimize: %w", err)
	}

	return deleted, nil
}
