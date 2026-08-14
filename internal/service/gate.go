package service

import (
	"strings"

	"lawyer-bot/internal/domain"
)

// Gate is the token budget guard. It runs before any OpenAI call and answers a
// single question: is this message worth spending tokens on?
//
// This is the layer that keeps "Привет", "Как дела?" and "Какая погода?" away
// from the model entirely. Those messages are still stored and traced — they
// simply cost nothing and receive no reply.
type Gate struct {
	triggers          *TriggerSet
	maxCallsPerDay    int
	analyzeUnmatched  bool
	minWordsUnmatched int
}

// GateConfig configures the pre-filter.
type GateConfig struct {
	// MaxCallsPerDay caps model calls per user per day. 0 disables the cap.
	MaxCallsPerDay int
	// AnalyzeUnmatched decides what happens to a substantive message that
	// matched no trigger. True spends one call to avoid losing an unusually
	// phrased lead; false is the strictest token-saving setting.
	AnalyzeUnmatched bool
	// MinWordsUnmatched is the length below which an unmatched message is
	// dropped without analysis.
	MinWordsUnmatched int
}

// NewGate builds a Gate.
func NewGate(triggers *TriggerSet, cfg GateConfig) *Gate {
	if cfg.MinWordsUnmatched <= 0 {
		cfg.MinWordsUnmatched = 3
	}
	return &Gate{
		triggers:          triggers,
		maxCallsPerDay:    cfg.MaxCallsPerDay,
		analyzeUnmatched:  cfg.AnalyzeUnmatched,
		minWordsUnmatched: cfg.MinWordsUnmatched,
	}
}

// Gate reasons, recorded verbatim in the trace so token spend is explainable.
const (
	GateReasonNoText          = "no_text"
	GateReasonMediaOnly       = "media_without_caption"
	GateReasonBudgetExceeded  = "ai_budget_exceeded"
	GateReasonTriggerMatched  = "trigger_matched"
	GateReasonActiveFlow      = "active_qualification_flow"
	GateReasonOffTopic        = "off_topic"
	GateReasonSmallTalkOnly   = "small_talk_only"
	GateReasonTooShort        = "unmatched_and_too_short"
	GateReasonUnmatchedPolicy = "unmatched_analysis_disabled"
	GateReasonUnmatchedProbe  = "unmatched_but_substantive"
)

// GateInput is everything the pre-filter needs.
type GateInput struct {
	Text         string
	MessageType  domain.MessageType
	State        domain.ConversationState
	AICallsToday int
}

// GateResult is the pre-filter verdict.
type GateResult struct {
	CallAI  bool
	Reason  string
	Trigger TriggerMatch
}

// Evaluate decides whether the message reaches the model.
func (g *Gate) Evaluate(in GateInput) GateResult {
	text := strings.TrimSpace(in.Text)
	trigger := g.triggers.Match(text)

	if text == "" {
		reason := GateReasonNoText
		if !in.MessageType.Analyzable() {
			reason = GateReasonMediaOnly
		}
		return GateResult{CallAI: false, Reason: reason, Trigger: trigger}
	}

	// A hard per-user daily cap. Protects against a single contact draining the
	// token budget, deliberately or by accident.
	if g.maxCallsPerDay > 0 && in.AICallsToday >= g.maxCallsPerDay {
		return GateResult{CallAI: false, Reason: GateReasonBudgetExceeded, Trigger: trigger}
	}

	// An explicit legal signal always earns analysis, even alongside a greeting:
	// "Здравствуйте, нужен юрист" is a lead, not small talk.
	if trigger.Matched {
		return GateResult{CallAI: true, Reason: GateReasonTriggerMatched, Trigger: trigger}
	}

	// Chit-chat never reaches the model, in any state.
	if g.triggers.IsOffTopic(text) {
		return GateResult{CallAI: false, Reason: GateReasonOffTopic, Trigger: trigger}
	}
	if g.triggers.IsSmallTalkOnly(text) {
		return GateResult{CallAI: false, Reason: GateReasonSmallTalkOnly, Trigger: trigger}
	}

	// Inside an active flow the user is answering the bot's own question.
	// "Уже работает" carries no legal keyword but is essential to qualification.
	if in.State.Active() {
		return GateResult{CallAI: true, Reason: GateReasonActiveFlow, Trigger: trigger}
	}

	if !g.analyzeUnmatched {
		return GateResult{CallAI: false, Reason: GateReasonUnmatchedPolicy, Trigger: trigger}
	}
	if len(strings.Fields(Normalize(text))) < g.minWordsUnmatched {
		return GateResult{CallAI: false, Reason: GateReasonTooShort, Trigger: trigger}
	}

	// A substantive first message with no keyword: spend one call rather than
	// lose a lead phrased in words the trigger list does not cover yet.
	return GateResult{CallAI: true, Reason: GateReasonUnmatchedProbe, Trigger: trigger}
}
