// Package lifecycle coordinates bounded application shutdown transitions.
package lifecycle

import (
	"errors"
	"sync"
	"time"
)

const defaultTransitionTimeout = 5 * time.Second

// Stopper is implemented by long-running application components.
type Stopper interface {
	Stop()
}

// Supervisor coordinates a terminal transition across all registered
// components. The transition is idempotent and becomes observable before
// component cleanup begins, so request handlers can fail closed immediately.
type Supervisor struct {
	timeout time.Duration

	mu         sync.Mutex
	stoppers   []Stopper
	terminal   bool
	reason     error
	done       chan struct{}
	transition sync.Once
}

// New creates a lifecycle supervisor. A non-positive timeout uses a
// conservative five-second bound.
func New(timeout time.Duration) *Supervisor {
	if timeout <= 0 {
		timeout = defaultTransitionTimeout
	}
	return &Supervisor{timeout: timeout, done: make(chan struct{})}
}

// Add registers a component for coordinated shutdown. Components added after
// a terminal transition are stopped immediately.
func (s *Supervisor) Add(stopper Stopper) {
	if s == nil || stopper == nil {
		return
	}
	s.mu.Lock()
	if !s.terminal {
		s.stoppers = append(s.stoppers, stopper)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	stopper.Stop()
}

// TokenRevoked enters the terminal token-revoked state. It returns the first
// recorded reason and is safe to call concurrently from any Telegram path.
func (s *Supervisor) TokenRevoked(err error) error {
	if s == nil {
		return err
	}
	if err == nil {
		err = errors.New("bot token revoked")
	}
	s.transition.Do(func() {
		s.mu.Lock()
		s.terminal = true
		s.reason = err
		stoppers := append([]Stopper(nil), s.stoppers...)
		s.mu.Unlock()

		go s.stopAll(stoppers)
	})
	s.mu.Lock()
	reason := s.reason
	s.mu.Unlock()
	return reason
}

// Done closes when the coordinated transition has completed or reached its
// configured bound.
func (s *Supervisor) Done() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.done
}

// Terminal reports whether the application has entered its bounded terminal
// state.
func (s *Supervisor) Terminal() (bool, error) {
	if s == nil {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal, s.reason
}

func (s *Supervisor) stopAll(stoppers []Stopper) {
	var wg sync.WaitGroup
	for _, stopper := range stoppers {
		wg.Add(1)
		go func(component Stopper) {
			defer wg.Done()
			component.Stop()
		}(stopper)
	}

	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()

	timer := time.NewTimer(s.timeout)
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C:
	}
	close(s.done)
}
