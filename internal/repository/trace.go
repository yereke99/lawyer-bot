package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"lawyer-bot/internal/domain"
)

// TraceRepository is the audit store. Every decision the pipeline makes lands
// here, so the question "why did the bot answer, or stay silent?" can always be
// answered from the database alone.
type TraceRepository struct {
	db *DB
}

// NewTraceRepository builds a TraceRepository.
func NewTraceRepository(db *DB) *TraceRepository {
	return &TraceRepository{db: db}
}

// Event appends one pipeline step.
func (r *TraceRepository) Event(ctx context.Context, e domain.TraceEvent) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO trace_events (trace_id, user_id, message_id, stage, decision,
			reason, detail, duration_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TraceID, e.UserID, e.MessageID, e.Stage, e.Decision,
		truncate(e.Reason, 500), truncate(e.Detail, 4000), e.DurationMS, e.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert trace event: %w", err)
	}
	return nil
}

// Detail marshals a small map for the trace detail column. Marshalling errors
// are swallowed on purpose: tracing must never break message processing.
func Detail(kv map[string]any) string {
	if len(kv) == 0 {
		return ""
	}
	b, err := json.Marshal(kv)
	if err != nil {
		return ""
	}
	return string(b)
}

// ByTraceID returns the ordered event chain of one incoming message.
func (r *TraceRepository) ByTraceID(ctx context.Context, traceID string) ([]domain.TraceEvent, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, trace_id, user_id, message_id, stage, decision, reason, detail,
			duration_ms, created_at
		FROM trace_events WHERE trace_id = ? ORDER BY id ASC`, traceID)
	if err != nil {
		return nil, fmt.Errorf("load trace: %w", err)
	}
	defer rows.Close()

	var out []domain.TraceEvent
	for rows.Next() {
		var e domain.TraceEvent
		if err := rows.Scan(&e.ID, &e.TraceID, &e.UserID, &e.MessageID, &e.Stage,
			&e.Decision, &e.Reason, &e.Detail, &e.DurationMS, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan trace event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// WebhookEvent stores the raw provider payload.
func (r *TraceRepository) WebhookEvent(ctx context.Context, e domain.WebhookEvent) (int64, error) {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO webhook_events (trace_id, provider, signature, payload,
			message_count, status, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TraceID, e.Provider, e.Signature, truncate(e.Payload, 64000),
		e.MessageCount, e.Status, truncate(e.Error, 500), e.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("insert webhook event: %w", err)
	}
	return res.LastInsertId()
}

// StateTransition records a conversation state change.
func (r *TraceRepository) StateTransition(ctx context.Context, t domain.StateTransition) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO state_transitions (user_id, trace_id, from_state, to_state,
			reason, message_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.UserID, t.TraceID, string(t.FromState), string(t.ToState),
		truncate(t.Reason, 300), t.MessageID, t.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert state transition: %w", err)
	}
	return nil
}

// Delivery records the outcome of an outbound send.
func (r *TraceRepository) Delivery(ctx context.Context, d domain.Delivery) (int64, error) {
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO deliveries (user_id, message_id, trace_id, recipient, kind,
			status, attempts, provider_message_id, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.UserID, d.MessageID, d.TraceID, d.Recipient, d.Kind,
		d.Status, d.Attempts, d.ProviderMessageID, truncate(d.Error, 500), d.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("insert delivery: %w", err)
	}
	return res.LastInsertId()
}

// FailedDeliveries lists sends that never reached the provider, so a lead is
// never silently lost.
func (r *TraceRepository) FailedDeliveries(ctx context.Context, limit int) ([]domain.Delivery, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, user_id, message_id, trace_id, recipient, kind, status, attempts,
			provider_message_id, error, created_at
		FROM deliveries WHERE status = ? ORDER BY created_at DESC LIMIT ?`,
		domain.DeliveryFailed, limit)
	if err != nil {
		return nil, fmt.Errorf("list failed deliveries: %w", err)
	}
	defer rows.Close()

	var out []domain.Delivery
	for rows.Next() {
		var d domain.Delivery
		if err := rows.Scan(&d.ID, &d.UserID, &d.MessageID, &d.TraceID, &d.Recipient,
			&d.Kind, &d.Status, &d.Attempts, &d.ProviderMessageID, &d.Error,
			&d.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan delivery: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Notification records an alert sent to Diana or an admin.
func (r *TraceRepository) Notification(ctx context.Context, n domain.Notification) (int64, error) {
	if n.CreatedAt.IsZero() {
		n.CreatedAt = time.Now().UTC()
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO notifications (lead_id, user_id, trace_id, channel, recipient,
			body, status, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.LeadID, n.UserID, n.TraceID, n.Channel, n.Recipient,
		truncate(n.Body, 4000), n.Status, truncate(n.Error, 500), n.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("insert notification: %w", err)
	}
	return res.LastInsertId()
}

// MediaAsset stores metadata for received media.
func (r *TraceRepository) MediaAsset(ctx context.Context, m domain.MediaAsset) (int64, error) {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO media_assets (user_id, message_id, media_id, mime_type, sha256,
			file_size, filename, caption, voice, downloaded, local_path, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.UserID, m.MessageID, m.MediaID, m.MimeType, m.SHA256, m.FileSize,
		m.Filename, m.Caption, boolToInt(m.Voice), boolToInt(m.Downloaded),
		m.LocalPath, m.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("insert media asset: %w", err)
	}
	return res.LastInsertId()
}

// SetFact stores or replaces one extracted fact about a user.
func (r *TraceRepository) SetFact(ctx context.Context, f domain.UserFact) error {
	now := time.Now().UTC()
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO user_facts (user_id, key, value, source, message_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, key) DO UPDATE SET
			value      = excluded.value,
			source     = excluded.source,
			message_id = excluded.message_id,
			updated_at = excluded.updated_at`,
		f.UserID, f.Key, truncate(f.Value, 300), f.Source, f.MessageID, now, now)
	if err != nil {
		return fmt.Errorf("set user fact: %w", err)
	}
	return nil
}

// Facts returns every known fact about a user as a plain map.
func (r *TraceRepository) Facts(ctx context.Context, userID int64) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT key, value FROM user_facts WHERE user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("load user facts: %w", err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan user fact: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}
