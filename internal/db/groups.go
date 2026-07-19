package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

// GroupRepository provides CRUD operations for Telegram groups.
type GroupRepository struct {
	db           *DB
	assignmentMu sync.Mutex
}

// Insert adds a new group and creates a default group_settings row.
func (r *GroupRepository) Insert(g *model.Group) (int64, error) {
	conn := r.db.Conn()

	result, err := conn.Exec(
		`INSERT INTO groups (telegram_chat_id, title, status)
		 VALUES (?, ?, ?)`,
		g.TelegramChatID, g.Title, normalizedGroupStatus(g.Status),
	)
	if err != nil {
		return 0, fmt.Errorf("insert group: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}

	// Create default settings row
	_, err = conn.Exec(
		`INSERT OR IGNORE INTO group_settings (group_id) VALUES (?)`,
		id,
	)
	if err != nil {
		return id, fmt.Errorf("insert group settings: %w", err)
	}

	return id, nil
}

// GetByID returns a group by its ID.
func (r *GroupRepository) GetByID(id int64) (*model.Group, error) {
	g := &model.Group{}
	err := r.db.Conn().QueryRow(
		`SELECT id, version, telegram_chat_id, title, status, created_at
		 FROM groups WHERE id = ?`, id,
	).Scan(&g.ID, &g.Version, &g.TelegramChatID, &g.Title, &g.Status, &g.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get group by id: %w", err)
	}
	return g, nil
}

// GetByChatID returns a group by its Telegram chat ID.
func (r *GroupRepository) GetByChatID(chatID int64) (*model.Group, error) {
	g := &model.Group{}
	err := r.db.Conn().QueryRow(
		`SELECT id, version, telegram_chat_id, title, status, created_at
		 FROM groups WHERE telegram_chat_id = ?`, chatID,
	).Scan(&g.ID, &g.Version, &g.TelegramChatID, &g.Title, &g.Status, &g.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get group by chat id: %w", err)
	}
	return g, nil
}

// List returns all groups.
func (r *GroupRepository) List() ([]model.Group, error) {
	rows, err := r.db.Conn().Query(
		`SELECT id, version, telegram_chat_id, title, status, created_at
		 FROM groups ORDER BY title ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer rows.Close()

	var groups []model.Group
	for rows.Next() {
		var g model.Group
		if err := rows.Scan(&g.ID, &g.Version, &g.TelegramChatID, &g.Title, &g.Status, &g.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan group: %w", err)
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// Update modifies an existing group's title.
func (r *GroupRepository) Update(g *model.Group) error {
	_, err := r.db.Conn().Exec(
		`UPDATE groups SET title = ?, status = ?, version = version + 1 WHERE id = ?`,
		g.Title, normalizedGroupStatus(g.Status), g.ID,
	)
	if err != nil {
		return fmt.Errorf("update group: %w", err)
	}
	return nil
}

// SetStatus changes a group's lifecycle status while preserving its settings
// and channel assignments.
func (r *GroupRepository) SetStatus(id int64, status string) error {
	status = normalizedGroupStatus(status)
	result, err := r.db.Conn().Exec(`UPDATE groups SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("set group status: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("group status rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a group by ID. Cascade removes group_channels, group_settings.
func (r *GroupRepository) Delete(id int64) error {
	result, err := r.db.Conn().Exec(`DELETE FROM groups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete group rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteOptimistic removes a group only when the supplied positive version is
// still current. The caller is responsible for coordinating external runtime
// state, such as the live scheduler, before invoking this durable mutation.
func (r *GroupRepository) DeleteOptimistic(id, version int64) error {
	if version <= 0 {
		return ErrConflict
	}
	result, err := r.db.Conn().Exec(
		`DELETE FROM groups WHERE id = ? AND version = ?`,
		id, version,
	)
	if err != nil {
		return fmt.Errorf("delete group optimistically: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete group rows affected: %w", err)
	}
	if affected == 0 {
		if _, err := r.GetByID(id); errors.Is(err, ErrNotFound) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("inspect group after optimistic delete: %w", err)
		}
		return ErrConflict
	}
	return nil
}

const groupSchedulerSyncKeyPrefix = "webapp_group_scheduler_sync:"

// GroupSchedulerSyncKey returns the durable reconciliation key for a group.
func GroupSchedulerSyncKey(groupID int64) string {
	return fmt.Sprintf("%s%d", groupSchedulerSyncKeyPrefix, groupID)
}

// RecordSchedulerSync records that an active group's scheduler registration
// must be retried after a runtime failure.
func (r *GroupRepository) RecordSchedulerSync(groupID int64) error {
	if groupID <= 0 {
		return errors.New("record scheduler sync: group ID must be positive")
	}
	if err := r.db.Config.Set(GroupSchedulerSyncKey(groupID), "register"); err != nil {
		return fmt.Errorf("record group scheduler sync: %w", err)
	}
	return nil
}

// RecordSchedulerRemoval records that a deleted group's scheduler job must
// be removed after a late persistence failure.
func (r *GroupRepository) RecordSchedulerRemoval(groupID int64) error {
	if groupID <= 0 {
		return errors.New("record scheduler removal: group ID must be positive")
	}
	if err := r.db.Config.Set(GroupSchedulerSyncKey(groupID), "remove"); err != nil {
		return fmt.Errorf("record group scheduler removal: %w", err)
	}
	return nil
}

// SchedulerSyncKind returns the desired reconciliation action for a group.
func (r *GroupRepository) SchedulerSyncKind(groupID int64) (string, error) {
	if groupID <= 0 {
		return "", errors.New("get scheduler sync: group ID must be positive")
	}
	kind, err := r.db.Config.Get(GroupSchedulerSyncKey(groupID))
	if err != nil {
		return "", fmt.Errorf("get group scheduler sync: %w", err)
	}
	if kind != "register" && kind != "remove" {
		return "", fmt.Errorf("get group scheduler sync: invalid action %q", kind)
	}
	return kind, nil
}

// ClearSchedulerSync removes a converged group scheduler reconciliation entry.
func (r *GroupRepository) ClearSchedulerSync(groupID int64) error {
	if groupID <= 0 {
		return errors.New("clear scheduler sync: group ID must be positive")
	}
	if err := r.db.Config.Delete(GroupSchedulerSyncKey(groupID)); err != nil {
		return fmt.Errorf("clear group scheduler sync: %w", err)
	}
	return nil
}

// ListPendingSchedulerSync returns group IDs whose live scheduler
// registration still needs reconciliation.
func (r *GroupRepository) ListPendingSchedulerSync() ([]int64, error) {
	entries, err := r.db.Config.GetAll()
	if err != nil {
		return nil, fmt.Errorf("list group scheduler sync: %w", err)
	}
	pending := make([]int64, 0)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Key, groupSchedulerSyncKeyPrefix) {
			continue
		}
		groupID, err := strconv.ParseInt(strings.TrimPrefix(entry.Key, groupSchedulerSyncKeyPrefix), 10, 64)
		if err != nil || groupID <= 0 {
			return nil, fmt.Errorf("list group scheduler sync: invalid key %q", entry.Key)
		}
		pending = append(pending, groupID)
	}
	return pending, nil
}

// AssignChannel links a channel to a group with optional topic.
func (r *GroupRepository) AssignChannel(groupID, channelID int64, topicThreadID *int64) error {
	var threadID interface{}
	if topicThreadID != nil {
		threadID = *topicThreadID
	}
	result, err := r.db.Conn().Exec(
		`INSERT OR IGNORE INTO group_channels (group_id, channel_id, topic_thread_id)
		 SELECT ?, ?, ?
		 WHERE ? IS NULL OR NOT EXISTS (
			SELECT 1 FROM forum_topics
			WHERE group_id = ? AND message_thread_id = ?
				AND (closed = 1 OR close_pending = 1)
			UNION ALL
			SELECT 1 FROM forum_topic_tombstones
			WHERE group_id = ? AND message_thread_id = ?
		 )`,
		groupID, channelID, threadID, threadID, groupID, threadID, groupID, threadID,
	)
	if err != nil {
		return fmt.Errorf("assign channel: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("assign channel rows affected: %w", err)
	}
	if affected == 0 {
		var exists int
		if err := r.db.Conn().QueryRow(`
			SELECT EXISTS(
				SELECT 1 FROM group_channels
				WHERE group_id = ? AND channel_id = ?
			)`, groupID, channelID).Scan(&exists); err != nil {
			return fmt.Errorf("check existing channel assignment: %w", err)
		}
		if exists == 1 {
			return ErrDuplicate
		}
		return ErrConflict
	}
	return nil
}

// AssignChannelOptimistic links a channel to a group and advances the group's
// aggregate version in the same transaction.
func (r *GroupRepository) AssignChannelOptimistic(groupID, channelID int64, topicThreadID *int64, expectedVersion int64) (int64, error) {
	if expectedVersion <= 0 {
		return 0, ErrConflict
	}
	r.assignmentMu.Lock()
	defer r.assignmentMu.Unlock()
	current, err := r.GetByID(groupID)
	if errors.Is(err, ErrNotFound) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("load group for optimistic channel assignment: %w", err)
	}
	if current.Version != expectedVersion {
		return 0, ErrConflict
	}
	tx, err := r.db.Conn().Begin()
	if err != nil {
		return 0, fmt.Errorf("begin optimistic channel assignment: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(
		`UPDATE groups SET version = version + 1 WHERE id = ? AND version = ?`,
		groupID, expectedVersion,
	)
	if err != nil {
		return 0, fmt.Errorf("advance group assignment version: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("group assignment version rows affected: %w", err)
	}
	if affected == 0 {
		var currentVersion int64
		err := tx.QueryRow(`SELECT version FROM groups WHERE id = ?`, groupID).Scan(&currentVersion)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		if err != nil {
			return 0, fmt.Errorf("inspect group assignment version: %w", err)
		}
		return 0, fmt.Errorf("expected group assignment version %d, current version %d: %w",
			expectedVersion, currentVersion, ErrConflict)
	}

	var threadID interface{}
	if topicThreadID != nil {
		threadID = *topicThreadID
	}
	result, err = tx.Exec(
		`INSERT OR IGNORE INTO group_channels (group_id, channel_id, topic_thread_id)
		 SELECT ?, ?, ?
		 WHERE ? IS NULL OR NOT EXISTS (
			SELECT 1 FROM forum_topics
			WHERE group_id = ? AND message_thread_id = ?
				AND (closed = 1 OR close_pending = 1)
			UNION ALL
			SELECT 1 FROM forum_topic_tombstones
			WHERE group_id = ? AND message_thread_id = ?
		 )`,
		groupID, channelID, threadID, threadID, groupID, threadID, groupID, threadID,
	)
	if err != nil {
		return 0, fmt.Errorf("optimistic channel assignment: %w", err)
	}
	affected, err = result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("optimistic channel assignment rows affected: %w", err)
	}
	if affected == 0 {
		var exists int
		if err := tx.QueryRow(`
			SELECT EXISTS(
				SELECT 1 FROM group_channels
				WHERE group_id = ? AND channel_id = ?
			)`, groupID, channelID).Scan(&exists); err != nil {
			return 0, fmt.Errorf("check optimistic channel assignment: %w", err)
		}
		if exists == 1 {
			return 0, ErrDuplicate
		}
		return 0, ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit optimistic channel assignment: %w", err)
	}
	return expectedVersion + 1, nil
}

// FinalizeCreatedTopicAssignment commits a newly created Telegram topic,
// its lifecycle-owned registry row, the channel assignment, and the group
// aggregate version in one serialized SQLite transaction.
func (r *GroupRepository) FinalizeCreatedTopicAssignment(
	groupID, channelID, threadID, expectedVersion int64, name string,
) (int64, error) {
	return r.FinalizeCreatedTopicAssignmentWithIntent(groupID, channelID, threadID, expectedVersion, name, 0)
}

// FinalizeCreatedTopicAssignmentWithIntent atomically commits the registry,
// assignment, aggregate version, and the pre-create intent cleanup.
func (r *GroupRepository) FinalizeCreatedTopicAssignmentWithIntent(
	groupID, channelID, threadID, expectedVersion int64, name string, intentID int64,
) (int64, error) {
	if groupID <= 0 || channelID <= 0 || threadID <= 0 || expectedVersion <= 0 {
		return 0, ErrConflict
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("created topic name is required")
	}
	r.assignmentMu.Lock()
	defer r.assignmentMu.Unlock()

	tx, err := r.db.Conn().Begin()
	if err != nil {
		return 0, fmt.Errorf("begin created topic assignment: %w", err)
	}
	defer tx.Rollback()

	var currentVersion int64
	if err := tx.QueryRow(`SELECT version FROM groups WHERE id = ?`, groupID).Scan(&currentVersion); errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	} else if err != nil {
		return 0, fmt.Errorf("load created topic group version: %w", err)
	}
	if currentVersion != expectedVersion {
		return 0, ErrConflict
	}
	var tombstone int
	if err := tx.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM forum_topic_tombstones
			WHERE group_id = ? AND message_thread_id = ?
		)`, groupID, threadID).Scan(&tombstone); err != nil {
		return 0, fmt.Errorf("inspect created topic tombstone: %w", err)
	}
	if tombstone == 1 {
		return 0, ErrConflict
	}

	if _, err := tx.Exec(`
		INSERT INTO forum_topics
			(group_id, message_thread_id, name, status, lifecycle_owned, closed, close_pending)
		VALUES (?, ?, ?, 'persisted', 1, 0, 0)`,
		groupID, threadID, name); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return 0, ErrConflict
		}
		return 0, fmt.Errorf("persist created topic registry: %w", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO group_channels (group_id, channel_id, topic_thread_id)
		VALUES (?, ?, ?)`, groupID, channelID, threadID); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return 0, ErrDuplicate
		}
		return 0, fmt.Errorf("persist created topic assignment: %w", err)
	}
	if intentID > 0 {
		if _, err := tx.Exec(`
			DELETE FROM forum_topic_creation_intents
			WHERE id = ? AND group_id = ? AND channel_id = ?
				AND message_thread_id = ?`,
			intentID, groupID, channelID, threadID); err != nil {
			return 0, fmt.Errorf("complete topic creation intent: %w", err)
		}
	}
	result, err := tx.Exec(`
		UPDATE groups SET version = version + 1
		WHERE id = ? AND version = ?`, groupID, expectedVersion)
	if err != nil {
		return 0, fmt.Errorf("advance created topic group version: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("created topic group version rows affected: %w", err)
	}
	if affected == 0 {
		return 0, ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit created topic assignment: %w", err)
	}
	return expectedVersion + 1, nil
}

// UpdateChannelTopic stores the Telegram message thread ID for an assignment.
func (r *GroupRepository) UpdateChannelTopic(groupID, channelID int64, topicThreadID int64) error {
	result, err := r.db.Conn().Exec(
		`UPDATE group_channels SET topic_thread_id = ? WHERE group_id = ? AND channel_id = ?`,
		topicThreadID, groupID, channelID,
	)
	if err != nil {
		return fmt.Errorf("update channel topic: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("channel topic rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// UnassignChannel removes a channel from a group.
func (r *GroupRepository) UnassignChannel(groupID, channelID int64) error {
	result, err := r.db.Conn().Exec(
		`DELETE FROM group_channels WHERE group_id = ? AND channel_id = ?`,
		groupID, channelID,
	)
	if err != nil {
		return fmt.Errorf("unassign channel: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("unassign channel rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// UnassignChannelOptimistic removes a channel assignment and advances the
// group's aggregate version in the same transaction.
func (r *GroupRepository) UnassignChannelOptimistic(groupID, channelID, expectedVersion int64) (int64, error) {
	if expectedVersion <= 0 {
		return 0, ErrConflict
	}
	r.assignmentMu.Lock()
	defer r.assignmentMu.Unlock()
	current, err := r.GetByID(groupID)
	if errors.Is(err, ErrNotFound) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("load group for optimistic channel unassignment: %w", err)
	}
	if current.Version != expectedVersion {
		return 0, ErrConflict
	}
	tx, err := r.db.Conn().Begin()
	if err != nil {
		return 0, fmt.Errorf("begin optimistic channel unassignment: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(
		`UPDATE groups SET version = version + 1 WHERE id = ? AND version = ?`,
		groupID, expectedVersion,
	)
	if err != nil {
		return 0, fmt.Errorf("advance group unassignment version: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("group unassignment version rows affected: %w", err)
	}
	if affected == 0 {
		var currentVersion int64
		err := tx.QueryRow(`SELECT version FROM groups WHERE id = ?`, groupID).Scan(&currentVersion)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		if err != nil {
			return 0, fmt.Errorf("inspect group unassignment version: %w", err)
		}
		return 0, fmt.Errorf("expected group unassignment version %d, current version %d: %w",
			expectedVersion, currentVersion, ErrConflict)
	}
	result, err = tx.Exec(
		`DELETE FROM group_channels WHERE group_id = ? AND channel_id = ?`,
		groupID, channelID,
	)
	if err != nil {
		return 0, fmt.Errorf("optimistic channel unassignment: %w", err)
	}
	affected, err = result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("optimistic channel unassignment rows affected: %w", err)
	}
	if affected == 0 {
		return 0, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit optimistic channel unassignment: %w", err)
	}
	return expectedVersion + 1, nil
}

// RollbackAssignmentOptimistic removes a provisional assignment and restores
// the prior group version after a failed topic-creation side effect.
func (r *GroupRepository) RollbackAssignmentOptimistic(groupID, channelID, assignedVersion int64) error {
	if assignedVersion <= 1 {
		return ErrConflict
	}
	r.assignmentMu.Lock()
	defer r.assignmentMu.Unlock()
	tx, err := r.db.Conn().Begin()
	if err != nil {
		return fmt.Errorf("begin optimistic assignment rollback: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.Exec(
		`DELETE FROM group_channels WHERE group_id = ? AND channel_id = ?`,
		groupID, channelID,
	)
	if err != nil {
		return fmt.Errorf("rollback channel assignment: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rollback assignment rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	result, err = tx.Exec(
		`UPDATE groups SET version = version - 1 WHERE id = ? AND version = ?`,
		groupID, assignedVersion,
	)
	if err != nil {
		return fmt.Errorf("restore group assignment version: %w", err)
	}
	affected, err = result.RowsAffected()
	if err != nil {
		return fmt.Errorf("restore group assignment version rows affected: %w", err)
	}
	if affected == 0 {
		return ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit optimistic assignment rollback: %w", err)
	}
	return nil
}

// GetChannelAssignments returns all channel assignments for a group.
func (r *GroupRepository) GetChannelAssignments(groupID int64) ([]model.GroupChannel, error) {
	rows, err := r.db.Conn().Query(
		`SELECT group_id, channel_id, topic_thread_id
		 FROM group_channels WHERE group_id = ?
		 ORDER BY channel_id`,
		groupID,
	)
	if err != nil {
		return nil, fmt.Errorf("get channel assignments: %w", err)
	}
	defer rows.Close()

	var assignments []model.GroupChannel
	for rows.Next() {
		var gc model.GroupChannel
		var threadID sql.NullInt64
		if err := rows.Scan(&gc.GroupID, &gc.ChannelID, &threadID); err != nil {
			return nil, fmt.Errorf("scan group channel: %w", err)
		}
		if threadID.Valid {
			gc.TopicThreadID = &threadID.Int64
		}
		assignments = append(assignments, gc)
	}
	return assignments, rows.Err()
}

// ListForumTopics returns durable, open topics observed for a group. The
// registry is deliberately separate from channel assignments.
func (r *GroupRepository) ListForumTopics(groupID int64) ([]model.ForumTopic, error) {
	if r == nil || r.db == nil || r.db.ForumTopics == nil {
		return nil, errors.New("forum topic repository is not configured")
	}
	return r.db.ForumTopics.ListOpen(groupID)
}

// GetForumTopic returns the durable registry state, including pending and
// closed topics, for consistency checks at presentation boundaries.
func (r *GroupRepository) GetForumTopic(groupID, threadID int64) (*model.ForumTopic, error) {
	if r == nil || r.db == nil || r.db.ForumTopics == nil {
		return nil, errors.New("forum topic repository is not configured")
	}
	return r.db.ForumTopics.Get(groupID, threadID)
}

// ListPendingTopicCreationRecoveries returns created topics whose durable
// assignment lifecycle still needs external cleanup.
func (r *GroupRepository) ListPendingTopicCreationRecoveries() ([]model.ForumTopicCreationRecovery, error) {
	if r == nil || r.db == nil || r.db.ForumTopics == nil {
		return nil, errors.New("forum topic repository is not configured")
	}
	return r.db.ForumTopics.ListCreationRecoveries()
}

// RecordTopicCreationRecovery stores a retryable cleanup intent for an
// externally created topic that has no committed assignment.
func (r *GroupRepository) RecordTopicCreationRecovery(groupID, threadID, chatID int64, name string) error {
	if r == nil || r.db == nil || r.db.ForumTopics == nil {
		return errors.New("forum topic repository is not configured")
	}
	return r.db.ForumTopics.RecordCreationRecovery(groupID, threadID, chatID, name)
}

// DeleteTopicCreationRecovery clears a converged topic cleanup intent.
func (r *GroupRepository) DeleteTopicCreationRecovery(groupID, threadID int64) error {
	if r == nil || r.db == nil || r.db.ForumTopics == nil {
		return errors.New("forum topic repository is not configured")
	}
	return r.db.ForumTopics.DeleteCreationRecovery(groupID, threadID)
}

// BeginTopicCreationIntent commits a pre-create intent before any Telegram
// topic is created.
func (r *GroupRepository) BeginTopicCreationIntent(groupID, channelID, chatID, expectedVersion int64, name string) (int64, error) {
	if r == nil || r.db == nil || r.db.ForumTopics == nil {
		return 0, errors.New("forum topic repository is not configured")
	}
	return r.db.ForumTopics.BeginCreationIntent(groupID, channelID, chatID, expectedVersion, name)
}

// JournalTopicCreationIntent records Telegram's returned thread ID.
func (r *GroupRepository) JournalTopicCreationIntent(intentID, threadID int64) error {
	if r == nil || r.db == nil || r.db.ForumTopics == nil {
		return errors.New("forum topic repository is not configured")
	}
	return r.db.ForumTopics.JournalCreationIntent(intentID, threadID)
}

// CompleteTopicCreationIntent clears a converged pre-create intent.
func (r *GroupRepository) CompleteTopicCreationIntent(intentID int64) error {
	if r == nil || r.db == nil || r.db.ForumTopics == nil {
		return errors.New("forum topic repository is not configured")
	}
	return r.db.ForumTopics.CompleteCreationIntent(intentID)
}

// PersistClosedTopicTombstone keeps successful compensation visible only as
// closed lifecycle-owned history.
func (r *GroupRepository) PersistClosedTopicTombstone(groupID, threadID int64, name string) error {
	if r == nil || r.db == nil || r.db.ForumTopics == nil {
		return errors.New("forum topic repository is not configured")
	}
	return r.db.ForumTopics.PersistClosedTombstone(groupID, threadID, name)
}

// PersistClosedTopicTombstoneByIdentity keeps compensation durable when the
// group foreign row has already been removed.
func (r *GroupRepository) PersistClosedTopicTombstoneByIdentity(groupID, threadID, chatID int64, name string) error {
	if r == nil || r.db == nil || r.db.ForumTopics == nil {
		return errors.New("forum topic repository is not configured")
	}
	return r.db.ForumTopics.PersistClosedTombstoneByIdentity(groupID, threadID, chatID, name)
}

// GetChannelsForGroup returns full channel objects assigned to a group.
func (r *GroupRepository) GetChannelsForGroup(groupID int64) ([]model.Channel, error) {
	rows, err := r.db.Conn().Query(
		`SELECT c.id, c.version, c.username, c.title, c.enabled, c.last_post_id, c.fetch_error_kind, c.fetch_error_message, c.fetch_error_at, c.created_at
		 FROM channels c
		 INNER JOIN group_channels gc ON c.id = gc.channel_id
		 WHERE gc.group_id = ? AND c.enabled = 1
		 ORDER BY c.username ASC`,
		groupID,
	)
	if err != nil {
		return nil, fmt.Errorf("get channels for group: %w", err)
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
		if fetchErrorAt.Valid {
			ch.FetchErrorAt = &fetchErrorAt.String
		}
		channels = append(channels, ch)
	}
	return channels, rows.Err()
}

// CountChannelsForGroup returns the number of channels assigned to a group.
func (r *GroupRepository) CountChannelsForGroup(groupID int64) (int, error) {
	var count int
	err := r.db.Conn().QueryRow(
		`SELECT COUNT(*) FROM group_channels WHERE group_id = ?`,
		groupID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count channels for group: %w", err)
	}
	return count, nil
}

// GetGroupSettings returns the settings for a group.
func (r *GroupRepository) GetGroupSettings(groupID int64) (*model.GroupSettings, error) {
	gs := &model.GroupSettings{GroupID: groupID}
	var providerID sql.NullInt64
	var model sql.NullString
	err := r.db.Conn().QueryRow(
		`SELECT group_id, provider_id, model, digest_time, timezone
		 FROM group_settings WHERE group_id = ?`, groupID,
	).Scan(&gs.GroupID, &providerID, &model, &gs.DigestTime, &gs.Timezone)
	if err == sql.ErrNoRows {
		// Return defaults if no settings row exists
		gs.DigestTime = "21:00"
		gs.Timezone = "Europe/Moscow"
		return gs, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get group settings: %w", err)
	}
	if providerID.Valid {
		gs.ProviderID = &providerID.Int64
	}
	if model.Valid {
		gs.Model = &model.String
	}
	return gs, nil
}

// ResolveAIConfig returns the effective provider and model for a group.
// Groups without an explicit provider use the provider marked as default.
// A non-empty group model overrides the provider's default model.
func (r *GroupRepository) ResolveAIConfig(groupID int64) (*model.GroupAIConfig, error) {
	settings, err := r.GetGroupSettings(groupID)
	if err != nil {
		return nil, fmt.Errorf("resolve group AI config: load settings: %w", err)
	}

	var provider *model.AIProvider
	if settings.ProviderID != nil {
		provider, err = r.db.Providers.GetByID(*settings.ProviderID)
		if err != nil {
			return nil, fmt.Errorf("resolve group AI config: load assigned provider: %w", err)
		}
	} else {
		provider, err = r.db.Providers.GetDefault()
		if err != nil {
			return nil, fmt.Errorf("resolve group AI config: load default provider: %w", err)
		}
	}

	effectiveModel := provider.DefaultModel
	if settings.Model != nil && strings.TrimSpace(*settings.Model) != "" {
		effectiveModel = strings.TrimSpace(*settings.Model)
	}
	if strings.TrimSpace(effectiveModel) == "" {
		return nil, fmt.Errorf("resolve group AI config: provider %d has no model configured", provider.ID)
	}

	return &model.GroupAIConfig{
		Provider: *provider,
		Model:    effectiveModel,
	}, nil
}

// GetDefaultProvider returns the configured default AI provider. It is kept
// separate from ResolveAIConfig so runtime callers can construct a fallback
// without changing the group's primary provider selection.
func (r *GroupRepository) GetDefaultProvider() (*model.AIProvider, error) {
	return r.db.Providers.GetDefault()
}

// GetOpenRouterProvider returns the persisted OpenRouter provider regardless
// of which provider is currently selected as the group's default.
func (r *GroupRepository) GetOpenRouterProvider() (*model.AIProvider, error) {
	return r.db.Providers.GetByName("OpenRouter")
}

// UpdateGroupSettings updates the settings for a group.
func (r *GroupRepository) UpdateGroupSettings(gs *model.GroupSettings) error {
	var providerID, modelVal interface{}
	if gs.ProviderID != nil {
		providerID = *gs.ProviderID
	}
	if gs.Model != nil {
		modelVal = *gs.Model
	}
	_, err := r.db.Conn().Exec(
		`INSERT INTO group_settings (group_id, provider_id, model, digest_time, timezone)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(group_id) DO UPDATE SET
		   provider_id = excluded.provider_id,
		   model = excluded.model,
		   digest_time = excluded.digest_time,
		   timezone = excluded.timezone`,
		gs.GroupID, providerID, modelVal, gs.DigestTime, gs.Timezone,
	)
	if err != nil {
		return fmt.Errorf("update group settings: %w", err)
	}
	return nil
}

func normalizedGroupStatus(status string) string {
	switch status {
	case model.GroupStatusInactive, model.GroupStatusIneligible:
		return status
	default:
		return model.GroupStatusActive
	}
}
