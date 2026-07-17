package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

// ChannelRepository provides CRUD operations for Telegram channels.
type ChannelRepository struct {
	db *DB
}

// Insert adds a new channel and normalizes its username before persistence.
func (r *ChannelRepository) Insert(ch *model.Channel) (int64, error) {
	if ch == nil {
		return 0, fmt.Errorf("insert channel: channel is required")
	}
	ch.Username = normalizeChannelUsername(ch.Username)
	result, err := r.db.Conn().Exec(
		`INSERT INTO channels (username, title, enabled, last_post_id, fetch_error_kind, fetch_error_message, fetch_error_at, version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 1)`,
		ch.Username, ch.Title, boolToInt(ch.Enabled), ch.LastPostID,
		ch.FetchErrorKind, ch.FetchErrorMessage, nullableString(ch.FetchErrorAt),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("insert channel %q: %w", ch.Username, ErrDuplicate)
		}
		return 0, fmt.Errorf("insert channel: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}
	return id, nil
}

// GetByID returns a channel by its ID, or ErrNotFound if not found.
func (r *ChannelRepository) GetByID(id int64) (*model.Channel, error) {
	ch := &model.Channel{}
	var enabled int
	var fetchErrorAt sql.NullString
	err := r.db.Conn().QueryRow(
		`SELECT id, version, username, title, enabled, last_post_id, fetch_error_kind, fetch_error_message, fetch_error_at, created_at
		 FROM channels WHERE id = ?`, id,
	).Scan(&ch.ID, &ch.Version, &ch.Username, &ch.Title, &enabled, &ch.LastPostID, &ch.FetchErrorKind, &ch.FetchErrorMessage, &fetchErrorAt, &ch.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get channel by id: %w", err)
	}
	ch.Enabled = intToBool(enabled)
	ch.FetchErrorAt = nullableStringPtr(fetchErrorAt)
	return ch, nil
}

// GetByUsername returns a channel by its username (case-insensitive).
func (r *ChannelRepository) GetByUsername(username string) (*model.Channel, error) {
	username = normalizeChannelUsername(username)
	ch := &model.Channel{}
	var enabled int
	var fetchErrorAt sql.NullString
	err := r.db.Conn().QueryRow(
		`SELECT id, version, username, title, enabled, last_post_id, fetch_error_kind, fetch_error_message, fetch_error_at, created_at
		 FROM channels WHERE username = ?`, username,
	).Scan(&ch.ID, &ch.Version, &ch.Username, &ch.Title, &enabled, &ch.LastPostID, &ch.FetchErrorKind, &ch.FetchErrorMessage, &fetchErrorAt, &ch.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get channel by username: %w", err)
	}
	ch.Enabled = intToBool(enabled)
	ch.FetchErrorAt = nullableStringPtr(fetchErrorAt)
	return ch, nil
}

// List returns all channels.
func (r *ChannelRepository) List() ([]model.Channel, error) {
	rows, err := r.db.Conn().Query(
		`SELECT id, version, username, title, enabled, last_post_id, fetch_error_kind, fetch_error_message, fetch_error_at, created_at
		 FROM channels ORDER BY username ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	defer rows.Close()

	var channels []model.Channel
	for rows.Next() {
		var ch model.Channel
		var enabled int
		var fetchErrorAt sql.NullString
		if err := rows.Scan(&ch.ID, &ch.Version, &ch.Username, &ch.Title, &enabled, &ch.LastPostID, &ch.FetchErrorKind, &ch.FetchErrorMessage, &fetchErrorAt, &ch.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan channel: %w", err)
		}
		ch.Enabled = intToBool(enabled)
		ch.FetchErrorAt = nullableStringPtr(fetchErrorAt)
		channels = append(channels, ch)
	}
	return channels, rows.Err()
}

// ListEnabled returns only channels where enabled = 1.
func (r *ChannelRepository) ListEnabled() ([]model.Channel, error) {
	rows, err := r.db.Conn().Query(
		`SELECT id, version, username, title, enabled, last_post_id, fetch_error_kind, fetch_error_message, fetch_error_at, created_at
		 FROM channels WHERE enabled = 1 ORDER BY username ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list enabled channels: %w", err)
	}
	defer rows.Close()

	var channels []model.Channel
	for rows.Next() {
		var ch model.Channel
		var enabled int
		var fetchErrorAt sql.NullString
		if err := rows.Scan(&ch.ID, &ch.Version, &ch.Username, &ch.Title, &enabled, &ch.LastPostID, &ch.FetchErrorKind, &ch.FetchErrorMessage, &fetchErrorAt, &ch.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan channel: %w", err)
		}
		ch.Enabled = intToBool(enabled)
		ch.FetchErrorAt = nullableStringPtr(fetchErrorAt)
		channels = append(channels, ch)
	}
	return channels, rows.Err()
}

// Update modifies an existing channel and normalizes its username.
func (r *ChannelRepository) Update(ch *model.Channel) error {
	if ch == nil {
		return fmt.Errorf("update channel: channel is required")
	}
	ch.Username = normalizeChannelUsername(ch.Username)
	result, err := r.db.Conn().Exec(
		`UPDATE channels SET username = ?, title = ?, enabled = ?, last_post_id = ?, version = version + 1
		 WHERE id = ?`,
		ch.Username, ch.Title, boolToInt(ch.Enabled), ch.LastPostID, ch.ID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("update channel %q: %w", ch.Username, ErrDuplicate)
		}
		return fmt.Errorf("update channel: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update channel rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateOptimistic updates a channel only when the caller still has the
// supplied positive version.
func (r *ChannelRepository) UpdateOptimistic(ch *model.Channel, version int64) error {
	if ch == nil {
		return fmt.Errorf("update channel: channel is required")
	}
	if version <= 0 {
		return fmt.Errorf("update channel: %w", ErrConflict)
	}
	ch.Username = normalizeChannelUsername(ch.Username)
	query := `UPDATE channels SET username = ?, title = ?, enabled = ?, last_post_id = ?, version = version + 1 WHERE id = ?`
	args := []any{ch.Username, ch.Title, boolToInt(ch.Enabled), ch.LastPostID, ch.ID}
	query += ` AND version = ?`
	args = append(args, version)
	result, err := r.db.Conn().Exec(query, args...)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("update channel %q: %w", ch.Username, ErrDuplicate)
		}
		return fmt.Errorf("update channel: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update channel rows affected: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("update channel: %w", ErrConflict)
	}
	return nil
}

// UpdateEnabledOptimistic changes the enabled flag only for the expected
// version and returns ErrConflict for a stale WebApp write.
func (r *ChannelRepository) UpdateEnabledOptimistic(id int64, enabled bool, version int64) error {
	if version <= 0 {
		return ErrConflict
	}
	query := `UPDATE channels SET enabled = ?, version = version + 1 WHERE id = ? AND version = ?`
	args := []any{boolToInt(enabled), id, version}
	result, err := r.db.Conn().Exec(query, args...)
	if err != nil {
		return fmt.Errorf("update channel enabled: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update channel enabled rows affected: %w", err)
	}
	if affected == 0 {
		return ErrConflict
	}
	return nil
}

// DeleteOptimistic removes a channel only when the caller still has the
// supplied positive version.
func (r *ChannelRepository) DeleteOptimistic(id, version int64) error {
	if version <= 0 {
		return ErrConflict
	}
	result, err := r.db.Conn().Exec(
		`DELETE FROM channels WHERE id = ? AND version = ?`,
		id, version,
	)
	if err != nil {
		return fmt.Errorf("delete channel: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete channel rows affected: %w", err)
	}
	if affected == 0 {
		return ErrConflict
	}
	return nil
}

// UpdateLastPostID sets the last_post_id for a channel.
func (r *ChannelRepository) UpdateLastPostID(id, lastPostID int64) error {
	_, err := r.db.Conn().Exec(
		`UPDATE channels SET last_post_id = ? WHERE id = ?`,
		lastPostID, id,
	)
	if err != nil {
		return fmt.Errorf("update last post id: %w", err)
	}
	return nil
}

// MarkFetchError persists the latest channel fetch failure without changing
// channel configuration, the cursor, or stored posts.
func (r *ChannelRepository) MarkFetchError(id int64, kind, message string) error {
	if strings.TrimSpace(kind) == "" {
		return fmt.Errorf("mark channel fetch error: error kind is required")
	}
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := r.db.Conn().Exec(
		`UPDATE channels
		 SET fetch_error_kind = ?, fetch_error_message = ?, fetch_error_at = ?
		 WHERE id = ?`,
		kind, message, timestamp, id,
	)
	if err != nil {
		return fmt.Errorf("mark channel %d fetch error: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark channel %d fetch error rows affected: %w", id, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearFetchError removes durable fetch-error state after a successful fetch.
func (r *ChannelRepository) ClearFetchError(id int64) error {
	result, err := r.db.Conn().Exec(
		`UPDATE channels
		 SET fetch_error_kind = '', fetch_error_message = '', fetch_error_at = NULL
		 WHERE id = ?`,
		id,
	)
	if err != nil {
		return fmt.Errorf("clear channel %d fetch error: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("clear channel %d fetch error rows affected: %w", id, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ToggleEnabled atomically flips a channel's enabled state.
func (r *ChannelRepository) ToggleEnabled(id int64) error {
	result, err := r.db.Conn().Exec(
		`UPDATE channels SET enabled = CASE enabled WHEN 0 THEN 1 ELSE 0 END WHERE id = ?`,
		id,
	)
	if err != nil {
		return fmt.Errorf("toggle channel %d: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("toggle channel %d rows affected: %w", id, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a channel by ID. Foreign key cascade removes related
// rows from group_channels, posts, etc.
func (r *ChannelRepository) Delete(id int64) error {
	result, err := r.db.Conn().Exec(`DELETE FROM channels WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete channel: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete channel rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ExistsByUsername checks whether a channel with the given username exists
// (case-insensitive comparison).
func (r *ChannelRepository) ExistsByUsername(username string) (bool, error) {
	username = normalizeChannelUsername(username)
	var count int
	err := r.db.Conn().QueryRow(
		`SELECT COUNT(*) FROM channels WHERE username = ?`,
		username,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check channel exists: %w", err)
	}
	return count > 0, nil
}

func normalizeChannelUsername(username string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(username), "@"))
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableStringPtr(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	result := value.String
	return &result
}
