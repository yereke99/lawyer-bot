package service

import (
	"strings"

	"lawyer-bot/internal/domain"
)

// Activation decides whether an inbound message is allowed to open a funnel
// session for a contact who does not have one yet.
//
// This is the backend enforcement of the rule that the assistant never speaks
// first. A contact becomes eligible for automated conversation only by writing
// one of these phrases themselves; everything else is stored, traced and left
// unanswered.
type Activation struct {
	// phrases are normalised activation phrases. A message activates when one
	// of them appears in the normalised message text.
	phrases []string
	// keywords is the deterministic legal-service trigger set. It activates a
	// session too unless strict mode is on, which preserves the behaviour the
	// business already relies on for customers who write "нужен юрист".
	keywords *TriggerSet
	strict   bool
}

// ActivationKind names what opened a session, recorded on the client record.
const (
	ActivationPhrase  = "activation_phrase"
	ActivationKeyword = "service_keyword"
)

// ActivationMatch is the verdict for one message.
type ActivationMatch struct {
	Matched bool
	// Kind is ActivationPhrase or ActivationKeyword.
	Kind string
	// Trigger is the phrase or keyword that matched, for the audit trail.
	Trigger string
}

// minActivationPhrase is the shortest normalised phrase accepted as an
// activation trigger. It stops a misconfigured one-word value from turning
// every private message into a funnel entry.
const minActivationPhrase = 8

// NewActivation builds the activation matcher.
func NewActivation(phrases []string, keywords *TriggerSet, strict bool) *Activation {
	normalised := make([]string, 0, len(phrases))
	for _, p := range phrases {
		p = Normalize(p)
		if len([]rune(p)) < minActivationPhrase {
			continue
		}
		normalised = append(normalised, p)
	}
	return &Activation{phrases: normalised, keywords: keywords, strict: strict}
}

// Phrases reports the configured activation phrases, for the settings screen.
func (a *Activation) Phrases() []string {
	if a == nil {
		return nil
	}
	out := make([]string, len(a.phrases))
	copy(out, a.phrases)
	return out
}

// Strict reports whether only the configured phrases may open a session.
func (a *Activation) Strict() bool { return a != nil && a.strict }

// Match reports whether this message may open a funnel session.
//
// Matching is tolerant of the formatting a real customer produces — extra
// spaces, line breaks, punctuation, capitalisation — because Normalize folds
// all of it away. It is not tolerant of unrelated text: the whole phrase must
// be present.
func (a *Activation) Match(text string, kind domain.MessageType) ActivationMatch {
	if a == nil || !kind.Analyzable() {
		return ActivationMatch{}
	}
	norm := Normalize(text)
	if norm == "" {
		return ActivationMatch{}
	}

	for _, phrase := range a.phrases {
		if strings.Contains(norm, phrase) {
			return ActivationMatch{Matched: true, Kind: ActivationPhrase, Trigger: phrase}
		}
	}
	if a.strict || a.keywords == nil {
		return ActivationMatch{}
	}

	// A greeting on its own is never a funnel entry, whatever else the keyword
	// matcher thinks: "Сәлеметсіз бе" must leave the bot silent.
	if a.keywords.IsSmallTalkOnly(text) {
		return ActivationMatch{}
	}
	if match := a.keywords.Match(text); match.Matched {
		return ActivationMatch{Matched: true, Kind: ActivationKeyword, Trigger: strings.Join(match.Codes, ",")}
	}
	return ActivationMatch{}
}
