package maintenance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
)

type fakeCleaner struct {
	retentionDays []int
	deleted       int64
	err           error
}

func (f *fakeCleaner) CleanupPosts(retentionDays int) (int64, error) {
	f.retentionDays = append(f.retentionDays, retentionDays)
	return f.deleted, f.err
}

type fakeConfigStore struct {
	values map[string]string
	errGet error
	errSet error
}

func (f *fakeConfigStore) Get(key string) (string, error) {
	if f.errGet != nil {
		return "", f.errGet
	}
	v, ok := f.values[key]
	if !ok {
		return "", db.ErrNotFound
	}
	return v, nil
}

func (f *fakeConfigStore) Set(key, value string) error {
	if f.errSet != nil {
		return f.errSet
	}
	if f.values == nil {
		f.values = make(map[string]string)
	}
	f.values[key] = value
	return nil
}

type fakeUsageChecker struct {
	usage DiskUsage
	err   error
}

func (f fakeUsageChecker) Check(dbPath string) (DiskUsage, error) {
	if f.err != nil {
		return DiskUsage{}, f.err
	}
	usage := f.usage
	if usage.Path == "" {
		usage.Path = dbPath
	}
	return usage, nil
}

type fakeNotifier struct {
	messages []string
	ctxErr   error
	err      error
}

func (f *fakeNotifier) NotifyOwner(ctx context.Context, text string) error {
	if f.ctxErr != nil {
		return f.ctxErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.err != nil {
		return f.err
	}
	f.messages = append(f.messages, text)
	return nil
}

type fakeLogger struct {
	lines []string
}

func (l *fakeLogger) Printf(format string, v ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, v...))
}

type fakeTicker struct {
	ch      chan time.Time
	stopped bool
}

func (t *fakeTicker) Chan() <-chan time.Time {
	return t.ch
}

func (t *fakeTicker) Stop() {
	t.stopped = true
}

func TestServiceRunOnceUsesDefaultRetentionAndStoresLastRun(t *testing.T) {
	cleaner := &fakeCleaner{deleted: 3}
	configStore := &fakeConfigStore{}
	logger := &fakeLogger{}
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	svc := New(Options{
		RetentionDays: 0,
		DBPath:        "bot.db",
		Cleaner:       cleaner,
		ConfigStore:   configStore,
		UsageChecker: fakeUsageChecker{usage: DiskUsage{
			Path:       "bot.db",
			UsedBytes:  40,
			TotalBytes: 100,
		}},
		Logger: logger,
		Now:    func() time.Time { return now },
	})

	if err := svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("run once: %v", err)
	}

	if len(cleaner.retentionDays) != 1 || cleaner.retentionDays[0] != 90 {
		t.Fatalf("cleanup retentionDays = %v, want [90]", cleaner.retentionDays)
	}
	if got := configStore.values[lastRunAtKey]; got != now.Format(time.RFC3339) {
		t.Fatalf("lastRunAt = %q, want %q", got, now.Format(time.RFC3339))
	}
	if len(logger.lines) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(logger.lines))
	}
	if !strings.Contains(logger.lines[0], "deleted=3") {
		t.Fatalf("log line %q does not mention deleted rows", logger.lines[0])
	}
}

func TestServiceRunOnceNotifiesOwnerOnceAboveThreshold(t *testing.T) {
	cleaner := &fakeCleaner{}
	configStore := &fakeConfigStore{}
	notifier := &fakeNotifier{}

	svc := New(Options{
		RetentionDays: 45,
		DBPath:        "/data/bot.db",
		Cleaner:       cleaner,
		ConfigStore:   configStore,
		UsageChecker: fakeUsageChecker{usage: DiskUsage{
			Path:       "/data",
			UsedBytes:  85,
			TotalBytes: 100,
		}},
		Notifier: notifier,
	})

	if err := svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("first run once: %v", err)
	}
	if err := svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("second run once: %v", err)
	}

	if len(notifier.messages) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notifier.messages))
	}
	if !strings.Contains(notifier.messages[0], "85.00%") {
		t.Fatalf("notification %q does not mention usage percentage", notifier.messages[0])
	}
	if got := configStore.values[volumeAlertStateKey]; got != "1" {
		t.Fatalf("volume alert state = %q, want %q", got, "1")
	}
	if cleaner.retentionDays[0] != 45 {
		t.Fatalf("cleanup retentionDays[0] = %d, want 45", cleaner.retentionDays[0])
	}
}

func TestServiceRunOnceClearsAlertStateWhenUsageRecovers(t *testing.T) {
	cleaner := &fakeCleaner{}
	configStore := &fakeConfigStore{values: map[string]string{volumeAlertStateKey: "1"}}

	svc := New(Options{
		DBPath:      "/data/bot.db",
		Cleaner:     cleaner,
		ConfigStore: configStore,
		UsageChecker: fakeUsageChecker{usage: DiskUsage{
			Path:       "/data",
			UsedBytes:  79,
			TotalBytes: 100,
		}},
	})

	if err := svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("run once: %v", err)
	}

	if got := configStore.values[volumeAlertStateKey]; got != "0" {
		t.Fatalf("volume alert state = %q, want %q", got, "0")
	}
}

func TestServiceRunOnceRequiresNotifierWhenThresholdExceeded(t *testing.T) {
	svc := New(Options{
		DBPath:      "/data/bot.db",
		Cleaner:     &fakeCleaner{},
		ConfigStore: &fakeConfigStore{},
		UsageChecker: fakeUsageChecker{usage: DiskUsage{
			Path:       "/data",
			UsedBytes:  81,
			TotalBytes: 100,
		}},
	})

	err := svc.RunOnce(context.Background())
	if err == nil {
		t.Fatal("expected notifier error when usage exceeds threshold")
	}
	if !strings.Contains(err.Error(), "notifier is required") {
		t.Fatalf("error = %v, want notifier requirement", err)
	}
}

func TestServiceRunOnceReturnsCleanupError(t *testing.T) {
	svc := New(Options{
		DBPath:      "bot.db",
		Cleaner:     &fakeCleaner{err: errors.New("boom")},
		ConfigStore: &fakeConfigStore{},
		UsageChecker: fakeUsageChecker{usage: DiskUsage{
			UsedBytes:  10,
			TotalBytes: 100,
		}},
	})

	err := svc.RunOnce(context.Background())
	if err == nil {
		t.Fatal("expected cleanup error")
	}
	if !strings.Contains(err.Error(), "cleanup posts: boom") {
		t.Fatalf("error = %v, want cleanup context", err)
	}
}

func TestServiceStartAndStopRunImmediateMaintenanceAndStopTicker(t *testing.T) {
	cleaner := &fakeCleaner{}
	configStore := &fakeConfigStore{}
	logger := &fakeLogger{}
	ft := &fakeTicker{ch: make(chan time.Time, 1)}

	svc := New(Options{
		DBPath:      "bot.db",
		Cleaner:     cleaner,
		ConfigStore: configStore,
		UsageChecker: fakeUsageChecker{usage: DiskUsage{
			UsedBytes:  10,
			TotalBytes: 100,
		}},
		Logger: logger,
	})

	svc.newTicker = func(d time.Duration) ticker { return ft }

	svc.Start()
	deadline := time.After(2 * time.Second)
	for len(cleaner.retentionDays) < 1 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for initial maintenance run")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	ft.ch <- time.Now()
	deadline = time.After(2 * time.Second)
	for len(cleaner.retentionDays) < 2 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for scheduled maintenance run")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	svc.Stop()

	if !ft.stopped {
		t.Fatal("expected ticker to be stopped")
	}
	if len(cleaner.retentionDays) != 2 {
		t.Fatalf("expected 2 maintenance runs, got %d", len(cleaner.retentionDays))
	}
	if len(logger.lines) < 3 {
		t.Fatalf("expected start, run, and stop logs, got %d lines", len(logger.lines))
	}
}
