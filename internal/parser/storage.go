package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

// ChannelCursorStore persists the last Telegram message ID processed for a channel.
type ChannelCursorStore interface {
	UpdateLastPostID(channelID, lastPostID int64) error
}

// PostStore persists parsed posts.
type PostStore interface {
	Insert(post *model.Post) (int64, error)
}

// PostStorage converts parsed Telegram posts into database rows and advances a
// channel cursor after processing the fetched batch.
type PostStorage struct {
	channels ChannelCursorStore
	posts    PostStore
}

// NewPostStorage creates a post storage service backed by channel and post repositories.
func NewPostStorage(channels ChannelCursorStore, posts PostStore) *PostStorage {
	return &PostStorage{channels: channels, posts: posts}
}

// Store inserts posts newer than channel.LastPostID and returns only rows newly
// inserted during this call. Existing rows are skipped as duplicates. The
// cursor is advanced to the highest fetched message ID, even when every post
// in the batch was already stored.
func (s *PostStorage) Store(channel *model.Channel, parsed []ParsedPost) ([]model.Post, error) {
	if channel == nil {
		return nil, errors.New("store posts: channel is required")
	}
	if s == nil || s.channels == nil || s.posts == nil {
		return nil, errors.New("store posts: repositories are required")
	}

	posts := make([]model.Post, 0, len(parsed))
	lastPostID := channel.LastPostID
	for _, parsedPost := range parsed {
		if parsedPost.MessageID <= channel.LastPostID {
			continue
		}
		if parsedPost.MessageID > lastPostID {
			lastPostID = parsedPost.MessageID
		}

		post := model.Post{
			ChannelID:    channel.ID,
			MessageID:    parsedPost.MessageID,
			Text:         parsedPost.Text,
			PostedAt:     parsedPost.PostedAt,
			URL:          postURL(channel.Username, parsedPost.MessageID),
			ContentHash:  HashContent(parsedPost.Text),
			LinkURLsHash: HashLinkURLs(parsedPost.LinkURLs),
		}
		id, err := s.posts.Insert(&post)
		if errors.Is(err, db.ErrDuplicate) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("store post %d: %w", parsedPost.MessageID, err)
		}
		post.ID = id
		posts = append(posts, post)
	}

	if lastPostID > channel.LastPostID {
		if err := s.channels.UpdateLastPostID(channel.ID, lastPostID); err != nil {
			return nil, fmt.Errorf("update channel %d last post ID: %w", channel.ID, err)
		}
		channel.LastPostID = lastPostID
	}
	return posts, nil
}

// HashContent returns the lowercase SHA-256 digest of the first 500 Unicode
// characters of text, matching the deduplication contract.
func HashContent(text string) string {
	runes := []rune(text)
	if len(runes) > 500 {
		text = string(runes[:500])
	}
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

// HashLinkURLs returns a SHA-256 digest of normalized, sorted, unique URLs.
// It returns nil when the post contains no links.
func HashLinkURLs(rawURLs []string) *string {
	if len(rawURLs) == 0 {
		return nil
	}

	unique := make(map[string]struct{}, len(rawURLs))
	for _, rawURL := range rawURLs {
		normalized := normalizeLinkURL(rawURL)
		if normalized != "" {
			unique[normalized] = struct{}{}
		}
	}
	if len(unique) == 0 {
		return nil
	}

	urls := make([]string, 0, len(unique))
	for normalized := range unique {
		urls = append(urls, normalized)
	}
	sort.Strings(urls)
	digest := sha256.Sum256([]byte(strings.Join(urls, "\n")))
	hash := hex.EncodeToString(digest[:])
	return &hash
}

func normalizeLinkURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return rawURL
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	return parsed.String()
}

func extractLinkURLs(selection *goquery.Selection) []string {
	var links []string
	selection.Find("a[href]").Each(func(_ int, anchor *goquery.Selection) {
		if href, ok := anchor.Attr("href"); ok {
			links = append(links, href)
		}
	})
	return links
}

func postURL(username string, messageID int64) string {
	return fmt.Sprintf("https://t.me/%s/%d", username, messageID)
}
