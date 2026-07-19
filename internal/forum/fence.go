// Package forum contains shared coordination primitives for forum-topic
// lifecycle mutations.
package forum

import (
	"errors"
	"sync"
)

// MutationFence serializes all mutations that can change the relationship
// between Telegram forum topics and durable assignments. A single instance is
// shared by the WebApp and Telegram services in production.
type MutationFence struct {
	mu sync.Mutex
}

// With runs fn while holding the fence.
func (f *MutationFence) With(fn func() error) error {
	if f == nil {
		return errors.New("forum mutation fence is not configured")
	}
	if fn == nil {
		return errors.New("forum mutation fence callback is required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return fn()
}

// Lock and Unlock are provided for callers that need to hold the fence across
// a narrow interface boundary.
func (f *MutationFence) Lock() {
	if f != nil {
		f.mu.Lock()
	}
}

func (f *MutationFence) Unlock() {
	if f != nil {
		f.mu.Unlock()
	}
}
