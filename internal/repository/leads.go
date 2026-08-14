package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"lawyer-bot/internal/domain"
)

// LeadRepository persists qualified sales opportunities.
type LeadRepository struct {
	db *DB
}

// NewLeadRepository builds a LeadRepository.
func NewLeadRepository(db *DB) *LeadRepository {
	return &LeadRepository{db: db}
}

const leadColumns = `id, user_id, service_code, service_name, language, phone_number,
	lead_score, status, source, qualification_summary, notified_at, created_at, updated_at`

// Upsert creates the user's open lead or updates the existing one. A user has
// at most one lead that is not yet converted, closed or rejected.
func (r *LeadRepository) Upsert(ctx context.Context, l *domain.Lead) (*domain.Lead, error) {
	existing, err := r.GetOpenByUser(ctx, l.UserID)
	switch {
	case errors.Is(err, ErrNotFound):
		return r.create(ctx, l)
	case err != nil:
		return nil, err
	}

	now := time.Now().UTC()
	_, err = r.db.ExecContext(ctx, `
		UPDATE leads SET
			service_code          = CASE WHEN ? <> '' THEN ? ELSE service_code END,
			service_name          = CASE WHEN ? <> '' THEN ? ELSE service_name END,
			language              = CASE WHEN ? <> '' THEN ? ELSE language END,
			phone_number          = CASE WHEN ? <> '' THEN ? ELSE phone_number END,
			lead_score            = MAX(lead_score, ?),
			status                = ?,
			qualification_summary = CASE WHEN ? <> '' THEN ? ELSE qualification_summary END,
			updated_at            = ?
		WHERE id = ?`,
		l.ServiceCode, l.ServiceCode,
		l.ServiceName, l.ServiceName,
		string(l.Language), string(l.Language),
		l.PhoneNumber, l.PhoneNumber,
		l.LeadScore,
		string(l.Status),
		l.QualificationSummary, l.QualificationSummary,
		now, existing.ID)
	if err != nil {
		return nil, fmt.Errorf("update lead: %w", err)
	}
	return r.GetByID(ctx, existing.ID)
}

func (r *LeadRepository) create(ctx context.Context, l *domain.Lead) (*domain.Lead, error) {
	now := time.Now().UTC()
	if l.Status == "" {
		l.Status = domain.LeadNew
	}
	if l.Source == "" {
		l.Source = domain.SourceWhatsApp
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO leads (user_id, service_code, service_name, language, phone_number,
			lead_score, status, source, qualification_summary, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.UserID, l.ServiceCode, l.ServiceName, string(l.Language), l.PhoneNumber,
		l.LeadScore, string(l.Status), l.Source, l.QualificationSummary, now, now)
	if err != nil {
		return nil, fmt.Errorf("insert lead: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("lead last insert id: %w", err)
	}
	return r.GetByID(ctx, id)
}

// GetByID loads a lead by primary key.
func (r *LeadRepository) GetByID(ctx context.Context, id int64) (*domain.Lead, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+leadColumns+` FROM leads WHERE id = ?`, id)
	return scanLead(row)
}

// GetOpenByUser returns the user's lead that is still in play.
func (r *LeadRepository) GetOpenByUser(ctx context.Context, userID int64) (*domain.Lead, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+leadColumns+` FROM leads
		WHERE user_id = ? AND status NOT IN ('converted','closed','rejected')
		ORDER BY id DESC LIMIT 1`, userID)
	return scanLead(row)
}

// MarkNotified records that Diana was alerted about this lead, which stops the
// bot from sending duplicate notifications.
func (r *LeadRepository) MarkNotified(ctx context.Context, id int64) error {
	now := time.Now().UTC()
	_, err := r.db.ExecContext(ctx,
		`UPDATE leads SET notified_at = ?, updated_at = ? WHERE id = ?`, now, now, id)
	if err != nil {
		return fmt.Errorf("mark lead notified: %w", err)
	}
	return nil
}

// UpdateStatus moves a lead through the sales lifecycle.
func (r *LeadRepository) UpdateStatus(ctx context.Context, id int64, status domain.LeadStatus) error {
	if !status.Valid() {
		return fmt.Errorf("invalid lead status %q", status)
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE leads SET status = ?, updated_at = ? WHERE id = ?`,
		string(status), time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("update lead status: %w", err)
	}
	return nil
}

// ListByStatus returns leads in a given status, newest first.
func (r *LeadRepository) ListByStatus(ctx context.Context, status domain.LeadStatus, limit int) ([]domain.Lead, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+leadColumns+` FROM leads WHERE status = ? ORDER BY created_at DESC LIMIT ?`,
		string(status), limit)
	if err != nil {
		return nil, fmt.Errorf("list leads: %w", err)
	}
	defer rows.Close()

	var out []domain.Lead
	for rows.Next() {
		var (
			l          domain.Lead
			lang       string
			st         string
			notifiedAt sql.NullTime
		)
		if err := rows.Scan(&l.ID, &l.UserID, &l.ServiceCode, &l.ServiceName, &lang,
			&l.PhoneNumber, &l.LeadScore, &st, &l.Source, &l.QualificationSummary,
			&notifiedAt, &l.CreatedAt, &l.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan lead: %w", err)
		}
		l.Language = domain.Language(lang)
		l.Status = domain.LeadStatus(st)
		if notifiedAt.Valid {
			t := notifiedAt.Time
			l.NotifiedAt = &t
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func scanLead(row *sql.Row) (*domain.Lead, error) {
	var (
		l          domain.Lead
		lang       string
		status     string
		notifiedAt sql.NullTime
	)
	err := row.Scan(&l.ID, &l.UserID, &l.ServiceCode, &l.ServiceName, &lang,
		&l.PhoneNumber, &l.LeadScore, &status, &l.Source, &l.QualificationSummary,
		&notifiedAt, &l.CreatedAt, &l.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan lead: %w", err)
	}
	l.Language = domain.Language(lang)
	l.Status = domain.LeadStatus(status)
	if notifiedAt.Valid {
		t := notifiedAt.Time
		l.NotifiedAt = &t
	}
	return &l, nil
}
