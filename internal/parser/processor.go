package parser

import (
	"context"
	"errors"
	"fmt"
	applog "github.com/boss/tg-channel-summary-by-ai/internal/log"
	"net/http"
	"strings"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

var (
	errPostValidation = errors.New("post validation failed")
	errPostStorage    = errors.New("post storage failed")
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
	ChannelTitle     string
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

const (
	FetchErrorKindNotFound    = "not_found"
	FetchErrorKindPrivate     = "private"
	FetchErrorKindUnavailable = "unavailable"
	FetchErrorKindRateLimited = "rate_limited"
	FetchErrorKindCloudflare  = "cloudflare_challenge"
	// FetchErrorKindCloudflareChallenge is the descriptive alias used by
	// callers that want the full classification name.
	FetchErrorKindCloudflareChallenge = FetchErrorKindCloudflare
	FetchErrorKindFetch               = "fetch"
)

// ChannelFailure records a channel that could not be fetched or stored.
type ChannelFailure struct {
	Channel    model.Channel
	Err        error
	HTTPStatus int
}

// ChannelBatchResult describes a best-effort batch across assigned channels.
type ChannelBatchResult struct {
	Results                 []ChannelProcessResult
	Failures                []ChannelFailure
	// ProcessingErrors contains post-validation and post-storage diagnostics.
	// These errors are intentionally separate from Failures because they do
	// not mean that the channel fetch failed and must not drive fetch recovery.
	ProcessingErrors        []ChannelFailure
	FailureNotificationSent bool
}

// OwnerNotifier is the dependency-injected transport for owner alerts. The
// parser package intentionally depends on this small interface rather than on
// the bot package.
type OwnerNotifier interface {
	NotifyOwner(ctx context.Context, text string) error
}

// Sleeper abstracts retry delays so callers can make fetch retries
// deterministic without waiting in tests.
type Sleeper interface {
	Sleep(context.Context, time.Duration) error
}

type contextSleeper struct{}

func (contextSleeper) Sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ChannelFetchErrorStore persists and clears the durable fetch state attached
// to a channel. It is optional so parser tests and alternate storage adapters
// can continue to implement only cursor persistence.
type ChannelFetchErrorStore interface {
	MarkFetchError(id int64, kind, message string) error
	ClearFetchError(id int64) error
}

// ChannelProcessor connects t.me/s parsing to persistent post storage.
type ChannelProcessor struct {
	fetcher    ChannelFetcher
	storage    *PostStorage
	notifier   OwnerNotifier
	maxRetries int
	sleeper    Sleeper
}

// NewChannelProcessor creates the production parser-to-storage adapter.
func NewChannelProcessor(fetcher ChannelFetcher, storage *PostStorage, notifiers ...OwnerNotifier) *ChannelProcessor {
	var notifier OwnerNotifier
	if len(notifiers) > 0 {
		notifier = notifiers[0]
	}
	return &ChannelProcessor{
		fetcher:    fetcher,
		storage:    storage,
		notifier:   notifier,
		maxRetries: 3,
		sleeper:    contextSleeper{},
	}
}

// WithMaxRetries sets the maximum number of retries after the initial fetch
// attempt. Values less than one are treated as one.
func (p *ChannelProcessor) WithMaxRetries(maxRetries int) *ChannelProcessor {
	if p == nil {
		return p
	}
	if maxRetries < 1 {
		maxRetries = 1
	}
	p.maxRetries = maxRetries
	return p
}

// WithSleeper injects the delay implementation used between fetch attempts.
// A nil sleeper restores the production context-aware sleeper.
func (p *ChannelProcessor) WithSleeper(sleeper Sleeper) *ChannelProcessor {
	if p == nil {
		return p
	}
	if sleeper == nil {
		sleeper = contextSleeper{}
	}
	p.sleeper = sleeper
	return p
}

// ProcessChannel fetches a channel, validates required post fields, stores new
// posts, skips duplicates, and advances the channel cursor through PostStorage.
func (p *ChannelProcessor) ProcessChannel(channel *model.Channel) (ChannelProcessResult, error) {
	return p.processChannel(context.Background(), channel)
}

func (p *ChannelProcessor) processChannel(ctx context.Context, channel *model.Channel) (ChannelProcessResult, error) {
	result, err := p.processChannelAttempt(ctx, channel)
	if err == nil {
		return result, nil
	}
	result, err = p.retryFetch(ctx, channel, 0, result, err)
	if err == nil {
		return result, nil
	}
	if !isDurableFetchError(err) {
		return result, err
	}
	if persistErr := p.persistFetchError(channel, err); persistErr != nil {
		return result, persistErr
	}
	p.notifyChannelFailure(ctx, *channel, fetchErrorKind(err), err)
	return result, err
}

func (p *ChannelProcessor) processChannelAttempt(ctx context.Context, channel *model.Channel) (ChannelProcessResult, error) {
	if p == nil || p.fetcher == nil || p.storage == nil {
		return ChannelProcessResult{}, errors.New("process channel: parser and storage are required")
	}
	if channel == nil {
		return ChannelProcessResult{}, errors.New("process channel: channel is required")
	}
	previouslyPopulated := channel.LastPostID > 0

	posts, stats, err := p.parse(channel.Username)
	if err != nil {
		kind := fetchErrorKind(err)
		channel.FetchErrorKind = kind
		channel.FetchErrorMessage = err.Error()
		timestamp := time.Now().UTC().Format(time.RFC3339Nano)
		channel.FetchErrorAt = &timestamp
		result := ChannelProcessResult{
			Channel:    *channel,
			HTTPStatus: stats.HTTPStatus,
		}
		return result, fmt.Errorf("process channel %q: %w", channel.Username, err)
	}
	for _, post := range posts {
		if strings.TrimSpace(post.PostedAt) == "" {
			return ChannelProcessResult{}, fmt.Errorf(
				"process channel %q post %d: %w: missing posted_at timestamp",
				channel.Username, post.MessageID, errPostValidation,
			)
		}
	}
	stored, err := p.storage.Store(channel, posts)
	if err != nil {
		return ChannelProcessResult{}, fmt.Errorf(
			"process channel %q: %w",
			channel.Username, errors.Join(errPostStorage, err),
		)
	}
	if errorStore, ok := p.storage.channels.(ChannelFetchErrorStore); ok {
		if err := errorStore.ClearFetchError(channel.ID); err != nil {
			return ChannelProcessResult{}, fmt.Errorf(
				"process channel %q: clear fetch error: %w",
				channel.Username, errors.Join(errPostStorage, err),
			)
		}
	}
	channel.FetchErrorKind = ""
	channel.FetchErrorMessage = ""
	channel.FetchErrorAt = nil
	return ChannelProcessResult{
		Channel:             *channel,
		ParsedPosts:         len(posts),
		StoredPosts:         len(stored),
		MediaOnlySkipped:    stats.MediaOnlySkipped,
		HTTPStatus:          stats.HTTPStatus,
		PreviouslyPopulated: previouslyPopulated,
	}, nil
}

func (p *ChannelProcessor) retryFetch(ctx context.Context, channel *model.Channel, retries int, result ChannelProcessResult, err error) (ChannelProcessResult, error) {
	if !isRetryableFetchError(err) {
		return result, err
	}
	maxRetries := p.maxRetries
	if maxRetries < 1 {
		maxRetries = 1
	}
	for retries < maxRetries {
		sleeper := p.sleeper
		if sleeper == nil {
			sleeper = contextSleeper{}
		}
		if sleepErr := sleeper.Sleep(ctx, retryDelay(err, retries)); sleepErr != nil {
			return result, fmt.Errorf("process channel %q: wait before retry: %w", channel.Username, sleepErr)
		}
		retries++
		result, err = p.processChannelAttempt(ctx, channel)
		if err == nil {
			return result, nil
		}
		if !isRetryableFetchError(err) {
			return result, err
		}
	}
	return result, err
}

func retryDelay(err error, retry int) time.Duration {
	var rateLimitErr *RateLimitError
	if errors.As(err, &rateLimitErr) {
		return rateLimitErr.RetryAfter
	}
	return fetchRetryBackoff(retry)
}

func fetchRetryBackoff(retry int) time.Duration {
	switch retry {
	case 0:
		return 5 * time.Second
	case 1:
		return 10 * time.Second
	default:
		return 20 * time.Second
	}
}

func isRetryableFetchError(err error) bool {
	if err == nil {
		return false
	}
	var rateLimitErr *RateLimitError
	return errors.As(err, &rateLimitErr) ||
		errors.Is(err, ErrChannelNotFound) ||
		errors.Is(err, ErrChannelPrivate) ||
		errors.Is(err, ErrChannelUnavailable) ||
		errors.Is(err, ErrCloudflareChallenge)
}

func isDurableFetchError(err error) bool {
	if err == nil {
		return false
	}
	return !errors.Is(err, errPostValidation) && !errors.Is(err, errPostStorage)
}

func fetchErrorKind(err error) string {
	var rateLimitErr *RateLimitError
	switch {
	case errors.Is(err, ErrChannelNotFound):
		return FetchErrorKindNotFound
	case errors.Is(err, ErrChannelPrivate):
		return FetchErrorKindPrivate
	case errors.Is(err, ErrChannelUnavailable):
		return FetchErrorKindUnavailable
	case errors.Is(err, ErrCloudflareChallenge):
		return FetchErrorKindCloudflare
	case errors.As(err, &rateLimitErr):
		return FetchErrorKindRateLimited
	default:
		return FetchErrorKindFetch
	}
}

func (p *ChannelProcessor) persistFetchError(channel *model.Channel, err error) error {
	if p == nil || p.storage == nil || channel == nil || err == nil {
		return nil
	}
	kind := fetchErrorKind(err)
	channel.FetchErrorKind = kind
	channel.FetchErrorMessage = err.Error()
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	channel.FetchErrorAt = &timestamp
	errorStore, ok := p.storage.channels.(ChannelFetchErrorStore)
	if !ok {
		return nil
	}
	if persistErr := errorStore.MarkFetchError(channel.ID, kind, err.Error()); persistErr != nil {
		return fmt.Errorf("process channel %q, persist fetch error: %w", channel.Username, persistErr)
	}
	return nil
}

func (p *ChannelProcessor) notifyChannelFailure(ctx context.Context, channel model.Channel, kind string, err error) {
	if p.notifier == nil {
		return
	}

	message := channelFailureNotification(channel.Username, kind)
	applog.Printf("WARNING: channel @%s fetch failed (%s): %v", channel.Username, kind, err)
	if notifyErr := p.notifier.NotifyOwner(ctx, message); notifyErr != nil {
		applog.Printf("WARNING: failed to notify owner about channel @%s: %v", channel.Username, notifyErr)
	}
}

func channelFailureNotification(username, kind string) string {
	switch kind {
	case FetchErrorKindNotFound:
		return fmt.Sprintf(
			"⚠️ Канал @%s не найден. Возможно, канал был переименован или удалён. Предыдущий username: @%s. Проверьте и обновите username канала в настройках.",
			username, username,
		)
	case FetchErrorKindPrivate:
		return fmt.Sprintf(
			"⚠️ Канал @%s стал приватным или недоступен для предпросмотра. Проверьте доступность канала и обновите username или настройки канала.",
			username,
		)
	case FetchErrorKindUnavailable:
		return fmt.Sprintf(
			"⚠️ Канал @%s недоступен, но Telegram не сообщил точную причину. Проверьте доступность канала и при необходимости обновите username в настройках.",
			username,
		)
	case FetchErrorKindCloudflare:
		return fmt.Sprintf(
			"⚠️ Канал @%s временно закрыт защитной страницей Cloudflare. Проверьте доступность канала и повторите запуск позже.",
			username,
		)
	case FetchErrorKindRateLimited:
		return fmt.Sprintf(
			"⚠️ Канал @%s ограничен Telegram (HTTP 429). Повторите запуск позже и проверьте доступность канала.",
			username,
		)
	default:
		return fmt.Sprintf(
			"⚠️ Не удалось получить канал @%s: %s. Проверьте доступность канала и настройки.",
			username, kind,
		)
	}
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
		Results:          make([]ChannelProcessResult, 0, len(channels)),
		Failures:         make([]ChannelFailure, 0),
		ProcessingErrors: make([]ChannelFailure, 0),
	}
	type channelOutcome struct {
		result ChannelProcessResult
		err    error
	}
	type pendingRetry struct {
		index int
	}
	outcomes := make([]channelOutcome, len(channels))
	pending := make([]pendingRetry, 0)
	for i := range channels {
		result, err := p.processChannelAttempt(ctx, &channels[i])
		outcomes[i] = channelOutcome{result: result, err: err}
		if isRetryableFetchError(err) {
			pending = append(pending, pendingRetry{index: i})
		}
	}
	for _, retry := range pending {
		channel := &channels[retry.index]
		outcome := outcomes[retry.index]
		outcome.result, outcome.err = p.retryFetch(ctx, channel, 0, outcome.result, outcome.err)
		outcomes[retry.index] = outcome
	}
	for i, outcome := range outcomes {
		if outcome.err == nil {
			batch.Results = append(batch.Results, outcome.result)
			continue
		}
		if isDurableFetchError(outcome.err) {
			if persistErr := p.persistFetchError(&channels[i], outcome.err); persistErr != nil {
				outcome.err = persistErr
				outcomes[i] = outcome
			}
			batch.Failures = append(batch.Failures, ChannelFailure{
				Channel:    channels[i],
				Err:        outcome.err,
				HTTPStatus: outcome.result.HTTPStatus,
			})
			continue
		}
		batch.ProcessingErrors = append(batch.ProcessingErrors, ChannelFailure{
			Channel:    channels[i],
			Err:        outcome.err,
			HTTPStatus: outcome.result.HTTPStatus,
		})
	}
	batch.FailureNotificationSent = p.notifyExhaustedFailures(ctx, batch.Failures)
	p.notifyStructuralChange(ctx, batch)
	return batch, nil
}

func (p *ChannelProcessor) notifyExhaustedFailures(ctx context.Context, failures []ChannelFailure) bool {
	if len(failures) == 0 || p.notifier == nil {
		return false
	}
	durableFailures := make([]ChannelFailure, 0, len(failures))
	for _, failure := range failures {
		if isDurableFetchError(failure.Err) {
			durableFailures = append(durableFailures, failure)
		}
	}
	if len(durableFailures) == 0 {
		return false
	}
	failures = durableFailures
	allRateLimited := true
	usernames := make([]string, 0, len(failures))
	for _, failure := range failures {
		usernames = append(usernames, "@"+failure.Channel.Username)
		if fetchErrorKind(failure.Err) != FetchErrorKindRateLimited {
			allRateLimited = false
		}
	}
	if allRateLimited {
		return p.notifyRateLimitPartial(ctx, failures)
	}
	details := make([]string, 0, len(failures))
	for _, failure := range failures {
		details = append(details, fmt.Sprintf("@%s (%s)", failure.Channel.Username, fetchErrorKind(failure.Err)))
	}
	message := fmt.Sprintf(
		"⚠️ частичный дайджест: канал(ы) %s не удалось обработать после исчерпания %d повторных попыток. Причины: %s. Посты из этих каналов не включены. Проверьте доступность каналов и обновите настройки.",
		strings.Join(usernames, ", "), p.maxRetries, strings.Join(details, ", "),
	)
	applog.Printf("WARNING: partial digest after exhausted channel failures: %s", strings.Join(usernames, ", "))
	if err := p.notifier.NotifyOwner(ctx, message); err != nil {
		applog.Printf("WARNING: failed to notify owner about exhausted channel failures: %v", err)
		return false
	}
	return true
}

func (p *ChannelProcessor) notifyRateLimitPartial(ctx context.Context, failures []ChannelFailure) bool {
	if len(failures) == 0 || p.notifier == nil {
		return false
	}
	usernames := make([]string, 0, len(failures))
	for _, failure := range failures {
		usernames = append(usernames, "@"+failure.Channel.Username)
	}
	message := fmt.Sprintf(
		"⚠️ частичный дайджест: канал(ы) %s ограничены Telegram (HTTP 429) после исчерпания %d повторных попыток. Посты из этих каналов не включены. Проверьте доступность каналов и повторите запуск позже.",
		strings.Join(usernames, ", "), p.maxRetries,
	)
	applog.Printf("WARNING: partial digest after rate-limited channels: %s", strings.Join(usernames, ", "))
	if err := p.notifier.NotifyOwner(ctx, message); err != nil {
		applog.Printf("WARNING: failed to notify owner about rate-limited channels: %v", err)
		return false
	}
	return true
}

func (p *ChannelProcessor) notifyStructuralChange(ctx context.Context, batch ChannelBatchResult) {
	if !batch.StructuralChangeDetected() {
		return
	}

	message := fmt.Sprintf(
		"⚠️ Возможно, Telegram изменил структуру t.me/s. Посты не извлекаются из %d каналов, ранее содержавших публикации. Проверьте парсер.",
		batch.PreviouslyPopulatedCount(),
	)
	applog.Printf("WARNING: Possible t.me/s HTML structure change - 0 posts extracted from %d channels that previously had content.", batch.PreviouslyPopulatedCount())
	if p.notifier == nil {
		return
	}
	if err := p.notifier.NotifyOwner(ctx, message); err != nil {
		applog.Printf("WARNING: failed to notify owner about parser structure change: %v", err)
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
