// Package model defines shared data structures used across the application.
// These types represent the core domain entities and are used by all
// internal packages for consistent data interchange.
package model

// Channel represents a Telegram channel being monitored.
type Channel struct {
	ID         int64
	Username   string
	Title      string
	Enabled    bool
	LastPostID int64
}

// Group represents a Telegram group where digests are sent.
type Group struct {
	ID             int64
	TelegramChatID int64
	Title          string
}

// Post represents a parsed post from a Telegram channel.
type Post struct {
	ID          int64
	ChannelID   int64
	MessageID   int64
	Text        string
	Summary     string
	PostedAt    string
	URL         string
	ContentHash string
	LinkURLHash string
}

// Digest represents a sent digest in a group.
type Digest struct {
	ID      int64
	GroupID int64
	SentAt  string
	MsgID   int64
	Count   int
}

// AIProvider represents an AI summarization provider configuration.
type AIProvider struct {
	ID           int64
	Name         string
	BaseURL      string
	APIKey       string
	DefaultModel string
	IsDefault    bool
}
