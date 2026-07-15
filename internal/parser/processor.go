package parser

import (
	"errors"
	"fmt"
	"strings"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

// ChannelFetcher is the parser contract used by the production channel path.
type ChannelFetcher interface {
	ParseChannel(username string) ([]ParsedPost, error)
}

// ChannelStatsFetcher is optionally implemented by fetchers that can report
// media-only posts skipped while parsing a channel page.
type ChannelStatsFetcher interface {
	ParseChannelWithStats(username string) ([]ParsedPost, ParseStats, error)
}

// ParseStats describes parser output that is useful to downstream reporting.
type ParseStats struct {
	MediaOnlySkipped int
}

// ChannelProcessResult describes one channel fetch and storage operation.
type ChannelProcessResult struct {
	Channel          model.Channel
	ParsedPosts      int
	StoredPosts      int
	MediaOnlySkipped int
}

// ChannelFailure records a channel that could not be fetched or stored.
type ChannelFailure struct {
	Channel model.Channel
	Err     error
}

// ChannelBatchResult describes a best-effort batch across assigned channels.
type ChannelBatchResult struct {
	Results  []ChannelProcessResult
	Failures []ChannelFailure
}

// ChannelProcessor connects t.me/s parsing to persistent post storage.
type ChannelProcessor struct {
	fetcher ChannelFetcher
	storage *PostStorage
}

// NewChannelProcessor creates the production parser-to-storage adapter.
func NewChannelProcessor(fetcher ChannelFetcher, storage *PostStorage) *ChannelProcessor {
	return &ChannelProcessor{fetcher: fetcher, storage: storage}
}

// ProcessChannel fetches a channel, validates required post fields, stores new
// posts, skips duplicates, and advances the channel cursor through PostStorage.
func (p *ChannelProcessor) ProcessChannel(channel *model.Channel) (ChannelProcessResult, error) {
	if p == nil || p.fetcher == nil || p.storage == nil {
		return ChannelProcessResult{}, errors.New("process channel: parser and storage are required")
	}
	if channel == nil {
		return ChannelProcessResult{}, errors.New("process channel: channel is required")
	}

	posts, stats, err := p.parse(channel.Username)
	if err != nil {
		return ChannelProcessResult{}, fmt.Errorf("process channel %q: %w", channel.Username, err)
	}
	for _, post := range posts {
		if strings.TrimSpace(post.PostedAt) == "" {
			return ChannelProcessResult{}, fmt.Errorf("process channel %q post %d: missing posted_at timestamp", channel.Username, post.MessageID)
		}
	}
	stored, err := p.storage.Store(channel, posts)
	if err != nil {
		return ChannelProcessResult{}, fmt.Errorf("process channel %q: %w", channel.Username, err)
	}
	return ChannelProcessResult{
		Channel:          *channel,
		ParsedPosts:      len(posts),
		StoredPosts:      len(stored),
		MediaOnlySkipped: stats.MediaOnlySkipped,
	}, nil
}

// ProcessChannels processes every channel independently. A failed channel is
// reported and does not prevent other channels in the batch from completing.
func (p *ChannelProcessor) ProcessChannels(channels []model.Channel) (ChannelBatchResult, error) {
	if p == nil || p.fetcher == nil || p.storage == nil {
		return ChannelBatchResult{}, errors.New("process channels: parser and storage are required")
	}
	batch := ChannelBatchResult{
		Results:  make([]ChannelProcessResult, 0, len(channels)),
		Failures: make([]ChannelFailure, 0),
	}
	for i := range channels {
		result, err := p.ProcessChannel(&channels[i])
		if err != nil {
			batch.Failures = append(batch.Failures, ChannelFailure{Channel: channels[i], Err: err})
			continue
		}
		batch.Results = append(batch.Results, result)
	}
	return batch, nil
}

func (p *ChannelProcessor) parse(username string) ([]ParsedPost, ParseStats, error) {
	if statsFetcher, ok := p.fetcher.(ChannelStatsFetcher); ok {
		return statsFetcher.ParseChannelWithStats(username)
	}
	posts, err := p.fetcher.ParseChannel(username)
	return posts, ParseStats{}, err
}
