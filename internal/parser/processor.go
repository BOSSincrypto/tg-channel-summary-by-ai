package parser

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
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
	HTTPStatus       int
}

// ChannelProcessResult describes one channel fetch and storage operation.
type ChannelProcessResult struct {
	Channel             model.Channel
	ParsedPosts         int
	StoredPosts         int
	MediaOnlySkipped    int
	HTTPStatus          int
	PreviouslyPopulated bool
}

// ChannelFailure records a channel that could not be fetched or stored.
type ChannelFailure struct {
	Channel    model.Channel
	Err        error
	HTTPStatus int
}

// ChannelBatchResult describes a best-effort batch across assigned channels.
type ChannelBatchResult struct {
	Results  []ChannelProcessResult
	Failures []ChannelFailure
}

// OwnerNotifier is the dependency-injected transport for owner alerts. The
// parser package intentionally depends on this small interface rather than on
// the bot package.
type OwnerNotifier interface {
	NotifyOwner(ctx context.Context, text string) error
}

// ChannelProcessor connects t.me/s parsing to persistent post storage.
type ChannelProcessor struct {
	fetcher  ChannelFetcher
	storage  *PostStorage
	notifier OwnerNotifier
}

// NewChannelProcessor creates the production parser-to-storage adapter.
func NewChannelProcessor(fetcher ChannelFetcher, storage *PostStorage, notifiers ...OwnerNotifier) *ChannelProcessor {
	var notifier OwnerNotifier
	if len(notifiers) > 0 {
		notifier = notifiers[0]
	}
	return &ChannelProcessor{fetcher: fetcher, storage: storage, notifier: notifier}
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
	previouslyPopulated := channel.LastPostID > 0

	posts, stats, err := p.parse(channel.Username)
	if err != nil {
		return ChannelProcessResult{
			Channel:    *channel,
			HTTPStatus: stats.HTTPStatus,
		}, fmt.Errorf("process channel %q: %w", channel.Username, err)
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
		Channel:             *channel,
		ParsedPosts:         len(posts),
		StoredPosts:         len(stored),
		MediaOnlySkipped:    stats.MediaOnlySkipped,
		HTTPStatus:          stats.HTTPStatus,
		PreviouslyPopulated: previouslyPopulated,
	}, nil
}

// ProcessChannels processes every channel independently. A failed channel is
// reported and does not prevent other channels in the batch from completing.
func (p *ChannelProcessor) ProcessChannels(channels []model.Channel) (ChannelBatchResult, error) {
	return p.ProcessChannelsContext(context.Background(), channels)
}

// ProcessChannelsContext processes a channel cycle and evaluates structural
// change detection once after all channels have been attempted.
func (p *ChannelProcessor) ProcessChannelsContext(ctx context.Context, channels []model.Channel) (ChannelBatchResult, error) {
	if p == nil || p.fetcher == nil || p.storage == nil {
		return ChannelBatchResult{}, errors.New("process channels: parser and storage are required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	batch := ChannelBatchResult{
		Results:  make([]ChannelProcessResult, 0, len(channels)),
		Failures: make([]ChannelFailure, 0),
	}
	for i := range channels {
		result, err := p.ProcessChannel(&channels[i])
		if err != nil {
			batch.Failures = append(batch.Failures, ChannelFailure{
				Channel:    channels[i],
				Err:        err,
				HTTPStatus: result.HTTPStatus,
			})
			continue
		}
		batch.Results = append(batch.Results, result)
	}
	p.notifyStructuralChange(ctx, batch)
	return batch, nil
}

func (p *ChannelProcessor) notifyStructuralChange(ctx context.Context, batch ChannelBatchResult) {
	if !batch.StructuralChangeDetected() {
		return
	}

	message := fmt.Sprintf(
		"⚠️ Возможно, Telegram изменил структуру t.me/s. Посты не извлекаются из %d каналов, ранее содержавших публикации. Проверьте парсер.",
		batch.PreviouslyPopulatedCount(),
	)
	log.Printf("WARNING: Possible t.me/s HTML structure change - 0 posts extracted from %d channels that previously had content.", batch.PreviouslyPopulatedCount())
	if p.notifier == nil {
		return
	}
	if err := p.notifier.NotifyOwner(ctx, message); err != nil {
		log.Printf("WARNING: failed to notify owner about parser structure change: %v", err)
	}
}

// PreviouslyPopulatedCount returns the number of successful HTTP 200 results
// from channels that had content before this cycle.
func (b ChannelBatchResult) PreviouslyPopulatedCount() int {
	count := 0
	for _, result := range b.Results {
		if result.HTTPStatus == http.StatusOK && result.ParsedPosts == 0 && result.PreviouslyPopulated {
			count++
		}
	}
	return count
}

// StructuralChangeDetected reports a cycle-level parser break. A failure or
// any extracted post makes the cycle inconclusive, avoiding false alerts.
func (b ChannelBatchResult) StructuralChangeDetected() bool {
	if len(b.Results) == 0 || len(b.Failures) != 0 {
		return false
	}
	if b.PreviouslyPopulatedCount() == 0 {
		return false
	}
	for _, result := range b.Results {
		if result.HTTPStatus != http.StatusOK || result.ParsedPosts != 0 {
			return false
		}
	}
	return true
}

func (p *ChannelProcessor) parse(username string) ([]ParsedPost, ParseStats, error) {
	if statsFetcher, ok := p.fetcher.(ChannelStatsFetcher); ok {
		return statsFetcher.ParseChannelWithStats(username)
	}
	posts, err := p.fetcher.ParseChannel(username)
	return posts, ParseStats{}, err
}
