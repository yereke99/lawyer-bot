package domain

import "time"

// ---------------------------------------------------------------- CRM status

// CRMStatus is the position of a client in the sales pipeline. It is stored on
// the client record and is the value the CRM filters and reports on.
//
// The legacy LeadStatus on the leads table is untouched by this type: leads keep
// their historical lifecycle, while CRMStatus describes the conversation the
// consultants actually work with.
type CRMStatus string

const (
	CRMNew                  CRMStatus = "new"
	CRMAIProcessing         CRMStatus = "ai_processing"
	CRMNeedsQualification   CRMStatus = "needs_qualification"
	CRMQualified            CRMStatus = "qualified"
	CRMWaitingForClient     CRMStatus = "waiting_for_client"
	CRMNeedsConsultant      CRMStatus = "needs_consultant"
	CRMConsultantProcessing CRMStatus = "consultant_processing"
	CRMConsultationSet      CRMStatus = "consultation_scheduled"
	CRMInProgress           CRMStatus = "in_progress"
	CRMWon                  CRMStatus = "won"
	CRMClosed               CRMStatus = "closed"
	CRMLost                 CRMStatus = "lost"
	CRMBlocked              CRMStatus = "blocked"
)

// AllCRMStatuses is the ordered pipeline, used by the CRM board and filters.
var AllCRMStatuses = []CRMStatus{
	CRMNew, CRMAIProcessing, CRMNeedsQualification, CRMQualified,
	CRMWaitingForClient, CRMNeedsConsultant, CRMConsultantProcessing,
	CRMConsultationSet, CRMInProgress, CRMWon, CRMClosed, CRMLost, CRMBlocked,
}

// Valid reports whether s is a known pipeline status.
func (s CRMStatus) Valid() bool {
	for _, known := range AllCRMStatuses {
		if s == known {
			return true
		}
	}
	return false
}

// Terminal reports whether the conversation is finished. Automatic follow-ups
// never run for a terminal status.
func (s CRMStatus) Terminal() bool {
	switch s {
	case CRMWon, CRMClosed, CRMLost, CRMBlocked:
		return true
	}
	return false
}

// OrDefault falls back to the entry status.
func (s CRMStatus) OrDefault() CRMStatus {
	if s.Valid() {
		return s
	}
	return CRMNew
}

// ------------------------------------------------------- conversation mode

// ConversationMode decides who is allowed to answer a client. It is the single
// interlock that stops the AI and a consultant from replying at the same time.
type ConversationMode string

const (
	// ModeAI is the default: automatic replies and follow-ups are allowed.
	ModeAI ConversationMode = "ai"
	// ModeHuman means a consultant owns the conversation. The AI never sends
	// anything, including follow-ups, until the mode is reset.
	ModeHuman ConversationMode = "human"
	// ModePaused stops automation without assigning a human owner.
	ModePaused ConversationMode = "paused"
)

// Valid reports whether m is a known mode.
func (m ConversationMode) Valid() bool {
	switch m {
	case ModeAI, ModeHuman, ModePaused:
		return true
	}
	return false
}

// AutomationAllowed reports whether the AI may answer in this mode.
func (m ConversationMode) AutomationAllowed() bool { return m == ModeAI || m == "" }

// OrDefault falls back to AI mode, which is how every legacy row behaves.
func (m ConversationMode) OrDefault() ConversationMode {
	if m.Valid() {
		return m
	}
	return ModeAI
}

// ------------------------------------------------------------- sender type

// SenderType records who produced a message, so the CRM can visually separate
// the client, the assistant, a consultant and system events.
type SenderType string

const (
	SenderClient     SenderType = "client"
	SenderAI         SenderType = "ai"
	SenderConsultant SenderType = "consultant"
	SenderSystem     SenderType = "system"
)

// Valid reports whether s is a known sender type.
func (s SenderType) Valid() bool {
	switch s {
	case SenderClient, SenderAI, SenderConsultant, SenderSystem:
		return true
	}
	return false
}

// ------------------------------------------------------------ admin users

// AdminRole is the permission level of a CRM account.
type AdminRole string

const (
	RoleAdmin      AdminRole = "admin"
	RoleConsultant AdminRole = "consultant"
	RoleReadOnly   AdminRole = "readonly"
)

// Valid reports whether r is a known role.
func (r AdminRole) Valid() bool {
	switch r {
	case RoleAdmin, RoleConsultant, RoleReadOnly:
		return true
	}
	return false
}

// CanWrite reports whether the role may change data or send messages.
func (r AdminRole) CanWrite() bool { return r == RoleAdmin || r == RoleConsultant }

// CanAdminister reports whether the role may manage accounts and settings.
func (r AdminRole) CanAdminister() bool { return r == RoleAdmin }

// AdminUser is a CRM account. Credential material is deliberately absent from
// this type: it lives in repository.AdminCredentials and never reaches a handler
// or a JSON response.
type AdminUser struct {
	ID             int64
	Email          string
	Name           string
	Role           AdminRole
	Active         bool
	LastLoginAt    *time.Time
	FailedAttempts int
	LockedUntil    *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Locked reports whether login is temporarily refused after repeated failures.
func (a AdminUser) Locked(now time.Time) bool {
	return a.LockedUntil != nil && now.Before(*a.LockedUntil)
}

// AdminSession is a server-side session. The cookie carries a random token; the
// database stores only its SHA-256 digest, so a database leak cannot be replayed.
type AdminSession struct {
	ID         string
	AdminID    int64
	CSRFToken  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

// Expired reports whether the session may no longer be used.
func (s AdminSession) Expired(now time.Time) bool { return !now.Before(s.ExpiresAt) }

// -------------------------------------------------------------- follow-ups

// FollowUpStatus is the lifecycle of one scheduled follow-up job.
type FollowUpStatus string

const (
	FollowUpPending   FollowUpStatus = "pending"
	FollowUpClaimed   FollowUpStatus = "claimed"
	FollowUpSent      FollowUpStatus = "sent"
	FollowUpCancelled FollowUpStatus = "cancelled"
	FollowUpFailed    FollowUpStatus = "failed"
	FollowUpSkipped   FollowUpStatus = "skipped"
)

// FollowUpJob is a durable reminder to nudge a client who stopped replying.
// Jobs live in the database, so a restart never loses one and no in-memory timer
// is involved.
type FollowUpJob struct {
	ID          int64
	UserID      int64
	Stage       int
	ScheduledAt time.Time
	Status      FollowUpStatus
	ClaimToken  string
	ClaimedAt   *time.Time
	Attempts    int
	LastError   string
	DedupeKey   string
	MessageID   int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ----------------------------------------------------------- notes & audit

// InternalNote is a private consultant note. It is never sent to WhatsApp.
type InternalNote struct {
	ID        int64
	UserID    int64
	AdminID   int64
	AdminName string
	Body      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Audit actions. Kept as constants so the log is queryable.
const (
	AuditLogin           = "auth.login"
	AuditLoginFailed     = "auth.login_failed"
	AuditLogout          = "auth.logout"
	AuditTakeover        = "client.takeover"
	AuditResumeAI        = "client.resume_ai"
	AuditPauseAI         = "client.pause_ai"
	AuditStatusChange    = "client.status_change"
	AuditAssign          = "client.assign"
	AuditUnassign        = "client.unassign"
	AuditBlock           = "client.block"
	AuditUnblock         = "client.unblock"
	AuditManualMessage   = "client.manual_message"
	AuditNoteCreate      = "note.create"
	AuditNoteUpdate      = "note.update"
	AuditNoteDelete      = "note.delete"
	AuditFollowUpCancel  = "followup.cancel"
	AuditFollowUpResched = "followup.reschedule"
	AuditSettingsChange  = "settings.change"
	AuditExport          = "crm.export"
	AuditClientUpdate    = "client.update"
)

// AuditLog is one recorded administrative action. Secrets are never stored here.
type AuditLog struct {
	ID        int64
	AdminID   int64
	AdminName string
	Action    string
	Entity    string
	EntityID  int64
	Detail    string
	IPHash    string
	CreatedAt time.Time
}

// ------------------------------------------------------------- CRM client

// CRMClient is the operational view of a WhatsApp contact: the User row plus
// every CRM field. It exists so the CRM never has to join five tables to render
// a list row.
type CRMClient struct {
	User

	CRMStatus          CRMStatus
	Mode               ConversationMode
	AIEnabled          bool
	Blocked            bool
	BlockedAt          *time.Time
	BlockedBy          int64
	BlockReason        string
	AssignedAdminID    int64
	AssignedAdminName  string
	AssignedAt         *time.Time
	AssignedBy         int64
	LastInboundAt      *time.Time
	LastOutboundAt     *time.Time
	UnreadCount        int
	AISummary          string
	ImportantFacts     []string
	NextAction         string
	QualificationStage string
	AIConfidence       float64
	Intent             string
	FollowUpStage      int
	NextFollowUpAt     *time.Time
	CloseReason        string
	Tags               []string
	LanguageLocked     bool
	SummaryWatermark   int64
	LastMessagePreview string
	LastMessageAt      *time.Time
}

// AutomationAllowed reports whether the assistant may answer this client.
// Every automatic send path in the application funnels through this method.
func (c *CRMClient) AutomationAllowed() bool {
	if c == nil {
		return false
	}
	return !c.Blocked &&
		c.AIEnabled &&
		c.Mode.AutomationAllowed() &&
		!c.CRMStatus.OrDefault().Terminal()
}

// QualificationStage values produced by the state machine. They are validated
// server-side; the model may only suggest one of these.
const (
	StageNone           = ""
	StageLanguage       = "language_detected"
	StageIntent         = "intent_detected"
	StageQualifying     = "qualifying"
	StageQualified      = "qualified"
	StageWaitingClient  = "waiting_for_client"
	StageConsultantReq  = "consultant_required"
	StageHumanTakeover  = "human_takeover"
	StageConversationOK = "converted"
)

// ValidQualificationStage reports whether s is a stage the backend accepts.
func ValidQualificationStage(s string) bool {
	switch s {
	case StageNone, StageLanguage, StageIntent, StageQualifying, StageQualified,
		StageWaitingClient, StageConsultantReq, StageHumanTakeover, StageConversationOK:
		return true
	}
	return false
}
