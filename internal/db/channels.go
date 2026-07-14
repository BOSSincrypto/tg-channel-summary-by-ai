package db

import (
	"database/sql"
	"fmt"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

// ChannelRepository provides CRUD operations for Telegram channels.
type ChannelRepository struct {
	db *DB
}

// Insert adds a new channel. Username should be lowercase without @.
func (r *ChannelRepository) Insert(ch *model.Channel) (int64, error) {
	result, err := r.db.Conn().Exec(
		`INSERT INTO channels (username, title, enabled, last_post_id)
		 VALUES (?, ?, ?, ?)`,
		ch.Username, ch.Title, boolToInt(ch.Enabled), ch.LastPostID,
	)
	if err != nil {
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
	err := r.db.Conn().QueryRow(
		`SELECT id, username, title, enabled, last_post_id, created_at
		 FROM channels WHERE id = ?`, id,
	).Scan(&ch.ID, &ch.Username, &ch.Title, &enabled, &ch.LastPostID, &ch.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get channel by id: %w", err)
	}
	ch.Enabled = intToBool(enabled)
	return ch, nil
}

// GetByUsername returns a channel by its username (case-insensitive).
func (r *ChannelRepository) GetByUsername(username string) (*model.Channel, error) {
	ch := &model.Channel{}
	var enabled int
	err := r.db.Conn().QueryRow(
		`SELECT id, username, title, enabled, last_post_id, created_at
		 FROM channels WHERE LOWER(username) = LOWER(?)`, username,
	).Scan(&ch.ID, &ch.Username, &ch.Title, &enabled, &ch.LastPostID, &ch.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get channel by username: %w", err)
	}
	ch.Enabled = intToBool(enabled)
	return ch, nil
}

// List returns all channels.
func (r *ChannelRepository) List() ([]model.Channel, error) {
	rows, err := r.db.Conn().Query(
		`SELECT id, username, title, enabled, last_post_id, created_at
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
		if err := rows.Scan(&ch.ID, &ch.Username, &ch.Title, &enabled, &ch.LastPostID, &ch.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan channel: %w", err)
		}
		ch.Enabled = intToBool(enabled)
		channels = append(channels, ch)
	}
	return channels, rows.Err()
}

// ListEnabled returns only channels where enabled = 1.
func (r *ChannelRepository) ListEnabled() ([]model.Channel, error) {
	rows, err := r.db.Conn().Query(
		`SELECT id, username, title, enabled, last_post_id, created_at
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
		if err := rows.Scan(&ch.ID, &ch.Username, &ch.Title, &enabled, &ch.LastPostID, &ch.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan channel: %w", err)
		}
		ch.Enabled = intToBool(enabled)
		channels = append(channels, ch)
	}
	return channels, rows.Err()
}

// Update modifies an existing channel.
func (r *ChannelRepository) Update(ch *model.Channel) error {
	_, err := r.db.Conn().Exec(
		`UPDATE channels SET username = ?, title = ?, enabled = ?, last_post_id = ?
		 WHERE id = ?`,
		ch.Username, ch.Title, boolToInt(ch.Enabled), ch.LastPostID, ch.ID,
	)
	if err != nil {
		return fmt.Errorf("update channel: %w", err)
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

// Delete removes a channel by ID. Foreign key cascade removes related
// rows from group_channels, posts, etc.
func (r *ChannelRepository) Delete(id int64) error {
	_, err := r.db.Conn().Exec(`DELETE FROM channels WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete channel: %w", err)
	}
	return nil
}

// ExistsByUsername checks whether a channel with the given username exists
// (case-insensitive comparison).
func (r *ChannelRepository) ExistsByUsername(username string) (bool, error) {
	var count int
	err := r.db.Conn().QueryRow(
		`SELECT COUNT(*) FROM channels WHERE LOWER(username) = LOWER(?)`,
		username,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check channel exists: %w", err)
	}
	return count > 0, nil
}
