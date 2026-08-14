package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"lawyer-bot/internal/domain"
)

// MessageRepository persists every message in both directions.
type MessageRepository struct {
	db *DB
}

// NewMessageRepository builds a MessageRepository.
func NewMessageRepository(db *DB) *MessageRepository {
	return &MessageRepository{db: db}
}

const messageColumns = `id, user_id, whatsapp_message_id, trace_id, message_type, text,
	media_id, caption, direction, processed, ai_processed, ai_intent,
	ai_confidence, bot_responded, created_at`

// Create stores a message and returns its assigned ID.
func (r *MessageRepository) Create(ctx context.Context, m *domain.Message) (int64, error) {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO messages (user_id, whatsapp_message_id, trace_id, message_type, text,
			media_id, caption, direction, processed, ai_processed, ai_intent,
			ai_confidence, bot_responded, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.UserID, m.WhatsAppMessageID, m.TraceID, string(m.MessageType), m.Text,
		m.MediaID, m.Caption, string(m.Direction), boolToInt(m.Processed),
		boolToInt(m.AIProcessed), m.AIIntent, m.AIConfidence,
		boolToInt(m.BotResponded), m.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("insert message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("message last insert id: %w", err)
	}
	m.ID = id
	return id, nil
}

// ExistsByWhatsAppID reports whether a provider message ID was already stored.
// WhatsApp retries webhook deliveries, so this guard prevents double processing
// and double replies.
func (r *MessageRepository) ExistsByWhatsAppID(ctx context.Context, whatsappMessageID string) (bool, error) {
	if whatsappMessageID == "" {
		return false, nil
	}
	var one int
	err := r.db.QueryRowContext(ctx,
		`SELECT 1 FROM messages WHERE whatsapp_message_id = ? LIMIT 1`, whatsappMessageID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check duplicate message: %w", err)
	}
	return true, nil
}

// MarkProcessed records the outcome of the pipeline for one incoming message.
func (r *MessageRepository) MarkProcessed(ctx context.Context, id int64, aiProcessed bool, intent string, confidence float64, botResponded bool) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE messages SET processed = 1, ai_processed = ?, ai_intent = ?,
			ai_confidence = ?, bot_responded = ?
		WHERE id = ?`,
		boolToInt(aiProcessed), intent, confidence, boolToInt(botResponded), id)
	if err != nil {
		return fmt.Errorf("mark message processed: %w", err)
	}
	return nil
}

// SetProviderID attaches the provider message ID to an outbound message once
// the send succeeds.
func (r *MessageRepository) SetProviderID(ctx context.Context, id int64, providerID string) error {
	if providerID == "" {
		return nil
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE messages SET whatsapp_message_id = ? WHERE id = ?`, providerID, id)
	if err != nil {
		return fmt.Errorf("set provider message id: %w", err)
	}
	return nil
}

// RecentByUser returns the newest limit messages for a user in chronological
// order. This is the only conversation history the model ever receives.
func (r *MessageRepository) RecentByUser(ctx context.Context, userID int64, limit int) ([]domain.Message, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM (
			SELECT `+messageColumns+` FROM messages
			WHERE user_id = ? AND (text <> '' OR caption <> '')
			ORDER BY created_at DESC, id DESC
			LIMIT ?
		) ORDER BY created_at ASC, id ASC`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("recent messages: %w", err)
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

// CountIncoming counts every incoming message a user has ever sent.
func (r *MessageRepository) CountIncoming(ctx context.Context, userID int64) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE user_id = ? AND direction = ?`,
		userID, string(domain.DirectionIncoming)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count incoming messages: %w", err)
	}
	return n, nil
}

// CountIncomingSince counts a user's incoming messages in a time window.
func (r *MessageRepository) CountIncomingSince(ctx context.Context, userID int64, since time.Time) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE user_id = ? AND direction = ? AND created_at >= ?`,
		userID, string(domain.DirectionIncoming), since).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count incoming messages: %w", err)
	}
	return n, nil
}

func scanMessage(rows *sql.Rows) (domain.Message, error) {
	var (
		m            domain.Message
		msgType      string
		direction    string
		processed    int
		aiProcessed  int
		botResponded int
	)
	err := rows.Scan(&m.ID, &m.UserID, &m.WhatsAppMessageID, &m.TraceID, &msgType, &m.Text,
		&m.MediaID, &m.Caption, &direction, &processed, &aiProcessed, &m.AIIntent,
		&m.AIConfidence, &botResponded, &m.CreatedAt)
	if err != nil {
		return m, fmt.Errorf("scan message: %w", err)
	}
	m.MessageType = domain.MessageType(msgType)
	m.Direction = domain.Direction(direction)
	m.Processed = processed != 0
	m.AIProcessed = aiProcessed != 0
	m.BotResponded = botResponded != 0
	return m, nil
}
