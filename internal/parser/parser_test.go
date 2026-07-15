package parser

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseChannelExtractsPostsAndDecodesText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/s/example" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`
			<section class="tgme_channel_info"></section>
			<div class="tgme_widget_message" data-post="example/42">
				<div class="tgme_widget_message_text">Hello &amp; <b>world</b><br>line &lt;two&gt; &#1084;&#1080;&#1088; <a href="https://example.com/story#section">story</a></div>
				<div class="tgme_widget_message_footer">
					<span class="tgme_widget_message_views">1.7M</span>
					<a class="tgme_widget_message_date"><time datetime="2026-07-15T18:30:00+00:00">18:30</time></a>
				</div>
			</div>
			<div class="tgme_widget_message" data-post="example/41">
				<div class="tgme_widget_message_text">Small post</div>
				<span class="tgme_widget_message_views">245K</span>
				<time datetime="2026-07-15T17:00:00+00:00">17:00</time>
			</div>
			<div class="tgme_widget_message" data-post="example/40">
				<a class="tgme_widget_message_photo_wrap" href="https://t.me/example/40"></a>
			</div>`))
	}))
	defer server.Close()

	posts, err := NewWithOptions(Options{Client: server.Client(), BaseURL: server.URL}).ParseChannel("@Example")
	if err != nil {
		t.Fatalf("ParseChannel() error = %v", err)
	}
	if len(posts) != 2 {
		t.Fatalf("expected 2 text posts, got %d", len(posts))
	}
	if posts[0].MessageID != 42 || posts[0].Text != "Hello & world\nline <two> мир story" {
		t.Fatalf("unexpected first post: %+v", posts[0])
	}
	if posts[0].Caption != posts[0].Text {
		t.Fatalf("caption should preserve the parsed text: %+v", posts[0])
	}
	if len(posts[0].LinkURLs) != 1 || posts[0].LinkURLs[0] != "https://example.com/story#section" {
		t.Fatalf("unexpected links in first post: %v", posts[0].LinkURLs)
	}
	if posts[0].PostedAt != "2026-07-15T18:30:00+00:00" {
		t.Fatalf("unexpected timestamp: %q", posts[0].PostedAt)
	}
	if posts[0].ViewCount != 1700000 || posts[1].ViewCount != 245000 {
		t.Fatalf("unexpected view counts: %d, %d", posts[0].ViewCount, posts[1].ViewCount)
	}
}

func TestParseChannelErrors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantErr    error
		wantNoPost bool
	}{
		{
			name:    "http not found",
			status:  http.StatusNotFound,
			body:    "not found",
			wantErr: ErrChannelNotFound,
		},
		{
			name:    "private page",
			status:  http.StatusOK,
			body:    `<div class="tgme_page_description">This channel is private</div>`,
			wantErr: ErrChannelPrivate,
		},
		{
			name:       "empty channel",
			status:     http.StatusOK,
			body:       `<div class="tgme_channel_info"><h1>Example</h1></div>`,
			wantNoPost: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			posts, err := NewWithOptions(Options{Client: server.Client(), BaseURL: server.URL}).ParseChannel("example")
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want errors.Is(..., %v)", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseChannel() error = %v", err)
			}
			if tt.wantNoPost && len(posts) != 0 {
				t.Fatalf("expected no posts, got %d", len(posts))
			}
		})
	}
}

func TestParseChannelRateLimitErrorIncludesRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	_, err := NewWithOptions(Options{Client: server.Client(), BaseURL: server.URL}).ParseChannel("example")
	var rateLimitErr *RateLimitError
	if !errors.As(err, &rateLimitErr) {
		t.Fatalf("error = %v, want RateLimitError", err)
	}
	if rateLimitErr.RetryAfter != 17*time.Second {
		t.Fatalf("retry after = %s, want 17s", rateLimitErr.RetryAfter)
	}
}

func TestParseChannelRateLimitDefaultsBackoff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	_, err := NewWithOptions(Options{Client: server.Client(), BaseURL: server.URL}).ParseChannel("example")
	var rateLimitErr *RateLimitError
	if !errors.As(err, &rateLimitErr) {
		t.Fatalf("error = %v, want RateLimitError", err)
	}
	if rateLimitErr.RetryAfter != defaultRateLimitBackoff {
		t.Fatalf("retry after = %s, want %s", rateLimitErr.RetryAfter, defaultRateLimitBackoff)
	}
}

func TestParseChannelRejectsInvalidUsername(t *testing.T) {
	_, err := New().ParseChannel("../private")
	if !errors.Is(err, ErrInvalidUsername) {
		t.Fatalf("error = %v, want ErrInvalidUsername", err)
	}
}
