// Package db provides the SQLite database layer using the repository pattern.
// It manages schema creation, migrations, and provides repository
// implementations for all entities: channels, groups, posts, digests, providers.
package db

// DB wraps the SQLite database connection.
type DB struct {
	// TODO: *sql.DB handle
}

// Open opens a SQLite database at the given path with WAL mode enabled.
func Open(path string) (*DB, error) {
	// TODO: open database, enable WAL mode, run migrations
	return &DB{}, nil
}

// Close closes the database connection.
func (d *DB) Close() error {
	// TODO: close database
	return nil
}
