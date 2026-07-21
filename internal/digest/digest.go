// Package digest handles digest assembly, formatting, and delivery.
// It collects posts for a group, deduplicates them, formats them into
// MarkdownV2 messages, and sends them via the Telegram bot API.
package digest

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/parser"
	"github.com/boss/tg-channel-summary-by-ai/internal/summarizer"
	"github.com/boss/tg-channel-summary-by-ai/internal/telegram"
)

// Digest represents a single digest for a group.
type Digest struct {
	GroupID        int64
	PostCount      int
	WindowID       string
	ChannelCount   int
	Outcome        string
	FailedChannels []string
	FailureDetails []string
	Message        string
	Text           string
	MessageID      *int64
	MessageURL     string
	SummariesSaved bool
	Delivered      bool
}

const (
	OutcomeSucceeded         = "succeeded"
	OutcomeNoPosts           = "no_posts"
	OutcomePartial           = "partial"
	OutcomeAllChannelsFailed = "all_channels_failed"
	OutcomeAIFailed          = "ai_failed"
	OutcomeDeliveryFailed    = "delivery_failed"
)

// ErrDigestInProgress is returned when a scheduled or manual run already
// owns the group's digest execution slot.
var ErrDigestInProgress = errors.New("digest already in progress")

// DeliveryReceipt contains Telegram metadata for a successfully delivered
// digest message.
type DeliveryReceipt struct {
	MessageID  int64
	MessageURL string
}

// Delivery is the narrow transport boundary used after summaries are saved.
// A delivery error must never be reported as a successful digest.
type Delivery interface {
	Deliver(context.Context, int64, *Digest) (DeliveryReceipt, error)
}

// Service assembles and delivers digests.
type Service struct {
	database           *db.DB
	processor          *parser.ChannelProcessor
	aiConfigSource     summarizer.GroupAIConfigSource
	providerFactory    func(summarizer.GroupAIConfigSource, int64, *http.Client, func(error)) (summarizer.Provider, error)
	providerHTTPClient *http.Client
	notifier           OwnerNotifier
	delivery           Delivery
	maxPostsPerChannel int
	creditWarning      float64
	creditMinimum      float64

	notificationMu      sync.Mutex
	notificationKeys    map[string]notificationKeyState
	notificationWaiters map[string]chan struct{}
	// notificationWaitObserved is a deterministic test barrier for callers
	// that observe an in-flight outage notification claim.
	notificationWaitObserved chan struct{}
	runMu                    sync.Mutex
	runningGroups            map[int64]struct{}
}

// notificationKeyState tracks delivery-aware outage notification deduplication.
// Keys are committed only after NotifyOwner succeeds so failed deliveries remain retryable.
type notificationKeyState int

const (
	notificationKeyClaimed notificationKeyState = iota + 1
	notificationKeyDelivered
)

const defaultMaxPostsPerChannel = 50

const (
	defaultCreditWarningThreshold = 0.50
	defaultCreditMinimumThreshold = 0.10
)

type aiFailureClass string

const (
	aiFailureParse              aiFailureClass = "parse"
	aiFailureTransientExhausted aiFailureClass = "transient_exhausted"
	aiFailurePermanent          aiFailureClass = "permanent"
	aiFailureOpenRouterOutage   aiFailureClass = "openrouter_outage"
	aiFailureProvider           aiFailureClass = "provider"
	aiFailureCreditExhausted    aiFailureClass = "credit_exhausted"
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

// WithCreditThresholds configures the OpenRouter pre-digest balance guard.
// Invalid values retain the service defaults.
func WithCreditThresholds(warning, minimum float64) Option {
	return func(s *Service) {
		if minimum >= 0 && warning >= minimum {
			s.creditWarning = warning
			s.creditMinimum = minimum
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
		creditWarning:      defaultCreditWarningThreshold,
		creditMinimum:      defaultCreditMinimumThreshold,
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
		creditWarning:      defaultCreditWarningThreshold,
		creditMinimum:      defaultCreditMinimumThreshold,
		runningGroups:      make(map[int64]struct{}),
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

// SetDelivery configures the Telegram delivery boundary shared by scheduled
// and manual digest execution.
func (s *Service) SetDelivery(delivery Delivery) {
	if s != nil {
		s.delivery = delivery
	}
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
//
// Production scheduler code uses GenerateWithWindow to pass one explicit ID
// to all groups in a logical scheduled window. Direct callers receive their
// own logical window.
func (s *Service) Generate(groupID int64) (*Digest, error) {
	return s.generate(groupID, NewWindowID("scheduled"), false)
}

// GenerateWithWindow runs a scheduled digest with an explicit correlation ID.
func (s *Service) GenerateWithWindow(groupID int64, windowID string) (*Digest, error) {
	return s.generate(groupID, firstWindowID([]string{windowID}, NewWindowID("scheduled")), false)
}

func (s *Service) generate(groupID int64, windowID string, manual bool) (*Digest, error) {
	if !s.tryStartGroup(groupID) {
		return nil, ErrDigestInProgress
	}
	defer s.finishGroup(groupID)

	if s.delivery != nil && s.database != nil && s.database.Digests != nil {
		if err := s.resumePendingLocked(groupID); err != nil {
			result := &Digest{
				GroupID: groupID, Outcome: OutcomeDeliveryFailed,
				Message: safeDigestMessage(err),
			}
			s.notifyDigestOutcome(result)
			return result, terminalDigestError(result, err, manual)
		}
	}

	batch, err := s.FetchAndStore(groupID)
	if err != nil {
		return nil, err
	}
	posts, err := s.database.Posts.ListUnsummarized(groupID, 24)
	if err != nil {
		return nil, fmt.Errorf("list digest posts for group %d: %w", groupID, err)
	}
	posts = capPostsPerChannel(posts, s.maxPostsPerChannel)
	result := &Digest{
		GroupID: groupID, PostCount: len(posts), ChannelCount: len(batch.Results),
		WindowID: windowID, FailedChannels: failedChannelNames(batch),
		FailureDetails: failedChannelDetails(batch),
	}
	if len(batch.Results) == 0 && len(batch.Failures) == 0 {
		// A group with no configured (enabled) channels is a configuration
		// error, not an empty-posts informational result. Keep the existing
		// terminal outcome vocabulary for WebApp clients, but use an
		// actionable message and never invoke AI.
		result.Outcome = OutcomeNoPosts
		result.Message = fmt.Sprintf(
			"⚠️ Группа «%s»: не настроены каналы для дайджеста. Добавьте каналы в настройках.",
			s.groupTitle(groupID),
		)
		result.Text = result.Message
		if s.delivery != nil {
			pendingID, checkpointErr := s.database.Digests.CreatePending(groupID, result.Text, nil)
			if checkpointErr != nil {
				result.Outcome = OutcomeDeliveryFailed
				result.Message = safeDigestMessage(checkpointErr)
				s.notifyDigestOutcome(result)
				return result, terminalDigestError(result, checkpointErr, manual)
			}
			receipt, deliveryErr := s.delivery.Deliver(context.Background(), groupID, result)
			if deliveryErr != nil {
				result.Outcome = OutcomeDeliveryFailed
				result.Message = safeDigestMessage(deliveryErr)
				s.notifyDigestOutcome(result)
				return result, terminalDigestError(result, deliveryErr, manual)
			}
			result.MessageID = optionalInt64(receipt.MessageID)
			result.MessageURL = receipt.MessageURL
			result.Delivered = true
			if markErr := s.database.Digests.MarkSent(pendingID, receipt.MessageID); markErr != nil {
				log.Printf("configuration warning %d delivered but checkpoint remains pending: %v", pendingID, markErr)
			}
		}
		return result, terminalDigestError(result, nil, manual)
	}
	if len(batch.Results) == 0 && len(batch.Failures) > 0 {
		result.Outcome = OutcomeAllChannelsFailed
		result.Message = "Не удалось собрать посты. Все каналы недоступны."
		s.notifyDigestOutcome(result)
		return result, terminalDigestError(result, nil, manual)
	}
	if len(posts) == 0 {
		result.Outcome = OutcomeNoPosts
		result.Message = "Нет новых постов для дайджеста."
		if len(batch.Failures) > 0 {
			result.Message = "Нет новых постов для дайджеста. Часть каналов недоступна."
			s.notifyDigestOutcome(result)
		} else if s.emptyDigestBehavior(groupID) == model.EmptyDigestSendMessage && s.delivery != nil {
			result.Message = "За сегодня нет новых постов"
			result.Text = result.Message
			pendingID, checkpointErr := s.database.Digests.CreatePending(groupID, result.Text, nil)
			if checkpointErr != nil {
				result.Outcome = OutcomeDeliveryFailed
				result.Message = safeDigestMessage(checkpointErr)
				s.notifyDigestOutcome(result)
				return result, terminalDigestError(result, checkpointErr, manual)
			}
			receipt, deliveryErr := s.delivery.Deliver(context.Background(), groupID, result)
			if deliveryErr != nil {
				result.Outcome = OutcomeDeliveryFailed
				result.Message = safeDigestMessage(deliveryErr)
				s.notifyDigestOutcome(result)
				return result, terminalDigestError(result, deliveryErr, manual)
			}
			result.MessageID = optionalInt64(receipt.MessageID)
			result.MessageURL = receipt.MessageURL
			result.Delivered = true
			if markErr := s.database.Digests.MarkSent(pendingID, receipt.MessageID); markErr != nil {
				log.Printf("empty digest %d delivered but checkpoint remains pending: %v", pendingID, markErr)
			}
		}
		return result, terminalDigestError(result, nil, manual)
	}
	if err := s.summarizeWithWindow(groupID, posts, windowID); err != nil {
		result.Outcome = OutcomeAIFailed
		result.Message = safeDigestMessage(err)
		// summarizeWithWindow already emits the detailed provider alert.
		return result, terminalDigestError(result, err, manual)
	}
	result.SummariesSaved = true
	result.Text = s.formatDigestMessage(groupID, posts)
	if s.delivery != nil {
		pendingID, err := s.database.Digests.CreatePending(groupID, result.Text, posts)
		if err != nil {
			result.Outcome = OutcomeDeliveryFailed
			result.Message = safeDigestMessage(err)
			s.notifyDigestOutcome(result)
			return result, terminalDigestError(result, err, manual)
		}
		receipt, err := s.delivery.Deliver(context.Background(), groupID, result)
		if err != nil {
			result.Outcome = OutcomeDeliveryFailed
			result.Message = safeDigestMessage(err)
			s.notifyDigestOutcome(result)
			return result, terminalDigestError(result, err, manual)
		}
		result.MessageID = optionalInt64(receipt.MessageID)
		result.MessageURL = receipt.MessageURL
		result.Delivered = true
		if err := s.database.Digests.MarkSent(pendingID, receipt.MessageID); err != nil {
			// The send succeeded and the pending row remains durable, so a
			// restart can reconcile it instead of losing the checkpoint.
			log.Printf("digest %d delivered but checkpoint remains pending: %v", pendingID, err)
		}
	}
	if len(batch.Failures) > 0 {
		result.Outcome = OutcomePartial
		result.Message = "Дайджест отправлен частично."
		s.notifyDigestOutcome(result)
	} else {
		result.Outcome = OutcomeSucceeded
		result.Message = "Дайджест отправлен."
	}
	return result, nil
}

func (s *Service) tryStartGroup(groupID int64) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.runningGroups == nil {
		s.runningGroups = make(map[int64]struct{})
	}
	if _, exists := s.runningGroups[groupID]; exists {
		return false
	}
	s.runningGroups[groupID] = struct{}{}
	return true
}

func (s *Service) finishGroup(groupID int64) {
	s.runMu.Lock()
	delete(s.runningGroups, groupID)
	s.runMu.Unlock()
}

func (s *Service) emptyDigestBehavior(groupID int64) string {
	if s == nil || s.database == nil || s.database.Groups == nil {
		return model.EmptyDigestSendMessage
	}
	settings, err := s.database.Groups.GetGroupSettings(groupID)
	if err != nil {
		return model.EmptyDigestSendMessage
	}
	if settings.EmptyDigestBehavior == model.EmptyDigestSilent {
		return model.EmptyDigestSilent
	}
	return model.EmptyDigestSendMessage
}

// ResumePending retries durable digest sends after a process restart. The
// summaries and exact rendered message are already persisted, so no parser or
// AI work is repeated.
func (s *Service) ResumePending(groupID int64) error {
	if s == nil || s.database == nil || s.database.Digests == nil || s.delivery == nil {
		return errors.New("resume pending: digest delivery is not configured")
	}
	if !s.tryStartGroup(groupID) {
		return ErrDigestInProgress
	}
	defer s.finishGroup(groupID)
	return s.resumePendingLocked(groupID)
}

func (s *Service) resumePendingLocked(groupID int64) error {
	pending, err := s.database.Digests.ListPendingByGroup(groupID)
	if err != nil {
		return fmt.Errorf("resume pending: list group %d: %w", groupID, err)
	}
	for _, item := range pending {
		result := &Digest{
			GroupID: groupID, PostCount: item.PostCount, Text: item.MessageText,
			Outcome: OutcomeDeliveryFailed, Message: "Повторная отправка дайджеста.",
		}
		receipt, deliverErr := s.delivery.Deliver(context.Background(), groupID, result)
		if deliverErr != nil {
			return fmt.Errorf("resume pending digest %d: %w", item.ID, deliverErr)
		}
		if err := s.database.Digests.MarkSent(item.ID, receipt.MessageID); err != nil {
			return fmt.Errorf("resume pending digest %d checkpoint: %w", item.ID, err)
		}
	}
	return nil
}

func (s *Service) notifyDigestOutcome(result *Digest) {
	if s == nil || s.notifier == nil || result == nil {
		return
	}
	message := result.Message
	if len(result.FailureDetails) > 0 {
		message += " Не удалось обработать: " + strings.Join(result.FailureDetails, "; ") + "."
	} else if len(result.FailedChannels) > 0 {
		message += " Не удалось обработать: " + strings.Join(result.FailedChannels, ", ") + "."
	}
	if result.Outcome == OutcomeDeliveryFailed && result.SummariesSaved {
		message += " Сводки сохранены, но не доставлены."
	}
	if err := s.notifier.NotifyOwner(context.Background(), message); err != nil {
		log.Printf("failed to notify owner about digest outcome %s for group %d: %v", result.Outcome, result.GroupID, err)
	}
}

func failedChannelNames(batch parser.ChannelBatchResult) []string {
	names := make([]string, 0, len(batch.Failures))
	for _, failure := range batch.Failures {
		name := strings.TrimSpace(failure.Channel.Username)
		if name != "" {
			names = append(names, "@"+name)
		}
	}
	return names
}

func failedChannelDetails(batch parser.ChannelBatchResult) []string {
	details := make([]string, 0, len(batch.Failures))
	for _, failure := range batch.Failures {
		name := strings.TrimSpace(failure.Channel.Username)
		if name == "" {
			name = "неизвестный канал"
		} else {
			name = "@" + name
		}
		reason := safeDigestMessage(failure.Err)
		if reason == "" {
			reason = "причина не указана"
		}
		details = append(details, name+": "+reason)
	}
	return details
}

func optionalInt64(value int64) *int64 {
	if value == 0 {
		return nil
	}
	return &value
}

func safeDigestMessage(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 300 {
		return message[:300]
	}
	return message
}

func terminalDigestError(result *Digest, err error, manual bool) error {
	if err != nil && errors.Is(err, telegram.ErrTokenRevoked) {
		return err
	}
	if manual || result == nil || result.Outcome == OutcomeNoPosts || result.Outcome == OutcomePartial {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New(result.Message)
}

func (s *Service) formatDigestMessage(groupID int64, posts []model.Post) string {
	var builder strings.Builder
	builder.WriteString("📋 ")
	builder.WriteString(s.groupTitle(groupID))
	builder.WriteString("\n\n")
	for _, post := range posts {
		summary := post.Summary
		if refreshed, err := s.database.Posts.GetByID(post.ID); err == nil {
			summary = refreshed.Summary
		}
		text := post.Text
		if summary != nil && strings.TrimSpace(*summary) != "" {
			text = *summary
		}
		builder.WriteString("• ")
		builder.WriteString(strings.TrimSpace(text))
		if strings.TrimSpace(post.URL) != "" {
			builder.WriteString(" (")
			builder.WriteString(post.URL)
			builder.WriteString(")")
		}
		builder.WriteString("\n")
	}
	return strings.TrimSpace(builder.String())
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
//
// A manual invocation always creates a new window. Callers coordinating
// several affected groups use GenerateManualWithWindow to share one ID.
func (s *Service) GenerateManual(groupID int64) (*Digest, error) {
	return s.generate(groupID, NewWindowID("manual"), false)
}

// GenerateManualWithWindow runs one group as part of an explicit manual
// digest window shared by all groups in that invocation.
func (s *Service) GenerateManualWithWindow(groupID int64, windowID string) (*Digest, error) {
	return s.generate(groupID, firstWindowID([]string{windowID}, NewWindowID("manual")), false)
}

// GenerateManualResult is the typed manual WebApp entry point. Terminal
// outcomes are returned as data so polling clients can distinguish expected
// digest failures from transport errors.
func (s *Service) GenerateManualResult(groupID int64) (*Digest, error) {
	return s.generate(groupID, NewWindowID("manual"), true)
}

// GenerateManualResultWithWindow coordinates typed outcomes across a shared
// manual digest window.
func (s *Service) GenerateManualResultWithWindow(groupID int64, windowID string) (*Digest, error) {
	return s.generate(groupID, firstWindowID([]string{windowID}, NewWindowID("manual")), true)
}

func (s *Service) summarize(groupID int64, posts []model.Post) error {
	return s.summarizeWithWindow(groupID, posts, NewWindowID("scheduled"))
}

func (s *Service) summarizeWithWindow(groupID int64, posts []model.Post, windowID string) error {
	if s.aiConfigSource == nil || len(posts) == 0 {
		return nil
	}
	windowID = firstWindowID([]string{windowID}, NewWindowID("scheduled"))
	if s.providerFactory == nil {
		err := errors.New("provider factory is not configured")
		s.notifyAIFailureForWindow(windowID, groupID, err)
		return fmt.Errorf("summarize group %d: %w", groupID, err)
	}

	var fallbackErr error
	provider, err := s.providerFactory(s.aiConfigSource, groupID, s.providerHTTPClient, func(providerErr error) {
		fallbackErr = providerErr
	})
	if err != nil {
		s.notifyAIFailureForWindow(windowID, groupID, err)
		return fmt.Errorf("summarize group %d: create provider: %w", groupID, err)
	}
	input := make([]summarizer.Post, 0, len(posts))
	for _, post := range posts {
		input = append(input, summarizer.Post{ID: post.ID, Text: post.Text})
	}
	summaryContext, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if checker, ok := provider.(summarizer.CreditChecker); ok {
		credits, creditErr := checker.CheckCredits(summaryContext)
		if creditErr != nil {
			// The balance endpoint is auxiliary. If it is unavailable, let
			// the completion request decide whether the provider is usable.
			log.Printf("OpenRouter credit check failed for group %d: %v", groupID, creditErr)
		} else if credits.Credits < s.creditMinimumOrDefault() {
			err := fmt.Errorf("OpenRouter credits exhausted: %.2f", credits.Credits)
			s.notifyAIFailureForWindow(windowID, groupID, err)
			return fmt.Errorf("summarize group %d: %w", groupID, err)
		} else if credits.Credits < s.creditWarningOrDefault() {
			_ = s.notifyAI(groupID, fmt.Sprintf(
				"⚠️ Баланс OpenRouter низкий: $%.2f. Пополните баланс для продолжения работы.",
				credits.Credits,
			))
		}
	}
	summaries, err := provider.Summarize(summaryContext, input)
	if err != nil {
		s.notifyAIFailureForWindow(windowID, groupID, err)
		return fmt.Errorf("summarize group %d: %w", groupID, err)
	}
	if err := validateSummaryBatch(posts, summaries); err != nil {
		s.notifyAIFailureForWindow(windowID, groupID, err)
		return fmt.Errorf("summarize group %d: %w", groupID, err)
	}
	if fallbackErr != nil {
		s.notifyAIFallback(groupID)
	}

	for _, summary := range summaries {
		if err := s.database.Posts.UpdateSummary(summary.PostID, summary.Text); err != nil {
			return fmt.Errorf("store summary for post %d: %w", summary.PostID, err)
		}
	}
	return nil
}

func validateSummaryBatch(posts []model.Post, summaries []summarizer.Summary) error {
	if len(summaries) != len(posts) {
		return fmt.Errorf("summary result count mismatch: received %d summaries for %d posts", len(summaries), len(posts))
	}

	expected := make(map[int64]struct{}, len(posts))
	for _, post := range posts {
		if _, duplicate := expected[post.ID]; duplicate {
			return fmt.Errorf("summary result contains duplicate input post %d", post.ID)
		}
		expected[post.ID] = struct{}{}
	}

	seen := make(map[int64]struct{}, len(summaries))
	for _, summary := range summaries {
		if _, ok := expected[summary.PostID]; !ok {
			return fmt.Errorf("summary result contains unknown post %d", summary.PostID)
		}
		if _, duplicate := seen[summary.PostID]; duplicate {
			return fmt.Errorf("summary result contains duplicate summary for post %d", summary.PostID)
		}
		if strings.TrimSpace(summary.Text) == "" {
			return fmt.Errorf("summary result contains empty summary for post %d", summary.PostID)
		}
		seen[summary.PostID] = struct{}{}
	}
	return nil
}

func (s *Service) notifyAIFallback(groupID int64) {
	group := s.groupTitle(groupID)
	message := fmt.Sprintf(
		"⚠️ Провайдер AI для группы «%s» временно недоступен, поэтому использован OpenRouter. Проверьте провайдера и повторите следующий цикл.",
		group,
	)
	_ = s.notifyAI(groupID, message)
}

func (s *Service) notifyAIFailure(groupID int64, err error) {
	s.notifyAIFailureForWindow("", groupID, err)
}

func (s *Service) notifyAIFailureForWindow(windowID string, groupID int64, err error) {
	class := classifyAIFailure(err)
	if class == aiFailureOpenRouterOutage {
		windowID = firstWindowID([]string{windowID}, NewWindowID("scheduled"))
	}
	key, claimed := s.claimAIFailureNotification(windowID, class)
	if !claimed {
		return
	}
	group := s.groupTitle(groupID)
	message := fmt.Sprintf("❌ %s", formatAIFailure(class, group, err))
	if strings.TrimSpace(windowID) != "" {
		message = fmt.Sprintf("%s Окно дайджеста: %s.", message, windowID)
	}
	if deliverErr := s.notifyAI(groupID, message); deliverErr != nil {
		// Release the transient claim so a later group or retry can attempt delivery.
		s.releaseAIFailureNotification(key)
		return
	}
	s.commitAIFailureNotification(key)
}

// claimAIFailureNotification atomically claims delivery for an outage window/category.
// Non-outage failures are always claimed (no dedup). Concurrent outage attempts for
// the same key wait for the active delivery to finish, then retry if that delivery
// failed, while delivered keys stay suppressed.
func (s *Service) claimAIFailureNotification(windowID string, class aiFailureClass) (string, bool) {
	if class != aiFailureOpenRouterOutage {
		// Non-outage classes are not window-deduplicated; use a unique key so
		// commit/release are no-ops for those paths.
		return "", true
	}
	if s == nil || s.notifier == nil {
		return "", false
	}

	windowID = strings.TrimSpace(windowID)
	if windowID == "" {
		windowID = NewWindowID("scheduled")
	}
	key := windowID + "|" + string(class)
	for {
		s.notificationMu.Lock()
		if s.notificationKeys == nil {
			s.notificationKeys = make(map[string]notificationKeyState)
		}
		if s.notificationWaiters == nil {
			s.notificationWaiters = make(map[string]chan struct{})
		}
		switch s.notificationKeys[key] {
		case notificationKeyDelivered:
			s.notificationMu.Unlock()
			return key, false
		case notificationKeyClaimed:
			waiter := s.notificationWaiters[key]
			if waiter == nil {
				waiter = make(chan struct{})
				s.notificationWaiters[key] = waiter
			}
			waitObserved := s.notificationWaitObserved
			s.notificationMu.Unlock()
			if waitObserved != nil {
				waitObserved <- struct{}{}
			}
			<-waiter
		default:
			s.notificationKeys[key] = notificationKeyClaimed
			s.notificationWaiters[key] = make(chan struct{})
			s.notificationMu.Unlock()
			return key, true
		}
	}
}

func (s *Service) commitAIFailureNotification(key string) {
	if s == nil || key == "" {
		return
	}
	s.notificationMu.Lock()
	defer s.notificationMu.Unlock()
	if s.notificationKeys == nil {
		s.notificationKeys = make(map[string]notificationKeyState)
	}
	s.notificationKeys[key] = notificationKeyDelivered
	if waiter := s.notificationWaiters[key]; waiter != nil {
		close(waiter)
		delete(s.notificationWaiters, key)
	}
}

func (s *Service) releaseAIFailureNotification(key string) {
	if s == nil || key == "" {
		return
	}
	s.notificationMu.Lock()
	defer s.notificationMu.Unlock()
	if s.notificationKeys == nil {
		return
	}
	// Only release a transient claim; never clear a successfully delivered key.
	if s.notificationKeys[key] == notificationKeyClaimed {
		delete(s.notificationKeys, key)
		if waiter := s.notificationWaiters[key]; waiter != nil {
			close(waiter)
			delete(s.notificationWaiters, key)
		}
	}
}

func firstWindowID(windowIDs []string, fallback string) string {
	if len(windowIDs) > 0 && strings.TrimSpace(windowIDs[0]) != "" {
		return strings.TrimSpace(windowIDs[0])
	}
	return fallback
}

// NewWindowID creates a unique correlation ID for one logical digest window.
// It intentionally does not truncate or otherwise derive the ID from the
// wall-clock minute, so adjacent windows cannot suppress one another.
func NewWindowID(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "digest"
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err == nil {
		return fmt.Sprintf("%s-%x", kind, random)
	}
	return fmt.Sprintf("%s-%d", kind, time.Now().UTC().UnixNano())
}

func classifyAIFailure(err error) aiFailureClass {
	if err == nil {
		return aiFailureProvider
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "credits exhausted") ||
		strings.Contains(lower, "баланс openrouter исчерпан") {
		return aiFailureCreditExhausted
	}
	if strings.Contains(lower, "parse summaries") ||
		strings.Contains(lower, "expected json") ||
		strings.Contains(lower, "summary response") ||
		strings.Contains(lower, "summary result") ||
		strings.Contains(lower, "not in russian") ||
		strings.Contains(lower, "one sentence") {
		return aiFailureParse
	}

	var providerErr *summarizer.ProviderError
	if errors.As(err, &providerErr) {
		if (strings.EqualFold(providerErr.Provider, "OpenRouter") || strings.Contains(lower, "openrouter")) &&
			(strings.Contains(lower, "timeout") ||
				strings.Contains(lower, "deadline exceeded") ||
				strings.Contains(lower, "connection refused") ||
				strings.Contains(lower, "connection reset") ||
				strings.Contains(lower, "network is unreachable")) {
			return aiFailureOpenRouterOutage
		}
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
		return fmt.Sprintf("OpenRouter недоступен. Дайджесты не могут быть созданы. Провайдер: OpenRouter. Ошибка: %s. Дайджест группы «%s» пропущен после исчерпания повторных попыток; проверьте статус OpenRouter и повторите позже.", providerStatusDetail(err), group)
	case aiFailureTransientExhausted:
		return fmt.Sprintf("Временная ошибка AI для группы «%s» (%s) не устранена после повторных попыток; дайджест пропущен, повторите запуск позже.", group, providerStatusDetail(err))
	case aiFailurePermanent:
		return fmt.Sprintf("Ошибка AI провайдера для группы «%s» (%s). Дайджест пропущен; проверьте API ключ, доступ и баланс.", group, safeProviderStatus(err))
	case aiFailureCreditExhausted:
		return fmt.Sprintf("Баланс OpenRouter исчерпан для группы «%s» (%s). Дайджест пропущен; пополните баланс и повторите запуск.", group, providerStatusDetail(err))
	default:
		return fmt.Sprintf("Ошибка AI провайдера для группы «%s»; дайджест пропущен. Проверьте настройки провайдера и повторите запуск.", group)
	}
}

func (s *Service) creditWarningOrDefault() float64 {
	if s == nil || s.creditWarning <= 0 {
		return defaultCreditWarningThreshold
	}
	return s.creditWarning
}

func (s *Service) creditMinimumOrDefault() float64 {
	if s == nil || s.creditMinimum < 0 {
		return defaultCreditMinimumThreshold
	}
	return s.creditMinimum
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
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "connection refused"):
		return "соединение отклонено"
	case strings.Contains(lower, "connection reset"):
		return "соединение сброшено"
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded"):
		return "тайм-аут"
	case strings.Contains(lower, "network is unreachable"):
		return "сеть недоступна"
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

func (s *Service) notifyAI(groupID int64, message string) error {
	if s == nil || s.notifier == nil {
		return nil
	}
	if err := s.notifier.NotifyOwner(context.Background(), message); err != nil {
		// The provider error remains the actionable digest failure; a transport
		// failure must not replace it or cause a retry to invoke the provider.
		log.Printf("failed to notify owner about AI digest group %d: %v", groupID, err)
		return err
	}
	return nil
}
