package repository

import (
	"context"
	"fmt"
	"time"

	"lawyer-bot/internal/domain"
)

// AIInteractionRepository stores one audit row per model call: what was asked,
// what came back, how many tokens it cost and how long it took.
type AIInteractionRepository struct {
	db *DB
}

// NewAIInteractionRepository builds an AIInteractionRepository.
func NewAIInteractionRepository(db *DB) *AIInteractionRepository {
	return &AIInteractionRepository{db: db}
}

// Create persists one model interaction.
func (r *AIInteractionRepository) Create(ctx context.Context, in *domain.AIInteraction) (int64, error) {
	if in.CreatedAt.IsZero() {
		in.CreatedAt = time.Now().UTC()
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO ai_interactions (user_id, message_id, trace_id, model, input_tokens,
			output_tokens, intent, service_code, confidence, raw_response, error,
			processing_time_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.UserID, in.MessageID, in.TraceID, in.Model, in.InputTokens,
		in.OutputTokens, in.Intent, in.ServiceCode, in.Confidence,
		truncate(in.RawResponse, 8000), truncate(in.Error, 1000),
		in.ProcessingTimeMS, in.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("insert ai interaction: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("ai interaction last insert id: %w", err)
	}
	in.ID = id
	return id, nil
}

// CountByUserSince reports how many model calls a user has triggered in a time
// window. The pipeline uses this as a hard token budget per contact.
func (r *AIInteractionRepository) CountByUserSince(ctx context.Context, userID int64, since time.Time) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ai_interactions WHERE user_id = ? AND created_at >= ?`,
		userID, since).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count ai interactions: %w", err)
	}
	return n, nil
}

// TokenUsage is aggregated model spend over a period.
type TokenUsage struct {
	Calls        int
	InputTokens  int
	OutputTokens int
}

// UsageSince aggregates token consumption, for cost monitoring.
func (r *AIInteractionRepository) UsageSince(ctx context.Context, since time.Time) (TokenUsage, error) {
	var u TokenUsage
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0)
		FROM ai_interactions WHERE created_at >= ?`, since).
		Scan(&u.Calls, &u.InputTokens, &u.OutputTokens)
	if err != nil {
		return u, fmt.Errorf("ai usage: %w", err)
	}
	return u, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
