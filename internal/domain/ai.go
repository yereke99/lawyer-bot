package domain

import (
	"context"
	"time"
)

// Intent is the classified purpose of an incoming message.
type Intent string

const (
	IntentGreeting            Intent = "greeting"
	IntentServiceInquiry      Intent = "service_inquiry"
	IntentConsultationRequest Intent = "consultation_request"
	IntentTrademark           Intent = "trademark_registration"
	IntentBusinessReg         Intent = "business_registration"
	IntentContractRequest     Intent = "contract_request"
	IntentPrivacyPolicy       Intent = "privacy_policy"
	IntentPublicOffer         Intent = "public_offer"
	IntentMobileAppDocs       Intent = "mobile_app_documents"
	IntentWebsiteDocs         Intent = "website_documents"
	IntentEcommerceDocs       Intent = "ecommerce_documents"
	IntentOtherLegal          Intent = "other_legal_service"
	IntentCallbackRequest     Intent = "callback_request"
	IntentIrrelevant          Intent = "irrelevant"
	IntentUnclear             Intent = "unclear"
)

// AllIntents is the closed set the model is allowed to return.
var AllIntents = []Intent{
	IntentGreeting, IntentServiceInquiry, IntentConsultationRequest,
	IntentTrademark, IntentBusinessReg, IntentContractRequest,
	IntentPrivacyPolicy, IntentPublicOffer, IntentMobileAppDocs,
	IntentWebsiteDocs, IntentEcommerceDocs, IntentOtherLegal,
	IntentCallbackRequest, IntentIrrelevant, IntentUnclear,
}

// Valid reports whether i is a known intent.
func (i Intent) Valid() bool {
	for _, known := range AllIntents {
		if i == known {
			return true
		}
	}
	return false
}

// LegalIntent reports whether the intent belongs to the legal-services domain.
// Greetings, small talk and off-topic questions are deliberately excluded.
func (i Intent) LegalIntent() bool {
	switch i {
	case IntentServiceInquiry, IntentConsultationRequest, IntentTrademark,
		IntentBusinessReg, IntentContractRequest, IntentPrivacyPolicy,
		IntentPublicOffer, IntentMobileAppDocs, IntentWebsiteDocs,
		IntentEcommerceDocs, IntentOtherLegal, IntentCallbackRequest:
		return true
	}
	return false
}

// AIContextMessage is one turn of trimmed conversation history sent to the model.
type AIContextMessage struct {
	Role string // user | assistant
	Text string
}

// AIInput is everything the classifier is allowed to see. It deliberately
// carries no database identifiers, logs or admin data.
type AIInput struct {
	Text            string
	History         []AIContextMessage
	CurrentState    ConversationState
	DetectedService string
	KnownLanguage   Language
	KnownFacts      map[string]string
	Services        []LegalService
}

// AIClassification is the structured result returned by the model. It is
// advisory only: the application decides whether to reply.
type AIClassification struct {
	IsRelevant            bool              `json:"is_relevant"`
	ShouldRespond         bool              `json:"should_respond"`
	Language              Language          `json:"language"`
	Intent                Intent            `json:"intent"`
	ServiceCode           string            `json:"service_code"`
	Confidence            float64           `json:"confidence"`
	NeedsClarification    bool              `json:"needs_clarification"`
	ClarificationQuestion string            `json:"clarification_question"`
	LeadScore             float64           `json:"lead_score"`
	Summary               string            `json:"summary"`
	Facts                 map[string]string `json:"facts"`

	// Populated by the client, not the model.
	Model            string `json:"-"`
	InputTokens      int    `json:"-"`
	OutputTokens     int    `json:"-"`
	RawResponse      string `json:"-"`
	ProcessingTimeMS int64  `json:"-"`
}

// AIClient classifies a message. The service layer depends on this interface
// only, which keeps OpenAI out of the business logic and makes tests trivial.
type AIClient interface {
	ClassifyMessage(ctx context.Context, input AIInput) (AIClassification, error)
}

// AIInteraction is the persisted audit record of one model call.
type AIInteraction struct {
	ID               int64
	UserID           int64
	MessageID        int64
	TraceID          string
	Model            string
	InputTokens      int
	OutputTokens     int
	Intent           string
	ServiceCode      string
	Confidence       float64
	RawResponse      string
	Error            string
	ProcessingTimeMS int64
	CreatedAt        time.Time
}
