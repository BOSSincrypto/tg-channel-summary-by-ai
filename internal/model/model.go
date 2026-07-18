// Package model defines shared data structures used across the application.
// These types represent the core domain entities and are used by all
// internal packages for consistent data interchange.
package model

// Channel represents a Telegram channel being monitored.
type Channel struct {
	ID                int64
	Version           int64
	Username          string // lowercase without @
	Title             string
	Enabled           bool
	LastPostID        int64
	FetchErrorKind    string
	FetchErrorMessage string
	FetchErrorAt      *string
	CreatedAt         string
}

// Group represents a Telegram group where digests are sent.
type Group struct {
	ID             int64
	Version        int64
	TelegramChatID int64
	Title          string
	Status         string
	CreatedAt      string
}

const (
	GroupStatusActive     = "active"
	GroupStatusInactive   = "inactive"
	GroupStatusIneligible = "ineligible"
)

// GroupChannel links a channel to a group with optional topic assignment.
type GroupChannel struct {
	GroupID       int64
	ChannelID     int64
	TopicThreadID *int64 // nil if no specific topic
}

// ForumTopic is a durable record of a topic observed in a Telegram forum.
// LifecycleOwned is true only for topics created by this bot.
type ForumTopic struct {
	GroupID         int64
	MessageThreadID int64
	Name            string
	Status          string
	LifecycleOwned  bool
	Closed          bool
	ClosePending    bool
	CreatedAt       string
	UpdatedAt       string
}

const (
	ForumTopicStatusObserved  = "observed"
	ForumTopicStatusPersisted = "persisted"
)

// GroupSettings holds per-group AI and scheduling configuration.
type GroupSettings struct {
	GroupID    int64
	ProviderID *int64  // nil if using default provider
	Model      *string // nil if using provider's default model
	DigestTime string  // HH:MM format
	Timezone   string  // e.g. Europe/Moscow
}

// Post represents a parsed post from a Telegram channel.
type Post struct {
	ID           int64
	ChannelID    int64
	MessageID    int64
	Text         string
	Summary      *string // nil until summarized
	PostedAt     string
	URL          string
	ContentHash  string
	LinkURLsHash *string // nil if no links in post
	CreatedAt    string
}

// Digest represents a sent digest in a group.
type Digest struct {
	ID        int64
	GroupID   int64
	SentAt    string
	MessageID *int64 // nil if not yet sent
	PostCount int
}

// DigestPost links posts to a digest (many-to-many).
type DigestPost struct {
	DigestID int64
	PostID   int64
}

// AIProvider represents an AI summarization provider configuration.
type AIProvider struct {
	ID           int64
	Version      int64
	Name         string
	BaseURL      string
	APIKey       string
	DefaultModel string
	IsDefault    bool
	CreatedAt    string
}

// GroupAIConfig is the effective AI configuration for a group. Provider is
// either the explicitly assigned provider or the configured default provider,
// and Model is either the group's override or the provider's default model.
type GroupAIConfig struct {
	Provider AIProvider
	Model    string
}

// ConfigKV represents a key-value configuration entry in the config table.
type ConfigKV struct {
	Key   string
	Value string
}
