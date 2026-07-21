package maintenance

import (
	"context"
	"errors"
	"fmt"
	applog "github.com/boss/tg-channel-summary-by-ai/internal/log"
	"sync"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
)

const (
	defaultRetentionDays        = 90
	defaultInterval             = 24 * time.Hour
	defaultVolumeAlertThreshold = 80.0

	volumeAlertStateKey = "maintenance.volume_alert_active"
	lastRunAtKey        = "maintenance.last_run_at"
)

// Cleaner removes retained posts from storage.
type Cleaner interface {
	CleanupPosts(retentionDays int) (int64, error)
}

// ConfigStore persists maintenance state.
type ConfigStore interface {
	Get(key string) (string, error)
	Set(key, value string) error
}

// Notifier sends owner-facing maintenance alerts.
type Notifier interface {
	NotifyOwner(ctx context.Context, text string) error
}

// Logger captures maintenance lifecycle and errors.
type Logger interface {
	Printf(format string, v ...any)
}

// Options configures a maintenance Service.
type Options struct {
	RetentionDays        int
	Interval             time.Duration
	VolumeAlertThreshold float64
	DBPath               string
	Cleaner              Cleaner
	ConfigStore          ConfigStore
	UsageChecker         UsageChecker
	Notifier             Notifier
	Logger               Logger
	Now                  func() time.Time
}

type ticker interface {
	Chan() <-chan time.Time
	Stop()
}

type stdTicker struct {
	*time.Ticker
}

func (t stdTicker) Chan() <-chan time.Time {
	return t.C
}

// Service runs periodic post-retention maintenance for the running app.
type Service struct {
	retentionDays        int
	interval             time.Duration
	volumeAlertThreshold float64
	dbPath               string
	cleaner              Cleaner
	configStore          ConfigStore
	usageChecker         UsageChecker
	notifier             Notifier
	logger               Logger
	now                  func() time.Time
	newTicker            func(time.Duration) ticker

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// New constructs a retention maintenance service with conservative defaults.
func New(opts Options) *Service {
	retentionDays := opts.RetentionDays
	if retentionDays <= 0 {
		retentionDays = defaultRetentionDays
	}

	interval := opts.Interval
	if interval <= 0 {
		interval = defaultInterval
	}

	threshold := opts.VolumeAlertThreshold
	if threshold <= 0 {
		threshold = defaultVolumeAlertThreshold
	}

	logger := opts.Logger
	if logger == nil {
		logger = applog.GetDefault()
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	checker := opts.UsageChecker
	if checker == nil {
		checker = RealUsageChecker{}
	}

	return &Service{
		retentionDays:        retentionDays,
		interval:             interval,
		volumeAlertThreshold: threshold,
		dbPath:               opts.DBPath,
		cleaner:              opts.Cleaner,
		configStore:          opts.ConfigStore,
		usageChecker:         checker,
		notifier:             opts.Notifier,
		logger:               logger,
		now:                  now,
		newTicker: func(d time.Duration) ticker {
			return stdTicker{Ticker: time.NewTicker(d)}
		},
	}
}

// Start launches a background loop that runs once immediately and then on the
// configured interval until Stop is called.
func (s *Service) Start() {
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	tk := s.newTicker(s.interval)
	done := make(chan struct{})
	s.cancel = cancel
	s.done = done
	s.mu.Unlock()

	s.logger.Printf("post retention maintenance started: retention_days=%d interval=%s volume_alert_threshold=%.2f%%", s.retentionDays, s.interval, s.volumeAlertThreshold)

	go func() {
		defer close(done)
		defer tk.Stop()

		s.runAndLog(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.Chan():
				s.runAndLog(ctx)
			}
		}
	}()
}

// Stop terminates the background maintenance loop.
func (s *Service) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	done := s.done
	s.cancel = nil
	s.done = nil
	s.mu.Unlock()

	if cancel == nil {
		return
	}

	cancel()
	<-done
	s.logger.Printf("post retention maintenance stopped")
}

// RunOnce executes one maintenance pass: cleanup, optimize, volume check, and
// owner notification when the configured threshold is exceeded.
func (s *Service) RunOnce(ctx context.Context) error {
	if s.cleaner == nil {
		return errors.New("maintenance cleaner is required")
	}
	if s.configStore == nil {
		return errors.New("maintenance config store is required")
	}
	if s.usageChecker == nil {
		return errors.New("maintenance usage checker is required")
	}

	deleted, err := s.cleaner.CleanupPosts(s.retentionDays)
	if err != nil {
		return fmt.Errorf("cleanup posts: %w", err)
	}

	if err := s.configStore.Set(lastRunAtKey, s.now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("record last maintenance run: %w", err)
	}

	usage, err := s.usageChecker.Check(s.dbPath)
	if err != nil {
		return fmt.Errorf("check database volume usage: %w", err)
	}

	usedPercent := usage.UsedPercent()
	s.logger.Printf("post retention maintenance completed: deleted=%d retention_days=%d volume_used=%.2f%% path=%s", deleted, s.retentionDays, usedPercent, usage.Path)

	alertActive, err := s.volumeAlertActive()
	if err != nil {
		return err
	}

	switch {
	case usedPercent >= s.volumeAlertThreshold && !alertActive:
		if s.notifier == nil {
			return errors.New("maintenance notifier is required when volume usage exceeds the alert threshold")
		}
		if err := s.notifier.NotifyOwner(ctx, s.volumeAlertMessage(usage)); err != nil {
			return fmt.Errorf("notify owner about database volume usage: %w", err)
		}
		if err := s.setVolumeAlertActive(true); err != nil {
			return err
		}
	case usedPercent < s.volumeAlertThreshold && alertActive:
		if err := s.setVolumeAlertActive(false); err != nil {
			return err
		}
	}

	return nil
}

func (s *Service) runAndLog(ctx context.Context) {
	if err := s.RunOnce(ctx); err != nil {
		s.logger.Printf("post retention maintenance error: %v", err)
	}
}

func (s *Service) volumeAlertActive() (bool, error) {
	value, err := s.configStore.Get(volumeAlertStateKey)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("read maintenance volume alert state: %w", err)
	}
	return value == "1", nil
}

func (s *Service) setVolumeAlertActive(active bool) error {
	value := "0"
	if active {
		value = "1"
	}
	if err := s.configStore.Set(volumeAlertStateKey, value); err != nil {
		return fmt.Errorf("persist maintenance volume alert state: %w", err)
	}
	return nil
}

func (s *Service) volumeAlertMessage(usage DiskUsage) string {
	return fmt.Sprintf(
		"Database volume usage is %.2f%% (%d of %d bytes) at %s. Consider pruning data or expanding the Fly volume.",
		usage.UsedPercent(),
		usage.UsedBytes,
		usage.TotalBytes,
		usage.Path,
	)
}
