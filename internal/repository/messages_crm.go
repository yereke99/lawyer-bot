package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"lawyer-bot/internal/domain"
)

// This file adds the CRM read paths over the same messages table the pipeline
// writes to. Nothing here duplicates the conversation: the CRM and the AI share
// one message model.

// PageByUser returns one page of a conversation, newest page first but each page
// in chronological order.
//
// beforeID pages backwards through history, so opening a client loads the tail
// of the conversation instead of every message ever exchanged.
func (r *MessageRepository) PageByUser(ctx context.Context, userID int64, beforeID int64, limit int) ([]domain.Message, bool, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	where := "user_id = ?"
	args := []any{userID}
	if beforeID > 0 {
		where += " AND id < ?"
		args = append(args, beforeID)
	}
	// One extra row tells the caller whether an older page exists.
	args = append(args, limit+1)

	rows, err := r.db.QueryContext(ctx, `SELECT `+messageColumns+` FROM (
			SELECT `+messageColumns+` FROM messages WHERE `+where+`
			ORDER BY id DESC LIMIT ?
		) ORDER BY id ASC`, args...)
	if err != nil {
		return nil, false, fmt.Errorf("page messages: %w", err)
	}
	defer rows.Close()

	var out []domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("page messages: %w", err)
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[1:]
	}
	return out, hasMore, nil
}

// SinceID returns messages newer than a given ID, which is how the CRM live
// stream catches up after a reconnect without refetching the conversation.
func (r *MessageRepository) SinceID(ctx context.Context, userID, afterID int64, limit int) ([]domain.Message, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE user_id = ? AND id > ? ORDER BY id ASC LIMIT ?`,
		userID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("messages since: %w", err)
	}
	defer rows.Close()

	var out []domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetByID loads one message.
func (r *MessageRepository) GetByID(ctx context.Context, id int64) (*domain.Message, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("load message: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("load message: %w", err)
		}
		return nil, ErrNotFound
	}
	m, err := scanMessage(rows)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// LastInboundID returns the newest client message ID, which anchors follow-up
// scheduling: a nudge is only valid while it is still the newest inbound row.
func (r *MessageRepository) LastInboundID(ctx context.Context, userID int64) (int64, error) {
	var id int64
	err := r.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM messages WHERE user_id = ? AND direction = ?`,
		userID, string(domain.DirectionIncoming)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("last inbound id: %w", err)
	}
	return id, nil
}

// SetDeliveryStatus records the outcome of an outbound send on the message row.
func (r *MessageRepository) SetDeliveryStatus(ctx context.Context, id int64, status string) error {
	if id == 0 {
		return nil
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE messages SET delivery_status = ? WHERE id = ?`, truncate(status, 100), id)
	if err != nil {
		return fmt.Errorf("set delivery status: %w", err)
	}
	return nil
}

// MessageSearchHit is one message-text search result with its client.
type MessageSearchHit struct {
	MessageID   int64     `json:"message_id"`
	UserID      int64     `json:"client_id"`
	ClientName  string    `json:"client_name"`
	ClientPhone string    `json:"client_phone"`
	Direction   string    `json:"direction"`
	SenderType  string    `json:"sender_type"`
	Snippet     string    `json:"snippet"`
	CreatedAt   time.Time `json:"created_at"`
}

// SearchText finds messages containing a phrase. It is a plain indexed LIKE
// scan bounded by a limit: search never calls the language model.
func (r *MessageRepository) SearchText(ctx context.Context, query string, limit int) ([]MessageSearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	like := "%" + strings.ToLower(query) + "%"

	rows, err := r.db.QueryContext(ctx, `
		SELECT m.id, m.user_id, u.display_name, u.phone_number, m.direction, m.sender_type,
			CASE WHEN m.text <> '' THEN m.text ELSE m.caption END AS body, m.created_at
		FROM messages m JOIN users u ON u.id = m.user_id
		WHERE lower(m.text) LIKE ? OR lower(m.caption) LIKE ?
		ORDER BY m.id DESC LIMIT ?`, like, like, limit)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	defer rows.Close()

	var out []MessageSearchHit
	for rows.Next() {
		var h MessageSearchHit
		if err := rows.Scan(&h.MessageID, &h.UserID, &h.ClientName, &h.ClientPhone,
			&h.Direction, &h.SenderType, &h.Snippet, &h.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan search hit: %w", err)
		}
		h.Snippet = truncateRunes(h.Snippet, 160)
		out = append(out, h)
	}
	return out, rows.Err()
}

// HistoryForSummary returns the messages a client exchanged after a watermark,
// which is the only history the rolling summariser ever reads.
func (r *MessageRepository) HistoryForSummary(ctx context.Context, userID, afterID int64, limit int) ([]domain.Message, error) {
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+messageColumns+` FROM messages
		WHERE user_id = ? AND id > ? AND (text <> '' OR caption <> '')
		ORDER BY id ASC LIMIT ?`, userID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("history for summary: %w", err)
	}
	defer rows.Close()

	var out []domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetMedia attaches a locally stored media file to a message row.
func (r *MessageRepository) SetMedia(ctx context.Context, id int64, path, mimeType, name string, size int64) error {
	if id == 0 {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE messages SET media_path = ?, media_mime = ?, media_name = ?, media_size = ?
		WHERE id = ?`, path, truncate(mimeType, 150), truncateRunes(name, 200), size, id)
	if err != nil {
		return fmt.Errorf("attach media: %w", err)
	}
	return nil
}
