package service

import (
	"context"
	"strings"
	"time"

	"go.uber.org/zap"

	"lawyer-bot/internal/domain"
	"lawyer-bot/internal/repository"
)

// This file is the CRM half of the pipeline: the gates that decide whether the
// assistant is allowed to speak at all, the durable state it writes afterwards,
// and the rolling summary that keeps prompts small.

// Trace stages added by the CRM layer.
const (
	StageWhatsAppChatGate = "whatsapp_chat_gate"
	StageCRMGate          = "crm_gate"
	StageCRMState         = "crm_state_updated"
	StageFollowUpPlan     = "follow_up_scheduled"
	StageMediaStored      = "media_stored"
)

// CRM gate reasons, recorded verbatim so silence is always explainable.
const (
	CRMReasonBlocked      = "client_blocked"
	CRMReasonHumanMode    = "human_takeover_active"
	CRMReasonPaused       = "automation_paused"
	CRMReasonAIOff        = "ai_disabled_for_client"
	CRMReasonClosed       = "lead_closed"
	CRMReasonGlobalBotOff = "whatsapp_bot_disabled"
	CRMReasonOK           = "automation_allowed"
)

// crmGate decides whether automation may answer this client.
//
// This is the single interlock between the assistant and the consultants. It
// runs after the inbound message has been stored and before a single token is
// spent, so a blocked, closed or human-owned conversation costs nothing and,
// more importantly, never produces a second answer next to a consultant's.
func crmGate(client *domain.CRMClient) (string, bool) {
	switch {
	case client == nil:
		return CRMReasonClosed, false
	case client.Blocked:
		return CRMReasonBlocked, false
	case client.Mode == domain.ModeHuman:
		return CRMReasonHumanMode, false
	case client.Mode == domain.ModePaused:
		return CRMReasonPaused, false
	case !client.AIEnabled:
		return CRMReasonAIOff, false
	case client.CRMStatus.OrDefault().Terminal():
		return CRMReasonClosed, false
	default:
		return CRMReasonOK, true
	}
}

func (p *Pipeline) whatsappBotEnabled(ctx context.Context, log *zap.Logger) bool {
	if p.settings == nil {
		return true
	}
	enabled, err := p.settings.WhatsAppBotEnabled(ctx)
	if err != nil {
		log.Warn("load whatsapp bot setting failed", zap.Error(err))
		return true
	}
	return enabled
}

// onInbound records the client-side effects of a received message: activity
// timestamps, the unread badge, and cancellation of every obsolete follow-up.
//
// Cancelling here rather than later is deliberate: the moment a client replies,
// any pending nudge is wrong, whatever the pipeline decides to do next.
func (p *Pipeline) onInbound(ctx context.Context, log *zap.Logger, userID int64, at time.Time) {
	if p.clients == nil {
		return
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if err := p.clients.TouchInbound(ctx, userID, at); err != nil {
		log.Warn("touch inbound failed", zap.Error(err))
	}
	if p.follow != nil {
		p.follow.CancelFor(ctx, userID, "client replied")
	}
	if p.onChange != nil {
		p.onChange(userID)
	}
}

// applyCRMState writes the validated outcome of one AI turn.
//
// Nothing the model returned is trusted directly: the stage and the status pass
// through the state machine, the summary is merged rather than replaced, and
// critical facts are preserved across the merge.
func (p *Pipeline) applyCRMState(ctx context.Context, log *zap.Logger, client *domain.CRMClient,
	cls domain.AIClassification, decision Decision, aiCalled bool, traceID string, messageID int64) {

	if p.clients == nil || client == nil {
		return
	}

	service := decision.Service
	if service == "" {
		service = cls.ServiceCode
	}

	stage := client.QualificationStage
	if aiCalled {
		stage = NextStage(stage, cls.QualificationStage)
	}
	if derived := StageForDecision(decision, service != ""); derived != "" {
		stage = NextStage(stage, derived)
	}
	if cls.NeedsHuman {
		stage = NextStage(stage, domain.StageConsultantReq)
	}

	// The status the model suggested, the status the stage implies, and the
	// status the database holds are reconciled by the state machine.
	suggested := domain.CRMStatus(cls.LeadStatus)
	if derived := StatusForStage(stage, cls.NeedsHuman); derived != "" {
		suggested = derived
	}
	status := NextStatus(client.CRMStatus, suggested)

	language := domain.Language("")
	if cls.Language.Valid() && !client.LanguageLocked {
		language = cls.Language
	}

	summary, facts := p.mergeSummary(client, cls)

	state := repository.AIState{
		Status:             status,
		QualificationStage: stage,
		Intent:             string(cls.Intent),
		Service:            service,
		Confidence:         cls.Confidence,
		Summary:            summary,
		ImportantFacts:     facts,
		NextAction:         cls.SuggestedFollowUp,
		Language:           language,
	}
	if err := p.clients.ApplyAIState(ctx, client.ID, state); err != nil {
		log.Warn("apply crm state failed", zap.Error(err))
		return
	}

	client.CRMStatus = status
	client.QualificationStage = stage
	client.AISummary = summary
	client.ImportantFacts = facts

	if messageID > 0 {
		if err := p.clients.SetSummaryWatermark(ctx, client.ID, messageID); err != nil {
			log.Warn("set summary watermark failed", zap.Error(err))
		}
	}

	p.event(ctx, domain.TraceEvent{
		TraceID: traceID, UserID: client.ID, MessageID: messageID,
		Stage: StageCRMState, Decision: domain.DecisionOK,
		Detail: repository.Detail(map[string]any{
			"crm_status":  string(status),
			"stage":       stage,
			"needs_human": cls.NeedsHuman,
			"intent":      string(cls.Intent),
			"service":     service,
			"confidence":  cls.Confidence,
		}),
	})
	log.Info("crm state updated",
		zap.String("crm_status", string(status)),
		zap.String("stage", stage),
		zap.Bool("needs_human", cls.NeedsHuman))
}

// mergeSummary folds a model summary update into the stored one.
//
// Token strategy: this is the whole reason the prompt does not grow with the
// conversation. The classifier already returns summary_update as part of the
// call the pipeline was making anyway, so keeping a durable summary costs zero
// extra API calls.
//
// Safety: business-critical facts are never summarised away. Anything the model
// previously recorded that mentions a deadline, a document, a company name, a
// consultation or a promise survives the merge even if the new summary omits it.
func (p *Pipeline) mergeSummary(client *domain.CRMClient, cls domain.AIClassification) (string, []string) {
	summary := strings.TrimSpace(cls.SummaryUpdate)
	if summary == "" {
		summary = strings.TrimSpace(client.AISummary)
	}
	if summary == "" {
		summary = strings.TrimSpace(cls.Summary)
	}

	facts := mergeFacts(client.ImportantFacts, cls.ImportantFacts)
	return truncateRunes(summary, 400), facts
}

// mergeFacts keeps the durable fact list bounded while never dropping a fact
// that carries business meaning.
func mergeFacts(existing, incoming []string) []string {
	seen := make(map[string]bool, len(existing)+len(incoming))
	out := make([]string, 0, maxFacts)

	add := func(fact string) bool {
		fact = strings.TrimSpace(fact)
		if fact == "" || len(out) >= maxFacts {
			return false
		}
		key := strings.ToLower(fact)
		if seen[key] {
			return false
		}
		seen[key] = true
		out = append(out, truncateRunes(fact, 160))
		return true
	}

	// Critical facts first: these must survive any compaction.
	for _, fact := range existing {
		if isCriticalFact(fact) {
			add(fact)
		}
	}
	for _, fact := range incoming {
		add(fact)
	}
	for _, fact := range existing {
		add(fact)
	}
	return out
}

const maxFacts = 8

// criticalFactMarkers are the words that mark a fact as business-critical in
// Russian, Kazakh and English. A fact containing one is never dropped.
var criticalFactMarkers = []string{
	"срок", "дедлайн", "до ", "консультац", "договор", "документ", "тоо", "ип",
	"компан", "заявк", "встреч", "созвон", "оплат", "обещал", "передал", "юрист",
	"мерзім", "құжат", "шарт", "кеңес", "компания", "өтінім", "кездесу", "заңгер",
	"deadline", "contract", "document", "consultation", "company", "meeting", "lawyer",
}

func isCriticalFact(fact string) bool {
	lower := strings.ToLower(fact)
	for _, marker := range criticalFactMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// planFollowUp schedules the first nudge after the assistant has answered.
//
// The anchor is the client's own message: the whole chain is keyed to it, so
// the moment a newer client message exists every stage of the chain becomes
// invalid at delivery time as well as being cancelled here.
func (p *Pipeline) planFollowUp(ctx context.Context, log *zap.Logger, client *domain.CRMClient,
	anchorMessageID int64, traceID string) {

	if p.follow == nil || client == nil {
		return
	}
	if err := p.follow.Schedule(ctx, client, 1, anchorMessageID); err != nil {
		log.Warn("schedule follow-up failed", zap.Error(err))
		return
	}
	p.event(ctx, domain.TraceEvent{
		TraceID: traceID, UserID: client.ID, MessageID: anchorMessageID,
		Stage: StageFollowUpPlan, Decision: domain.DecisionOK,
		Detail: repository.Detail(map[string]any{"stage": 1}),
	})
}

// storeInboundMedia downloads and stores a client's media when the provider
// supports it. A failure is recorded and never loses the message itself.
func (p *Pipeline) storeInboundMedia(ctx context.Context, log *zap.Logger,
	in domain.InboundMessage, userID, messageID int64) {

	if p.media == nil || !p.media.CanDownload() {
		return
	}
	if in.MediaID == "" && in.MediaURL == "" {
		return
	}

	stored, err := p.media.FetchInbound(ctx, in)
	if err != nil {
		log.Warn("download inbound media failed", zap.Error(err))
		p.event(ctx, domain.TraceEvent{
			TraceID: in.TraceID, UserID: userID, MessageID: messageID,
			Stage: StageMediaStored, Decision: domain.DecisionError,
			Reason: "media download failed", Detail: errDetail(err),
		})
		return
	}
	if err := p.messages.SetMedia(ctx, messageID, stored.Path, stored.MimeType,
		stored.DisplayName, stored.Size); err != nil {
		log.Warn("attach media to message failed", zap.Error(err))
		return
	}
	p.event(ctx, domain.TraceEvent{
		TraceID: in.TraceID, UserID: userID, MessageID: messageID,
		Stage: StageMediaStored, Decision: domain.DecisionOK,
		Detail: repository.Detail(map[string]any{
			"mime": stored.MimeType, "bytes": stored.Size}),
	})
}

// crmContext returns the compact durable state sent to the model instead of the
// whole conversation.
func crmContext(client *domain.CRMClient) (summary string, facts []string, stage, status string) {
	if client == nil {
		return "", nil, "", ""
	}
	return client.AISummary, client.ImportantFacts, client.QualificationStage, string(client.CRMStatus)
}
