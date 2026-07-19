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

// BeginCreationIntent records the durable pre-create intent before Telegram
// is called. The row has no group foreign key so it survives group deletion.
func (r *ForumTopicRepository) BeginCreationIntent(groupID, channelID, chatID, expectedVersion int64, name string) (int64, error) {
	if r == nil || r.db == nil {
		return 0, errors.New("forum topic repository is not configured")
	}
	if groupID <= 0 || channelID <= 0 || chatID == 0 || expectedVersion <= 0 {
		return 0, errors.New("forum topic creation intent identifiers are invalid")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("forum topic creation intent name is required")
	}
	if _, err := r.db.Conn().Exec(`
		UPDATE forum_topic_creation_intents
		SET state = 'unknown_outcome', updated_at = datetime('now')
		WHERE group_id = ? AND channel_id = ?
			AND state = 'creating' AND message_thread_id = 0`,
		groupID, channelID); err != nil {
		return 0, fmt.Errorf("normalize unresolved forum topic creation: %w", err)
	}
	var unknown int
	if err := r.db.Conn().QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM forum_topic_creation_intents
			WHERE group_id = ? AND channel_id = ? AND state = 'unknown_outcome'
		)`, groupID, channelID).Scan(&unknown); err != nil {
		return 0, fmt.Errorf("inspect unknown forum topic creation outcome: %w", err)
	}
	if unknown == 1 {
		return 0, ErrConflict
	}
	_, err := r.db.Conn().Exec(`
		INSERT INTO forum_topic_creation_intents
			(group_id, channel_id, chat_id, expected_version, name, state)
		VALUES (?, ?, ?, ?, ?, 'creating')
		ON CONFLICT(group_id, channel_id, state) DO UPDATE SET
			chat_id = excluded.chat_id,
			expected_version = excluded.expected_version,
			name = excluded.name,
			updated_at = datetime('now')`,
		groupID, channelID, chatID, expectedVersion, name)
	if err != nil {
		return 0, fmt.Errorf("begin forum topic creation intent: %w", err)
	}
	var existing int64
	if err := r.db.Conn().QueryRow(`
		SELECT id FROM forum_topic_creation_intents
		WHERE group_id = ? AND channel_id = ? AND state = 'creating'`,
		groupID, channelID).Scan(&existing); err != nil {
		return 0, fmt.Errorf("load forum topic creation intent: %w", err)
	}
	return existing, nil
}

// JournalCreationIntent writes the real Telegram thread ID before finalization.
func (r *ForumTopicRepository) JournalCreationIntent(intentID, threadID int64) error {
	if r == nil || r.db == nil {
		return errors.New("forum topic repository is not configured")
	}
	if intentID <= 0 || threadID <= 0 {
		return errors.New("forum topic creation journal identifiers are invalid")
	}
	result, err := r.db.Conn().Exec(`
		UPDATE forum_topic_creation_intents
		SET message_thread_id = ?, state = 'pending_cleanup', updated_at = datetime('now')
		WHERE id = ? AND state = 'creating'`,
		threadID, intentID)
	if err != nil {
		if unknownErr := r.markUnknownOutcome(intentID); unknownErr != nil {
			return fmt.Errorf("journal forum topic creation: %w; mark unknown outcome: %v", err, unknownErr)
		}
		return fmt.Errorf("journal forum topic creation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("forum topic creation journal rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkUnknownOutcome preserves a pre-create intent when Telegram may have
// created a topic but its positive thread ID was not durably observed.
func (r *ForumTopicRepository) MarkUnknownOutcome(intentID int64) error {
	if r == nil || r.db == nil {
		return errors.New("forum topic repository is not configured")
	}
	if intentID <= 0 {
		return errors.New("forum topic creation intent identifier is invalid")
	}
	return r.markUnknownOutcome(intentID)
}

func (r *ForumTopicRepository) markUnknownOutcome(intentID int64) error {
	result, err := r.db.Conn().Exec(`
		UPDATE forum_topic_creation_intents
		SET message_thread_id = 0, state = 'unknown_outcome', updated_at = datetime('now')
		WHERE id = ? AND state IN ('creating', 'pending_cleanup')`,
		intentID)
	if err != nil {
		return fmt.Errorf("mark forum topic creation unknown outcome: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("unknown forum topic creation rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ResolveUnknownCreationObservation binds a uniquely matching Telegram
// observation to an unknown create intent. Ambiguous matches remain visible
// for operator cleanup and are never guessed.
func (r *ForumTopicRepository) ResolveUnknownCreationObservation(
	chatID, threadID int64, name string,
) (bound, ambiguous bool, err error) {
	if r == nil || r.db == nil {
		return false, false, errors.New("forum topic repository is not configured")
	}
	if chatID == 0 || threadID <= 0 || strings.TrimSpace(name) == "" {
		return false, false, errors.New("unknown forum topic observation identifiers are invalid")
	}
	var count int
	if err := r.db.Conn().QueryRow(`
		SELECT COUNT(*)
		FROM forum_topic_creation_intents
		WHERE chat_id = ? AND message_thread_id = 0
			AND state = 'unknown_outcome'
			AND lower(trim(name)) = lower(trim(?))`,
		chatID, name).Scan(&count); err != nil {
		return false, false, fmt.Errorf("count unknown forum topic outcomes: %w", err)
	}
	if count == 0 {
		return false, false, nil
	}
	if count != 1 {
		return false, true, nil
	}
	var tombstone int
	if err := r.db.Conn().QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM forum_topic_tombstones
			WHERE chat_id = ? AND message_thread_id = ?
		)`, chatID, threadID).Scan(&tombstone); err != nil {
		return false, false, fmt.Errorf("inspect unknown topic tombstone: %w", err)
	}
	if tombstone == 1 {
		return false, false, nil
	}
	result, err := r.db.Conn().Exec(`
		UPDATE forum_topic_creation_intents
		SET message_thread_id = ?, state = 'pending_cleanup', updated_at = datetime('now')
		WHERE chat_id = ? AND message_thread_id = 0
			AND state = 'unknown_outcome'
			AND lower(trim(name)) = lower(trim(?))`,
		threadID, chatID, name)
	if err != nil {
		return false, false, fmt.Errorf("bind unknown forum topic outcome: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, false, fmt.Errorf("bind unknown forum topic rows affected: %w", err)
	}
	return affected == 1, false, nil
}

// CompleteCreationIntent removes an intent after assignment finalization or
// successful external compensation.
func (r *ForumTopicRepository) CompleteCreationIntent(intentID int64) error {
	if r == nil || r.db == nil {
		return errors.New("forum topic repository is not configured")
	}
	result, err := r.db.Conn().Exec(`DELETE FROM forum_topic_creation_intents WHERE id = ?`, intentID)
	if err != nil {
		return fmt.Errorf("complete forum topic creation intent: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("forum topic creation intent delete rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// Observe records a topic seen in an incoming Telegram update.
func (r *ForumTopicRepository) Observe(groupID, threadID int64, name string) error {
	return r.upsert(groupID, threadID, name, false)
}

// PersistOwned records a topic created by this bot and marks it as lifecycle-owned.
func (r *ForumTopicRepository) PersistOwned(groupID, threadID int64, name string) error {
	return r.upsert(groupID, threadID, name, true)
}

// PersistClosedTombstone records a lifecycle-owned topic that has already
// been compensated. Future observation updates cannot reopen this tombstone.
func (r *ForumTopicRepository) PersistClosedTombstone(groupID, threadID int64, name string) error {
	if r == nil || r.db == nil {
		return errors.New("forum topic repository is not configured")
	}
	name = strings.TrimSpace(name)
	if groupID <= 0 || threadID <= 0 || name == "" {
		return errors.New("forum topic tombstone identifiers are invalid")
	}
	var chatID int64
	if err := r.db.Conn().QueryRow(`SELECT telegram_chat_id FROM groups WHERE id = ?`, groupID).Scan(&chatID); err != nil {
		return fmt.Errorf("load forum topic tombstone chat: %w", err)
	}
	return r.persistClosedTombstoneIdentity(groupID, threadID, chatID, name, true)
}

// PersistClosedTombstoneByIdentity preserves a compensation tombstone after
// the group row has been deleted.
func (r *ForumTopicRepository) PersistClosedTombstoneByIdentity(groupID, threadID, chatID int64, name string) error {
	if r == nil || r.db == nil {
		return errors.New("forum topic repository is not configured")
	}
	return r.persistClosedTombstoneIdentity(groupID, threadID, chatID, name, false)
}

func (r *ForumTopicRepository) persistClosedTombstoneIdentity(groupID, threadID, chatID int64, name string, mirrorRegistry bool) error {
	if groupID <= 0 || threadID <= 0 || chatID == 0 || strings.TrimSpace(name) == "" {
		return errors.New("forum topic tombstone identifiers are invalid")
	}
	if _, err := r.db.Conn().Exec(`
		INSERT INTO forum_topic_tombstones (chat_id, message_thread_id, group_id, name)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(chat_id, message_thread_id) DO UPDATE SET
			group_id = excluded.group_id, name = excluded.name`,
		chatID, threadID, groupID, name); err != nil {
		return fmt.Errorf("persist forum topic tombstone: %w", err)
	}
	if !mirrorRegistry {
		return nil
	}
	_, err := r.db.Conn().Exec(`
		INSERT INTO forum_topics
			(group_id, message_thread_id, name, status, lifecycle_owned, closed, close_pending, updated_at)
		VALUES (?, ?, ?, 'persisted', 1, 1, 0, datetime('now'))
		ON CONFLICT(group_id, message_thread_id) DO UPDATE SET
			name = excluded.name,
			status = 'persisted',
			lifecycle_owned = 1,
			closed = 1,
			close_pending = 0,
			updated_at = datetime('now')`,
		groupID, threadID, name)
	if err != nil {
		return fmt.Errorf("persist closed forum topic tombstone: %w", err)
	}
	return nil
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
	var chatID int64
	if err := r.db.Conn().QueryRow(
		`SELECT telegram_chat_id FROM groups WHERE id = ?`, groupID,
	).Scan(&chatID); err != nil {
		return fmt.Errorf("load forum topic chat identity: %w", err)
	}
	var tombstone int
	if err := r.db.Conn().QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM forum_topic_tombstones
			WHERE message_thread_id = ?
				AND (group_id = ? OR chat_id = ?)
		)`, threadID, groupID, chatID).Scan(&tombstone); err != nil {
		return fmt.Errorf("inspect forum topic tombstone: %w", err)
	}
	_, err := r.db.Conn().Exec(`
		INSERT INTO forum_topics
			(group_id, message_thread_id, name, status, lifecycle_owned, closed, close_pending, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, 0, datetime('now'))
		ON CONFLICT(group_id, message_thread_id) DO UPDATE SET
			name = excluded.name,
			status = CASE WHEN forum_topics.lifecycle_owned = 1 OR excluded.lifecycle_owned = 1
				THEN 'persisted' ELSE 'observed' END,
			lifecycle_owned = MAX(forum_topics.lifecycle_owned, excluded.lifecycle_owned),
			closed = CASE WHEN forum_topics.lifecycle_owned = 1 THEN forum_topics.closed ELSE 0 END,
			close_pending = CASE WHEN forum_topics.lifecycle_owned = 1 THEN forum_topics.close_pending ELSE 0 END,
			updated_at = datetime('now')`,
		groupID, threadID, name, status, boolToInt(owned),
	)
	if err != nil {
		return fmt.Errorf("upsert forum topic: %w", err)
	}
	if tombstone == 1 {
		if _, err := r.db.Conn().Exec(`
			UPDATE forum_topics
			SET name = COALESCE(
					(SELECT name FROM forum_topic_tombstones
					 WHERE message_thread_id = ?
						AND (group_id = ? OR chat_id = ?)
					 LIMIT 1),
					name),
				status = 'persisted', lifecycle_owned = 1,
				closed = 1, close_pending = 0, updated_at = datetime('now')
			WHERE group_id = ? AND message_thread_id = ?`,
			threadID, groupID, chatID, groupID, threadID); err != nil {
			return fmt.Errorf("reapply forum topic tombstone: %w", err)
		}
	}
	return nil
}

// Get returns a topic regardless of whether it is currently closed.
func (r *ForumTopicRepository) Get(groupID, threadID int64) (*model.ForumTopic, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("forum topic repository is not configured")
	}
	topic := &model.ForumTopic{}
	var owned, closed, pending int
	err := r.db.Conn().QueryRow(`
		SELECT group_id, message_thread_id, name, status, lifecycle_owned, closed, close_pending, created_at, updated_at
		FROM forum_topics
		WHERE group_id = ? AND message_thread_id = ?`,
		groupID, threadID,
	).Scan(&topic.GroupID, &topic.MessageThreadID, &topic.Name, &topic.Status,
		&owned, &closed, &pending, &topic.CreatedAt, &topic.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		var tombstone model.ForumTopic
		var chatID int64
		if chatErr := r.db.Conn().QueryRow(
			`SELECT telegram_chat_id FROM groups WHERE id = ?`, groupID,
		).Scan(&chatID); chatErr != nil {
			return nil, fmt.Errorf("load forum topic tombstone chat identity: %w", chatErr)
		}
		err = r.db.Conn().QueryRow(`
			SELECT group_id, message_thread_id, name, 'persisted', 1, 1, 0,
				created_at, created_at
			FROM forum_topic_tombstones
			WHERE message_thread_id = ?
				AND (group_id = ? OR chat_id = ?)
			ORDER BY CASE WHEN chat_id = ? THEN 0 ELSE 1 END
			LIMIT 1`,
			threadID, groupID, chatID, chatID).Scan(&tombstone.GroupID, &tombstone.MessageThreadID,
			&tombstone.Name, &tombstone.Status, &owned, &closed, &pending,
			&tombstone.CreatedAt, &tombstone.UpdatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("get forum topic tombstone: %w", err)
		}
		tombstone.GroupID = groupID
		tombstone.LifecycleOwned = true
		tombstone.Closed = true
		return &tombstone, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get forum topic: %w", err)
	}
	topic.LifecycleOwned = intToBool(owned)
	topic.Closed = intToBool(closed)
	topic.ClosePending = intToBool(pending)
	var chatID int64
	if err := r.db.Conn().QueryRow(
		`SELECT telegram_chat_id FROM groups WHERE id = ?`, groupID,
	).Scan(&chatID); err != nil {
		return nil, fmt.Errorf("load forum topic chat identity: %w", err)
	}
	var tombstone int
	if err := r.db.Conn().QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM forum_topic_tombstones
			WHERE message_thread_id = ?
				AND (group_id = ? OR chat_id = ?)
		)`, threadID, groupID, chatID).Scan(&tombstone); err != nil {
		return nil, fmt.Errorf("load forum topic tombstone: %w", err)
	}
	if tombstone == 1 {
		topic.LifecycleOwned = true
		topic.Closed = true
		topic.ClosePending = false
		topic.Status = model.ForumTopicStatusPersisted
	}
	return topic, nil
}

// ListOpen returns observed and persisted topics that can be selected.
func (r *ForumTopicRepository) ListOpen(groupID int64) ([]model.ForumTopic, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("forum topic repository is not configured")
	}
	var chatID int64
	if err := r.db.Conn().QueryRow(
		`SELECT telegram_chat_id FROM groups WHERE id = ?`, groupID,
	).Scan(&chatID); err != nil {
		return nil, fmt.Errorf("load forum topic list chat identity: %w", err)
	}
	rows, err := r.db.Conn().Query(`
		SELECT group_id, message_thread_id, name, status, lifecycle_owned, closed, created_at, updated_at
		FROM forum_topics
		WHERE group_id = ? AND closed = 0 AND close_pending = 0
			AND NOT EXISTS (
				SELECT 1 FROM forum_topic_tombstones t
				WHERE t.message_thread_id = forum_topics.message_thread_id
					AND (t.group_id = forum_topics.group_id OR t.chat_id = ?)
			)
		ORDER BY name COLLATE NOCASE, message_thread_id`,
		groupID, chatID,
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

// BeginClose durably records that an owned topic is being closed before the
// Telegram side effect. Pending topics are hidden from the WebApp catalog and
// can be retried after a process restart. The assignment guard is part of the
// same SQLite write so a surviving group_channels row rejects the intent.
func (r *ForumTopicRepository) BeginClose(groupID, threadID int64) error {
	if r == nil || r.db == nil {
		return errors.New("forum topic repository is not configured")
	}
	result, err := r.db.Conn().Exec(`
		UPDATE forum_topics
		SET close_pending = 1, updated_at = datetime('now')
		WHERE group_id = ? AND message_thread_id = ?
			AND lifecycle_owned = 1 AND closed = 0
			AND NOT EXISTS (
				SELECT 1 FROM group_channels
				WHERE group_id = ? AND topic_thread_id = ?
			)`,
		groupID, threadID, groupID, threadID,
	)
	if err != nil {
		return fmt.Errorf("begin forum topic close: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("begin forum topic close rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ListPending returns owned topic closes that need bounded retry/reconciliation.
func (r *ForumTopicRepository) ListPending() ([]model.ForumTopic, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("forum topic repository is not configured")
	}
	rows, err := r.db.Conn().Query(`
		SELECT group_id, message_thread_id, name, status, lifecycle_owned, closed, close_pending, created_at, updated_at
		FROM forum_topics
		WHERE close_pending = 1 AND closed = 0 AND lifecycle_owned = 1
		ORDER BY group_id, message_thread_id`)
	if err != nil {
		return nil, fmt.Errorf("list pending forum topics: %w", err)
	}
	defer rows.Close()
	var topics []model.ForumTopic
	for rows.Next() {
		var topic model.ForumTopic
		var owned, closed, pending int
		if err := rows.Scan(&topic.GroupID, &topic.MessageThreadID, &topic.Name, &topic.Status,
			&owned, &closed, &pending, &topic.CreatedAt, &topic.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan pending forum topic: %w", err)
		}
		topic.LifecycleOwned = intToBool(owned)
		topic.Closed = intToBool(closed)
		topic.ClosePending = intToBool(pending)
		topics = append(topics, topic)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending forum topics: %w", err)
	}
	return topics, nil
}

func (r *ForumTopicRepository) setClosed(groupID, threadID int64, closed bool) error {
	if r == nil || r.db == nil {
		return errors.New("forum topic repository is not configured")
	}
	if !closed {
		var chatID int64
		if err := r.db.Conn().QueryRow(
			`SELECT telegram_chat_id FROM groups WHERE id = ?`, groupID,
		).Scan(&chatID); err != nil {
			return fmt.Errorf("load forum topic reopen chat identity: %w", err)
		}
		var tombstone int
		if err := r.db.Conn().QueryRow(`
			SELECT EXISTS(
				SELECT 1 FROM forum_topic_tombstones
				WHERE message_thread_id = ?
					AND (group_id = ? OR chat_id = ?)
			)`, threadID, groupID, chatID).Scan(&tombstone); err != nil {
			return fmt.Errorf("inspect forum topic reopen tombstone: %w", err)
		}
		if tombstone == 1 {
			return ErrConflict
		}
	}
	result, err := r.db.Conn().Exec(`
		UPDATE forum_topics SET closed = ?, close_pending = 0, updated_at = datetime('now')
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

// RecordCreationRecovery durably records an externally created topic that
// could not be committed with its assignment and registry state.
func (r *ForumTopicRepository) RecordCreationRecovery(groupID, threadID, chatID int64, name string) error {
	if r == nil || r.db == nil {
		return errors.New("forum topic repository is not configured")
	}
	if groupID <= 0 || threadID <= 0 || chatID == 0 {
		return errors.New("forum topic creation recovery identifiers are invalid")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("forum topic creation recovery name is required")
	}
	_, err := r.db.Conn().Exec(`
		INSERT INTO forum_topic_creation_recovery
			(group_id, message_thread_id, chat_id, name)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(group_id, message_thread_id) DO UPDATE SET
			chat_id = excluded.chat_id,
			name = excluded.name`,
		groupID, threadID, chatID, name,
	)
	if err != nil {
		return fmt.Errorf("record forum topic creation recovery: %w", err)
	}
	if _, err := r.db.Conn().Exec(`
		DELETE FROM forum_topic_creation_intents
		WHERE group_id = ? AND message_thread_id = ?`,
		groupID, threadID); err != nil {
		return fmt.Errorf("complete forum topic creation intent after recovery: %w", err)
	}
	return nil
}

// RecordCreationRecoveryForIntent records known-positive cleanup while
// deleting the exact pre-create intent, including when journaling the thread
// ID itself failed.
func (r *ForumTopicRepository) RecordCreationRecoveryForIntent(
	intentID, groupID, threadID, chatID int64, name string,
) error {
	if r == nil || r.db == nil {
		return errors.New("forum topic repository is not configured")
	}
	if intentID <= 0 || groupID <= 0 || threadID <= 0 || chatID == 0 {
		return errors.New("forum topic creation recovery identifiers are invalid")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("forum topic creation recovery name is required")
	}
	tx, err := r.db.Conn().Begin()
	if err != nil {
		return fmt.Errorf("begin forum topic creation recovery: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		INSERT INTO forum_topic_creation_recovery
			(group_id, message_thread_id, chat_id, name)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(group_id, message_thread_id) DO UPDATE SET
			chat_id = excluded.chat_id, name = excluded.name`,
		groupID, threadID, chatID, name); err != nil {
		return fmt.Errorf("record forum topic creation recovery: %w", err)
	}
	result, err := tx.Exec(`
		DELETE FROM forum_topic_creation_intents
		WHERE id = ? AND group_id = ?`,
		intentID, groupID)
	if err != nil {
		return fmt.Errorf("complete forum topic creation intent after recovery: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("forum topic creation intent recovery rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit forum topic creation recovery: %w", err)
	}
	return nil
}

// ListCreationRecoveries returns durable topic cleanup records.
func (r *ForumTopicRepository) ListCreationRecoveries() ([]model.ForumTopicCreationRecovery, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("forum topic repository is not configured")
	}
	if _, err := r.db.Conn().Exec(`
		UPDATE forum_topic_creation_intents
		SET state = 'unknown_outcome', updated_at = datetime('now')
		WHERE state = 'creating' AND message_thread_id = 0`); err != nil {
		return nil, fmt.Errorf("normalize unresolved forum topic recoveries: %w", err)
	}
	rows, err := r.db.Conn().Query(`
		SELECT 0, group_id, 0, message_thread_id, chat_id, name, 0, 'recovery', created_at
		FROM forum_topic_creation_recovery
		UNION ALL
		SELECT id, group_id, channel_id, message_thread_id, chat_id, name,
			expected_version, state, created_at
		FROM forum_topic_creation_intents
		WHERE state IN ('creating', 'pending_cleanup', 'unknown_outcome')
		ORDER BY 2, 4`)
	if err != nil {
		return nil, fmt.Errorf("list forum topic creation recoveries: %w", err)
	}
	defer rows.Close()
	var recoveries []model.ForumTopicCreationRecovery
	for rows.Next() {
		var recovery model.ForumTopicCreationRecovery
		if err := rows.Scan(&recovery.IntentID, &recovery.GroupID, &recovery.ChannelID,
			&recovery.MessageThreadID, &recovery.ChatID, &recovery.Name,
			&recovery.ExpectedVersion, &recovery.State, &recovery.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan forum topic creation recovery: %w", err)
		}
		recoveries = append(recoveries, recovery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate forum topic creation recoveries: %w", err)
	}
	return recoveries, nil
}

// DeleteCreationRecovery removes a converged topic cleanup record.
func (r *ForumTopicRepository) DeleteCreationRecovery(groupID, threadID int64) error {
	if r == nil || r.db == nil {
		return errors.New("forum topic repository is not configured")
	}
	result, err := r.db.Conn().Exec(`
		DELETE FROM forum_topic_creation_recovery
		WHERE group_id = ? AND message_thread_id = ?`,
		groupID, threadID)
	if err != nil {
		return fmt.Errorf("delete forum topic creation recovery: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("forum topic creation recovery rows affected: %w", err)
	}
	intentResult, intentErr := r.db.Conn().Exec(`
		DELETE FROM forum_topic_creation_intents
		WHERE group_id = ? AND message_thread_id = ?`,
		groupID, threadID)
	if intentErr != nil {
		return fmt.Errorf("delete forum topic creation intent: %w", intentErr)
	}
	intentAffected, intentErr := intentResult.RowsAffected()
	if intentErr != nil {
		return fmt.Errorf("forum topic creation intent rows affected: %w", intentErr)
	}
	if affected == 0 && intentAffected == 0 {
		return ErrNotFound
	}
	return nil
}
