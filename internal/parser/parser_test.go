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
			name:    "cloudflare challenge page",
			status:  http.StatusOK,
			body:    `<html><head><title>Just a moment...</title><script src="/cdn-cgi/challenge-platform/h/g/orchestrate/chl_page/v1"></script></head><body><div id="cf-chl-widget"></div></body></html>`,
			wantErr: ErrCloudflareChallenge,
		},
		{
			name:    "cloudflare challenge response with forbidden status",
			status:  http.StatusForbidden,
			body:    `<html><head><title>Checking your browser</title></head><body>Checking your browser before accessing</body></html>`,
			wantErr: ErrCloudflareChallenge,
		},
		{
			name:    "cloudflare challenge response with service unavailable status",
			status:  http.StatusServiceUnavailable,
			body:    `<html><head><title>Just a moment...</title></head><body><div class="cf-turnstile"></div></body></html>`,
			wantErr: ErrCloudflareChallenge,
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

func TestParseChannelKeepsNotFoundPrivateAndAmbiguousPagesDistinct(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr error
	}{
		{
			name:    "reliable not found page",
			body:    `<div class="tgme_page_title">Channel not found</div>`,
			wantErr: ErrChannelNotFound,
		},
		{
			name:    "reliable private page",
			body:    `<div class="tgme_page_description">This channel is private</div>`,
			wantErr: ErrChannelPrivate,
		},
		{
			name:    "ambiguous contact page",
			body:    `<div class="tgme_page"><div class="tgme_page_description">If you have Telegram, you can contact <strong>Telegram</strong> right away.</div></div>`,
			wantErr: ErrChannelUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			_, err := NewWithOptions(Options{Client: server.Client(), BaseURL: server.URL}).ParseChannel("example")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want errors.Is(..., %v)", err, tt.wantErr)
			}
			if tt.wantErr == ErrChannelUnavailable && (errors.Is(err, ErrChannelNotFound) || errors.Is(err, ErrChannelPrivate)) {
				t.Fatalf("ambiguous page was over-classified: %v", err)
			}
		})
	}
}

func TestParseChannelWithStatsReportsHTTPStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "empty successful page", status: http.StatusOK, body: `<div class="tgme_channel_info"></div>`},
		{name: "failed request", status: http.StatusBadGateway, body: `upstream failed`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			_, stats, err := NewWithOptions(Options{Client: server.Client(), BaseURL: server.URL}).ParseChannelWithStats("example")
			if stats.HTTPStatus != tt.status {
				t.Fatalf("HTTP status = %d, want %d", stats.HTTPStatus, tt.status)
			}
			if tt.status == http.StatusOK && err != nil {
				t.Fatalf("successful page error = %v", err)
			}
			if tt.status != http.StatusOK && err == nil {
				t.Fatal("failed request returned nil error")
			}
		})
	}
}

func TestParseChannelWithStatsPreservesVerifiedChannelTitle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`
			<div class="tgme_channel_info">
				<div class="tgme_channel_info_header_title">Verified Channel</div>
			</div>`))
	}))
	defer server.Close()

	_, stats, err := NewWithOptions(Options{Client: server.Client(), BaseURL: server.URL}).ParseChannelWithStats("example")
	if err != nil {
		t.Fatalf("ParseChannelWithStats() error = %v", err)
	}
	if stats.ChannelTitle != "Verified Channel" {
		t.Fatalf("channel title = %q, want Verified Channel", stats.ChannelTitle)
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

func TestParseChannelRateLimitInvalidHeaderDefaultsBackoff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "not-a-duration")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	_, err := NewWithOptions(Options{Client: server.Client(), BaseURL: server.URL}).ParseChannel("example")
	var rateLimitErr *RateLimitError
	if !errors.As(err, &rateLimitErr) {
		t.Fatalf("error = %v, want RateLimitError", err)
	}
	if rateLimitErr.RetryAfter != defaultRateLimitBackoff {
		t.Fatalf("retry after = %s, want default %s", rateLimitErr.RetryAfter, defaultRateLimitBackoff)
	}
}

func TestParseChannelRateLimitHTTPDateUsesInjectedClock(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", now.Add(45*time.Second).Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	_, err := NewWithOptions(Options{
		Client:  server.Client(),
		BaseURL: server.URL,
		Now:     func() time.Time { return now },
	}).ParseChannel("example")
	var rateLimitErr *RateLimitError
	if !errors.As(err, &rateLimitErr) {
		t.Fatalf("error = %v, want RateLimitError", err)
	}
	if rateLimitErr.RetryAfter != 45*time.Second {
		t.Fatalf("retry after = %s, want 45s", rateLimitErr.RetryAfter)
	}
}

func TestParseChannelRateLimitOverflowUsesDefaultBackoff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "9223372036854775807")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	_, err := NewWithOptions(Options{Client: server.Client(), BaseURL: server.URL}).ParseChannel("example")
	var rateLimitErr *RateLimitError
	if !errors.As(err, &rateLimitErr) {
		t.Fatalf("error = %v, want RateLimitError", err)
	}
	if rateLimitErr.RetryAfter != defaultRateLimitBackoff {
		t.Fatalf("retry after = %s, want default %s", rateLimitErr.RetryAfter, defaultRateLimitBackoff)
	}
}

func TestParseChannelRejectsInvalidUsername(t *testing.T) {
	_, err := New().ParseChannel("../private")
	if !errors.Is(err, ErrInvalidUsername) {
		t.Fatalf("error = %v, want ErrInvalidUsername", err)
	}
}
