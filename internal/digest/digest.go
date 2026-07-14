// Package digest handles digest assembly, formatting, and delivery.
// It collects posts for a group, deduplicates them, formats them into
// MarkdownV2 messages, and sends them via the Telegram bot API.
package digest

// Digest represents a single digest for a group.
type Digest struct {
	GroupID   int64
	PostCount int
	// TODO: formatted message parts
}

// Service assembles and delivers digests.
type Service struct {
	// TODO: database, bot, summarizer, parser
}

// New creates a new digest Service.
func New() *Service {
	return &Service{}
}

// Generate creates a digest for a specific group.
func (s *Service) Generate(groupID int64) (*Digest, error) {
	// TODO: parse channels, dedup posts, summarize, format, send
	return nil, nil
}
