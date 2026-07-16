// Package db provides the SQLite database layer using the repository pattern.
// It manages schema creation, migrations, and provides repository
// implementations for all entities: channels, groups, posts, digests, providers.
package db

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

// DB wraps the SQLite database connection and provides access to all
// entity repositories.
type DB struct {
	conn              *sql.DB
	Channels          *ChannelRepository
	Groups            *GroupRepository
	Posts             *PostRepository
	Digests           *DigestRepository
	Providers         *ProviderRepository
	Config            *ConfigRepository
	providerKeyCipher *secretCipher
}

// Open opens a SQLite database at the given path with WAL mode enabled,
// runs migrations, performs integrity check, and returns an initialized DB.
// Any startup failure is returned with path-aware recovery guidance so callers
// can fail closed before serving HTTP traffic.
func Open(path string) (*DB, error) {
	keyMaterial := os.Getenv("PROVIDER_ENCRYPTION_KEY")
	if keyMaterial == "" {
		keyMaterial = os.Getenv("BOT_TOKEN")
	}
	if keyMaterial == "" {
		// Tests and local repository-only tools may not have application
		// secrets. A process-local key still prevents accidental plaintext
		// storage for that process; production always supplies BOT_TOKEN.
		var err error
		keyMaterial, err = localKeyMaterial()
		if err != nil {
			return nil, fmt.Errorf("configure local provider key encryption: %w", err)
		}
	}
	return OpenWithEncryptionKey(path, keyMaterial)
}

// OpenWithEncryptionKey opens SQLite and encrypts provider API keys with the
// supplied application secret. The key is never persisted in the database.
func OpenWithEncryptionKey(path, keyMaterial string) (*DB, error) {
	keyCipher, err := newSecretCipher(keyMaterial)
	if err != nil {
		return nil, fmt.Errorf("configure provider key encryption: %w", err)
	}
	// Use WAL mode by default via pragma in the DSN.
	dsn := path + "?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, startupError(path, "open database", err)
	}

	closeOnError := func(operation string, operationErr error) (*DB, error) {
		if closeErr := conn.Close(); closeErr != nil {
			operationErr = fmt.Errorf("%w; close database: %v", operationErr, closeErr)
		}
		return nil, startupError(path, operation, operationErr)
	}

	// Verify connection.
	if err := conn.Ping(); err != nil {
		return closeOnError("ping database", err)
	}

	// Enable WAL mode explicitly (belt and suspenders).
	if _, err := conn.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return closeOnError("enable WAL", err)
	}

	// Enable foreign keys.
	if _, err := conn.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return closeOnError("enable foreign keys", err)
	}

	// Run integrity check before migrations.
	if err := integrityCheck(conn); err != nil {
		return closeOnError("integrity check", err)
	}

	// Run migrations.
	if err := runMigrations(conn); err != nil {
		return closeOnError("run migrations", err)
	}

	// Verify schema after migrations.
	if err := integrityCheck(conn); err != nil {
		return closeOnError("post-migration integrity check", err)
	}
	if err := reencryptLegacyProviderKeys(conn, keyCipher); err != nil {
		return closeOnError("encrypt legacy provider keys", err)
	}

	db := &DB{conn: conn, providerKeyCipher: keyCipher}
	db.Channels = &ChannelRepository{db: db}
	db.Groups = &GroupRepository{db: db}
	db.Posts = &PostRepository{db: db}
	db.Digests = &DigestRepository{db: db}
	db.Providers = &ProviderRepository{db: db, keyCipher: keyCipher}
	db.Config = &ConfigRepository{db: db}

	return db, nil
}

func reencryptLegacyProviderKeys(conn *sql.DB, keyCipher *secretCipher) error {
	rows, err := conn.Query(`SELECT id, api_key FROM ai_providers WHERE api_key NOT LIKE ?`, encryptedAPIKeyPrefix+"%")
	if err != nil {
		return fmt.Errorf("select legacy provider keys: %w", err)
	}
	defer rows.Close()

	type legacyKey struct {
		id  int64
		key string
	}
	var keys []legacyKey
	for rows.Next() {
		var item legacyKey
		if err := rows.Scan(&item.id, &item.key); err != nil {
			return fmt.Errorf("scan legacy provider key: %w", err)
		}
		keys = append(keys, item)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy provider keys: %w", err)
	}
	if len(keys) == 0 {
		return nil
	}

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("begin legacy provider key migration: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`UPDATE ai_providers SET api_key = ? WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("prepare legacy provider key migration: %w", err)
	}
	defer stmt.Close()
	for _, item := range keys {
		encrypted, err := keyCipher.encrypt(item.key)
		if err != nil {
			return fmt.Errorf("encrypt legacy provider key %d: %w", item.id, err)
		}
		if _, err := stmt.Exec(encrypted, item.id); err != nil {
			return fmt.Errorf("update legacy provider key %d: %w", item.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy provider key migration: %w", err)
	}
	return nil
}

// Close closes the database connection.
func (d *DB) Close() error {
	return d.conn.Close()
}

// Conn returns the underlying sql.DB for direct access when needed.
func (d *DB) Conn() *sql.DB {
	return d.conn
}

// startupError adds the database path and actionable recovery guidance to
// every startup failure. Full-disk errors are called out separately because
// they indicate an operational capacity problem rather than corruption.
func startupError(path, operation string, err error) error {
	if isDatabaseFullError(err) {
		return fmt.Errorf("database startup failed at %s during %s: database full: sqlite driver details: %w", path, operation, err)
	}
	return fmt.Errorf("database startup failed at %s during %s: sqlite driver details: %w; Database corruption detected at %s. Restore from backup or manually repair.", path, operation, err, path)
}

func isDatabaseFullError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "sqlite_full") ||
		strings.Contains(message, "database or disk is full") ||
		strings.Contains(message, "database full")
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
	cutoff := fmt.Sprintf("-%d days", retentionDays)
	rows, err := d.conn.Query(
		`SELECT p.id
		 FROM posts p
		 WHERE datetime(p.posted_at) < datetime('now', ?)
		   AND NOT EXISTS (
			SELECT 1
			FROM digest_posts dp
			INNER JOIN digests d2 ON d2.id = dp.digest_id
			WHERE dp.post_id = p.id
			  AND datetime(d2.sent_at) >= datetime('now', ?)
		   )
		 ORDER BY p.id`,
		cutoff,
		cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("select cleanup candidates: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("scan cleanup candidate: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate cleanup candidates: %w", err)
	}

	if len(ids) == 0 {
		if _, err := d.conn.Exec("PRAGMA optimize"); err != nil {
			return 0, fmt.Errorf("pragma optimize: %w", err)
		}
		return 0, nil
	}

	tx, err := d.conn.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin cleanup transaction: %w", err)
	}
	defer tx.Rollback() // no-op after commit

	stmt, err := tx.Prepare(`DELETE FROM posts WHERE id = ?`)
	if err != nil {
		return 0, fmt.Errorf("prepare cleanup delete: %w", err)
	}
	defer stmt.Close()

	var deleted int64
	for _, id := range ids {
		result, err := stmt.Exec(id)
		if err != nil {
			return 0, fmt.Errorf("delete post %d: %w", id, err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("rows affected for post %d: %w", id, err)
		}
		deleted += n
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit cleanup transaction: %w", err)
	}

	// Reclaim disk space
	if _, err := d.conn.Exec("PRAGMA optimize"); err != nil {
		return deleted, fmt.Errorf("pragma optimize: %w", err)
	}

	return deleted, nil
}
