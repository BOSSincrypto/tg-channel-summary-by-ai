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

type windowedDigestRunner interface {
	GenerateWithWindow(groupID int64, windowID string) (*digest.Digest, error)
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
	windows map[string]*scheduledWindow
}

type scheduledWindow struct {
	id        string
	remaining int
}

// New creates a scheduler that can register production digest jobs.
func New(runner DigestRunner, options ...Option) *Scheduler {
	s := &Scheduler{
		runner:  runner,
		engine:  newRealCronEngine(),
		jobIDs:  make(map[int64]cron.EntryID),
		windows: make(map[string]*scheduledWindow),
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
	groupsBySpec := make(map[string]int)
	specByGroup := make(map[int64]string, len(groups))
	for _, group := range groups {
		if group.Status != "" && group.Status != model.GroupStatusActive {
			continue
		}
		settings, err := s.groups.GetGroupSettings(group.ID)
		if err != nil {
			return fmt.Errorf("start scheduler: load settings for group %d: %w", group.ID, err)
		}
		spec, err := scheduleSpec(settings)
		if err != nil {
			return fmt.Errorf("start scheduler: build schedule for group %d: %w", group.ID, err)
		}
		specByGroup[group.ID] = spec
		groupsBySpec[spec]++
	}
	for _, group := range groups {
		if group.Status != "" && group.Status != model.GroupStatusActive {
			continue
		}
		spec := specByGroup[group.ID]
		groupID := group.ID
		entryID, err := s.engine.AddFunc(spec, func() {
			windowID := s.nextScheduledWindow(spec, groupsBySpec[spec])
			if _, runErr := s.RunGroupWithWindow(groupID, windowID); runErr != nil {
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

// RemoveGroup cancels the scheduled digest job for a group that is no longer
// eligible to receive messages. Its persisted configuration is untouched so a
// later re-add can restore the previous assignments.
func (s *Scheduler) RemoveGroup(groupID int64) {
	s.mu.Lock()
	jobID, ok := s.jobIDs[groupID]
	if ok {
		delete(s.jobIDs, groupID)
	}
	engine := s.engine
	s.mu.Unlock()
	if ok {
		engine.Remove(jobID)
	}
}

// RestoreGroup registers the scheduled digest job for an eligible group that
// was previously removed from the scheduler. It is idempotent and preserves
// the group's persisted settings and assignments.
func (s *Scheduler) RestoreGroup(groupID int64) error {
	if s == nil {
		return errors.New("restore group: scheduler is not configured")
	}

	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	if _, exists := s.jobIDs[groupID]; exists {
		s.mu.Unlock()
		return nil
	}
	source := s.groups
	engine := s.engine
	if source == nil || engine == nil {
		s.mu.Unlock()
		return errors.New("restore group: scheduler dependencies are not configured")
	}
	group, err := source.List()
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("restore group: list groups: %w", err)
	}
	var target *model.Group
	for index := range group {
		if group[index].ID == groupID {
			target = &group[index]
			break
		}
	}
	if target == nil {
		s.mu.Unlock()
		return fmt.Errorf("restore group %d: group not found", groupID)
	}
	if target.Status != "" && target.Status != model.GroupStatusActive {
		s.mu.Unlock()
		return fmt.Errorf("restore group %d: group is not active", groupID)
	}
	settings, err := source.GetGroupSettings(groupID)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("restore group: load settings for group %d: %w", groupID, err)
	}
	spec, err := scheduleSpec(settings)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("restore group: build schedule for group %d: %w", groupID, err)
	}
	entryID, err := engine.AddFunc(spec, func() {
		windowID := s.nextScheduledWindow(spec, 1)
		if _, runErr := s.RunGroupWithWindow(groupID, windowID); runErr != nil {
			log.Printf("scheduler group %d failed: %v", groupID, runErr)
		}
	})
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("restore group: register group %d schedule %q: %w", groupID, spec, err)
	}
	s.jobIDs[groupID] = entryID
	s.mu.Unlock()
	return nil
}

// RunGroup executes one group's production digest path. The digest service
// fetches and stores parser output before selecting digest candidates.
func (s *Scheduler) RunGroup(groupID int64) (*digest.Digest, error) {
	return s.RunGroupWithWindow(groupID, digest.NewWindowID("scheduled"))
}

// RunGroupWithWindow executes one group's digest path using the caller's
// explicit logical window ID.
func (s *Scheduler) RunGroupWithWindow(groupID int64, windowID string) (*digest.Digest, error) {
	if s == nil || s.runner == nil {
		return nil, errors.New("run group: digest runner is not configured")
	}
	var (
		result *digest.Digest
		err    error
	)
	if runner, ok := s.runner.(windowedDigestRunner); ok {
		result, err = runner.GenerateWithWindow(groupID, windowID)
	} else {
		result, err = s.runner.Generate(groupID)
	}
	if err != nil {
		return nil, fmt.Errorf("run group %d: %w", groupID, err)
	}
	return result, nil
}

func (s *Scheduler) nextScheduledWindow(spec string, groupCount int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	window := s.windows[spec]
	if window == nil || window.remaining <= 0 {
		window = &scheduledWindow{id: digest.NewWindowID("scheduled"), remaining: groupCount}
		s.windows[spec] = window
	}
	windowID := window.id
	window.remaining--
	return windowID
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
	s.windows = make(map[string]*scheduledWindow)
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
