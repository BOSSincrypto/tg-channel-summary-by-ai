package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

// ConfigRepository provides access to the key-value config store.
type ConfigRepository struct {
	db *DB
}

// GetWithVersion returns a config value and the version used for optimistic
// locking. It is intended for WebApp settings writes.
func (r *ConfigRepository) GetWithVersion(key string) (string, int64, error) {
	var value string
	var version int64
	err := r.db.Conn().QueryRow(
		`SELECT value, version FROM config WHERE key = ?`, key,
	).Scan(&value, &version)
	if err == sql.ErrNoRows {
		return "", 0, ErrNotFound
	}
	if err != nil {
		return "", 0, fmt.Errorf("get config with version: %w", err)
	}
	return value, version, nil
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
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, version = config.version + 1`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf("set config: %w", err)
	}
	return nil
}

// SetOptimistic updates a config key only when the supplied positive version
// matches. Missing versions are rejected so authenticated mutations cannot
// bypass optimistic locking.
func (r *ConfigRepository) SetOptimistic(key, value string, expectedVersion int64) (int64, error) {
	if expectedVersion <= 0 {
		return 0, ErrConflict
	}
	result, err := r.db.Conn().Exec(
		`UPDATE config SET value = ?, version = version + 1 WHERE key = ? AND version = ?`,
		value, key, expectedVersion,
	)
	if err != nil {
		return 0, fmt.Errorf("update config optimistically: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("update config rows affected: %w", err)
	}
	if affected == 0 {
		if _, _, err := r.GetWithVersion(key); errors.Is(err, ErrNotFound) {
			return 0, ErrConflict
		} else if err != nil {
			return 0, err
		}
		return 0, ErrConflict
	}
	return expectedVersion + 1, nil
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
