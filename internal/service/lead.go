package service

import (
	"fmt"
	"strings"

	"lawyer-bot/internal/domain"
)

// Qualifier turns the outcome of one processed message into a lead verdict.
// It is deliberately deterministic: the model contributes a score, the rules
// decide whether a lead exists.
type Qualifier struct {
	catalog       *Catalog
	minConfidence float64
}

// NewQualifier builds a Qualifier.
func NewQualifier(catalog *Catalog, minConfidence float64) *Qualifier {
	return &Qualifier{catalog: catalog, minConfidence: minConfidence}
}

// QualificationInput describes the conversation after the response decision.
type QualificationInput struct {
	Decision      Decision
	AI            domain.AIClassification
	AICalled      bool
	Service       string
	Language      domain.Language
	HasPhone      bool
	Facts         map[string]string
	LastUserText  string
	IncomingCount int
}

// QualificationResult is the lead verdict for this turn.
type QualificationResult struct {
	// Track is true when a lead row should exist at all. Not every tracked
	// lead is qualified: an in-progress conversation is worth recording.
	Track bool
	// Qualified is true when the lead is ready for Diana.
	Qualified bool
	Status    domain.LeadStatus
	Score     float64
	Summary   string
}

// Qualify applies the lead rules:
//   - a relevant legal service has been identified;
//   - the user has shown real interest, not just curiosity;
//   - the necessary clarification has been collected;
//   - contact information is available.
func (q *Qualifier) Qualify(in QualificationInput) QualificationResult {
	res := QualificationResult{
		Status: domain.LeadNew,
		Score:  q.score(in),
	}

	if in.Service == "" && !in.AI.Intent.LegalIntent() {
		return res
	}

	// Any legal-domain conversation is worth a lead row, so nothing is lost if
	// the customer stops replying halfway.
	res.Track = true
	res.Summary = q.summary(in)

	switch {
	case in.Decision.NextState == domain.StateReadyForDiana &&
		in.Service != "" &&
		in.HasPhone &&
		in.AI.Confidence >= q.minConfidence:
		res.Qualified = true
		res.Status = domain.LeadQualified
	case in.Service != "":
		res.Status = domain.LeadQualifying
	default:
		res.Status = domain.LeadNew
	}

	return res
}

// score blends the model's own lead score with deterministic signals, so a
// confident model can never single-handedly declare a hot lead.
func (q *Qualifier) score(in QualificationInput) float64 {
	deterministic := 0.0
	if in.Service != "" && in.Service != domain.ServiceOtherLegalService {
		deterministic += 0.40
	} else if in.AI.Intent.LegalIntent() {
		deterministic += 0.15
	}
	if in.HasPhone {
		deterministic += 0.20
	}
	if in.Facts[domain.FactClarifyAnswer] != "" || in.IncomingCount >= 2 {
		deterministic += 0.20
	}
	if in.AI.Confidence >= q.minConfidence {
		deterministic += 0.20
	}

	modelScore := clamp01(in.AI.LeadScore)
	if !in.AICalled {
		return clamp01(deterministic)
	}
	return clamp01(0.5*deterministic + 0.5*modelScore)
}

// summary is the short human-readable description Diana reads first.
func (q *Qualifier) summary(in QualificationInput) string {
	var parts []string

	if in.Service != "" {
		parts = append(parts, q.catalog.Name(in.Service, in.Language))
	}
	if platform := in.Facts[domain.FactPlatform]; platform != "" {
		parts = append(parts, platformLabel(platform, in.Language))
	}
	if status := in.Facts[domain.FactAppStatus]; status != "" {
		parts = append(parts, status)
	}

	head := strings.Join(parts, " · ")

	// The model's summary is used only as supporting text; it is trimmed and
	// never allowed to carry pricing.
	if s := strings.TrimSpace(in.AI.Summary); s != "" && !containsPrice(s) {
		if len([]rune(s)) > 300 {
			s = string([]rune(s)[:300]) + "…"
		}
		if head == "" {
			return s
		}
		return head + "\n" + s
	}

	if head == "" {
		return truncateRunes(strings.TrimSpace(in.LastUserText), 300)
	}
	if txt := strings.TrimSpace(in.LastUserText); txt != "" {
		return head + "\n" + truncateRunes(txt, 300)
	}
	return head
}

// NotificationText renders the alert sent to Diana when a lead qualifies.
func NotificationText(catalog *Catalog, u *domain.User, l *domain.Lead) string {
	name := strings.TrimSpace(u.DisplayName)
	if name == "" {
		name = "—"
	}
	phone := FormatE164(l.PhoneNumber)
	if phone == "" {
		phone = FormatE164(u.PhoneNumber)
	}
	if phone == "" {
		phone = "—"
	}

	serviceName := l.ServiceName
	if serviceName == "" && l.ServiceCode != "" {
		serviceName = catalog.Name(l.ServiceCode, l.Language)
	}
	if serviceName == "" {
		serviceName = "—"
	}

	summary := strings.TrimSpace(l.QualificationSummary)
	if summary == "" {
		summary = "—"
	}

	return fmt.Sprintf(
		"🆕 Новый квалифицированный лид\n\n"+
			"Имя: %s\n"+
			"Телефон: %s\n"+
			"Язык: %s\n"+
			"Услуга: %s\n\n"+
			"Запрос клиента:\n%s\n\n"+
			"Оценка: %.2f\n"+
			"Источник: %s",
		name, phone, languageLabel(l.Language), serviceName, summary, l.LeadScore, l.Source)
}

func languageLabel(l domain.Language) string {
	switch l.OrDefault() {
	case domain.LangKK:
		return "KZ"
	case domain.LangEN:
		return "EN"
	default:
		return "RU"
	}
}

func platformLabel(platform string, lang domain.Language) string {
	labels := map[string]map[domain.Language]string{
		"mobile_app": {
			domain.LangRU: "мобильное приложение",
			domain.LangKK: "мобильді қосымша",
			domain.LangEN: "mobile application",
		},
		"website": {
			domain.LangRU: "сайт",
			domain.LangKK: "сайт",
			domain.LangEN: "website",
		},
	}
	if table, ok := labels[platform]; ok {
		return tr(lang, table)
	}
	return platform
}

func containsPrice(s string) bool {
	lower := strings.ToLower(s)
	for _, token := range priceTokens {
		if strings.Contains(lower, token) {
			return true
		}
	}
	return false
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
