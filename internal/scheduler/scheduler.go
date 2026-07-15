// Package scheduler manages cron-based digest scheduling per group.
// It uses robfig/cron to trigger digest generation at configured times.
package scheduler

import (
	"errors"
	"fmt"

	"github.com/boss/tg-channel-summary-by-ai/internal/digest"
)

// DigestRunner is the runtime digest path used by scheduled group jobs.
type DigestRunner interface {
	Generate(groupID int64) (*digest.Digest, error)
}

// Scheduler manages per-group cron jobs for digest generation.
type Scheduler struct {
	runner DigestRunner
}

// New creates a scheduler. An optional runner keeps the original constructor
// usable while allowing production startup to inject the digest pipeline.
func New(runners ...DigestRunner) *Scheduler {
	var runner DigestRunner
	if len(runners) > 0 {
		runner = runners[0]
	}
	return &Scheduler{runner: runner}
}

// Start begins scheduled jobs. Job registration is provided by the digest
// scheduling milestone; this method remains a lifecycle hook for startup.
func (s *Scheduler) Start() {}

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

// Stop gracefully stops all scheduled jobs.
func (s *Scheduler) Stop() {}
