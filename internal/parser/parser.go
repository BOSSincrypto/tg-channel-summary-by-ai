// Package parser provides HTML parsing of t.me/s public channel pages
// using the goquery library. It extracts posts with message_id, text,
// media captions, timestamps, and view counts.
package parser

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

var (
	// ErrInvalidUsername indicates that a channel username cannot safely be used
	// in a t.me/s URL.
	ErrInvalidUsername = errors.New("invalid channel username")
	// ErrChannelNotFound indicates that Telegram could not resolve the channel.
	ErrChannelNotFound = errors.New("channel not found")
	// ErrChannelPrivate indicates that Telegram explicitly identifies a private
	// channel or a channel with previews disabled.
	ErrChannelPrivate = errors.New("channel is private or unavailable")
	// ErrChannelUnavailable indicates that Telegram returned an ambiguous page
	// which does not reliably identify a missing or private channel.
	ErrChannelUnavailable = errors.New("channel unavailable or ambiguous")
	// ErrCloudflareChallenge indicates that the response is an anti-bot
	// challenge page rather than a Telegram channel preview.
	ErrCloudflareChallenge = errors.New("cloudflare challenge page")
	// ErrChannelCloudflareChallenge is retained as a descriptive alias for
	// callers that classify channel failures by their domain.
	ErrChannelCloudflareChallenge = ErrCloudflareChallenge
)

const defaultRateLimitBackoff = 5 * time.Minute

// RateLimitError describes a t.me/s HTTP 429 response. RetryAfter is taken
// from Retry-After when present, or defaults to five minutes.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("t.me/s rate limited, retry after %s", e.RetryAfter)
}

// ParsedPost represents a single post extracted from t.me/s.
type ParsedPost struct {
	MessageID int64
	Text      string
	Caption   string
	PostedAt  string
	ViewCount int64
	LinkURLs  []string
}

// Options configures a Parser. BaseURL and Client are primarily useful for
// tests and local fixtures; production defaults target Telegram directly.
type Options struct {
	Client    *http.Client
	BaseURL   string
	UserAgent string
	Now       func() time.Time
}

// Parser fetches and parses t.me/s HTML pages.
type Parser struct {
	client    *http.Client
	baseURL   string
	userAgent string
	now       func() time.Time
}

// New creates a Parser using Telegram's public web preview endpoint.
func New() *Parser {
	return NewWithOptions(Options{})
}

// NewWithOptions creates a Parser with optional HTTP and endpoint overrides.
func NewWithOptions(options Options) *Parser {
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	baseURL := strings.TrimRight(options.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://t.me"
	}
	userAgent := options.UserAgent
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (compatible; TelegramDigestBot/1.0)"
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Parser{client: client, baseURL: baseURL, userAgent: userAgent, now: now}
}

// ParseChannel fetches and parses posts from t.me/s/{username}. Posts without
// text are media-only posts and are skipped so callers never submit empty text
// to a summarizer.
func (p *Parser) ParseChannel(username string) ([]ParsedPost, error) {
	posts, _, err := p.ParseChannelWithStats(username)
	return posts, err
}

// ParseChannelWithStats returns parsed text posts and counts media-only widgets
// skipped from the result.
func (p *Parser) ParseChannelWithStats(username string) ([]ParsedPost, ParseStats, error) {
	username, err := normalizeUsername(username)
	if err != nil {
		return nil, ParseStats{}, err
	}

	endpoint := p.baseURL + "/s/" + url.PathEscape(username)
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, ParseStats{}, fmt.Errorf("create t.me/s request: %w", err)
	}
	request.Header.Set("Accept", "text/html")
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	request.Header.Set("User-Agent", p.userAgent)

	response, err := p.client.Do(request)
	if err != nil {
		return nil, ParseStats{}, fmt.Errorf("fetch t.me/s/%s: %w", username, err)
	}
	defer response.Body.Close()
	stats := ParseStats{HTTPStatus: response.StatusCode}

	if response.StatusCode == http.StatusTooManyRequests {
		now := time.Now
		if p.now != nil {
			now = p.now
		}
		return nil, stats, &RateLimitError{RetryAfter: retryAfter(response.Header.Get("Retry-After"), now())}
	}
	document, err := goquery.NewDocumentFromReader(response.Body)
	if err != nil {
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return nil, stats, fmt.Errorf("fetch t.me/s/%s: unexpected HTTP status %s", username, response.Status)
		}
		return nil, stats, fmt.Errorf("parse t.me/s/%s HTML: %w", username, err)
	}
	if isCloudflareChallengePage(document) {
		return nil, stats, fmt.Errorf("fetch t.me/s/%s: %w", username, ErrCloudflareChallenge)
	}
	if response.StatusCode == http.StatusNotFound {
		return nil, stats, fmt.Errorf("fetch t.me/s/%s: %w", username, ErrChannelNotFound)
	}
	if response.StatusCode == http.StatusForbidden {
		return nil, stats, fmt.Errorf("fetch t.me/s/%s: %w", username, ErrChannelPrivate)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, stats, fmt.Errorf("fetch t.me/s/%s: unexpected HTTP status %s", username, response.Status)
	}

	if isPrivatePage(document) {
		return nil, stats, fmt.Errorf("parse t.me/s/%s: %w", username, ErrChannelPrivate)
	}
	if isNotFoundPage(document) {
		return nil, stats, fmt.Errorf("parse t.me/s/%s: %w", username, ErrChannelNotFound)
	}
	if document.Find(".tgme_widget_message[data-post]").Length() == 0 &&
		document.Find(".tgme_channel_info").Length() == 0 {
		return nil, stats, fmt.Errorf("parse t.me/s/%s: %w", username, ErrChannelUnavailable)
	}
	stats.ChannelTitle = extractChannelTitle(document)

	posts := make([]ParsedPost, 0)
	document.Find(".tgme_widget_message[data-post]").Each(func(_ int, selection *goquery.Selection) {
		post, ok := parsePost(selection)
		if ok {
			posts = append(posts, post)
			return
		}
		stats.MediaOnlySkipped++
	})
	return posts, stats, nil
}

func extractChannelTitle(document *goquery.Document) string {
	if document == nil {
		return ""
	}
	for _, selector := range []string{
		".tgme_channel_info_header_title",
		".tgme_channel_info .tgme_page_title",
		".tgme_channel_info h1",
	} {
		title := strings.TrimSpace(document.Find(selector).First().Text())
		if title != "" {
			return title
		}
	}
	return ""
}

func parsePost(selection *goquery.Selection) (ParsedPost, bool) {
	postName, exists := selection.Attr("data-post")
	if !exists {
		return ParsedPost{}, false
	}
	parts := strings.Split(strings.TrimSpace(postName), "/")
	if len(parts) != 2 {
		return ParsedPost{}, false
	}
	messageID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || messageID < 1 {
		return ParsedPost{}, false
	}

	textSelection := selection.Find(".tgme_widget_message_text").First()
	if textSelection.Length() == 0 {
		return ParsedPost{}, false
	}
	text := plainText(textSelection)
	if text == "" {
		return ParsedPost{}, false
	}

	postedAt, _ := selection.Find("time[datetime]").First().Attr("datetime")
	views := parseViewCount(selection.Find(".tgme_widget_message_views").First().Text())
	return ParsedPost{
		MessageID: messageID,
		Text:      text,
		Caption:   text,
		PostedAt:  strings.TrimSpace(postedAt),
		ViewCount: views,
		LinkURLs:  extractLinkURLs(textSelection),
	}, true
}

func normalizeUsername(username string) (string, error) {
	username = strings.TrimSpace(strings.TrimPrefix(username, "@"))
	if username == "" || len(username) > 32 {
		return "", ErrInvalidUsername
	}
	for _, char := range username {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' {
			return "", ErrInvalidUsername
		}
	}
	return strings.ToLower(username), nil
}

func isPrivatePage(document *goquery.Document) bool {
	text := strings.ToLower(strings.TrimSpace(document.Find(".tgme_page_description, .tgme_page_title").Text()))
	return strings.Contains(text, "channel is private") ||
		strings.Contains(text, "this channel is private") ||
		strings.Contains(text, "private channel") ||
		strings.Contains(text, "канал является приватным") ||
		strings.Contains(text, "канал приватный")
}

func isCloudflareChallengePage(document *goquery.Document) bool {
	if document == nil {
		return false
	}
	title := strings.ToLower(strings.TrimSpace(document.Find("title").First().Text()))
	if title == "just a moment..." || title == "checking your browser" {
		return true
	}
	if document.Find(`script[src*="challenge-platform"], script[src*="cdn-cgi/challenge"], #cf-chl-widget, .cf-chl-widget, .cf-turnstile, form#challenge-form`).Length() > 0 {
		return true
	}
	text := strings.ToLower(strings.TrimSpace(document.Text()))
	return strings.Contains(text, "checking your browser before accessing") ||
		strings.Contains(text, "enable javascript and cookies to continue") ||
		strings.Contains(text, "performing security verification")
}

func isNotFoundPage(document *goquery.Document) bool {
	if document.Find(".tgme_widget_message[data-post]").Length() > 0 {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(document.Find(".tgme_page_description, .tgme_page_title, .tgme_page_action").Text()))
	return strings.Contains(text, "not found") ||
		strings.Contains(text, "doesn't exist") ||
		strings.Contains(text, "does not exist") ||
		strings.Contains(text, "не найден") ||
		strings.Contains(text, "deleted") ||
		strings.Contains(text, "удалён") ||
		strings.Contains(text, "удален")
}

func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		maxSeconds := int64(time.Duration(1<<63-1) / time.Second)
		if seconds > maxSeconds {
			return defaultRateLimitBackoff
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		if delay := date.Sub(now); delay > 0 {
			return delay
		}
		return 0
	}
	return defaultRateLimitBackoff
}

func parseViewCount(value string) int64 {
	value = strings.TrimSpace(strings.ReplaceAll(value, ",", ""))
	if value == "" {
		return 0
	}
	multiplier := float64(1)
	last := value[len(value)-1]
	switch last {
	case 'k', 'K':
		multiplier = 1_000
		value = value[:len(value)-1]
	case 'm', 'M':
		multiplier = 1_000_000
		value = value[:len(value)-1]
	case 'b', 'B':
		multiplier = 1_000_000_000
		value = value[:len(value)-1]
	}
	number, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || number < 0 {
		return 0
	}
	return int64(number * multiplier)
}

func plainText(selection *goquery.Selection) string {
	var builder strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			builder.WriteString(node.Data)
			return
		}
		if node.Type == html.ElementNode && node.Data == "br" {
			builder.WriteByte('\n')
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	for _, node := range selection.Nodes {
		walk(node)
	}

	lines := strings.Split(strings.ReplaceAll(builder.String(), "\r", ""), "\n")
	clean := make([]string, 0, len(lines))
	for _, line := range lines {
		clean = append(clean, strings.Join(strings.Fields(line), " "))
	}
	text := strings.TrimSpace(strings.Join(clean, "\n"))
	text = regexp.MustCompile(`\n{3,}`).ReplaceAllString(text, "\n\n")
	return text
}
