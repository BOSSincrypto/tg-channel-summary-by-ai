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

// WithRefreshFailureHook injects a refresh failure before live registrations
// are changed. It is intended for production-boundary failure tests.
func WithRefreshFailureHook(hook func() error) Option {
	return func(s *Scheduler) {
		s.refreshFailureHook = hook
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

	mu                 sync.Mutex
	started            bool
	jobIDs             map[int64]cron.EntryID
	jobSpecs           map[int64]string
	windows            map[string]*scheduledWindow
	refreshFailureHook func() error
}

type scheduledWindow struct {
	id        string
	remaining int
}

// SettingsRefreshPlan is a prepared replacement for the live scheduler
// registrations. Apply removes the old registrations only after all target
// schedules have been validated, and compensates with the previous schedules
// if a new registration fails.
type SettingsRefreshPlan struct {
	scheduler *Scheduler
	targets   map[int64]string
	previous  map[int64]string
	applied   bool
}

// New creates a scheduler that can register production digest jobs.
func New(runner DigestRunner, options ...Option) *Scheduler {
	s := &Scheduler{
		runner:   runner,
		engine:   newRealCronEngine(),
		jobIDs:   make(map[int64]cron.EntryID),
		jobSpecs: make(map[int64]string),
		windows:  make(map[string]*scheduledWindow),
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
	s.jobSpecs = specByGroup
	s.engine.Start()
	s.started = true
	return nil
}

// RefreshGroup replaces a running group's cron registration after its
// persisted settings change. It is idempotent and intentionally keeps the
// scheduler instance shared by Telegram and WebApp callers.
func (s *Scheduler) RefreshGroup(groupID int64) error {
	if s == nil {
		return errors.New("refresh group: scheduler is not configured")
	}
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	source, engine := s.groups, s.engine
	if source == nil || engine == nil {
		s.mu.Unlock()
		return errors.New("refresh group: scheduler dependencies are not configured")
	}
	if jobID, ok := s.jobIDs[groupID]; ok {
		engine.Remove(jobID)
		delete(s.jobIDs, groupID)
		delete(s.jobSpecs, groupID)
	}
	groups, err := source.List()
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("refresh group: list groups: %w", err)
	}
	var target *model.Group
	for index := range groups {
		if groups[index].ID == groupID {
			target = &groups[index]
			break
		}
	}
	if target == nil {
		s.mu.Unlock()
		return fmt.Errorf("refresh group %d: group not found", groupID)
	}
	if target.Status != "" && target.Status != model.GroupStatusActive {
		s.mu.Unlock()
		return nil
	}
	settings, err := source.GetGroupSettings(groupID)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("refresh group: load settings for group %d: %w", groupID, err)
	}
	spec, err := scheduleSpec(settings)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("refresh group: build schedule for group %d: %w", groupID, err)
	}
	entryID, err := engine.AddFunc(spec, func() {
		windowID := s.nextScheduledWindow(spec, 1)
		if _, runErr := s.RunGroupWithWindow(groupID, windowID); runErr != nil {
			log.Printf("scheduler group %d failed: %v", groupID, runErr)
		}
	})
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("refresh group: register group %d schedule %q: %w", groupID, spec, err)
	}
	s.jobIDs[groupID] = entryID
	s.jobSpecs[groupID] = spec
	s.mu.Unlock()
	return nil
}

// PrepareSettingsRefresh validates the complete desired active-group schedule
// set without changing the live scheduler. The returned plan can be applied
// after a database transaction commits.
func (s *Scheduler) PrepareSettingsRefresh(settings map[int64]*model.GroupSettings) (*SettingsRefreshPlan, error) {
	if s == nil {
		return nil, errors.New("prepare settings refresh: scheduler is not configured")
	}
	targets := make(map[int64]string, len(settings))
	for groupID, groupSettings := range settings {
		if groupID <= 0 {
			return nil, errors.New("prepare settings refresh: group ID must be positive")
		}
		spec, err := scheduleSpec(groupSettings)
		if err != nil {
			return nil, fmt.Errorf("prepare settings refresh: group %d: %w", groupID, err)
		}
		targets[groupID] = spec
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	previous := make(map[int64]string, len(s.jobSpecs))
	for groupID, spec := range s.jobSpecs {
		previous[groupID] = spec
	}
	return &SettingsRefreshPlan{
		scheduler: s,
		targets:   targets,
		previous:  previous,
	}, nil
}

// Apply replaces the live registrations represented by a prepared settings
// plan. If registration fails, it restores the previous schedule set when
// possible and returns the original failure.
func (p *SettingsRefreshPlan) Apply() error {
	if p == nil || p.scheduler == nil {
		return errors.New("apply settings refresh: plan is not configured")
	}
	s := p.scheduler
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.applied {
		return nil
	}
	if !s.started {
		p.applied = true
		return nil
	}
	if s.refreshFailureHook != nil {
		if err := s.refreshFailureHook(); err != nil {
			return fmt.Errorf("apply settings refresh: injected failure: %w", err)
		}
	}

	removed := make([]cron.EntryID, 0, len(s.jobIDs))
	for _, jobID := range s.jobIDs {
		removed = append(removed, jobID)
		s.engine.Remove(jobID)
	}
	s.jobIDs = make(map[int64]cron.EntryID)
	s.jobSpecs = make(map[int64]string)

	registered, err := s.addSchedulesLocked(p.targets)
	if err == nil {
		s.jobIDs = registered
		s.jobSpecs = cloneScheduleSpecs(p.targets)
		p.applied = true
		return nil
	}

	for _, jobID := range registered {
		s.engine.Remove(jobID)
	}
	restored, restoreErr := s.addSchedulesLocked(p.previous)
	if restoreErr == nil {
		s.jobIDs = restored
		s.jobSpecs = cloneScheduleSpecs(p.previous)
	}
	if restoreErr != nil {
		return fmt.Errorf("apply settings refresh: %w; restore previous schedules: %v", err, restoreErr)
	}
	return fmt.Errorf("apply settings refresh: %w", err)
}

// Rollback restores the schedule set captured before Apply. It is idempotent
// and gives callers an explicit compensation seam for late failures.
func (p *SettingsRefreshPlan) Rollback() error {
	if p == nil || p.scheduler == nil {
		return errors.New("rollback settings refresh: plan is not configured")
	}
	s := p.scheduler
	s.mu.Lock()
	defer s.mu.Unlock()
	if !p.applied || !s.started {
		return nil
	}
	for _, jobID := range s.jobIDs {
		s.engine.Remove(jobID)
	}
	registered, err := s.addSchedulesLocked(p.previous)
	if err != nil {
		return fmt.Errorf("rollback settings refresh: %w", err)
	}
	s.jobIDs = registered
	s.jobSpecs = cloneScheduleSpecs(p.previous)
	p.applied = false
	return nil
}

func (s *Scheduler) addSchedulesLocked(specs map[int64]string) (map[int64]cron.EntryID, error) {
	bySpec := make(map[string]int)
	for _, spec := range specs {
		bySpec[spec]++
	}
	registered := make(map[int64]cron.EntryID, len(specs))
	for groupID, spec := range specs {
		id := groupID
		entryID, err := s.engine.AddFunc(spec, func() {
			windowID := s.nextScheduledWindow(spec, bySpec[spec])
			if _, runErr := s.RunGroupWithWindow(id, windowID); runErr != nil {
				log.Printf("scheduler group %d failed: %v", id, runErr)
			}
		})
		if err != nil {
			return registered, fmt.Errorf("register group %d schedule %q: %w", groupID, spec, err)
		}
		registered[groupID] = entryID
	}
	return registered, nil
}

func cloneScheduleSpecs(specs map[int64]string) map[int64]string {
	cloned := make(map[int64]string, len(specs))
	for groupID, spec := range specs {
		cloned[groupID] = spec
	}
	return cloned
}

// ScheduleForGroup exposes the active cron specification for integration
// checks and operational inspection without exposing cron implementation
// details.
func (s *Scheduler) ScheduleForGroup(groupID int64) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	spec, ok := s.jobSpecs[groupID]
	return spec, ok
}

// RemoveGroup cancels the scheduled digest job for a group that is no longer
// eligible to receive messages. Its persisted configuration is untouched so a
// later re-add can restore the previous assignments.
func (s *Scheduler) RemoveGroup(groupID int64) {
	s.mu.Lock()
	jobID, ok := s.jobIDs[groupID]
	if ok {
		delete(s.jobIDs, groupID)
		delete(s.jobSpecs, groupID)
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
	s.jobSpecs[groupID] = spec
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
	s.jobSpecs = make(map[int64]string)
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
