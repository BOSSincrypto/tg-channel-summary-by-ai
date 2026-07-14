package db

import (
	"database/sql"
	"fmt"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

// DigestRepository provides CRUD operations for digests.
type DigestRepository struct {
	db *DB
}

// Insert creates a new digest entry and returns its ID.
func (r *DigestRepository) Insert(d *model.Digest) (int64, error) {
	var msgID interface{}
	if d.MessageID != nil {
		msgID = *d.MessageID
	}
	result, err := r.db.Conn().Exec(
		`INSERT INTO digests (group_id, message_id, post_count) VALUES (?, ?, ?)`,
		d.GroupID, msgID, d.PostCount,
	)
	if err != nil {
		return 0, fmt.Errorf("insert digest: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}
	return id, nil
}

// GetByID returns a digest by its ID.
func (r *DigestRepository) GetByID(id int64) (*model.Digest, error) {
	d := &model.Digest{}
	var msgID sql.NullInt64
	err := r.db.Conn().QueryRow(
		`SELECT id, group_id, sent_at, message_id, post_count
		 FROM digests WHERE id = ?`, id,
	).Scan(&d.ID, &d.GroupID, &d.SentAt, &msgID, &d.PostCount)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get digest by id: %w", err)
	}
	if msgID.Valid {
		d.MessageID = &msgID.Int64
	}
	return d, nil
}

// ListByGroup returns digests for a given group, ordered by sent_at descending,
// limited to the given count.
func (r *DigestRepository) ListByGroup(groupID int64, limit int) ([]model.Digest, error) {
	rows, err := r.db.Conn().Query(
		`SELECT id, group_id, sent_at, message_id, post_count
		 FROM digests WHERE group_id = ?
		 ORDER BY sent_at DESC LIMIT ?`,
		groupID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list digests: %w", err)
	}
	defer rows.Close()

	var digests []model.Digest
	for rows.Next() {
		var d model.Digest
		var msgID sql.NullInt64
		if err := rows.Scan(&d.ID, &d.GroupID, &d.SentAt, &msgID, &d.PostCount); err != nil {
			return nil, fmt.Errorf("scan digest: %w", err)
		}
		if msgID.Valid {
			d.MessageID = &msgID.Int64
		}
		digests = append(digests, d)
	}
	return digests, rows.Err()
}

// UpdateMessageID sets the Telegram message ID after sending the digest.
func (r *DigestRepository) UpdateMessageID(id int64, messageID int64) error {
	_, err := r.db.Conn().Exec(
		`UPDATE digests SET message_id = ? WHERE id = ?`,
		messageID, id,
	)
	if err != nil {
		return fmt.Errorf("update digest message_id: %w", err)
	}
	return nil
}

// AddPost links a post to a digest.
func (r *DigestRepository) AddPost(digestID, postID int64) error {
	_, err := r.db.Conn().Exec(
		`INSERT OR IGNORE INTO digest_posts (digest_id, post_id) VALUES (?, ?)`,
		digestID, postID,
	)
	if err != nil {
		return fmt.Errorf("add post to digest: %w", err)
	}
	return nil
}

// GetPostsForDigest returns all posts linked to a digest.
func (r *DigestRepository) GetPostsForDigest(digestID int64) ([]model.Post, error) {
	rows, err := r.db.Conn().Query(
		`SELECT p.id, p.channel_id, p.message_id, p.text, p.summary,
		        p.posted_at, p.url, p.content_hash, p.link_urls_hash, p.created_at
		 FROM posts p
		 INNER JOIN digest_posts dp ON p.id = dp.post_id
		 WHERE dp.digest_id = ?
		 ORDER BY p.channel_id, p.posted_at DESC`,
		digestID,
	)
	if err != nil {
		return nil, fmt.Errorf("get posts for digest: %w", err)
	}
	defer rows.Close()

	var posts []model.Post
	for rows.Next() {
		var p model.Post
		var summary, linkURLsHash sql.NullString
		if err := rows.Scan(&p.ID, &p.ChannelID, &p.MessageID, &p.Text, &summary,
			&p.PostedAt, &p.URL, &p.ContentHash, &linkURLsHash, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan digest post: %w", err)
		}
		if summary.Valid {
			p.Summary = &summary.String
		}
		if linkURLsHash.Valid {
			p.LinkURLsHash = &linkURLsHash.String
		}
		posts = append(posts, p)
	}
	return posts, rows.Err()
}

// DeleteOlderThan deletes digests and their post associations older than the
// given number of days. Returns the number of deleted digests.
func (r *DigestRepository) DeleteOlderThan(days int) (int64, error) {
	result, err := r.db.Conn().Exec(
		`DELETE FROM digests WHERE sent_at < datetime('now', ? || ' days')`,
		fmt.Sprintf("-%d", days),
	)
	if err != nil {
		return 0, fmt.Errorf("delete old digests: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, nil
}
