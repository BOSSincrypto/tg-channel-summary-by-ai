package db

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

// GroupRepository provides CRUD operations for Telegram groups.
type GroupRepository struct {
	db *DB
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
		`SELECT id, telegram_chat_id, title, status, created_at
		 FROM groups WHERE id = ?`, id,
	).Scan(&g.ID, &g.TelegramChatID, &g.Title, &g.Status, &g.CreatedAt)
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
		`SELECT id, telegram_chat_id, title, status, created_at
		 FROM groups WHERE telegram_chat_id = ?`, chatID,
	).Scan(&g.ID, &g.TelegramChatID, &g.Title, &g.Status, &g.CreatedAt)
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
		`SELECT id, telegram_chat_id, title, status, created_at
		 FROM groups ORDER BY title ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer rows.Close()

	var groups []model.Group
	for rows.Next() {
		var g model.Group
		if err := rows.Scan(&g.ID, &g.TelegramChatID, &g.Title, &g.Status, &g.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan group: %w", err)
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// Update modifies an existing group's title.
func (r *GroupRepository) Update(g *model.Group) error {
	_, err := r.db.Conn().Exec(
		`UPDATE groups SET title = ?, status = ? WHERE id = ?`,
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
	_, err := r.db.Conn().Exec(`DELETE FROM groups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	return nil
}

// AssignChannel links a channel to a group with optional topic.
func (r *GroupRepository) AssignChannel(groupID, channelID int64, topicThreadID *int64) error {
	var threadID interface{}
	if topicThreadID != nil {
		threadID = *topicThreadID
	}
	_, err := r.db.Conn().Exec(
		`INSERT OR IGNORE INTO group_channels (group_id, channel_id, topic_thread_id)
		 VALUES (?, ?, ?)`,
		groupID, channelID, threadID,
	)
	if err != nil {
		return fmt.Errorf("assign channel: %w", err)
	}
	return nil
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
	_, err := r.db.Conn().Exec(
		`DELETE FROM group_channels WHERE group_id = ? AND channel_id = ?`,
		groupID, channelID,
	)
	if err != nil {
		return fmt.Errorf("unassign channel: %w", err)
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

// GetChannelsForGroup returns full channel objects assigned to a group.
func (r *GroupRepository) GetChannelsForGroup(groupID int64) ([]model.Channel, error) {
	rows, err := r.db.Conn().Query(
		`SELECT c.id, c.username, c.title, c.enabled, c.last_post_id, c.fetch_error_kind, c.fetch_error_message, c.fetch_error_at, c.created_at
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
		if err := rows.Scan(&ch.ID, &ch.Username, &ch.Title, &enabled, &ch.LastPostID, &ch.FetchErrorKind, &ch.FetchErrorMessage, &fetchErrorAt, &ch.CreatedAt); err != nil {
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
