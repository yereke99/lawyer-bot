package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"lawyer-bot/internal/domain"
)

// CRMRepository is the operational read/write side of the CRM: the client list,
// the client detail record and every state change a consultant can make.
//
// It reads and writes the existing users table rather than duplicating contacts
// into a second table, so the CRM works with real production clients from the
// first request.
type CRMRepository struct {
	db *DB
}

// NewCRMRepository builds a CRMRepository.
func NewCRMRepository(db *DB) *CRMRepository { return &CRMRepository{db: db} }

const crmClientColumns = `u.id, u.whatsapp_user_id, u.phone_number, u.display_name, u.language,
	u.current_state, u.detected_service, u.lead_score, u.is_lead,
	u.first_seen_at, u.last_seen_at, u.created_at, u.updated_at,
	u.crm_status, u.conversation_mode, u.ai_enabled, u.blocked, u.blocked_at, u.blocked_by,
	u.block_reason, u.assigned_admin_id, u.assigned_at, u.assigned_by,
	u.last_inbound_at, u.last_outbound_at, u.unread_count, u.ai_summary, u.important_facts,
	u.next_action, u.qualification_stage, u.ai_confidence, u.intent, u.follow_up_stage,
	u.next_follow_up_at, u.close_reason, u.tags, u.language_locked, u.summary_watermark,
	COALESCE(a.name, '')`

const crmClientFrom = ` FROM users u LEFT JOIN admin_users a ON a.id = u.assigned_admin_id`

// ClientFilter is the server-side filter for the CRM client list. Filtering and
// pagination happen in SQL: the browser never receives the whole table.
type ClientFilter struct {
	Search     string
	Statuses   []string
	Services   []string
	Languages  []string
	Modes      []string
	AssignedTo *int64
	Blocked    *bool
	Unread     bool
	From       *time.Time
	To         *time.Time
	Sort       string
	Desc       bool
	Limit      int
	Offset     int
}

// sortColumns whitelists the orderable columns. User input never reaches SQL
// directly: an unknown value falls back to last activity.
var sortColumns = map[string]string{
	"last_activity": "COALESCE(u.last_inbound_at, u.last_outbound_at, u.updated_at)",
	"created_at":    "u.created_at",
	"name":          "u.display_name",
	"status":        "u.crm_status",
	"unread":        "u.unread_count",
	"follow_up":     "u.next_follow_up_at",
	"score":         "u.lead_score",
}

// where renders the filter into a SQL fragment plus its bound arguments.
func (f ClientFilter) where() (string, []any) {
	var (
		clauses []string
		args    []any
	)

	if s := strings.TrimSpace(f.Search); s != "" {
		like := "%" + strings.ToLower(s) + "%"
		digits := digitsOf(s)
		clause := `(lower(u.display_name) LIKE ? OR lower(u.ai_summary) LIKE ? OR lower(u.tags) LIKE ?`
		args = append(args, like, like, like)
		if digits != "" {
			clause += ` OR u.phone_number LIKE ? OR u.whatsapp_user_id LIKE ?`
			args = append(args, "%"+digits+"%", "%"+digits+"%")
		}
		clause += `)`
		clauses = append(clauses, clause)
	}
	if in, a := inClause("u.crm_status", f.Statuses); in != "" {
		clauses, args = append(clauses, in), append(args, a...)
	}
	if in, a := inClause("u.detected_service", f.Services); in != "" {
		clauses, args = append(clauses, in), append(args, a...)
	}
	if in, a := inClause("u.language", f.Languages); in != "" {
		clauses, args = append(clauses, in), append(args, a...)
	}
	if in, a := inClause("u.conversation_mode", f.Modes); in != "" {
		clauses, args = append(clauses, in), append(args, a...)
	}
	if f.AssignedTo != nil {
		clauses = append(clauses, "u.assigned_admin_id = ?")
		args = append(args, *f.AssignedTo)
	}
	if f.Blocked != nil {
		clauses = append(clauses, "u.blocked = ?")
		args = append(args, boolToInt(*f.Blocked))
	}
	if f.Unread {
		clauses = append(clauses, "u.unread_count > 0")
	}
	if f.From != nil {
		clauses = append(clauses, "u.created_at >= ?")
		args = append(args, *f.From)
	}
	if f.To != nil {
		clauses = append(clauses, "u.created_at <= ?")
		args = append(args, *f.To)
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func inClause(column string, values []string) (string, []any) {
	clean := make([]any, 0, len(values))
	holders := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		clean = append(clean, v)
		holders = append(holders, "?")
	}
	if len(clean) == 0 {
		return "", nil
	}
	return column + " IN (" + strings.Join(holders, ",") + ")", clean
}

func digitsOf(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ListClients returns one page of CRM clients plus the total row count.
func (r *CRMRepository) ListClients(ctx context.Context, f ClientFilter) ([]domain.CRMClient, int, error) {
	where, args := f.where()

	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*)`+crmClientFrom+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count crm clients: %w", err)
	}

	order, ok := sortColumns[f.Sort]
	if !ok {
		order = sortColumns["last_activity"]
	}
	direction := "ASC"
	if f.Desc {
		direction = "DESC"
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	query := `SELECT ` + crmClientColumns + crmClientFrom + where +
		` ORDER BY ` + order + ` ` + direction + `, u.id DESC LIMIT ? OFFSET ?`
	rows, err := r.db.QueryContext(ctx, query, append(append([]any{}, args...), limit, max0(f.Offset))...)
	if err != nil {
		return nil, 0, fmt.Errorf("list crm clients: %w", err)
	}
	defer rows.Close()

	var out []domain.CRMClient
	for rows.Next() {
		c, err := scanCRMClient(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list crm clients: %w", err)
	}

	if err := r.attachLastMessages(ctx, out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// attachLastMessages fills the list preview with one query for the whole page,
// which is what keeps the client list free of an N+1 pattern.
func (r *CRMRepository) attachLastMessages(ctx context.Context, clients []domain.CRMClient) error {
	if len(clients) == 0 {
		return nil
	}
	ids := make([]any, 0, len(clients))
	holders := make([]string, 0, len(clients))
	index := make(map[int64]int, len(clients))
	for i := range clients {
		ids = append(ids, clients[i].ID)
		holders = append(holders, "?")
		index[clients[i].ID] = i
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT m.user_id, m.text, m.caption, m.message_type, m.created_at
		FROM messages m
		JOIN (
			SELECT user_id, MAX(id) AS id FROM messages
			WHERE user_id IN (`+strings.Join(holders, ",")+`)
			GROUP BY user_id
		) last ON last.id = m.id`, ids...)
	if err != nil {
		return fmt.Errorf("load last messages: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			userID   int64
			text     string
			caption  string
			kind     string
			createdA time.Time
		)
		if err := rows.Scan(&userID, &text, &caption, &kind, &createdA); err != nil {
			return fmt.Errorf("scan last message: %w", err)
		}
		i, ok := index[userID]
		if !ok {
			continue
		}
		preview := text
		if preview == "" {
			preview = caption
		}
		if preview == "" {
			preview = "[" + kind + "]"
		}
		clients[i].LastMessagePreview = truncateRunes(preview, 120)
		t := createdA
		clients[i].LastMessageAt = &t
	}
	return rows.Err()
}

// GetClient loads one CRM client by internal ID.
func (r *CRMRepository) GetClient(ctx context.Context, id int64) (*domain.CRMClient, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+crmClientColumns+crmClientFrom+` WHERE u.id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("load crm client: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("load crm client: %w", err)
		}
		return nil, ErrNotFound
	}
	c, err := scanCRMClient(rows)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// GetClientByWhatsAppID loads a CRM client by their WhatsApp identity.
func (r *CRMRepository) GetClientByWhatsAppID(ctx context.Context, waID string) (*domain.CRMClient, error) {
	var id int64
	err := r.db.QueryRowContext(ctx, `SELECT id FROM users WHERE whatsapp_user_id = ?`, waID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup client: %w", err)
	}
	return r.GetClient(ctx, id)
}

func scanCRMClient(rows *sql.Rows) (domain.CRMClient, error) {
	var (
		c              domain.CRMClient
		lang           string
		state          string
		isLead         int
		crmStatus      string
		mode           string
		aiEnabled      int
		blocked        int
		blockedAt      sql.NullTime
		assignedAt     sql.NullTime
		lastInbound    sql.NullTime
		lastOutbound   sql.NullTime
		facts          string
		nextFollowUpAt sql.NullTime
		tags           string
		langLocked     int
	)
	err := rows.Scan(
		&c.ID, &c.WhatsAppUserID, &c.PhoneNumber, &c.DisplayName, &lang,
		&state, &c.DetectedService, &c.LeadScore, &isLead,
		&c.FirstSeenAt, &c.LastSeenAt, &c.User.CreatedAt, &c.User.UpdatedAt,
		&crmStatus, &mode, &aiEnabled, &blocked, &blockedAt, &c.BlockedBy,
		&c.BlockReason, &c.AssignedAdminID, &assignedAt, &c.AssignedBy,
		&lastInbound, &lastOutbound, &c.UnreadCount, &c.AISummary, &facts,
		&c.NextAction, &c.QualificationStage, &c.AIConfidence, &c.Intent, &c.FollowUpStage,
		&nextFollowUpAt, &c.CloseReason, &tags, &langLocked, &c.SummaryWatermark,
		&c.AssignedAdminName)
	if err != nil {
		return c, fmt.Errorf("scan crm client: %w", err)
	}

	c.Language = domain.Language(lang)
	c.CurrentState = domain.ConversationState(state)
	c.IsLead = isLead != 0
	c.CRMStatus = domain.CRMStatus(crmStatus).OrDefault()
	c.Mode = domain.ConversationMode(mode).OrDefault()
	c.AIEnabled = aiEnabled != 0
	c.Blocked = blocked != 0
	c.LanguageLocked = langLocked != 0
	c.ImportantFacts = decodeStringList(facts)
	c.Tags = decodeStringList(tags)
	c.BlockedAt = nullTime(blockedAt)
	c.AssignedAt = nullTime(assignedAt)
	c.LastInboundAt = nullTime(lastInbound)
	c.LastOutboundAt = nullTime(lastOutbound)
	c.NextFollowUpAt = nullTime(nextFollowUpAt)
	return c, nil
}

func nullTime(v sql.NullTime) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time
	return &t
}

func decodeStringList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// EncodeStringList renders a string slice for storage in a TEXT column.
func EncodeStringList(values []string) string {
	clean := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			clean = append(clean, truncateRunes(v, 200))
		}
	}
	if len(clean) == 0 {
		return ""
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return ""
	}
	return string(b)
}

// ------------------------------------------------------------ state writes

// AIState is the validated result of one AI turn, as the backend decided it.
// It is written in a single statement so a concurrent read never observes a
// half-updated client.
type AIState struct {
	Status             domain.CRMStatus
	QualificationStage string
	Intent             string
	Service            string
	Confidence         float64
	Summary            string
	ImportantFacts     []string
	NextAction         string
	Language           domain.Language
}

// ApplyAIState stores the analysed state of a conversation. Empty values leave
// the stored value untouched, so one uncertain turn never erases known facts.
func (r *CRMRepository) ApplyAIState(ctx context.Context, userID int64, s AIState) error {
	facts := EncodeStringList(s.ImportantFacts)
	_, err := r.db.ExecContext(ctx, `
		UPDATE users SET
			crm_status          = CASE WHEN ? <> '' THEN ? ELSE crm_status END,
			qualification_stage = CASE WHEN ? <> '' THEN ? ELSE qualification_stage END,
			intent              = CASE WHEN ? <> '' THEN ? ELSE intent END,
			detected_service    = CASE WHEN ? <> '' THEN ? ELSE detected_service END,
			ai_confidence       = ?,
			ai_summary          = CASE WHEN ? <> '' THEN ? ELSE ai_summary END,
			important_facts     = CASE WHEN ? <> '' THEN ? ELSE important_facts END,
			next_action         = CASE WHEN ? <> '' THEN ? ELSE next_action END,
			language            = CASE WHEN ? <> '' AND language_locked = 0 THEN ? ELSE language END,
			updated_at          = ?
		WHERE id = ?`,
		string(s.Status), string(s.Status),
		s.QualificationStage, s.QualificationStage,
		s.Intent, s.Intent,
		s.Service, s.Service,
		s.Confidence,
		s.Summary, s.Summary,
		facts, facts,
		s.NextAction, s.NextAction,
		string(s.Language), string(s.Language),
		time.Now().UTC(), userID)
	if err != nil {
		return fmt.Errorf("apply ai state: %w", err)
	}
	return nil
}

// TouchInbound records a client message: activity time and the unread badge.
func (r *CRMRepository) TouchInbound(ctx context.Context, userID int64, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE users SET last_inbound_at = ?, unread_count = unread_count + 1, updated_at = ?
		WHERE id = ?`, at.UTC(), time.Now().UTC(), userID)
	if err != nil {
		return fmt.Errorf("touch inbound: %w", err)
	}
	return nil
}

// TouchOutbound records that something was sent to the client.
func (r *CRMRepository) TouchOutbound(ctx context.Context, userID int64, at time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE users SET last_outbound_at = ?, updated_at = ? WHERE id = ?`,
		at.UTC(), time.Now().UTC(), userID)
	if err != nil {
		return fmt.Errorf("touch outbound: %w", err)
	}
	return nil
}

// MarkRead clears the unread badge for a client.
func (r *CRMRepository) MarkRead(ctx context.Context, userID int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE users SET unread_count = 0, updated_at = ? WHERE id = ?`, time.Now().UTC(), userID)
	if err != nil {
		return fmt.Errorf("mark read: %w", err)
	}
	return nil
}

// SetMode switches the conversation between AI, human and paused.
//
// The update is conditional on the current mode when expect is non-empty, which
// makes takeover a compare-and-swap: two consultants clicking "Take over" at the
// same moment cannot both believe they won.
func (r *CRMRepository) SetMode(ctx context.Context, userID int64, mode domain.ConversationMode,
	adminID int64, expect domain.ConversationMode) (bool, error) {

	if !mode.Valid() {
		return false, fmt.Errorf("invalid conversation mode %q", mode)
	}
	now := time.Now().UTC()

	query := `UPDATE users SET conversation_mode = ?, ai_enabled = ?, updated_at = ?`
	args := []any{string(mode), boolToInt(mode == domain.ModeAI), now}

	if mode == domain.ModeHuman && adminID > 0 {
		query += `, assigned_admin_id = CASE WHEN assigned_admin_id = 0 THEN ? ELSE assigned_admin_id END,
			assigned_at = CASE WHEN assigned_admin_id = 0 THEN ? ELSE assigned_at END,
			assigned_by = CASE WHEN assigned_admin_id = 0 THEN ? ELSE assigned_by END,
			crm_status = CASE WHEN crm_status IN ('new','ai_processing','needs_qualification','qualified','waiting_for_client','needs_consultant')
				THEN 'consultant_processing' ELSE crm_status END`
		args = append(args, adminID, now, adminID)
	}
	query += ` WHERE id = ?`
	args = append(args, userID)
	if expect != "" {
		query += ` AND conversation_mode = ?`
		args = append(args, string(expect))
	}

	res, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("set conversation mode: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set conversation mode: %w", err)
	}
	return n > 0, nil
}

// SetStatus moves a client through the pipeline.
func (r *CRMRepository) SetStatus(ctx context.Context, userID int64, status domain.CRMStatus, reason string) error {
	if !status.Valid() {
		return fmt.Errorf("invalid crm status %q", status)
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE users SET crm_status = ?, close_reason = ?, updated_at = ? WHERE id = ?`,
		string(status), truncate(reason, 500), time.Now().UTC(), userID)
	if err != nil {
		return fmt.Errorf("set crm status: %w", err)
	}
	return nil
}

// SetBlocked blocks or unblocks a client. History is never deleted.
func (r *CRMRepository) SetBlocked(ctx context.Context, userID int64, blocked bool, adminID int64, reason string) error {
	now := time.Now().UTC()
	if blocked {
		_, err := r.db.ExecContext(ctx, `
			UPDATE users SET blocked = 1, blocked_at = ?, blocked_by = ?, block_reason = ?,
				crm_status = 'blocked', ai_enabled = 0, conversation_mode = 'paused',
				next_follow_up_at = NULL, updated_at = ?
			WHERE id = ?`, now, adminID, truncate(reason, 500), now, userID)
		if err != nil {
			return fmt.Errorf("block client: %w", err)
		}
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE users SET blocked = 0, blocked_at = NULL, blocked_by = 0, block_reason = '',
			crm_status = CASE WHEN crm_status = 'blocked' THEN 'needs_qualification' ELSE crm_status END,
			ai_enabled = 1, conversation_mode = 'ai', updated_at = ?
		WHERE id = ?`, now, userID)
	if err != nil {
		return fmt.Errorf("unblock client: %w", err)
	}
	return nil
}

// Assign links a client to a consultant, or clears the assignment when
// consultantID is zero.
func (r *CRMRepository) Assign(ctx context.Context, userID, consultantID, byAdminID int64) error {
	now := time.Now().UTC()
	if consultantID == 0 {
		_, err := r.db.ExecContext(ctx,
			`UPDATE users SET assigned_admin_id = 0, assigned_at = NULL, assigned_by = 0, updated_at = ? WHERE id = ?`,
			now, userID)
		if err != nil {
			return fmt.Errorf("unassign client: %w", err)
		}
		return nil
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE users SET assigned_admin_id = ?, assigned_at = ?, assigned_by = ?, updated_at = ? WHERE id = ?`,
		consultantID, now, byAdminID, now, userID)
	if err != nil {
		return fmt.Errorf("assign client: %w", err)
	}
	return nil
}

// ClientPatch is the set of fields a consultant may edit by hand.
type ClientPatch struct {
	DisplayName *string
	Language    *domain.Language
	Service     *string
	Status      *domain.CRMStatus
	Tags        *[]string
	NextAction  *string
	CloseReason *string
}

// UpdateClient applies a manual edit. Only the supplied fields change.
func (r *CRMRepository) UpdateClient(ctx context.Context, userID int64, p ClientPatch) error {
	var (
		sets []string
		args []any
	)
	if p.DisplayName != nil {
		sets = append(sets, "display_name = ?")
		args = append(args, truncateRunes(strings.TrimSpace(*p.DisplayName), 120))
	}
	if p.Language != nil {
		if !p.Language.Valid() {
			return fmt.Errorf("invalid language %q", *p.Language)
		}
		// A manual language choice wins over future automatic detection.
		sets = append(sets, "language = ?", "language_locked = 1")
		args = append(args, string(*p.Language))
	}
	if p.Service != nil {
		sets = append(sets, "detected_service = ?")
		args = append(args, truncate(strings.TrimSpace(*p.Service), 100))
	}
	if p.Status != nil {
		if !p.Status.Valid() {
			return fmt.Errorf("invalid crm status %q", *p.Status)
		}
		sets = append(sets, "crm_status = ?")
		args = append(args, string(*p.Status))
	}
	if p.Tags != nil {
		sets = append(sets, "tags = ?")
		args = append(args, EncodeStringList(*p.Tags))
	}
	if p.NextAction != nil {
		sets = append(sets, "next_action = ?")
		args = append(args, truncateRunes(strings.TrimSpace(*p.NextAction), 500))
	}
	if p.CloseReason != nil {
		sets = append(sets, "close_reason = ?")
		args = append(args, truncateRunes(strings.TrimSpace(*p.CloseReason), 500))
	}
	if len(sets) == 0 {
		return nil
	}

	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().UTC(), userID)
	_, err := r.db.ExecContext(ctx,
		`UPDATE users SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return fmt.Errorf("update client: %w", err)
	}
	return nil
}

// SetFollowUpPlan stores the client-level view of the next scheduled nudge.
func (r *CRMRepository) SetFollowUpPlan(ctx context.Context, userID int64, stage int, at *time.Time) error {
	var when any
	if at != nil {
		when = at.UTC()
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE users SET follow_up_stage = ?, next_follow_up_at = ?, updated_at = ? WHERE id = ?`,
		stage, when, time.Now().UTC(), userID)
	if err != nil {
		return fmt.Errorf("set follow up plan: %w", err)
	}
	return nil
}

// SetSummaryWatermark records the last message folded into the rolling summary,
// so the summariser never re-reads the same history twice.
func (r *CRMRepository) SetSummaryWatermark(ctx context.Context, userID, messageID int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE users SET summary_watermark = ? WHERE id = ? AND summary_watermark < ?`,
		messageID, userID, messageID)
	if err != nil {
		return fmt.Errorf("set summary watermark: %w", err)
	}
	return nil
}

// ------------------------------------------------------------- dashboard

// Dashboard is the operational overview of the whole CRM.
type Dashboard struct {
	ByStatus         map[string]int `json:"by_status"`
	NewToday         int            `json:"new_today"`
	MessagesToday    int            `json:"messages_today"`
	InboundToday     int            `json:"inbound_today"`
	ActiveDialogs    int            `json:"active_dialogs"`
	AIConversations  int            `json:"ai_conversations"`
	HumanDialogs     int            `json:"human_conversations"`
	ScheduledFollows int            `json:"scheduled_follow_ups"`
	Unread           int            `json:"unread_conversations"`
	Blocked          int            `json:"blocked"`
	TokensToday      TokenUsage     `json:"-"`
}

// Dashboard aggregates the headline metrics in a handful of indexed queries.
func (r *CRMRepository) Dashboard(ctx context.Context, since time.Time) (Dashboard, error) {
	out := Dashboard{ByStatus: map[string]int{}}

	rows, err := r.db.QueryContext(ctx, `SELECT crm_status, COUNT(*) FROM users GROUP BY crm_status`)
	if err != nil {
		return out, fmt.Errorf("dashboard status counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return out, fmt.Errorf("scan status count: %w", err)
		}
		out.ByStatus[status] = n
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("dashboard status counts: %w", err)
	}

	scalars := []struct {
		query string
		args  []any
		dest  *int
	}{
		{`SELECT COUNT(*) FROM users WHERE created_at >= ?`, []any{since}, &out.NewToday},
		{`SELECT COUNT(*) FROM messages WHERE created_at >= ?`, []any{since}, &out.MessagesToday},
		{`SELECT COUNT(*) FROM messages WHERE created_at >= ? AND direction = 'incoming'`, []any{since}, &out.InboundToday},
		{`SELECT COUNT(*) FROM users WHERE blocked = 0 AND crm_status NOT IN ('won','closed','lost','blocked')`, nil, &out.ActiveDialogs},
		{`SELECT COUNT(*) FROM users WHERE conversation_mode = 'ai' AND blocked = 0`, nil, &out.AIConversations},
		{`SELECT COUNT(*) FROM users WHERE conversation_mode = 'human'`, nil, &out.HumanDialogs},
		{`SELECT COUNT(*) FROM follow_up_jobs WHERE status = 'pending'`, nil, &out.ScheduledFollows},
		{`SELECT COUNT(*) FROM users WHERE unread_count > 0`, nil, &out.Unread},
		{`SELECT COUNT(*) FROM users WHERE blocked = 1`, nil, &out.Blocked},
	}
	for _, s := range scalars {
		if err := r.db.QueryRowContext(ctx, s.query, s.args...).Scan(s.dest); err != nil {
			return out, fmt.Errorf("dashboard metric: %w", err)
		}
	}
	return out, nil
}

// ServiceCount is one row of the service breakdown.
type ServiceCount struct {
	Service string `json:"service"`
	Count   int    `json:"count"`
}

// ServiceBreakdown reports how many clients asked for each service.
func (r *CRMRepository) ServiceBreakdown(ctx context.Context, limit int) ([]ServiceCount, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT detected_service, COUNT(*) AS n FROM users
		WHERE detected_service <> '' GROUP BY detected_service ORDER BY n DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("service breakdown: %w", err)
	}
	defer rows.Close()

	var out []ServiceCount
	for rows.Next() {
		var s ServiceCount
		if err := rows.Scan(&s.Service, &s.Count); err != nil {
			return nil, fmt.Errorf("scan service breakdown: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DailyCount is one point of a time series.
type DailyCount struct {
	Day   string `json:"day"`
	Count int    `json:"count"`
}

// DailyNewClients returns new clients per day for the analytics chart.
//
// The day is taken with substr rather than SQLite's date(): the driver stores a
// timestamp as text like "2026-09-10 09:21:22.999999999+00:00", and date()
// returns NULL for any value it cannot parse, which used to fail the scan and
// break the whole analytics screen. The first ten characters are the calendar
// day for every layout the driver writes, whether the separator is a space or a
// "T", and everything is stored in UTC. Rows are still filtered on the raw
// column, so the index on created_at is used.
func (r *CRMRepository) DailyNewClients(ctx context.Context, since time.Time) ([]DailyCount, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT substr(created_at, 1, 10) AS d, COUNT(*) FROM users
		WHERE created_at >= ? AND created_at <> ''
		GROUP BY d ORDER BY d ASC`, since)
	if err != nil {
		return nil, fmt.Errorf("daily new clients: %w", err)
	}
	defer rows.Close()

	var out []DailyCount
	for rows.Next() {
		var (
			day   sql.NullString
			count int
		)
		// A malformed timestamp must never take the analytics page down, so a
		// NULL day is tolerated here and skipped below.
		if err := rows.Scan(&day, &count); err != nil {
			return nil, fmt.Errorf("scan daily count: %w", err)
		}
		if !day.Valid || len(day.String) != 10 {
			continue
		}
		out = append(out, DailyCount{Day: day.String, Count: count})
	}
	return out, rows.Err()
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// truncateRunes shortens text on a rune boundary, so multi-byte Cyrillic and
// Kazakh characters are never cut in half.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
