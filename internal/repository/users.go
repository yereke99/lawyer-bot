package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"lawyer-bot/internal/domain"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("not found")

// UserRepository persists WhatsApp contacts and their qualification state.
type UserRepository struct {
	db *DB
}

// NewUserRepository builds a UserRepository.
func NewUserRepository(db *DB) *UserRepository {
	return &UserRepository{db: db}
}

const userColumns = `id, whatsapp_user_id, phone_number, display_name, language,
	current_state, detected_service, lead_score, is_lead,
	first_seen_at, last_seen_at, created_at, updated_at`

// Upsert creates the user on first contact and refreshes the volatile contact
// fields on every later message. It never overwrites a known value with an
// empty one.
func (r *UserRepository) Upsert(ctx context.Context, whatsappUserID, phone, displayName string) (*domain.User, error) {
	if whatsappUserID == "" {
		return nil, errors.New("whatsapp user id is required")
	}
	now := time.Now().UTC()

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO users (whatsapp_user_id, phone_number, display_name, language,
			current_state, detected_service, lead_score, is_lead,
			first_seen_at, last_seen_at, created_at, updated_at)
		VALUES (?, ?, ?, '', ?, '', 0, 0, ?, ?, ?, ?)
		ON CONFLICT(whatsapp_user_id) DO UPDATE SET
			phone_number = CASE WHEN excluded.phone_number <> '' THEN excluded.phone_number ELSE users.phone_number END,
			display_name = CASE WHEN excluded.display_name <> '' THEN excluded.display_name ELSE users.display_name END,
			last_seen_at = excluded.last_seen_at,
			updated_at   = excluded.updated_at`,
		whatsappUserID, phone, displayName, string(domain.StateNew), now, now, now, now)
	if err != nil {
		return nil, fmt.Errorf("upsert user: %w", err)
	}

	return r.GetByWhatsAppID(ctx, whatsappUserID)
}

// GetByWhatsAppID loads a user by their WhatsApp identifier.
func (r *UserRepository) GetByWhatsAppID(ctx context.Context, whatsappUserID string) (*domain.User, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE whatsapp_user_id = ?`, whatsappUserID)
	return scanUser(row)
}

// GetByID loads a user by primary key.
func (r *UserRepository) GetByID(ctx context.Context, id int64) (*domain.User, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id)
	return scanUser(row)
}

// UpdateQualification stores the qualification fields produced by the pipeline.
// Empty language/service values leave the stored value untouched.
func (r *UserRepository) UpdateQualification(ctx context.Context, u *domain.User) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE users SET
			language         = CASE WHEN ? <> '' THEN ? ELSE language END,
			current_state    = ?,
			detected_service = CASE WHEN ? <> '' THEN ? ELSE detected_service END,
			lead_score       = ?,
			is_lead          = ?,
			phone_number     = CASE WHEN ? <> '' THEN ? ELSE phone_number END,
			last_seen_at     = ?,
			updated_at       = ?
		WHERE id = ?`,
		string(u.Language), string(u.Language),
		string(u.CurrentState),
		u.DetectedService, u.DetectedService,
		u.LeadScore,
		boolToInt(u.IsLead),
		u.PhoneNumber, u.PhoneNumber,
		time.Now().UTC(), time.Now().UTC(),
		u.ID)
	if err != nil {
		return fmt.Errorf("update user qualification: %w", err)
	}
	return nil
}

// SetState moves the user to a new conversation state.
func (r *UserRepository) SetState(ctx context.Context, userID int64, state domain.ConversationState) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE users SET current_state = ?, updated_at = ? WHERE id = ?`,
		string(state), time.Now().UTC(), userID)
	if err != nil {
		return fmt.Errorf("set user state: %w", err)
	}
	return nil
}

// SetPhone stores a phone number collected from the conversation.
func (r *UserRepository) SetPhone(ctx context.Context, userID int64, phone string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE users SET phone_number = ?, updated_at = ? WHERE id = ?`,
		phone, time.Now().UTC(), userID)
	if err != nil {
		return fmt.Errorf("set user phone: %w", err)
	}
	return nil
}

func scanUser(row *sql.Row) (*domain.User, error) {
	var (
		u      domain.User
		lang   string
		state  string
		isLead int
	)
	err := row.Scan(&u.ID, &u.WhatsAppUserID, &u.PhoneNumber, &u.DisplayName, &lang,
		&state, &u.DetectedService, &u.LeadScore, &isLead,
		&u.FirstSeenAt, &u.LastSeenAt, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan user: %w", err)
	}
	u.Language = domain.Language(lang)
	u.CurrentState = domain.ConversationState(state)
	u.IsLead = isLead != 0
	return &u, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
