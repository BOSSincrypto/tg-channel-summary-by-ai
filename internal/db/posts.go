package db

import (
	"database/sql"
	"fmt"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

// PostRepository provides CRUD operations for channel posts.
type PostRepository struct {
	db *DB
}

// Insert adds a new post. Returns the inserted ID.
// If a post with the same (channel_id, message_id) already exists,
// it returns the existing row's ID and an error satisfying errors.Is(..., ErrDuplicate).
func (r *PostRepository) Insert(p *model.Post) (int64, error) {
	var linkURLsHash interface{}
	if p.LinkURLsHash != nil {
		linkURLsHash = *p.LinkURLsHash
	}
	var summary interface{}
	if p.Summary != nil {
		summary = *p.Summary
	}

	result, err := r.db.Conn().Exec(
		`INSERT INTO posts (channel_id, message_id, text, summary, posted_at, url, content_hash, link_urls_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ChannelID, p.MessageID, p.Text, summary, p.PostedAt, p.URL, p.ContentHash, linkURLsHash,
	)
	if err != nil {
		// Check for UNIQUE constraint violation
		if isUniqueViolation(err) {
			// Fetch the existing ID
			var existingID int64
			scanErr := r.db.Conn().QueryRow(
				`SELECT id FROM posts WHERE channel_id = ? AND message_id = ?`,
				p.ChannelID, p.MessageID,
			).Scan(&existingID)
			if scanErr != nil {
				return 0, fmt.Errorf("insert post (duplicate, fetch existing): %w", err)
			}
			return existingID, ErrDuplicate
		}
		return 0, fmt.Errorf("insert post: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}
	return id, nil
}

// GetByID returns a post by its ID.
func (r *PostRepository) GetByID(id int64) (*model.Post, error) {
	p := &model.Post{}
	var summary, linkURLsHash sql.NullString
	err := r.db.Conn().QueryRow(
		`SELECT id, channel_id, message_id, text, summary, posted_at, url, content_hash, link_urls_hash, created_at
		 FROM posts WHERE id = ?`, id,
	).Scan(&p.ID, &p.ChannelID, &p.MessageID, &p.Text, &summary,
		&p.PostedAt, &p.URL, &p.ContentHash, &linkURLsHash, &p.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get post by id: %w", err)
	}
	if summary.Valid {
		p.Summary = &summary.String
	}
	if linkURLsHash.Valid {
		p.LinkURLsHash = &linkURLsHash.String
	}
	return p, nil
}

// GetByChannelAndMessageID returns a post by its channel and Telegram message ID.
func (r *PostRepository) GetByChannelAndMessageID(channelID, messageID int64) (*model.Post, error) {
	p := &model.Post{}
	var summary, linkURLsHash sql.NullString
	err := r.db.Conn().QueryRow(
		`SELECT id, channel_id, message_id, text, summary, posted_at, url, content_hash, link_urls_hash, created_at
		 FROM posts WHERE channel_id = ? AND message_id = ?`, channelID, messageID,
	).Scan(&p.ID, &p.ChannelID, &p.MessageID, &p.Text, &summary,
		&p.PostedAt, &p.URL, &p.ContentHash, &linkURLsHash, &p.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get post by channel+message: %w", err)
	}
	if summary.Valid {
		p.Summary = &summary.String
	}
	if linkURLsHash.Valid {
		p.LinkURLsHash = &linkURLsHash.String
	}
	return p, nil
}

// UpdateSummary sets the AI-generated summary for a post.
func (r *PostRepository) UpdateSummary(id int64, summary string) error {
	_, err := r.db.Conn().Exec(
		`UPDATE posts SET summary = ? WHERE id = ?`,
		summary, id,
	)
	if err != nil {
		return fmt.Errorf("update post summary: %w", err)
	}
	return nil
}

// ListUnsummarized returns posts without summaries for a given group's channels,
// posted within the last hours (e.g., 24). Results are deduplicated by the
// composite signature of content_hash + link_urls_hash across all assigned channels.
func (r *PostRepository) ListUnsummarized(groupID int64, hours int) ([]model.Post, error) {
	rows, err := r.db.Conn().Query(
		`SELECT p.id, p.channel_id, p.message_id, p.text, p.summary,
		        p.posted_at, p.url, p.content_hash, p.link_urls_hash, p.created_at
		 FROM posts p
		 INNER JOIN group_channels gc ON p.channel_id = gc.channel_id
		 WHERE gc.group_id = ?
		   AND p.summary IS NULL
		   AND datetime(p.posted_at) >= datetime('now', ? || ' hours')
		 ORDER BY datetime(p.posted_at) DESC, p.channel_id ASC, p.message_id ASC`,
		groupID, fmt.Sprintf("-%d", hours),
	)
	if err != nil {
		return nil, fmt.Errorf("list unsummarized posts: %w", err)
	}
	defer rows.Close()

	var posts []model.Post
	seen := make(map[string]struct{})
	for rows.Next() {
		var p model.Post
		var summary, linkURLsHash sql.NullString
		if err := rows.Scan(&p.ID, &p.ChannelID, &p.MessageID, &p.Text, &summary,
			&p.PostedAt, &p.URL, &p.ContentHash, &linkURLsHash, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan post: %w", err)
		}
		if summary.Valid {
			p.Summary = &summary.String
		}
		if linkURLsHash.Valid {
			p.LinkURLsHash = &linkURLsHash.String
		}

		key := dedupSignatureKey(p.ContentHash, linkURLsHash)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		posts = append(posts, p)
	}
	return posts, rows.Err()
}

func dedupSignatureKey(contentHash string, linkURLsHash sql.NullString) string {
	if linkURLsHash.Valid {
		return contentHash + "\x00" + linkURLsHash.String
	}
	return contentHash + "\x00"
}

// ExistsByContentHash checks if a post with the same content_hash already exists
// within a given group (across all channels assigned to that group).
func (r *PostRepository) ExistsByContentHash(groupID int64, contentHash string) (bool, error) {
	var count int
	err := r.db.Conn().QueryRow(
		`SELECT COUNT(*)
		 FROM posts p
		 INNER JOIN group_channels gc ON p.channel_id = gc.channel_id
		 WHERE gc.group_id = ? AND p.content_hash = ?`,
		groupID, contentHash,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check content hash: %w", err)
	}
	return count > 0, nil
}

// DeleteOlderThan deletes posts posted before the given date.
// Returns the number of rows deleted.
func (r *PostRepository) DeleteOlderThan(days int) (int64, error) {
	result, err := r.db.Conn().Exec(
		`DELETE FROM posts
		 WHERE datetime(posted_at) < datetime('now', ? || ' days')
		   AND NOT EXISTS (
			SELECT 1
			FROM digest_posts dp
			INNER JOIN digests d ON d.id = dp.digest_id
			WHERE dp.post_id = posts.id
			  AND datetime(d.sent_at) >= datetime('now', ? || ' days')
		   )`,
		fmt.Sprintf("-%d", days),
		fmt.Sprintf("-%d", days),
	)
	if err != nil {
		return 0, fmt.Errorf("delete old posts: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete old posts rows affected: %w", err)
	}
	return n, nil
}
