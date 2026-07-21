package bot

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
)

const (
	deliveryMaxAttempts       = 4
	deliveryQueueNotifyAt     = 50
	deliveryQueueCapacity     = 100
	deliveryRetryBase         = time.Second
	deliveryChatThrottle      = time.Second
	deliveryDefaultRetryAfter = time.Second
)

type deliveryJob struct {
	ctx    context.Context
	params *telego.SendMessageParams
	result chan deliveryResult
}

type deliveryResult struct {
	message *telego.Message
	err     error
}

// sendMessage serializes outbound Telegram messages through one global queue.
// Telegram's global 429 blocks all chats, so a rate-limited head-of-line job
// deliberately holds the queue until its retry_after delay has elapsed.
func (s *Service) sendMessage(ctx context.Context, params *telego.SendMessageParams) (*telego.Message, error) {
	if s == nil || s.api == nil {
		return nil, errors.New("bot service is not configured")
	}
	if params == nil {
		return nil, errors.New("Telegram message parameters are required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	job := deliveryJob{
		ctx:    ctx,
		params: cloneSendMessageParams(params),
		result: make(chan deliveryResult, 1),
	}
	s.deliveryQueueMu.Lock()
	if len(s.deliveryQueue) >= deliveryQueueCapacity {
		s.deliveryQueueMu.Unlock()
		return nil, fmt.Errorf("Telegram delivery queue is full (%d messages)", deliveryQueueCapacity)
	}
	s.deliveryQueue = append(s.deliveryQueue, job)
	queueSize := len(s.deliveryQueue)
	if queueSize >= deliveryQueueNotifyAt && !s.deliveryQueueNotified {
		s.deliveryQueueNotified = true
		go s.notifyDeliveryQueue(queueSize)
	}
	s.ensureDeliveryWorkerLocked()
	s.deliveryQueueMu.Unlock()

	select {
	case result := <-job.result:
		return result.message, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) ensureDeliveryWorkerLocked() {
	if s.deliveryQueueRunning {
		return
	}
	s.deliveryQueueRunning = true
	go s.runDeliveryQueue()
}

func (s *Service) runDeliveryQueue() {
	for {
		s.deliveryQueueMu.Lock()
		if len(s.deliveryQueue) == 0 {
			s.deliveryQueueRunning = false
			s.deliveryQueueMu.Unlock()
			return
		}
		job := s.deliveryQueue[0]
		s.deliveryQueue = s.deliveryQueue[1:]
		s.deliveryQueueMu.Unlock()

		if err := job.ctx.Err(); err != nil {
			job.result <- deliveryResult{err: job.ctx.Err()}
			continue
		}

		chatID := job.params.ChatID.ID
		if err := s.waitForChatThrottle(job.ctx, chatID); err != nil {
			job.result <- deliveryResult{err: err}
			continue
		}
		message, err := s.sendMessageWithRetry(job.ctx, job.params)
		if err == nil {
			s.recordChatSend(chatID)
		}
		job.result <- deliveryResult{message: message, err: err}

		s.deliveryQueueMu.Lock()
		if len(s.deliveryQueue) < deliveryQueueNotifyAt {
			s.deliveryQueueNotified = false
		}
		s.deliveryQueueMu.Unlock()
	}
}

func (s *Service) sendMessageWithRetry(ctx context.Context, params *telego.SendMessageParams) (*telego.Message, error) {
	var lastErr error
	for attempt := 0; attempt < deliveryMaxAttempts; attempt++ {
		message, err := s.api.SendMessage(ctx, params)
		if err == nil {
			return message, nil
		}
		classified := s.classifyTelegramError(err)
		lastErr = classified
		if isClosedTopicError(classified) {
			s.notifyClosedTopic(ctx, params.ChatID.ID, classified)
			return nil, classified
		}
		if !isTransientDeliveryError(err) || attempt == deliveryMaxAttempts-1 {
			return nil, classified
		}
		delay := deliveryRetryBase * time.Duration(1<<attempt)
		if retryAfter := telegramRetryAfter(err); retryAfter > 0 {
			delay = retryAfter
		}
		if err := s.sleepDelivery(ctx, delay); err != nil {
			return nil, fmt.Errorf("wait before Telegram retry: %w", err)
		}
	}
	return nil, lastErr
}

func (s *Service) sleepDelivery(ctx context.Context, delay time.Duration) error {
	if s.deliverySleeper != nil {
		return s.deliverySleeper(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) waitForChatThrottle(ctx context.Context, chatID int64) error {
	s.deliveryQueueMu.Lock()
	last := s.deliveryLastChatSend[chatID]
	throttle := s.deliveryChatThrottle
	s.deliveryQueueMu.Unlock()
	if throttle <= 0 || last.IsZero() {
		return nil
	}
	delay := time.Until(last.Add(throttle))
	if delay <= 0 {
		return nil
	}
	return s.sleepDelivery(ctx, delay)
}

func (s *Service) recordChatSend(chatID int64) {
	s.deliveryQueueMu.Lock()
	defer s.deliveryQueueMu.Unlock()
	if s.deliveryLastChatSend == nil {
		s.deliveryLastChatSend = make(map[int64]time.Time)
	}
	s.deliveryLastChatSend[chatID] = time.Now()
}

func (s *Service) notifyDeliveryQueue(size int) {
	if s == nil || s.notifier == nil {
		return
	}
	if err := s.notifier.NotifyOwner(context.Background(), fmt.Sprintf(
		"⚠️ Задержка отправки дайджестов из-за ограничения Telegram. %d сообщений в очереди.",
		size,
	)); err != nil {
		s.logf("notify Telegram delivery queue: %v", err)
	}
}

func (s *Service) notifyClosedTopic(ctx context.Context, chatID int64, err error) {
	if s == nil || s.notifier == nil {
		return
	}
	message := fmt.Sprintf(
		"⚠️ Не удалось отправить дайджест в группу %d: топик закрыт или удалён. Проверьте настройки топика. Ошибка: %s",
		chatID, safeDeliveryError(err),
	)
	if notifyErr := s.notifier.NotifyOwner(ctx, message); notifyErr != nil {
		s.logf("notify closed Telegram topic: %v", notifyErr)
	}
}

func isTransientDeliveryError(err error) bool {
	var apiErr *telegoapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode == 429 || apiErr.ErrorCode >= 500
	}
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}
	message := strings.ToLower(errString(err))
	return strings.Contains(message, "429") ||
		strings.Contains(message, "500") ||
		strings.Contains(message, "502") ||
		strings.Contains(message, "503") ||
		strings.Contains(message, "504") ||
		strings.Contains(message, "timeout") ||
		strings.Contains(message, "temporarily unavailable")
}

func telegramRetryAfter(err error) time.Duration {
	var apiErr *telegoapi.Error
	if errors.As(err, &apiErr) && apiErr.Parameters != nil && apiErr.Parameters.RetryAfter > 0 {
		return time.Duration(apiErr.Parameters.RetryAfter) * time.Second
	}
	if isRateLimitText(err) {
		return deliveryDefaultRetryAfter
	}
	return 0
}

func isClosedTopicError(err error) bool {
	// Keep delivery classification aligned with topic lifecycle recovery.
	// Telegram uses several descriptions for closed or deleted topics, and
	// all of them should fail fast with an actionable owner notification.
	return isAlreadyClosedTopicError(err)
}

func isRateLimitText(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "429") || strings.Contains(message, "too many requests")
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func safeDeliveryError(err error) string {
	if err == nil {
		return "неизвестная ошибка"
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 240 {
		return message[:240]
	}
	return message
}

func cloneSendMessageParams(params *telego.SendMessageParams) *telego.SendMessageParams {
	if params == nil {
		return nil
	}
	copy := *params
	if params.Entities != nil {
		copy.Entities = append([]telego.MessageEntity(nil), params.Entities...)
	}
	return &copy
}
