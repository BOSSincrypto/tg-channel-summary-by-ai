package summarizer

import (
	"context"
	"errors"
	"fmt"
	"log"
)

// FallbackProvider retries a failed transient request with a secondary
// provider. The primary provider is intentionally used again on the next
// call, so a temporary outage does not permanently disable it.
type FallbackProvider struct {
	primary    Provider
	fallback   Provider
	onFallback func(error)
}

// NewFallbackProvider creates a provider that uses fallback after a
// transient primary-provider failure. The optional callback can notify an
// administrator that fallback was used.
func NewFallbackProvider(primary, fallback Provider, onFallback func(error)) (*FallbackProvider, error) {
	if primary == nil {
		return nil, errors.New("primary AI provider is required")
	}
	if fallback == nil {
		return nil, errors.New("fallback AI provider is required")
	}
	return &FallbackProvider{primary: primary, fallback: fallback, onFallback: onFallback}, nil
}

// Summarize tries the configured primary provider, then uses the fallback for
// transient failures such as timeouts, rate limits, and server errors.
func (p *FallbackProvider) Summarize(ctx context.Context, posts []Post) ([]Summary, error) {
	if p == nil || p.primary == nil || p.fallback == nil {
		return nil, errors.New("fallback AI provider is not configured")
	}
	summaries, err := p.primary.Summarize(ctx, posts)
	if err == nil {
		return summaries, nil
	}
	if !isTransientProviderError(ctx, err) {
		return nil, fmt.Errorf("primary AI provider failed: %w", err)
	}
	if p.onFallback != nil {
		p.onFallback(err)
	} else {
		log.Printf("primary AI provider failed, using fallback: %v", err)
	}
	summaries, fallbackErr := p.fallback.Summarize(ctx, posts)
	if fallbackErr != nil {
		return nil, fmt.Errorf("primary AI provider failed: %v; fallback AI provider failed: %w", err, fallbackErr)
	}
	return summaries, nil
}

var _ Provider = (*FallbackProvider)(nil)
