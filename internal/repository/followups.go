package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"lawyer-bot/internal/domain"
)

// FollowUpRepository is the durable job store behind automatic follow-ups.
//
// Two properties matter and are both enforced in SQL rather than in Go:
//
//   - scheduling is idempotent, because dedupe_key is UNIQUE. Re-running the
//     same schedule for the same client, stage and anchor message is a no-op.
//   - claiming is atomic, because Claim is a conditional UPDATE that stamps a
//     random token. Two workers can race; exactly one sees rows affected.
type FollowUpRepository struct {
	db *DB
}

// NewFollowUpRepository builds a FollowUpRepository.
func NewFollowUpRepository(db *DB) *FollowUpRepository { return &FollowUpRepository{db: db} }

const followUpColumns = `id, user_id, stage, scheduled_at, status, claim_token, claimed_at,
	attempts, last_error, dedupe_key, message_id, created_at, updated_at`

// Schedule stores a pending follow-up. A job with the same dedupe key is left
// exactly as it is, so a retried pipeline run cannot create a second nudge.
func (r *FollowUpRepository) Schedule(ctx context.Context, job domain.FollowUpJob) (int64, bool, error) {
	if job.DedupeKey == "" {
		return 0, false, errors.New("follow-up dedupe key is required")
	}
	now := time.Now().UTC()

	res, err := r.db.ExecContext(ctx, `
		INSERT INTO follow_up_jobs (user_id, stage, scheduled_at, status, claim_token,
			attempts, last_error, dedupe_key, message_id, created_at, updated_at)
		VALUES (?, ?, ?, 'pending', '', 0, '', ?, ?, ?, ?)
		ON CONFLICT(dedupe_key) DO NOTHING`,
		job.UserID, job.Stage, job.ScheduledAt.UTC(), job.DedupeKey, job.MessageID, now, now)
	if err != nil {
		return 0, false, fmt.Errorf("schedule follow-up: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("schedule follow-up: %w", err)
	}
	if affected == 0 {
		return 0, false, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("schedule follow-up: %w", err)
	}
	return id, true, nil
}

// CancelPendingForUser invalidates every outstanding nudge for a client. It is
// called the moment the client replies, so an obsolete follow-up can never be
// delivered after the conversation moved on.
func (r *FollowUpRepository) CancelPendingForUser(ctx context.Context, userID int64, reason string) (int, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE follow_up_jobs SET status = 'cancelled', last_error = ?, updated_at = ?
		WHERE user_id = ? AND status IN ('pending','claimed')`,
		truncate(reason, 200), time.Now().UTC(), userID)
	if err != nil {
		return 0, fmt.Errorf("cancel follow-ups: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("cancel follow-ups: %w", err)
	}
	return int(n), nil
}

// Cancel invalidates a single job.
func (r *FollowUpRepository) Cancel(ctx context.Context, id int64, reason string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE follow_up_jobs SET status = 'cancelled', last_error = ?, updated_at = ?
		WHERE id = ? AND status IN ('pending','claimed')`,
		truncate(reason, 200), time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("cancel follow-up: %w", err)
	}
	return nil
}

// Reschedule moves a pending job to a new time.
func (r *FollowUpRepository) Reschedule(ctx context.Context, id int64, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE follow_up_jobs SET scheduled_at = ?, status = 'pending', claim_token = '',
			claimed_at = NULL, updated_at = ?
		WHERE id = ? AND status IN ('pending','claimed','failed')`,
		at.UTC(), time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("reschedule follow-up: %w", err)
	}
	return nil
}

// Claim atomically takes ownership of at most limit due jobs.
//
// The UPDATE only matches rows that are still pending, so a second worker
// running the identical statement claims a disjoint set. Jobs stuck in the
// claimed state longer than staleAfter are reclaimed, which is what makes a
// crash mid-send recoverable rather than permanently blocking.
func (r *FollowUpRepository) Claim(ctx context.Context, token string, now time.Time, limit int, staleAfter time.Duration) ([]domain.FollowUpJob, error) {
	if token == "" {
		return nil, errors.New("claim token is required")
	}
	if limit <= 0 {
		limit = 10
	}
	nowUTC := now.UTC()
	staleBefore := nowUTC.Add(-staleAfter)

	res, err := r.db.ExecContext(ctx, `
		UPDATE follow_up_jobs
		SET status = 'claimed', claim_token = ?, claimed_at = ?, attempts = attempts + 1, updated_at = ?
		WHERE id IN (
			SELECT id FROM follow_up_jobs
			WHERE scheduled_at <= ?
			  AND (status = 'pending' OR (status = 'claimed' AND claimed_at < ?))
			ORDER BY scheduled_at ASC
			LIMIT ?
		)`, token, nowUTC, nowUTC, nowUTC, staleBefore, limit)
	if err != nil {
		return nil, fmt.Errorf("claim follow-ups: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return nil, nil
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT `+followUpColumns+` FROM follow_up_jobs WHERE claim_token = ? AND status = 'claimed'`, token)
	if err != nil {
		return nil, fmt.Errorf("load claimed follow-ups: %w", err)
	}
	defer rows.Close()

	var out []domain.FollowUpJob
	for rows.Next() {
		job, err := scanFollowUp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// Complete marks a claimed job as delivered. The claim token must still match,
// so a job reclaimed by another worker is never overwritten.
func (r *FollowUpRepository) Complete(ctx context.Context, id int64, token string, status domain.FollowUpStatus, note string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE follow_up_jobs SET status = ?, last_error = ?, updated_at = ?
		WHERE id = ? AND claim_token = ?`,
		string(status), truncate(note, 300), time.Now().UTC(), id, token)
	if err != nil {
		return fmt.Errorf("complete follow-up: %w", err)
	}
	return nil
}

// Release returns a claimed job to the pending pool for a later retry.
func (r *FollowUpRepository) Release(ctx context.Context, id int64, token string, retryAt time.Time, reason string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE follow_up_jobs SET status = 'pending', claim_token = '', claimed_at = NULL,
			scheduled_at = ?, last_error = ?, updated_at = ?
		WHERE id = ? AND claim_token = ?`,
		retryAt.UTC(), truncate(reason, 300), time.Now().UTC(), id, token)
	if err != nil {
		return fmt.Errorf("release follow-up: %w", err)
	}
	return nil
}

// NextPendingForUser returns the soonest outstanding nudge for a client.
func (r *FollowUpRepository) NextPendingForUser(ctx context.Context, userID int64) (*domain.FollowUpJob, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+followUpColumns+` FROM follow_up_jobs
		WHERE user_id = ? AND status = 'pending' ORDER BY scheduled_at ASC LIMIT 1`, userID)
	if err != nil {
		return nil, fmt.Errorf("next follow-up: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("next follow-up: %w", err)
		}
		return nil, ErrNotFound
	}
	job, err := scanFollowUp(rows)
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// FollowUpView is a job joined with the client it belongs to, for the CRM list.
type FollowUpView struct {
	domain.FollowUpJob
	ClientName  string `json:"client_name"`
	ClientPhone string `json:"client_phone"`
	Language    string `json:"language"`
	CRMStatus   string `json:"crm_status"`
}

// List returns follow-up jobs filtered by status.
func (r *FollowUpRepository) List(ctx context.Context, statuses []string, limit, offset int) ([]FollowUpView, int, error) {
	where := ""
	var args []any
	if clause, a := inClause("j.status", statuses); clause != "" {
		where = " WHERE " + clause
		args = a
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM follow_up_jobs j`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count follow-ups: %w", err)
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT j.id, j.user_id, j.stage, j.scheduled_at, j.status, j.claim_token, j.claimed_at,
			j.attempts, j.last_error, j.dedupe_key, j.message_id, j.created_at, j.updated_at,
			u.display_name, u.phone_number, u.language, u.crm_status
		FROM follow_up_jobs j JOIN users u ON u.id = j.user_id`+where+`
		ORDER BY j.scheduled_at ASC LIMIT ? OFFSET ?`,
		append(append([]any{}, args...), limit, max0(offset))...)
	if err != nil {
		return nil, 0, fmt.Errorf("list follow-ups: %w", err)
	}
	defer rows.Close()

	var out []FollowUpView
	for rows.Next() {
		var (
			v         FollowUpView
			status    string
			claimedAt sql.NullTime
		)
		if err := rows.Scan(&v.ID, &v.UserID, &v.Stage, &v.ScheduledAt, &status, &v.ClaimToken,
			&claimedAt, &v.Attempts, &v.LastError, &v.DedupeKey, &v.MessageID,
			&v.CreatedAt, &v.UpdatedAt, &v.ClientName, &v.ClientPhone, &v.Language,
			&v.CRMStatus); err != nil {
			return nil, 0, fmt.Errorf("scan follow-up view: %w", err)
		}
		v.Status = domain.FollowUpStatus(status)
		v.ClaimedAt = nullTime(claimedAt)
		v.ClaimToken = ""
		out = append(out, v)
	}
	return out, total, rows.Err()
}

func scanFollowUp(rows *sql.Rows) (domain.FollowUpJob, error) {
	var (
		job       domain.FollowUpJob
		status    string
		claimedAt sql.NullTime
	)
	err := rows.Scan(&job.ID, &job.UserID, &job.Stage, &job.ScheduledAt, &status, &job.ClaimToken,
		&claimedAt, &job.Attempts, &job.LastError, &job.DedupeKey, &job.MessageID,
		&job.CreatedAt, &job.UpdatedAt)
	if err != nil {
		return job, fmt.Errorf("scan follow-up: %w", err)
	}
	job.Status = domain.FollowUpStatus(status)
	job.ClaimedAt = nullTime(claimedAt)
	return job, nil
}

// FollowUpDedupeKey builds the idempotency key for one nudge.
func FollowUpDedupeKey(userID int64, stage int, anchorMessageID int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "u%d:s%d:m%d", userID, stage, anchorMessageID)
	return b.String()
}
