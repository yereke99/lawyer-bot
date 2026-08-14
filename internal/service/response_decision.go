package service

import "lawyer-bot/internal/domain"

// ReplyAction is the kind of reply the bot is allowed to produce. The decision
// engine picks the action; the composer turns it into text.
type ReplyAction string

const (
	ActionNone        ReplyAction = ""
	ActionServiceMenu ReplyAction = "service_menu"
	ActionClarify     ReplyAction = "clarify"
	ActionServiceInfo ReplyAction = "service_info"
	ActionAskContact  ReplyAction = "ask_contact"
	ActionHandoff     ReplyAction = "handoff"
)

// Decision reasons, stored in the trace so silence is always explainable.
const (
	ReasonNoAIAndNoTrigger    = "no_ai_call_and_no_trigger"
	ReasonAIMarkedIrrelevant  = "ai_marked_irrelevant"
	ReasonGreetingOnly        = "greeting_without_trigger"
	ReasonLowConfidenceNew    = "low_confidence_new_user"
	ReasonLowConfidenceActive = "low_confidence_in_active_flow"
	ReasonServiceInquiry      = "service_inquiry"
	ReasonServiceIdentified   = "service_identified"
	ReasonContinueFlow        = "continue_qualification"
	ReasonContactNeeded       = "contact_required"
	ReasonReadyForHandoff     = "ready_for_handoff"
	ReasonAIFallbackTrigger   = "ai_unavailable_trigger_fallback"
	ReasonAIUnavailable       = "ai_unavailable_no_trigger"
	ReasonAlreadyHandedOff    = "already_handed_off"
	ReasonNotLegalIntent      = "intent_outside_legal_domain"
)

// DecisionInput carries everything the deterministic engine needs. The model's
// own `should_respond` field is present but is treated as advice only.
type DecisionInput struct {
	TriggerMatched bool
	Trigger        TriggerMatch
	AICalled       bool
	AIFailed       bool
	AI             domain.AIClassification
	State          domain.ConversationState
	MinConfidence  float64

	// HasPhone is true when a usable phone number is already known, which is
	// normally the case on WhatsApp.
	HasPhone bool
	// ClarifyAlreadyAsked prevents the bot from asking the same clarification
	// question twice.
	ClarifyAlreadyAsked bool
	// KnownService is the service already attached to the conversation.
	KnownService string
}

// Decision is the verdict of the response engine.
type Decision struct {
	Respond bool
	Action  ReplyAction
	Reason  string

	// Service is the service the reply should talk about, if any.
	Service string
	// NextState is the state the user moves to when the reply is sent.
	NextState domain.ConversationState
}

// ShouldRespond is the single place where the bot decides to speak.
//
// Deliberate properties:
//   - The model never decides on its own; every path below is application code.
//   - Silence is the default: any case that falls through returns no reply.
//   - Nothing here can start a conversation. The function is only ever reached
//     from an inbound message.
func ShouldRespond(in DecisionInput) Decision {
	silent := func(reason string) Decision {
		return Decision{Respond: false, Action: ActionNone, Reason: reason}
	}

	// The model was never called: the gate already decided this message is not
	// worth tokens. Stay silent, no exceptions.
	if !in.AICalled {
		return silent(ReasonNoAIAndNoTrigger)
	}

	// OpenAI is unreachable. Deterministic triggers become the fallback: a clear
	// legal keyword still deserves the service menu, anything else stays silent
	// rather than risking a wrong answer.
	if in.AIFailed {
		if in.TriggerMatched {
			return Decision{
				Respond:   true,
				Action:    ActionServiceMenu,
				Reason:    ReasonAIFallbackTrigger,
				NextState: domain.StateWaitingIntent,
			}
		}
		return silent(ReasonAIUnavailable)
	}

	ai := in.AI
	service := ai.ServiceCode
	if service == "" {
		service = ServiceFromIntent(ai.Intent)
	}
	if service == "" {
		service = in.KnownService
	}

	// The message is not about legal services. Store it, say nothing.
	if !ai.IsRelevant {
		return silent(ReasonAIMarkedIrrelevant)
	}
	if ai.Intent == domain.IntentGreeting && !in.TriggerMatched {
		return silent(ReasonGreetingOnly)
	}
	if ai.Intent == domain.IntentIrrelevant {
		return silent(ReasonNotLegalIntent)
	}

	// Low confidence: never guess at a stranger. Inside an active flow a single
	// short clarification is preferable to dropping a live conversation.
	if ai.Confidence < in.MinConfidence {
		if in.State.Active() {
			return Decision{
				Respond:   true,
				Action:    ActionClarify,
				Reason:    ReasonLowConfidenceActive,
				Service:   service,
				NextState: domain.StateQualifying,
			}
		}
		if in.TriggerMatched {
			return Decision{
				Respond:   true,
				Action:    ActionServiceMenu,
				Reason:    ReasonServiceInquiry,
				NextState: domain.StateWaitingIntent,
			}
		}
		return silent(ReasonLowConfidenceNew)
	}

	// The handoff already happened. Only a fresh, specific service request
	// reopens the conversation; small talk after handoff stays unanswered.
	if in.State == domain.StateReadyForDiana || in.State == domain.StateCompleted {
		if service != "" && service != in.KnownService {
			return Decision{
				Respond:   true,
				Action:    ActionClarify,
				Reason:    ReasonServiceIdentified,
				Service:   service,
				NextState: domain.StateQualifying,
			}
		}
		return silent(ReasonAlreadyHandedOff)
	}

	// A concrete service is on the table.
	if service != "" && service != domain.ServiceOtherLegalService {
		needsClarification := ai.NeedsClarification && !in.ClarifyAlreadyAsked
		if needsClarification {
			return Decision{
				Respond:   true,
				Action:    ActionClarify,
				Reason:    ReasonServiceIdentified,
				Service:   service,
				NextState: domain.StateQualifying,
			}
		}
		if !in.HasPhone {
			return Decision{
				Respond:   true,
				Action:    ActionAskContact,
				Reason:    ReasonContactNeeded,
				Service:   service,
				NextState: domain.StateWaitingContact,
			}
		}
		return Decision{
			Respond:   true,
			Action:    ActionServiceInfo,
			Reason:    ReasonReadyForHandoff,
			Service:   service,
			NextState: domain.StateReadyForDiana,
		}
	}

	// Relevant, but the specific service is still unknown.
	switch ai.Intent {
	case domain.IntentServiceInquiry, domain.IntentConsultationRequest,
		domain.IntentOtherLegal, domain.IntentUnclear:
		if in.State.Active() {
			return Decision{
				Respond:   true,
				Action:    ActionClarify,
				Reason:    ReasonContinueFlow,
				Service:   service,
				NextState: domain.StateQualifying,
			}
		}
		return Decision{
			Respond:   true,
			Action:    ActionServiceMenu,
			Reason:    ReasonServiceInquiry,
			NextState: domain.StateWaitingIntent,
		}
	case domain.IntentCallbackRequest:
		if in.HasPhone {
			return Decision{
				Respond:   true,
				Action:    ActionHandoff,
				Reason:    ReasonReadyForHandoff,
				Service:   service,
				NextState: domain.StateReadyForDiana,
			}
		}
		return Decision{
			Respond:   true,
			Action:    ActionAskContact,
			Reason:    ReasonContactNeeded,
			Service:   service,
			NextState: domain.StateWaitingContact,
		}
	}

	if in.State.Active() {
		return Decision{
			Respond:   true,
			Action:    ActionClarify,
			Reason:    ReasonContinueFlow,
			Service:   service,
			NextState: domain.StateQualifying,
		}
	}
	return silent(ReasonNotLegalIntent)
}
