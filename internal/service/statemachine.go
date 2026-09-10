package service

import (
	"strings"

	"lawyer-bot/internal/domain"
)

// The conversation state machine.
//
// The model is allowed to SUGGEST a stage, a status and a next action. It is
// never allowed to set them: every value it returns passes through this file,
// which validates the value itself and then validates the transition from the
// state the database currently holds. An unknown or illegal suggestion is
// downgraded to the nearest legal state rather than accepted.
//
// The practical consequence: no prompt injection, no hallucinated status and no
// model error can move a client into "won", unblock a blocked contact, or take a
// conversation away from a consultant.

// stageOrder gives each qualification stage a rank. Progress is normally
// forward; a backwards jump is only honoured for the explicit escalation and
// takeover stages, which may happen at any point.
var stageOrder = map[string]int{
	domain.StageNone:           0,
	domain.StageLanguage:       1,
	domain.StageIntent:         2,
	domain.StageQualifying:     3,
	domain.StageQualified:      4,
	domain.StageWaitingClient:  5,
	domain.StageConsultantReq:  6,
	domain.StageHumanTakeover:  7,
	domain.StageConversationOK: 8,
}

// NextStage validates a suggested qualification stage against the current one.
func NextStage(current, suggested string) string {
	suggested = strings.TrimSpace(suggested)
	if !domain.ValidQualificationStage(suggested) || suggested == domain.StageNone {
		return current
	}
	// Escalation and takeover are always allowed, from anywhere.
	if suggested == domain.StageConsultantReq || suggested == domain.StageHumanTakeover {
		return suggested
	}
	// A finished conversation is never silently reopened by the model.
	if current == domain.StageConversationOK || current == domain.StageHumanTakeover {
		return current
	}
	// Waiting for the client is a holding state the machine may re-enter.
	if suggested == domain.StageWaitingClient {
		return suggested
	}
	if stageOrder[suggested] >= stageOrder[current] {
		return suggested
	}
	return current
}

// statusTransitions declares which pipeline moves the AI may make. Statuses
// absent from this table are reachable only from the CRM by a human.
var statusTransitions = map[domain.CRMStatus]map[domain.CRMStatus]bool{
	domain.CRMNew: {
		domain.CRMAIProcessing:       true,
		domain.CRMNeedsQualification: true,
		domain.CRMQualified:          true,
		domain.CRMNeedsConsultant:    true,
		domain.CRMWaitingForClient:   true,
	},
	domain.CRMAIProcessing: {
		domain.CRMNeedsQualification: true,
		domain.CRMQualified:          true,
		domain.CRMNeedsConsultant:    true,
		domain.CRMWaitingForClient:   true,
	},
	domain.CRMNeedsQualification: {
		domain.CRMQualified:        true,
		domain.CRMNeedsConsultant:  true,
		domain.CRMWaitingForClient: true,
		domain.CRMAIProcessing:     true,
	},
	domain.CRMQualified: {
		domain.CRMNeedsConsultant:  true,
		domain.CRMWaitingForClient: true,
	},
	domain.CRMWaitingForClient: {
		domain.CRMNeedsQualification: true,
		domain.CRMQualified:          true,
		domain.CRMNeedsConsultant:    true,
		domain.CRMAIProcessing:       true,
	},
	domain.CRMNeedsConsultant: {
		// Only a human moves a lead out of "needs consultant".
	},
}

// NextStatus validates a suggested pipeline status.
//
// Terminal statuses (won, closed, lost, blocked) and the consultant-owned
// statuses are never reachable from the model: those are human decisions.
func NextStatus(current, suggested domain.CRMStatus) domain.CRMStatus {
	current = current.OrDefault()
	if !suggested.Valid() || suggested == current {
		return current
	}
	if current.Terminal() || current == domain.CRMConsultantProcessing ||
		current == domain.CRMConsultationSet || current == domain.CRMInProgress {
		return current
	}
	if suggested.Terminal() || suggested == domain.CRMConsultantProcessing ||
		suggested == domain.CRMConsultationSet || suggested == domain.CRMInProgress {
		return current
	}
	if allowed, ok := statusTransitions[current]; ok && allowed[suggested] {
		return suggested
	}
	return current
}

// StatusForStage derives the pipeline status a validated stage implies. It is
// the bridge between the conversation machine and the sales board.
func StatusForStage(stage string, needsHuman bool) domain.CRMStatus {
	if needsHuman {
		return domain.CRMNeedsConsultant
	}
	switch stage {
	case domain.StageConsultantReq:
		return domain.CRMNeedsConsultant
	case domain.StageHumanTakeover:
		return domain.CRMConsultantProcessing
	case domain.StageQualified:
		return domain.CRMQualified
	case domain.StageWaitingClient:
		return domain.CRMWaitingForClient
	case domain.StageQualifying, domain.StageIntent:
		return domain.CRMNeedsQualification
	case domain.StageLanguage:
		return domain.CRMAIProcessing
	default:
		return ""
	}
}

// ManualStatusAllowed reports whether a human may move a client into a status.
// Consultants control the whole board; only "blocked" is reserved, because it
// is set by the block action rather than by editing a dropdown.
func ManualStatusAllowed(target domain.CRMStatus) bool {
	return target.Valid() && target != domain.CRMBlocked
}

// StageForDecision maps the deterministic reply decision onto a conversation
// stage, so the CRM shows a coherent stage even when the model suggested none.
func StageForDecision(d Decision, hasService bool) string {
	switch d.Action {
	case ActionServiceMenu:
		return domain.StageIntent
	case ActionClarify:
		return domain.StageQualifying
	case ActionAskContact:
		return domain.StageQualifying
	case ActionServiceInfo, ActionHandoff:
		if hasService {
			return domain.StageQualified
		}
		return domain.StageQualifying
	default:
		return ""
	}
}
