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

// ErrStaleSettingsRefresh reports that a prepared scheduler plan was created
// against a live registration set that changed before it was applied.
var ErrStaleSettingsRefresh = errors.New("settings refresh plan is stale")

// GroupSource loads groups and their scheduling configuration.
type GroupSource interface {
	List() ([]model.Group, error)
	GetGroupSettings(groupID int64) (*model.GroupSettings, error)
}

// DigestHistory provides access to past digest records for catch-up
// detection. It is the narrow interface the scheduler needs from the digest
// repository to determine whether a scheduled fire was missed while the bot
// was down.
type DigestHistory interface {
	ListByGroup(groupID int64, limit int) ([]model.Digest, error)
}

// DSTSkipNotifier is called when a scheduled digest is skipped because the
// configured wall-clock time does not exist due to a DST spring-forward
// transition. Callers wire this to the owner notifier so the admin is
// informed of any skipped digests.
type DSTSkipNotifier func(groupID int64, groupTitle, digestTime, timezone, reason string)

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

// WithDigestHistory configures the digest history source used for missed
// schedule catch-up detection on restart.
func WithDigestHistory(history DigestHistory) Option {
	return func(s *Scheduler) {
		s.history = history
	}
}

// WithDSTSkipNotifier wires a callback that is invoked when a scheduled
// digest is skipped because the configured time does not exist due to a DST
// spring-forward transition. The admin should be notified so the skip is
// visible.
func WithDSTSkipNotifier(notifier DSTSkipNotifier) Option {
	return func(s *Scheduler) {
		s.dstNotifier = notifier
	}
}

// WithNowFunc overrides the clock used for catch-up and DST dedup checks.
// It is primarily intended for deterministic tests; production code uses the
// default time.Now.
func WithNowFunc(fn func() time.Time) Option {
	return func(s *Scheduler) {
		if fn != nil {
			s.nowFunc = fn
		}
	}
}

// WithRefreshFailureHook injects a refresh failure before live registrations
// are changed. It is intended for production-boundary failure tests.
func WithRefreshFailureHook(hook func() error) Option {
	return func(s *Scheduler) {
		s.refreshFailureHook = hook
	}
}

// WithLifecycleHooks injects deterministic hooks around the shared scheduler
// lifecycle boundary. Hooks are intended for production-boundary race tests
// and must not mutate scheduler state.
func WithLifecycleHooks(before, after func()) Option {
	return func(s *Scheduler) {
		s.lifecycleBefore = before
		s.lifecycleAfter = after
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
	lifecycleMu        sync.Mutex
	started            bool
	generation         uint64
	jobIDs             map[int64]cron.EntryID
	jobSpecs           map[int64]string
	windows            map[string]*scheduledWindow
	refreshFailureHook func() error
	lifecycleBefore    func()
	lifecycleAfter     func()

	history     DigestHistory
	dstNotifier DSTSkipNotifier
	nowFunc     func() time.Time

	fireMu   sync.Mutex
	lastFire map[int64]fireRecord
}

// fireRecord tracks the last scheduled fire for DST fall-back deduplication.
// During a fall-back transition the same wall-clock time occurs twice with
// different UTC offsets; the second occurrence is skipped.
type fireRecord struct {
	wallClock string
	offset    int
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
	scheduler  *Scheduler
	targets    map[int64]string
	previous   map[int64]string
	generation uint64
	applied    bool
}

// New creates a scheduler that can register production digest jobs.
func New(runner DigestRunner, options ...Option) *Scheduler {
	s := &Scheduler{
		runner:   runner,
		engine:   newRealCronEngine(),
		jobIDs:   make(map[int64]cron.EntryID),
		jobSpecs: make(map[int64]string),
		windows:  make(map[string]*scheduledWindow),
		nowFunc:  time.Now,
		lastFire: make(map[int64]fireRecord),
	}
	for _, option := range options {
		option(s)
	}
	return s
}

// now returns the current time, honoring an injected clock for tests.
func (s *Scheduler) now() time.Time {
	if s.nowFunc != nil {
		return s.nowFunc()
	}
	return time.Now()
}

// groupJobFunc wraps the cron callback for a group with DST fall-back
// deduplication. During a DST fall-back transition the same wall-clock time
// occurs twice; robfig/cron may invoke the callback for both occurrences.
// The wrapper records the wall-clock string per group and skips the second
// occurrence so exactly one digest is delivered per ambiguous hour.
func (s *Scheduler) groupJobFunc(groupID int64, spec string, groupCount int) func() {
	return func() {
		if s.shouldSkipDSTDuplicate(groupID) {
			return
		}
		windowID := s.nextScheduledWindow(spec, groupCount)
		if _, runErr := s.RunGroupWithWindow(groupID, windowID); runErr != nil {
			log.Printf("scheduler group %d failed: %v", groupID, runErr)
		}
	}
}

// shouldSkipDSTDuplicate returns true if the current wall-clock time in the
// group's timezone was already fired with a different UTC offset, indicating
// a DST fall-back duplicate. During a fall-back transition the same
// wall-clock time occurs twice (first in the pre-transition offset, then in
// the post-transition offset); the wrapper skips the second occurrence so
// exactly one digest is delivered per ambiguous hour. Repeated fires with the
// same offset (e.g., test double-triggers) are not treated as duplicates.
func (s *Scheduler) shouldSkipDSTDuplicate(groupID int64) bool {
	if s.groups == nil {
		return false
	}
	settings, err := s.groups.GetGroupSettings(groupID)
	if err != nil {
		return false
	}
	loc, err := time.LoadLocation(strings.TrimSpace(settings.Timezone))
	if err != nil {
		return false
	}
	now := s.now().In(loc)
	wallClock := now.Format("2006-01-02 15:04")
	_, offset := now.Zone()
	s.fireMu.Lock()
	defer s.fireMu.Unlock()
	if last, ok := s.lastFire[groupID]; ok && last.wallClock == wallClock && last.offset != offset {
		log.Printf("DST fall-back: skipping duplicate digest for group %d at %s (wall-clock already fired)", groupID, wallClock)
		return true
	}
	s.lastFire[groupID] = fireRecord{wallClock: wallClock, offset: offset}
	return false
}

// WithLifecycle serializes scheduler registration changes and digest starts
// with group create/delete and settings refresh operations. The lock is held
// for the complete callback, so a successful deletion can fence a runner
// before it reaches the injected digest service.
func (s *Scheduler) WithLifecycle(fn func() error) error {
	if s == nil {
		return errors.New("scheduler lifecycle: scheduler is not configured")
	}
	if fn == nil {
		return errors.New("scheduler lifecycle: callback is required")
	}
	s.lifecycleMu.Lock()
	if s.lifecycleBefore != nil {
		s.lifecycleBefore()
	}
	defer func() {
		if s.lifecycleAfter != nil {
			s.lifecycleAfter()
		}
		s.lifecycleMu.Unlock()
	}()
	return fn()
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
		entryID, err := s.engine.AddFunc(spec, s.groupJobFunc(groupID, spec, groupsBySpec[spec]))
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
	s.generation++
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
	entryID, err := engine.AddFunc(spec, s.groupJobFunc(groupID, spec, 1))
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("refresh group: register group %d schedule %q: %w", groupID, spec, err)
	}
	s.jobIDs[groupID] = entryID
	s.jobSpecs[groupID] = spec
	s.generation++
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
		scheduler:  s,
		targets:    targets,
		previous:   previous,
		generation: s.generation,
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
	if p.generation != s.generation {
		return fmt.Errorf("apply settings refresh: %w", ErrStaleSettingsRefresh)
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
		s.generation++
		p.generation = s.generation
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
		s.generation++
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
	if p.generation != s.generation {
		return fmt.Errorf("rollback settings refresh: %w", ErrStaleSettingsRefresh)
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
	s.generation++
	p.generation = s.generation
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
		entryID, err := s.engine.AddFunc(spec, s.groupJobFunc(id, spec, bySpec[spec]))
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
	if s == nil {
		return
	}
	_ = s.WithLifecycle(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.removeGroupLocked(groupID)
		return nil
	})
}

// RemoveGroupWithinLifecycle removes a job while the shared lifecycle
// boundary is already held by the caller.
func (s *Scheduler) RemoveGroupWithinLifecycle(groupID int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeGroupLocked(groupID)
}

func (s *Scheduler) removeGroupLocked(groupID int64) {
	jobID, ok := s.jobIDs[groupID]
	if ok {
		delete(s.jobIDs, groupID)
		delete(s.jobSpecs, groupID)
		s.generation++
	}
	engine := s.engine
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
	return s.WithLifecycle(func() error {
		return s.restoreGroupWithinLifecycle(groupID)
	})
}

// RestoreGroupWithinLifecycle registers a job while the shared lifecycle
// boundary is already held by the caller.
func (s *Scheduler) RestoreGroupWithinLifecycle(groupID int64) error {
	if s == nil {
		return errors.New("restore group: scheduler is not configured")
	}
	return s.restoreGroupWithinLifecycle(groupID)
}

func (s *Scheduler) restoreGroupWithinLifecycle(groupID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return nil
	}
	if _, exists := s.jobIDs[groupID]; exists {
		return nil
	}
	source := s.groups
	engine := s.engine
	if source == nil || engine == nil {
		return errors.New("restore group: scheduler dependencies are not configured")
	}
	group, err := source.List()
	if err != nil {
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
		return fmt.Errorf("restore group %d: group not found", groupID)
	}
	if target.Status != "" && target.Status != model.GroupStatusActive {
		return fmt.Errorf("restore group %d: group is not active", groupID)
	}
	settings, err := source.GetGroupSettings(groupID)
	if err != nil {
		return fmt.Errorf("restore group: load settings for group %d: %w", groupID, err)
	}
	spec, err := scheduleSpec(settings)
	if err != nil {
		return fmt.Errorf("restore group: build schedule for group %d: %w", groupID, err)
	}
	entryID, err := engine.AddFunc(spec, s.groupJobFunc(groupID, spec, 1))
	if err != nil {
		return fmt.Errorf("restore group: register group %d schedule %q: %w", groupID, spec, err)
	}
	s.jobIDs[groupID] = entryID
	s.jobSpecs[groupID] = spec
	s.generation++
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
	if lifecycleErr := s.WithLifecycle(func() error {
		result, err = s.runGroupWithWindow(groupID, windowID)
		return err
	}); lifecycleErr != nil {
		return nil, lifecycleErr
	}
	return result, err
}

func (s *Scheduler) runGroupWithWindow(groupID int64, windowID string) (*digest.Digest, error) {
	if s.groups != nil {
		groups, err := s.groups.List()
		if err != nil {
			return nil, fmt.Errorf("run group %d: verify group: %w", groupID, err)
		}
		active := false
		for _, group := range groups {
			if group.ID == groupID {
				active = group.Status == "" || group.Status == model.GroupStatusActive
				break
			}
		}
		if !active {
			return nil, fmt.Errorf("run group %d: group is not active", groupID)
		}
	}
	var result *digest.Digest
	var err error
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

// CatchUp checks each active group for a missed scheduled fire time and
// runs a catch-up digest if the most recent scheduled time passed without a
// corresponding digest. It also detects DST spring-forward transitions where
// the configured wall-clock time does not exist on the current day and
// notifies the admin so the skip is visible. This implements the deterministic
// "always catch up on missed schedules" behavior required after a restart.
func (s *Scheduler) CatchUp() error {
	if s == nil {
		return errors.New("catch-up: scheduler is not configured")
	}
	s.mu.Lock()
	started := s.started
	history := s.history
	source := s.groups
	s.mu.Unlock()

	if !started || history == nil || source == nil {
		return nil
	}

	groups, err := source.List()
	if err != nil {
		return fmt.Errorf("catch-up: list groups: %w", err)
	}

	now := s.now()
	for _, group := range groups {
		if group.Status != "" && group.Status != model.GroupStatusActive {
			continue
		}
		settings, err := source.GetGroupSettings(group.ID)
		if err != nil {
			log.Printf("catch-up: load settings for group %d: %v", group.ID, err)
			continue
		}
		if err := s.catchUpGroup(group, settings, now, history); err != nil {
			log.Printf("catch-up: group %d: %v", group.ID, err)
		}
	}
	return nil
}

// catchUpGroup evaluates a single group for missed-schedule catch-up and DST
// spring-forward detection. It computes the most recent past scheduled fire
// time, checks the digest history for a corresponding digest, and fires a
// catch-up run if the schedule was missed.
func (s *Scheduler) catchUpGroup(group model.Group, settings *model.GroupSettings, now time.Time, history DigestHistory) error {
	loc, err := time.LoadLocation(strings.TrimSpace(settings.Timezone))
	if err != nil {
		return fmt.Errorf("load timezone %q: %w", settings.Timezone, err)
	}
	parsed, err := time.Parse("15:04", strings.TrimSpace(settings.DigestTime))
	if err != nil {
		return fmt.Errorf("parse digest time %q: %w", settings.DigestTime, err)
	}

	nowLocal := now.In(loc)
	todayScheduled := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(),
		parsed.Hour(), parsed.Minute(), 0, 0, loc)
	todayExists := todayScheduled.Hour() == parsed.Hour() && todayScheduled.Minute() == parsed.Minute()

	if !todayExists {
		log.Printf("DST transition: skipping digest at %s — time does not exist due to DST", settings.DigestTime)
		s.notifyDSTSkip(group.ID, group.Title, settings.DigestTime, settings.Timezone,
			"spring-forward: scheduled time does not exist")
	}

	// Determine the most recent past scheduled fire time.
	var lastScheduled time.Time
	if todayExists && !todayScheduled.After(nowLocal) {
		lastScheduled = todayScheduled
	} else {
		yesterday := nowLocal.AddDate(0, 0, -1)
		lastScheduled = time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(),
			parsed.Hour(), parsed.Minute(), 0, 0, loc)
	}

	// Skip catch-up if a digest was already sent after the last scheduled time.
	digests, err := history.ListByGroup(group.ID, 1)
	if err != nil {
		return fmt.Errorf("list digests for group %d: %w", group.ID, err)
	}
	if len(digests) > 0 {
		lastDigestTime, parseErr := parseDigestSentAt(digests[0].SentAt)
		if parseErr == nil && lastDigestTime.After(lastScheduled) {
			return nil
		}
	}

	log.Printf("Missed schedule for group %d, running catch-up digest", group.ID)
	if _, runErr := s.RunGroup(group.ID); runErr != nil {
		return fmt.Errorf("catch-up run: %w", runErr)
	}
	return nil
}

// notifyDSTSkip invokes the configured DST skip notifier if one is wired.
func (s *Scheduler) notifyDSTSkip(groupID int64, groupTitle, digestTime, timezone, reason string) {
	if s.dstNotifier != nil {
		s.dstNotifier(groupID, groupTitle, digestTime, timezone, reason)
	}
}

// parseDigestSentAt parses a digest SentAt timestamp. SQLite datetime('now')
// stores UTC in "2006-01-02 15:04:05" format; RFC3339 is tried as a fallback.
func parseDigestSentAt(sentAt string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02 15:04:05", sentAt); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, sentAt)
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
