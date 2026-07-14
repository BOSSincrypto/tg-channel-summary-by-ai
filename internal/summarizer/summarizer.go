// Package summarizer provides AI-powered post summarization.
// It defines a Provider interface for different AI backends
// (OpenRouter, custom OpenAI-compatible APIs) and implements
// batch summarization with one-sentence Russian summaries per post.
package summarizer

import "context"

// Provider defines the interface for AI summarization backends.
type Provider interface {
	// Summarize generates one-sentence Russian summaries for each post.
	Summarize(ctx context.Context, posts []Post) ([]Summary, error)
}

// Post represents a post to be summarized.
type Post struct {
	ID   int64
	Text string
}

// Summary represents the AI-generated summary of a post.
type Summary struct {
	PostID int64
	Text   string
}

// Service orchestrates summarization using a configured provider.
type Service struct {
	provider Provider
}

// New creates a new summarizer Service.
func New(provider Provider) *Service {
	return &Service{provider: provider}
}

// SummarizePosts summarizes a batch of posts.
func (s *Service) SummarizePosts(ctx context.Context, posts []Post) ([]Summary, error) {
	return s.provider.Summarize(ctx, posts)
}
