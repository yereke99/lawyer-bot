package domain

import "time"

// Language is a supported conversation language.
type Language string

const (
	LangRU      Language = "ru"
	LangKK      Language = "kk"
	LangEN      Language = "en"
	LangUnknown Language = ""
)

// Valid reports whether l is a language the bot is allowed to answer in.
func (l Language) Valid() bool {
	switch l {
	case LangRU, LangKK, LangEN:
		return true
	}
	return false
}

// OrDefault falls back to Russian, the primary business language.
func (l Language) OrDefault() Language {
	if l.Valid() {
		return l
	}
	return LangRU
}

// ConversationState is the persistent qualification state of a user.
type ConversationState string

const (
	StateNew               ConversationState = "new"
	StateWaitingIntent     ConversationState = "waiting_intent"
	StateQualifying        ConversationState = "qualifying"
	StateServiceIdentified ConversationState = "service_identified"
	StateWaitingContact    ConversationState = "waiting_contact"
	StateReadyForDiana     ConversationState = "ready_for_diana"
	StateCompleted         ConversationState = "completed"
)

// Active reports whether the user is inside an ongoing qualification flow.
// Messages from users in an active flow are always worth analysing, because the
// bot has already engaged and the user is answering its question.
func (s ConversationState) Active() bool {
	switch s {
	case StateWaitingIntent, StateQualifying, StateServiceIdentified, StateWaitingContact:
		return true
	}
	return false
}

// Valid reports whether s is a known state.
func (s ConversationState) Valid() bool {
	switch s {
	case StateNew, StateWaitingIntent, StateQualifying, StateServiceIdentified,
		StateWaitingContact, StateReadyForDiana, StateCompleted:
		return true
	}
	return false
}

// User is a WhatsApp contact known to the system.
type User struct {
	ID              int64
	WhatsAppUserID  string
	PhoneNumber     string
	DisplayName     string
	Language        Language
	CurrentState    ConversationState
	DetectedService string
	LeadScore       float64
	IsLead          bool
	FirstSeenAt     time.Time
	LastSeenAt      time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// UserFact is a single piece of structured information extracted from the
// conversation, e.g. platform=mobile_app or app_status=launched. Facts let the
// bot avoid asking the same clarification question twice.
type UserFact struct {
	ID        int64
	UserID    int64
	Key       string
	Value     string
	Source    string // ai | trigger | system
	MessageID int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Well-known fact keys.
const (
	FactPlatform      = "platform"
	FactAppStatus     = "app_status"
	FactCountry       = "country"
	FactContactAsked  = "contact_asked"
	FactMenuSent      = "menu_sent"
	FactClarifyAsked  = "clarify_asked"
	FactClarifyAnswer = "clarify_answered"
)

// StateTransition records every change of a user's conversation state, so the
// full qualification path can be reconstructed later.
type StateTransition struct {
	ID        int64
	UserID    int64
	TraceID   string
	FromState ConversationState
	ToState   ConversationState
	Reason    string
	MessageID int64
	CreatedAt time.Time
}
