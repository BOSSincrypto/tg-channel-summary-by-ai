// Package digest handles digest assembly, formatting, and delivery.
// It collects posts for a group, deduplicates them, formats them into
// MarkdownV2 messages, and sends them via the Telegram bot API.
package digest

import (
	"errors"
	"fmt"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/parser"
)

// Digest represents a single digest for a group.
type Digest struct {
	GroupID   int64
	PostCount int
	// TODO: formatted message parts
}

// Service assembles and delivers digests.
type Service struct {
	database  *db.DB
	processor *parser.ChannelProcessor
}

// New creates an unconfigured digest Service. It is retained for callers that
// only need the value type; Generate returns an actionable configuration error.
func New() *Service {
	return &Service{}
}

// NewWithProcessor creates a digest service that fetches assigned channels and
// persists parser output before selecting posts for the digest.
func NewWithProcessor(database *db.DB, processor *parser.ChannelProcessor) *Service {
	return &Service{database: database, processor: processor}
}

// FetchAndStore processes all enabled channels assigned to a group. Individual
// channel failures are captured in the batch result so other channels continue.
func (s *Service) FetchAndStore(groupID int64) (parser.ChannelBatchResult, error) {
	if s == nil || s.database == nil || s.processor == nil {
		return parser.ChannelBatchResult{}, errors.New("fetch and store: digest service is not configured")
	}
	channels, err := s.database.Groups.GetChannelsForGroup(groupID)
	if err != nil {
		return parser.ChannelBatchResult{}, fmt.Errorf("load channels for group %d: %w", groupID, err)
	}
	batch, err := s.processor.ProcessChannels(channels)
	if err != nil {
		return parser.ChannelBatchResult{}, fmt.Errorf("process channels for group %d: %w", groupID, err)
	}
	return batch, nil
}

// Generate fetches and stores current channel output, then returns the
// unsummarized posts available for this group's digest window. Storage happens
// before selection so runtime cursor advancement and deduplication are applied.
func (s *Service) Generate(groupID int64) (*Digest, error) {
	if _, err := s.FetchAndStore(groupID); err != nil {
		return nil, err
	}
	posts, err := s.database.Posts.ListUnsummarized(groupID, 24)
	if err != nil {
		return nil, fmt.Errorf("list digest posts for group %d: %w", groupID, err)
	}
	return &Digest{GroupID: groupID, PostCount: len(posts)}, nil
}
