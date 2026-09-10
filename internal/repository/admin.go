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

// AdminRepository stores CRM accounts, their sessions and the login attempt
// history used for rate limiting.
type AdminRepository struct {
	db *DB
}

// NewAdminRepository builds an AdminRepository.
func NewAdminRepository(db *DB) *AdminRepository { return &AdminRepository{db: db} }

// AdminCredentials is the stored password material. It never leaves this
// package's callers in the auth service and is never serialised.
type AdminCredentials struct {
	Hash       string
	Salt       string
	Iterations int
}

const adminColumns = `id, email, name, role, active, last_login_at, failed_attempts,
	locked_until, created_at, updated_at`

// CreateAdmin inserts a CRM account.
func (r *AdminRepository) CreateAdmin(ctx context.Context, u domain.AdminUser, cred AdminCredentials) (*domain.AdminUser, error) {
	email := normaliseEmail(u.Email)
	if email == "" {
		return nil, errors.New("email is required")
	}
	if !u.Role.Valid() {
		u.Role = domain.RoleConsultant
	}
	now := time.Now().UTC()

	res, err := r.db.ExecContext(ctx, `
		INSERT INTO admin_users (email, name, password_hash, password_salt, password_iter,
			role, active, failed_attempts, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		email, strings.TrimSpace(u.Name), cred.Hash, cred.Salt, cred.Iterations,
		string(u.Role), boolToInt(u.Active), now, now)
	if err != nil {
		if isUniqueConstraint(err) {
			return nil, fmt.Errorf("admin account %q already exists", email)
		}
		return nil, fmt.Errorf("create admin: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create admin: %w", err)
	}
	return r.GetAdmin(ctx, id)
}

// GetAdmin loads an account by ID.
func (r *AdminRepository) GetAdmin(ctx context.Context, id int64) (*domain.AdminUser, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+adminColumns+` FROM admin_users WHERE id = ?`, id)
	return scanAdmin(row)
}

// GetAdminByEmail loads an account by email, case-insensitively.
func (r *AdminRepository) GetAdminByEmail(ctx context.Context, email string) (*domain.AdminUser, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+adminColumns+` FROM admin_users WHERE lower(email) = ?`, normaliseEmail(email))
	return scanAdmin(row)
}

// Credentials loads the password material for an account.
func (r *AdminRepository) Credentials(ctx context.Context, id int64) (AdminCredentials, error) {
	var c AdminCredentials
	err := r.db.QueryRowContext(ctx,
		`SELECT password_hash, password_salt, password_iter FROM admin_users WHERE id = ?`, id).
		Scan(&c.Hash, &c.Salt, &c.Iterations)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, fmt.Errorf("load credentials: %w", err)
	}
	return c, nil
}

// SetPassword replaces an account's password material.
func (r *AdminRepository) SetPassword(ctx context.Context, id int64, cred AdminCredentials) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE admin_users SET password_hash = ?, password_salt = ?, password_iter = ?,
			failed_attempts = 0, locked_until = NULL, updated_at = ?
		WHERE id = ?`, cred.Hash, cred.Salt, cred.Iterations, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	return nil
}

// ListAdmins returns every account, newest last.
func (r *AdminRepository) ListAdmins(ctx context.Context) ([]domain.AdminUser, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+adminColumns+` FROM admin_users ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list admins: %w", err)
	}
	defer rows.Close()

	var out []domain.AdminUser
	for rows.Next() {
		u, err := scanAdminRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetAdminActive enables or disables an account without deleting its history.
func (r *AdminRepository) SetAdminActive(ctx context.Context, id int64, active bool) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE admin_users SET active = ?, updated_at = ? WHERE id = ?`,
		boolToInt(active), time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("set admin active: %w", err)
	}
	return nil
}

// UpdateAdmin changes the display name and role of an account.
func (r *AdminRepository) UpdateAdmin(ctx context.Context, id int64, name string, role domain.AdminRole) error {
	if !role.Valid() {
		return fmt.Errorf("invalid role %q", role)
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE admin_users SET name = ?, role = ?, updated_at = ? WHERE id = ?`,
		strings.TrimSpace(name), string(role), time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("update admin: %w", err)
	}
	return nil
}

// CountAdmins reports how many accounts exist, used to decide whether the
// bootstrap account has to be created on start-up.
func (r *AdminRepository) CountAdmins(ctx context.Context) (int, error) {
	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count admins: %w", err)
	}
	return n, nil
}

// RecordLoginSuccess clears the failure counters and stamps the login time.
func (r *AdminRepository) RecordLoginSuccess(ctx context.Context, id int64) error {
	now := time.Now().UTC()
	_, err := r.db.ExecContext(ctx, `
		UPDATE admin_users SET last_login_at = ?, failed_attempts = 0, locked_until = NULL, updated_at = ?
		WHERE id = ?`, now, now, id)
	if err != nil {
		return fmt.Errorf("record login: %w", err)
	}
	return nil
}

// RecordLoginFailure increments the failure counter and locks the account once
// the threshold is reached.
func (r *AdminRepository) RecordLoginFailure(ctx context.Context, id int64, threshold int, lockFor time.Duration) error {
	now := time.Now().UTC()
	_, err := r.db.ExecContext(ctx, `
		UPDATE admin_users SET
			failed_attempts = failed_attempts + 1,
			locked_until = CASE WHEN failed_attempts + 1 >= ? THEN ? ELSE locked_until END,
			updated_at = ?
		WHERE id = ?`, threshold, now.Add(lockFor), now, id)
	if err != nil {
		return fmt.Errorf("record login failure: %w", err)
	}
	return nil
}

// ------------------------------------------------------------- rate limiting

// NoteLoginAttempt records one login attempt against an identifier, which is a
// hash of the client address, never the address itself.
func (r *AdminRepository) NoteLoginAttempt(ctx context.Context, identifier string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO login_attempts (identifier, created_at) VALUES (?, ?)`,
		identifier, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("note login attempt: %w", err)
	}
	return nil
}

// CountLoginAttempts counts recent attempts for an identifier.
func (r *AdminRepository) CountLoginAttempts(ctx context.Context, identifier string, since time.Time) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM login_attempts WHERE identifier = ? AND created_at >= ?`,
		identifier, since).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count login attempts: %w", err)
	}
	return n, nil
}

// ClearLoginAttempts drops the history for an identifier after a success.
func (r *AdminRepository) ClearLoginAttempts(ctx context.Context, identifier string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM login_attempts WHERE identifier = ?`, identifier)
	if err != nil {
		return fmt.Errorf("clear login attempts: %w", err)
	}
	return nil
}

// PruneLoginAttempts removes attempts older than the retention window.
func (r *AdminRepository) PruneLoginAttempts(ctx context.Context, before time.Time) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM login_attempts WHERE created_at < ?`, before)
	if err != nil {
		return fmt.Errorf("prune login attempts: %w", err)
	}
	return nil
}

// ----------------------------------------------------------------- sessions

// CreateSession stores a session keyed by the digest of the cookie token.
func (r *AdminRepository) CreateSession(ctx context.Context, s domain.AdminSession) error {
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO admin_sessions (id, admin_id, csrf_token, created_at, expires_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		s.ID, s.AdminID, s.CSRFToken, s.CreatedAt, s.ExpiresAt.UTC(), now)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// GetSession loads a session by token digest.
func (r *AdminRepository) GetSession(ctx context.Context, id string) (*domain.AdminSession, error) {
	var s domain.AdminSession
	err := r.db.QueryRowContext(ctx, `
		SELECT id, admin_id, csrf_token, created_at, expires_at, last_seen_at
		FROM admin_sessions WHERE id = ?`, id).
		Scan(&s.ID, &s.AdminID, &s.CSRFToken, &s.CreatedAt, &s.ExpiresAt, &s.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	return &s, nil
}

// TouchSession extends a session's sliding expiry.
func (r *AdminRepository) TouchSession(ctx context.Context, id string, expiresAt time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE admin_sessions SET last_seen_at = ?, expires_at = ? WHERE id = ?`,
		time.Now().UTC(), expiresAt.UTC(), id)
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// DeleteSession logs one session out.
func (r *AdminRepository) DeleteSession(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteSessionsForAdmin logs an account out everywhere.
func (r *AdminRepository) DeleteSessionsForAdmin(ctx context.Context, adminID int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE admin_id = ?`, adminID)
	if err != nil {
		return fmt.Errorf("delete admin sessions: %w", err)
	}
	return nil
}

// PruneSessions removes expired sessions.
func (r *AdminRepository) PruneSessions(ctx context.Context, before time.Time) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE expires_at < ?`, before.UTC())
	if err != nil {
		return fmt.Errorf("prune sessions: %w", err)
	}
	return nil
}

// ------------------------------------------------------------------ scans

func scanAdmin(row *sql.Row) (*domain.AdminUser, error) {
	var (
		u           domain.AdminUser
		role        string
		active      int
		lastLogin   sql.NullTime
		lockedUntil sql.NullTime
	)
	err := row.Scan(&u.ID, &u.Email, &u.Name, &role, &active, &lastLogin,
		&u.FailedAttempts, &lockedUntil, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan admin: %w", err)
	}
	u.Role = domain.AdminRole(role)
	u.Active = active != 0
	u.LastLoginAt = nullTime(lastLogin)
	u.LockedUntil = nullTime(lockedUntil)
	return &u, nil
}

func scanAdminRows(rows *sql.Rows) (domain.AdminUser, error) {
	var (
		u           domain.AdminUser
		role        string
		active      int
		lastLogin   sql.NullTime
		lockedUntil sql.NullTime
	)
	err := rows.Scan(&u.ID, &u.Email, &u.Name, &role, &active, &lastLogin,
		&u.FailedAttempts, &lockedUntil, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return u, fmt.Errorf("scan admin: %w", err)
	}
	u.Role = domain.AdminRole(role)
	u.Active = active != 0
	u.LastLoginAt = nullTime(lastLogin)
	u.LockedUntil = nullTime(lockedUntil)
	return u, nil
}

func normaliseEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
