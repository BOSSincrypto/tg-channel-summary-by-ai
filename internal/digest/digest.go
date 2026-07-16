// Package digest handles digest assembly, formatting, and delivery.
// It collects posts for a group, deduplicates them, formats them into
// MarkdownV2 messages, and sends them via the Telegram bot API.
package digest

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/parser"
	"github.com/boss/tg-channel-summary-by-ai/internal/summarizer"
)

// Digest represents a single digest for a group.
type Digest struct {
	GroupID   int64
	PostCount int
	// TODO: formatted message parts
}

// Service assembles and delivers digests.
type Service struct {
	database           *db.DB
	processor          *parser.ChannelProcessor
	aiConfigSource     summarizer.GroupAIConfigSource
	providerFactory    func(summarizer.GroupAIConfigSource, int64, *http.Client, func(error)) (summarizer.Provider, error)
	providerHTTPClient *http.Client
	notifier           OwnerNotifier
	maxPostsPerChannel int
}

const defaultMaxPostsPerChannel = 50

// Option customizes digest candidate selection.
type Option func(*Service)

// WithMaxPostsPerChannel limits how many unsummarized posts are selected from
// each channel in one digest cycle. Non-positive values use the default.
func WithMaxPostsPerChannel(limit int) Option {
	return func(s *Service) {
		if limit > 0 {
			s.maxPostsPerChannel = limit
		}
	}
}

// OwnerNotifier is the small transport contract needed for digest AI alerts.
// The digest package does not depend on the Telegram bot implementation.
type OwnerNotifier interface {
	NotifyOwner(context.Context, string) error
}

// New creates an unconfigured digest Service. It is retained for callers that
// only need the value type; Generate returns an actionable configuration error.
func New() *Service {
	return &Service{}
}

// NewWithProcessor creates a digest service that fetches assigned channels and
// persists parser output before selecting posts for the digest.
func NewWithProcessor(database *db.DB, processor *parser.ChannelProcessor, options ...Option) *Service {
	service := &Service{
		database:           database,
		processor:          processor,
		maxPostsPerChannel: defaultMaxPostsPerChannel,
	}
	for _, option := range options {
		option(service)
	}
	return service
}

// NewWithProcessorAndAI creates a digest service that resolves the effective
// provider and model for each group before summarizing its posts.
func NewWithProcessorAndAI(database *db.DB, processor *parser.ChannelProcessor, source summarizer.GroupAIConfigSource, client *http.Client, notifiers ...OwnerNotifier) *Service {
	return newWithProcessorAndAI(database, processor, source, client, summarizer.NewProviderForGroupWithFallback, notifiers...)
}

// NewWithProcessorAndAIForTesting is equivalent to NewWithProcessorAndAI but
// permits loopback provider endpoints used by deterministic tests.
func NewWithProcessorAndAIForTesting(database *db.DB, processor *parser.ChannelProcessor, source summarizer.GroupAIConfigSource, client *http.Client, notifiers ...OwnerNotifier) *Service {
	return newWithProcessorAndAI(database, processor, source, client, summarizer.NewProviderForGroupWithFallbackForTesting, notifiers...)
}

func newWithProcessorAndAI(database *db.DB, processor *parser.ChannelProcessor, source summarizer.GroupAIConfigSource, client *http.Client, factory func(summarizer.GroupAIConfigSource, int64, *http.Client, func(error)) (summarizer.Provider, error), notifiers ...OwnerNotifier) *Service {
	var notifier OwnerNotifier
	if len(notifiers) > 0 {
		notifier = notifiers[0]
	}
	return &Service{
		database:           database,
		processor:          processor,
		aiConfigSource:     source,
		providerFactory:    factory,
		providerHTTPClient: client,
		notifier:           notifier,
		maxPostsPerChannel: defaultMaxPostsPerChannel,
	}
}

// NewWithProcessorAndAIWithMaxPostsPerChannel wires production candidate
// selection to the configured per-channel limit. Scheduled and manual entry
// points both use the returned service.
func NewWithProcessorAndAIWithMaxPostsPerChannel(database *db.DB, processor *parser.ChannelProcessor, source summarizer.GroupAIConfigSource, client *http.Client, maxPostsPerChannel int, notifiers ...OwnerNotifier) *Service {
	service := newWithProcessorAndAI(database, processor, source, client, summarizer.NewProviderForGroupWithFallback, notifiers...)
	WithMaxPostsPerChannel(maxPostsPerChannel)(service)
	return service
}

// NewWithProcessorAndAIForTestingWithMaxPostsPerChannel is the loopback-enabled
// equivalent used by deterministic production-boundary tests.
func NewWithProcessorAndAIForTestingWithMaxPostsPerChannel(database *db.DB, processor *parser.ChannelProcessor, source summarizer.GroupAIConfigSource, client *http.Client, maxPostsPerChannel int, notifiers ...OwnerNotifier) *Service {
	service := newWithProcessorAndAI(database, processor, source, client, summarizer.NewProviderForGroupWithFallbackForTesting, notifiers...)
	WithMaxPostsPerChannel(maxPostsPerChannel)(service)
	return service
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

// Generate fetches and stores current channel output, then selects at most the
// configured number of unsummarized posts per channel for this group's digest
// window. Storage happens before selection so runtime cursor advancement and
// deduplication are applied, while unselected posts remain eligible later.
func (s *Service) Generate(groupID int64) (*Digest, error) {
	if _, err := s.FetchAndStore(groupID); err != nil {
		return nil, err
	}
	posts, err := s.database.Posts.ListUnsummarized(groupID, 24)
	if err != nil {
		return nil, fmt.Errorf("list digest posts for group %d: %w", groupID, err)
	}
	posts = capPostsPerChannel(posts, s.maxPostsPerChannel)
	if err := s.summarize(groupID, posts); err != nil {
		return nil, err
	}
	return &Digest{GroupID: groupID, PostCount: len(posts)}, nil
}

func capPostsPerChannel(posts []model.Post, limit int) []model.Post {
	if limit <= 0 {
		limit = defaultMaxPostsPerChannel
	}
	selected := make([]model.Post, 0, len(posts))
	counts := make(map[int64]int)
	for _, post := range posts {
		if counts[post.ChannelID] >= limit {
			continue
		}
		selected = append(selected, post)
		counts[post.ChannelID]++
	}
	return selected
}

// GenerateManual is the manual trigger entry point. It deliberately shares
// the same provider-resolving path as scheduled Generate calls.
func (s *Service) GenerateManual(groupID int64) (*Digest, error) {
	return s.Generate(groupID)
}

func (s *Service) summarize(groupID int64, posts []model.Post) error {
	if s.aiConfigSource == nil || len(posts) == 0 {
		return nil
	}
	if s.providerFactory == nil {
		return fmt.Errorf("summarize group %d: provider factory is not configured", groupID)
	}

	fallbackNotified := false
	provider, err := s.providerFactory(s.aiConfigSource, groupID, s.providerHTTPClient, func(providerErr error) {
		fallbackNotified = true
		s.notifyAI(groupID, fmt.Sprintf("⚠️ Провайдер AI для группы %d временно недоступен, использован OpenRouter: %v", groupID, providerErr))
	})
	if err != nil {
		return fmt.Errorf("summarize group %d: create provider: %w", groupID, err)
	}
	input := make([]summarizer.Post, 0, len(posts))
	for _, post := range posts {
		input = append(input, summarizer.Post{ID: post.ID, Text: post.Text})
	}
	summaryContext, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	summaries, err := provider.Summarize(summaryContext, input)
	if err != nil {
		if !fallbackNotified {
			s.notifyAI(groupID, fmt.Sprintf("⚠️ Не удалось создать дайджест группы %d: %v", groupID, err))
		}
		return fmt.Errorf("summarize group %d: %w", groupID, err)
	}

	expected := make(map[int64]struct{}, len(posts))
	for _, post := range posts {
		expected[post.ID] = struct{}{}
	}
	for _, summary := range summaries {
		if _, ok := expected[summary.PostID]; !ok {
			continue
		}
		if err := s.database.Posts.UpdateSummary(summary.PostID, summary.Text); err != nil {
			return fmt.Errorf("store summary for post %d: %w", summary.PostID, err)
		}
	}
	return nil
}

func (s *Service) notifyAI(groupID int64, message string) {
	if s == nil || s.notifier == nil {
		return
	}
	if err := s.notifier.NotifyOwner(context.Background(), message); err != nil {
		// The provider error remains the actionable digest failure; a transport
		// failure must not replace it or cause a retry to invoke the provider.
		log.Printf("failed to notify owner about AI digest group %d: %v", groupID, err)
	}
}
