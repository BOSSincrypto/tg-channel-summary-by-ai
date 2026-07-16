// Package scheduler manages cron-based digest scheduling per group.
// It uses robfig/cron to trigger digest generation at configured times.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/digest"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/robfig/cron/v3"
)

// DigestRunner is the runtime digest path used by scheduled group jobs.
type DigestRunner interface {
	Generate(groupID int64) (*digest.Digest, error)
}

// GroupSource loads groups and their scheduling configuration.
type GroupSource interface {
	List() ([]model.Group, error)
	GetGroupSettings(groupID int64) (*model.GroupSettings, error)
}

type cronEngine interface {
	AddFunc(spec string, cmd func()) (cron.EntryID, error)
	Start()
	Stop() context.Context
	Remove(id cron.EntryID)
}

type realCronEngine struct {
	cron *cron.Cron
}

func (e *realCronEngine) AddFunc(spec string, cmd func()) (cron.EntryID, error) {
	return e.cron.AddFunc(spec, cmd)
}

func (e *realCronEngine) Start() {
	e.cron.Start()
}

func (e *realCronEngine) Stop() context.Context {
	return e.cron.Stop()
}

func (e *realCronEngine) Remove(id cron.EntryID) {
	e.cron.Remove(id)
}

func newRealCronEngine() cronEngine {
	return &realCronEngine{cron: cron.New()}
}

// Option customizes scheduler dependencies.
type Option func(*Scheduler)

// WithGroupSource configures the repository used to load group schedules.
func WithGroupSource(source GroupSource) Option {
	return func(s *Scheduler) {
		s.groups = source
	}
}

func withCronEngine(engine cronEngine) Option {
	return func(s *Scheduler) {
		s.engine = engine
	}
}

// Scheduler manages per-group cron jobs for digest generation.
type Scheduler struct {
	runner DigestRunner
	groups GroupSource
	engine cronEngine

	mu      sync.Mutex
	started bool
	jobIDs  map[int64]cron.EntryID
}

// New creates a scheduler that can register production digest jobs.
func New(runner DigestRunner, options ...Option) *Scheduler {
	s := &Scheduler{
		runner: runner,
		engine: newRealCronEngine(),
		jobIDs: make(map[int64]cron.EntryID),
	}
	for _, option := range options {
		option(s)
	}
	return s
}

// Start loads configured groups, registers their cron jobs, and starts the
// scheduling engine. A repeated Start is a no-op.
func (s *Scheduler) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return nil
	}
	if s.runner == nil {
		return errors.New("start scheduler: digest runner is not configured")
	}
	if s.groups == nil {
		return errors.New("start scheduler: group source is not configured")
	}

	groups, err := s.groups.List()
	if err != nil {
		return fmt.Errorf("start scheduler: list groups: %w", err)
	}

	registered := make(map[int64]cron.EntryID, len(groups))
	for _, group := range groups {
		settings, err := s.groups.GetGroupSettings(group.ID)
		if err != nil {
			for _, id := range registered {
				s.engine.Remove(id)
			}
			return fmt.Errorf("start scheduler: load settings for group %d: %w", group.ID, err)
		}

		spec, err := scheduleSpec(settings)
		if err != nil {
			for _, id := range registered {
				s.engine.Remove(id)
			}
			return fmt.Errorf("start scheduler: build schedule for group %d: %w", group.ID, err)
		}

		groupID := group.ID
		entryID, err := s.engine.AddFunc(spec, func() {
			if _, runErr := s.RunGroup(groupID); runErr != nil {
				log.Printf("scheduler group %d failed: %v", groupID, runErr)
			}
		})
		if err != nil {
			for _, id := range registered {
				s.engine.Remove(id)
			}
			return fmt.Errorf("start scheduler: register group %d schedule %q: %w", group.ID, spec, err)
		}
		registered[group.ID] = entryID
	}

	s.jobIDs = registered
	s.engine.Start()
	s.started = true
	return nil
}

// RunGroup executes one group's production digest path. The digest service
// fetches and stores parser output before selecting digest candidates.
func (s *Scheduler) RunGroup(groupID int64) (*digest.Digest, error) {
	if s == nil || s.runner == nil {
		return nil, errors.New("run group: digest runner is not configured")
	}
	result, err := s.runner.Generate(groupID)
	if err != nil {
		return nil, fmt.Errorf("run group %d: %w", groupID, err)
	}
	return result, nil
}

// Stop removes all registered jobs and waits for the scheduler to stop.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}

	jobIDs := s.jobIDs
	s.jobIDs = make(map[int64]cron.EntryID)
	s.started = false
	engine := s.engine
	s.mu.Unlock()

	for _, id := range jobIDs {
		engine.Remove(id)
	}
	if done := engine.Stop(); done != nil {
		<-done.Done()
	}
}

func scheduleSpec(settings *model.GroupSettings) (string, error) {
	if settings == nil {
		return "", errors.New("group settings are required")
	}

	digestTime := strings.TrimSpace(settings.DigestTime)
	parsed, err := time.Parse("15:04", digestTime)
	if err != nil {
		return "", fmt.Errorf("parse digest time %q: %w", digestTime, err)
	}

	timezone := strings.TrimSpace(settings.Timezone)
	if timezone == "" {
		return "", errors.New("timezone is required")
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return "", fmt.Errorf("load timezone %q: %w", timezone, err)
	}

	return fmt.Sprintf("CRON_TZ=%s %d %d * * *", timezone, parsed.Minute(), parsed.Hour()), nil
}
