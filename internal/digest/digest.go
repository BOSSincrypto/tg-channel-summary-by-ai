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
	"strings"
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

type aiFailureClass string

const (
	aiFailureParse              aiFailureClass = "parse"
	aiFailureTransientExhausted aiFailureClass = "transient_exhausted"
	aiFailurePermanent          aiFailureClass = "permanent"
	aiFailureOpenRouterOutage   aiFailureClass = "openrouter_outage"
	aiFailureProvider           aiFailureClass = "provider"
)

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
		err := errors.New("provider factory is not configured")
		s.notifyAIFailure(groupID, err)
		return fmt.Errorf("summarize group %d: %w", groupID, err)
	}

	var fallbackErr error
	provider, err := s.providerFactory(s.aiConfigSource, groupID, s.providerHTTPClient, func(providerErr error) {
		fallbackErr = providerErr
	})
	if err != nil {
		s.notifyAIFailure(groupID, err)
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
		s.notifyAIFailure(groupID, err)
		return fmt.Errorf("summarize group %d: %w", groupID, err)
	}
	if fallbackErr != nil {
		s.notifyAIFallback(groupID)
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

func (s *Service) notifyAIFallback(groupID int64) {
	group := s.groupTitle(groupID)
	message := fmt.Sprintf(
		"⚠️ Провайдер AI для группы «%s» временно недоступен, поэтому использован OpenRouter. Проверьте провайдера и повторите следующий цикл.",
		group,
	)
	s.notifyAI(groupID, message)
}

func (s *Service) notifyAIFailure(groupID int64, err error) {
	class := classifyAIFailure(err)
	group := s.groupTitle(groupID)
	message := fmt.Sprintf("❌ %s", formatAIFailure(class, group, err))
	s.notifyAI(groupID, message)
}

func classifyAIFailure(err error) aiFailureClass {
	if err == nil {
		return aiFailureProvider
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "parse summaries") ||
		strings.Contains(lower, "expected json") ||
		strings.Contains(lower, "summary response") ||
		strings.Contains(lower, "not in russian") ||
		strings.Contains(lower, "one sentence") {
		return aiFailureParse
	}

	var providerErr *summarizer.ProviderError
	if errors.As(err, &providerErr) {
		switch {
		case providerErr.StatusCode == http.StatusUnauthorized ||
			providerErr.StatusCode == http.StatusPaymentRequired ||
			providerErr.StatusCode == http.StatusForbidden:
			return aiFailurePermanent
		case providerErr.StatusCode == http.StatusRequestTimeout ||
			providerErr.StatusCode == http.StatusTooManyRequests ||
			providerErr.StatusCode >= http.StatusInternalServerError:
			if strings.EqualFold(providerErr.Provider, "OpenRouter") ||
				strings.Contains(lower, "openrouter") {
				return aiFailureOpenRouterOutage
			}
			return aiFailureTransientExhausted
		}
	}
	if strings.Contains(lower, "timeout") ||
		strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "temporarily unavailable") ||
		strings.Contains(lower, "retry") {
		return aiFailureTransientExhausted
	}
	return aiFailureProvider
}

func formatAIFailure(class aiFailureClass, group string, err error) string {
	switch class {
	case aiFailureParse:
		return fmt.Sprintf("Не удалось разобрать ответ AI для группы «%s». Ответ провайдера не сохранён; проверьте формат ответа и повторите запуск.", group)
	case aiFailureOpenRouterOutage:
		return fmt.Sprintf("OpenRouter недоступен (%s). Дайджест группы «%s» пропущен после исчерпания повторных попыток; проверьте статус OpenRouter и повторите позже.", providerStatusDetail(err), group)
	case aiFailureTransientExhausted:
		return fmt.Sprintf("Временная ошибка AI для группы «%s» (%s) не устранена после повторных попыток; дайджест пропущен, повторите запуск позже.", group, providerStatusDetail(err))
	case aiFailurePermanent:
		return fmt.Sprintf("Ошибка AI провайдера для группы «%s» (%s). Дайджест пропущен; проверьте API ключ, доступ и баланс.", group, safeProviderStatus(err))
	default:
		return fmt.Sprintf("Ошибка AI провайдера для группы «%s»; дайджест пропущен. Проверьте настройки провайдера и повторите запуск.", group)
	}
}

func safeProviderStatus(err error) string {
	var providerErr *summarizer.ProviderError
	if errors.As(err, &providerErr) && providerErr.StatusCode > 0 {
		return fmt.Sprintf("HTTP %d", providerErr.StatusCode)
	}
	return "неизвестный статус"
}

func providerStatusDetail(err error) string {
	status := safeProviderStatus(err)
	var providerErr *summarizer.ProviderError
	if errors.As(err, &providerErr) {
		switch {
		case providerErr.StatusCode >= http.StatusInternalServerError:
			return status + " (5xx)"
		case providerErr.StatusCode == http.StatusTooManyRequests:
			return status + " (ограничение запросов)"
		}
	}
	return status
}

func (s *Service) groupTitle(groupID int64) string {
	if s != nil && s.database != nil {
		group, err := s.database.Groups.GetByID(groupID)
		if err == nil && group != nil && strings.TrimSpace(group.Title) != "" {
			return strings.TrimSpace(group.Title)
		}
	}
	return fmt.Sprintf("группа %d", groupID)
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
