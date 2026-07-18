package lifecycle

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countingStopper struct {
	count atomic.Int32
	wait  time.Duration
}

func (s *countingStopper) Stop() {
	s.count.Add(1)
	if s.wait > 0 {
		time.Sleep(s.wait)
	}
}

func TestSupervisorTokenRevokedStopsAllComponentsOnce(t *testing.T) {
	supervisor := New(time.Second)
	components := []*countingStopper{{}, {}, {}}
	for _, component := range components {
		supervisor.Add(component)
	}

	first := errors.New("401 Unauthorized")
	if got := supervisor.TokenRevoked(first); !errors.Is(got, first) {
		t.Fatalf("first transition error = %v, want %v", got, first)
	}
	if got := supervisor.TokenRevoked(errors.New("another 401")); !errors.Is(got, first) {
		t.Fatalf("second transition error = %v, want original %v", got, first)
	}
	select {
	case <-supervisor.Done():
	case <-time.After(time.Second):
		t.Fatal("supervisor transition did not complete")
	}
	for index, component := range components {
		if got := component.count.Load(); got != 1 {
			t.Fatalf("component %d stop count = %d, want 1", index, got)
		}
	}
	terminal, reason := supervisor.Terminal()
	if !terminal || !errors.Is(reason, first) {
		t.Fatalf("terminal state = %v, reason = %v", terminal, reason)
	}
}

func TestSupervisorTransitionIsBounded(t *testing.T) {
	supervisor := New(20 * time.Millisecond)
	supervisor.Add(&countingStopper{wait: time.Second})
	start := time.Now()
	supervisor.TokenRevoked(errors.New("revoked"))
	select {
	case <-supervisor.Done():
	case <-time.After(250 * time.Millisecond):
		t.Fatal("bounded transition exceeded test limit")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("transition took %s, want bounded completion", elapsed)
	}
}

func TestSupervisorAddAfterTransitionStopsImmediately(t *testing.T) {
	supervisor := New(time.Second)
	supervisor.TokenRevoked(errors.New("revoked"))
	component := &countingStopper{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		supervisor.Add(component)
	}()
	wg.Wait()
	if got := component.count.Load(); got != 1 {
		t.Fatalf("late component stop count = %d, want 1", got)
	}
}
