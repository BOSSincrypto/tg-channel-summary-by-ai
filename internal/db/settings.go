package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

// SettingsUpdate describes one durable settings mutation. The global config
// row, all group settings, and the pending scheduler intent are committed
// together so a restart can recover an update after a late runtime failure.
type SettingsUpdate struct {
	ConfigKey       string
	ConfigValue     string
	ExpectedVersion int64
	GroupSettings   []*model.GroupSettings
	PendingKey      string
	PendingValue    string
}

// ApplySettingsTransaction atomically persists global and group settings and
// records the scheduler intent that must be applied after commit.
func (d *DB) ApplySettingsTransaction(update SettingsUpdate) (int64, error) {
	if d == nil || d.conn == nil {
		return 0, errors.New("settings transaction: database is not configured")
	}
	if update.ConfigKey == "" {
		return 0, errors.New("settings transaction: config key is required")
	}
	if update.ExpectedVersion <= 0 {
		return 0, ErrConflict
	}

	tx, err := d.conn.Begin()
	if err != nil {
		return 0, fmt.Errorf("settings transaction: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(
		`UPDATE config SET value = ?, version = version + 1
		 WHERE key = ? AND version = ?`,
		update.ConfigValue, update.ConfigKey, update.ExpectedVersion,
	)
	if err != nil {
		return 0, fmt.Errorf("settings transaction: update global settings: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("settings transaction: global rows affected: %w", err)
	}
	if affected == 0 {
		var currentVersion int64
		err := tx.QueryRow(`SELECT version FROM config WHERE key = ?`, update.ConfigKey).Scan(&currentVersion)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrConflict
		}
		if err != nil {
			return 0, fmt.Errorf("settings transaction: inspect global version: %w", err)
		}
		return 0, fmt.Errorf("settings transaction: expected version %d, current version %d: %w", update.ExpectedVersion, currentVersion, ErrConflict)
	}

	for _, settings := range update.GroupSettings {
		if settings == nil || settings.GroupID <= 0 {
			return 0, errors.New("settings transaction: group settings require a positive group ID")
		}
		var providerID, modelValue any
		if settings.ProviderID != nil {
			providerID = *settings.ProviderID
		}
		if settings.Model != nil {
			modelValue = *settings.Model
		}
		emptyBehavior := normalizeEmptyDigestBehavior(settings.EmptyDigestBehavior)
		if strings.TrimSpace(settings.EmptyDigestBehavior) == "" {
			var currentBehavior string
			err := tx.QueryRow(
				`SELECT empty_digest_behavior FROM group_settings WHERE group_id = ?`,
				settings.GroupID,
			).Scan(&currentBehavior)
			if err == nil && currentBehavior != "" {
				emptyBehavior = currentBehavior
			}
		}
		if _, err := tx.Exec(
			`INSERT INTO group_settings (group_id, provider_id, model, digest_time, timezone, empty_digest_behavior, silent_digest)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(group_id) DO UPDATE SET
			   provider_id = excluded.provider_id,
			   model = excluded.model,
			   digest_time = excluded.digest_time,
			   timezone = excluded.timezone,
			   empty_digest_behavior = excluded.empty_digest_behavior,
			   silent_digest = excluded.silent_digest`,
			settings.GroupID, providerID, modelValue, settings.DigestTime, settings.Timezone,
			emptyBehavior, boolToSQLite(settings.SilentDigest),
		); err != nil {
			return 0, fmt.Errorf("settings transaction: update group %d settings: %w", settings.GroupID, err)
		}
	}

	if update.PendingKey != "" {
		if _, err := tx.Exec(
			`INSERT INTO config (key, value, version) VALUES (?, ?, 1)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value, version = config.version + 1`,
			update.PendingKey, update.PendingValue,
		); err != nil {
			return 0, fmt.Errorf("settings transaction: record scheduler intent: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("settings transaction: commit: %w", err)
	}
	return update.ExpectedVersion + 1, nil
}

// ClearSettingsSyncPending removes a scheduler intent after the live scheduler
// has converged. Leaving it present is safe and makes restart reconciliation
// retryable if this cleanup itself fails.
func (d *DB) ClearSettingsSyncPending(key string) error {
	if d == nil || d.conn == nil {
		return errors.New("clear settings sync intent: database is not configured")
	}
	if key == "" {
		return errors.New("clear settings sync intent: key is required")
	}
	if _, err := d.conn.Exec(`DELETE FROM config WHERE key = ?`, key); err != nil {
		return fmt.Errorf("clear settings sync intent: %w", err)
	}
	return nil
}
