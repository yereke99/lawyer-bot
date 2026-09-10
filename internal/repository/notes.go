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

// NoteRepository stores private consultant notes. Notes are CRM-only data and
// are never part of the WhatsApp conversation.
type NoteRepository struct {
	db *DB
}

// NewNoteRepository builds a NoteRepository.
func NewNoteRepository(db *DB) *NoteRepository { return &NoteRepository{db: db} }

// Create adds a note authored by a consultant.
func (r *NoteRepository) Create(ctx context.Context, userID, adminID int64, body string) (*domain.InternalNote, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, errors.New("note body is required")
	}
	now := time.Now().UTC()
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO internal_notes (user_id, admin_id, body, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`, userID, adminID, truncateRunes(body, 4000), now, now)
	if err != nil {
		return nil, fmt.Errorf("create note: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create note: %w", err)
	}
	return r.Get(ctx, id)
}

// Get loads one note.
func (r *NoteRepository) Get(ctx context.Context, id int64) (*domain.InternalNote, error) {
	var (
		n         domain.InternalNote
		adminName sql.NullString
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT n.id, n.user_id, n.admin_id, n.body, n.created_at, n.updated_at, a.name
		FROM internal_notes n LEFT JOIN admin_users a ON a.id = n.admin_id
		WHERE n.id = ? AND n.deleted_at IS NULL`, id).
		Scan(&n.ID, &n.UserID, &n.AdminID, &n.Body, &n.CreatedAt, &n.UpdatedAt, &adminName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load note: %w", err)
	}
	n.AdminName = adminName.String
	return &n, nil
}

// ListByClient returns a client's notes, newest first.
func (r *NoteRepository) ListByClient(ctx context.Context, userID int64, limit int) ([]domain.InternalNote, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT n.id, n.user_id, n.admin_id, n.body, n.created_at, n.updated_at, COALESCE(a.name, '')
		FROM internal_notes n LEFT JOIN admin_users a ON a.id = n.admin_id
		WHERE n.user_id = ? AND n.deleted_at IS NULL
		ORDER BY n.created_at DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("list notes: %w", err)
	}
	defer rows.Close()

	var out []domain.InternalNote
	for rows.Next() {
		var n domain.InternalNote
		if err := rows.Scan(&n.ID, &n.UserID, &n.AdminID, &n.Body, &n.CreatedAt,
			&n.UpdatedAt, &n.AdminName); err != nil {
			return nil, fmt.Errorf("scan note: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Update rewrites a note's body.
func (r *NoteRepository) Update(ctx context.Context, id int64, body string) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return errors.New("note body is required")
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE internal_notes SET body = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		truncateRunes(body, 4000), time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("update note: %w", err)
	}
	return nil
}

// Delete soft-deletes a note, so an accidental click never destroys context.
func (r *NoteRepository) Delete(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE internal_notes SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("delete note: %w", err)
	}
	return nil
}

// ------------------------------------------------------------------ audit

// AuditRepository records administrative actions.
type AuditRepository struct {
	db *DB
}

// NewAuditRepository builds an AuditRepository.
func NewAuditRepository(db *DB) *AuditRepository { return &AuditRepository{db: db} }

// Record appends one audit entry. Secrets are never passed in: callers supply a
// short human-readable detail string only.
func (r *AuditRepository) Record(ctx context.Context, entry domain.AuditLog) error {
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO audit_logs (admin_id, action, entity, entity_id, detail, ip_hash, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		entry.AdminID, entry.Action, entry.Entity, entry.EntityID,
		truncateRunes(entry.Detail, 1000), entry.IPHash, entry.CreatedAt)
	if err != nil {
		return fmt.Errorf("record audit entry: %w", err)
	}
	return nil
}

// List returns audit entries, newest first, optionally scoped to one entity.
func (r *AuditRepository) List(ctx context.Context, entity string, entityID int64, limit, offset int) ([]domain.AuditLog, int, error) {
	where := ""
	var args []any
	if entity != "" {
		where = " WHERE l.entity = ? AND l.entity_id = ?"
		args = append(args, entity, entityID)
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs l`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count audit entries: %w", err)
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT l.id, l.admin_id, COALESCE(a.name, ''), l.action, l.entity, l.entity_id,
			l.detail, l.created_at
		FROM audit_logs l LEFT JOIN admin_users a ON a.id = l.admin_id`+where+`
		ORDER BY l.id DESC LIMIT ? OFFSET ?`,
		append(append([]any{}, args...), limit, max0(offset))...)
	if err != nil {
		return nil, 0, fmt.Errorf("list audit entries: %w", err)
	}
	defer rows.Close()

	var out []domain.AuditLog
	for rows.Next() {
		var e domain.AuditLog
		if err := rows.Scan(&e.ID, &e.AdminID, &e.AdminName, &e.Action, &e.Entity,
			&e.EntityID, &e.Detail, &e.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan audit entry: %w", err)
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// --------------------------------------------------------------- settings

// SettingsRepository stores runtime CRM configuration. Secrets never live here:
// API keys and provider credentials stay in the process environment.
type SettingsRepository struct {
	db *DB
}

// NewSettingsRepository builds a SettingsRepository.
func NewSettingsRepository(db *DB) *SettingsRepository { return &SettingsRepository{db: db} }

// All returns every stored setting.
func (r *SettingsRepository) All(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT key, value FROM crm_settings`)
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// Get returns one setting, or def when it is unset.
func (r *SettingsRepository) Get(ctx context.Context, key, def string) (string, error) {
	var v string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM crm_settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return def, fmt.Errorf("load setting %q: %w", key, err)
	}
	return v, nil
}

// Set stores one setting.
func (r *SettingsRepository) Set(ctx context.Context, key, value string, adminID int64) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO crm_settings (key, value, updated_at, updated_by) VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value,
			updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		key, truncateRunes(value, 2000), time.Now().UTC(), adminID)
	if err != nil {
		return fmt.Errorf("store setting %q: %w", key, err)
	}
	return nil
}
