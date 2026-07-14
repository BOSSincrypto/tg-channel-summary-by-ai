// Package scheduler manages cron-based digest scheduling per group.
// It uses robfig/cron to trigger digest generation at configured times.
package scheduler

// Scheduler manages per-group cron jobs for digest generation.
type Scheduler struct {
	// TODO: cron instance, database handle
}

// New creates a new Scheduler.
func New() *Scheduler {
	return &Scheduler{}
}

// Start begins all scheduled digest jobs.
func (s *Scheduler) Start() {
	// TODO: load group configs, create cron jobs
}

// Stop gracefully stops all scheduled jobs.
func (s *Scheduler) Stop() {
	// TODO: stop cron
}
