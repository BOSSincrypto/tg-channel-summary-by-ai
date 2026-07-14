package db

import (
	"database/sql"
	"fmt"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

// ConfigRepository provides access to the key-value config store.
type ConfigRepository struct {
	db *DB
}

// Get returns the value for a given key. Returns ErrNotFound if the key does not exist.
func (r *ConfigRepository) Get(key string) (string, error) {
	var value string
	err := r.db.Conn().QueryRow(
		`SELECT value FROM config WHERE key = ?`, key,
	).Scan(&value)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get config: %w", err)
	}
	return value, nil
}

// Set upserts a key-value pair.
func (r *ConfigRepository) Set(key, value string) error {
	_, err := r.db.Conn().Exec(
		`INSERT INTO config (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf("set config: %w", err)
	}
	return nil
}

// GetAll returns all key-value pairs.
func (r *ConfigRepository) GetAll() ([]model.ConfigKV, error) {
	rows, err := r.db.Conn().Query(
		`SELECT key, value FROM config ORDER BY key ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("get all config: %w", err)
	}
	defer rows.Close()

	var entries []model.ConfigKV
	for rows.Next() {
		var entry model.ConfigKV
		if err := rows.Scan(&entry.Key, &entry.Value); err != nil {
			return nil, fmt.Errorf("scan config: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// Delete removes a key from config.
func (r *ConfigRepository) Delete(key string) error {
	_, err := r.db.Conn().Exec(`DELETE FROM config WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("delete config: %w", err)
	}
	return nil
}
