package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

// ForumTopicRepository stores topics learned from Telegram lifecycle events.
// It is intentionally independent from group_channels assignments.
type ForumTopicRepository struct {
	db *DB
}

// Observe records a topic seen in an incoming Telegram update.
func (r *ForumTopicRepository) Observe(groupID, threadID int64, name string) error {
	return r.upsert(groupID, threadID, name, false)
}

// PersistOwned records a topic created by this bot and marks it as lifecycle-owned.
func (r *ForumTopicRepository) PersistOwned(groupID, threadID int64, name string) error {
	return r.upsert(groupID, threadID, name, true)
}

func (r *ForumTopicRepository) upsert(groupID, threadID int64, name string, owned bool) error {
	if r == nil || r.db == nil {
		return errors.New("forum topic repository is not configured")
	}
	if groupID <= 0 {
		return errors.New("forum topic group id must be positive")
	}
	if threadID <= 0 {
		return errors.New("forum topic thread id must be positive")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("forum topic name is required")
	}
	status := model.ForumTopicStatusObserved
	if owned {
		status = model.ForumTopicStatusPersisted
	}
	_, err := r.db.Conn().Exec(`
		INSERT INTO forum_topics
			(group_id, message_thread_id, name, status, lifecycle_owned, closed, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, datetime('now'))
		ON CONFLICT(group_id, message_thread_id) DO UPDATE SET
			name = excluded.name,
			status = CASE WHEN forum_topics.lifecycle_owned = 1 OR excluded.lifecycle_owned = 1
				THEN 'persisted' ELSE 'observed' END,
			lifecycle_owned = MAX(forum_topics.lifecycle_owned, excluded.lifecycle_owned),
			closed = 0,
			updated_at = datetime('now')`,
		groupID, threadID, name, status, boolToInt(owned),
	)
	if err != nil {
		return fmt.Errorf("upsert forum topic: %w", err)
	}
	return nil
}

// Get returns a topic regardless of whether it is currently closed.
func (r *ForumTopicRepository) Get(groupID, threadID int64) (*model.ForumTopic, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("forum topic repository is not configured")
	}
	topic := &model.ForumTopic{}
	var owned, closed int
	err := r.db.Conn().QueryRow(`
		SELECT group_id, message_thread_id, name, status, lifecycle_owned, closed, created_at, updated_at
		FROM forum_topics
		WHERE group_id = ? AND message_thread_id = ?`,
		groupID, threadID,
	).Scan(&topic.GroupID, &topic.MessageThreadID, &topic.Name, &topic.Status,
		&owned, &closed, &topic.CreatedAt, &topic.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get forum topic: %w", err)
	}
	topic.LifecycleOwned = intToBool(owned)
	topic.Closed = intToBool(closed)
	return topic, nil
}

// ListOpen returns observed and persisted topics that can be selected.
func (r *ForumTopicRepository) ListOpen(groupID int64) ([]model.ForumTopic, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("forum topic repository is not configured")
	}
	rows, err := r.db.Conn().Query(`
		SELECT group_id, message_thread_id, name, status, lifecycle_owned, closed, created_at, updated_at
		FROM forum_topics
		WHERE group_id = ? AND closed = 0
		ORDER BY name COLLATE NOCASE, message_thread_id`,
		groupID,
	)
	if err != nil {
		return nil, fmt.Errorf("list open forum topics: %w", err)
	}
	defer rows.Close()
	var topics []model.ForumTopic
	for rows.Next() {
		var topic model.ForumTopic
		var owned, closed int
		if err := rows.Scan(&topic.GroupID, &topic.MessageThreadID, &topic.Name, &topic.Status,
			&owned, &closed, &topic.CreatedAt, &topic.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan forum topic: %w", err)
		}
		topic.LifecycleOwned = intToBool(owned)
		topic.Closed = intToBool(closed)
		topics = append(topics, topic)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate forum topics: %w", err)
	}
	return topics, nil
}

// MarkEdited updates the name learned from an edited-topic service message.
func (r *ForumTopicRepository) MarkEdited(groupID, threadID int64, name string) error {
	if r == nil || r.db == nil {
		return errors.New("forum topic repository is not configured")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	result, err := r.db.Conn().Exec(`
		UPDATE forum_topics SET name = ?, updated_at = datetime('now')
		WHERE group_id = ? AND message_thread_id = ?`,
		name, groupID, threadID,
	)
	if err != nil {
		return fmt.Errorf("mark forum topic edited: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("forum topic edit rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkClosed hides a topic from future assignments without deleting its history.
func (r *ForumTopicRepository) MarkClosed(groupID, threadID int64) error {
	return r.setClosed(groupID, threadID, true)
}

// MarkReopened makes a previously closed observed topic selectable again.
func (r *ForumTopicRepository) MarkReopened(groupID, threadID int64) error {
	return r.setClosed(groupID, threadID, false)
}

func (r *ForumTopicRepository) setClosed(groupID, threadID int64, closed bool) error {
	if r == nil || r.db == nil {
		return errors.New("forum topic repository is not configured")
	}
	result, err := r.db.Conn().Exec(`
		UPDATE forum_topics SET closed = ?, updated_at = datetime('now')
		WHERE group_id = ? AND message_thread_id = ?`,
		boolToInt(closed), groupID, threadID,
	)
	if err != nil {
		return fmt.Errorf("set forum topic closed state: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("forum topic closed rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteOwned removes a topic only when the registry proves the bot created it.
func (r *ForumTopicRepository) DeleteOwned(groupID, threadID int64) error {
	if r == nil || r.db == nil {
		return errors.New("forum topic repository is not configured")
	}
	result, err := r.db.Conn().Exec(`
		DELETE FROM forum_topics
		WHERE group_id = ? AND message_thread_id = ? AND lifecycle_owned = 1`,
		groupID, threadID,
	)
	if err != nil {
		return fmt.Errorf("delete owned forum topic: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("owned forum topic delete rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}
