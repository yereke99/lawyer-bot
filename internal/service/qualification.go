package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"lawyer-bot/internal/domain"
	"lawyer-bot/internal/repository"
	"lawyer-bot/traits/logger"
)

// Pipeline processes one incoming WhatsApp message end to end.
//
// The order of operations is fixed and deliberate:
//
//	store  ->  gate  ->  classify  ->  decide  ->  reply  ->  qualify  ->  notify
//
// Every message completes the "store" step. Everything after it is conditional,
// and every branch writes a trace event, so the database alone explains why any
// message did or did not receive an answer.
type Pipeline struct {
	users    *repository.UserRepository
	messages *repository.MessageRepository
	leads    *repository.LeadRepository
	aiLog    *repository.AIInteractionRepository
	trace    *repository.TraceRepository

	ai       domain.AIClient
	wa       domain.WhatsAppClient
	gate     *Gate
	catalog  *Catalog
	composer *Composer
	qualify  *Qualifier
	triggers *TriggerSet
	log      *zap.Logger

	cfg PipelineConfig
}

// PipelineConfig holds the tunables the pipeline needs.
type PipelineConfig struct {
	MinConfidence   float64
	ContextMessages int
	NotifyRecipient string
	DefaultSource   string
	DryRun          bool
}

// PipelineDeps groups the pipeline's collaborators.
type PipelineDeps struct {
	Users    *repository.UserRepository
	Messages *repository.MessageRepository
	Leads    *repository.LeadRepository
	AILog    *repository.AIInteractionRepository
	Trace    *repository.TraceRepository

	AI       domain.AIClient
	WhatsApp domain.WhatsAppClient
	Gate     *Gate
	Catalog  *Catalog
	Composer *Composer
	Qualify  *Qualifier
	Triggers *TriggerSet
	Logger   *zap.Logger
}

// NewPipeline builds a Pipeline.
func NewPipeline(deps PipelineDeps, cfg PipelineConfig) *Pipeline {
	if cfg.DefaultSource == "" {
		cfg.DefaultSource = domain.SourceWhatsApp
	}
	if cfg.ContextMessages < 0 {
		cfg.ContextMessages = 0
	}
	return &Pipeline{
		users:    deps.Users,
		messages: deps.Messages,
		leads:    deps.Leads,
		aiLog:    deps.AILog,
		trace:    deps.Trace,
		ai:       deps.AI,
		wa:       deps.WhatsApp,
		gate:     deps.Gate,
		catalog:  deps.Catalog,
		composer: deps.Composer,
		qualify:  deps.Qualify,
		triggers: deps.Triggers,
		log:      deps.Logger,
		cfg:      cfg,
	}
}

// NewTraceID returns the identifier that links every record produced while
// handling one incoming message.
func NewTraceID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Handle processes one incoming message.
//
// It is only ever invoked from an inbound webhook. There is no code path in
// this package that contacts a user first.
func (p *Pipeline) Handle(ctx context.Context, in domain.InboundMessage) error {
	started := time.Now()
	if in.TraceID == "" {
		in.TraceID = NewTraceID()
	}
	log := p.log.With(
		zap.String("trace_id", in.TraceID),
		zap.String("whatsapp_message_id", in.WhatsAppMessageID),
		logger.Phone("phone", in.PhoneNumber),
	)

	// ------------------------------------------------------ 1. deduplication
	duplicate, err := p.messages.ExistsByWhatsAppID(ctx, in.WhatsAppMessageID)
	if err != nil {
		p.event(ctx, domain.TraceEvent{TraceID: in.TraceID, Stage: domain.StagePipelineError,
			Decision: domain.DecisionError, Reason: "duplicate_check_failed", Detail: errDetail(err)})
		return fmt.Errorf("duplicate check: %w", err)
	}
	if duplicate {
		p.event(ctx, domain.TraceEvent{TraceID: in.TraceID, Stage: domain.StageDuplicate,
			Decision: domain.DecisionSilent, Reason: "message already processed"})
		log.Info("duplicate webhook delivery ignored")
		return nil
	}

	// ------------------------------------------------------ 2. identify user
	phone, _ := NormalizePhone(in.PhoneNumber)
	user, err := p.users.Upsert(ctx, in.WhatsAppUserID, phone, in.DisplayName)
	if err != nil {
		p.event(ctx, domain.TraceEvent{TraceID: in.TraceID, Stage: domain.StagePipelineError,
			Decision: domain.DecisionError, Reason: "user_upsert_failed", Detail: errDetail(err)})
		return fmt.Errorf("upsert user: %w", err)
	}
	log = log.With(zap.Int64("user_id", user.ID), zap.String("state", string(user.CurrentState)))
	p.event(ctx, domain.TraceEvent{TraceID: in.TraceID, UserID: user.ID,
		Stage: domain.StageUserUpserted, Decision: domain.DecisionOK,
		Detail: repository.Detail(map[string]any{
			"state":    string(user.CurrentState),
			"language": string(user.Language),
			"service":  user.DetectedService,
		})})

	// ---------------------------------------------------- 3. store the message
	msg := &domain.Message{
		UserID:            user.ID,
		WhatsAppMessageID: in.WhatsAppMessageID,
		TraceID:           in.TraceID,
		MessageType:       in.MessageType,
		Text:              in.Text,
		MediaID:           in.MediaID,
		Caption:           in.Caption,
		Direction:         domain.DirectionIncoming,
		CreatedAt:         in.Timestamp,
	}
	messageID, err := p.messages.Create(ctx, msg)
	if err != nil {
		p.event(ctx, domain.TraceEvent{TraceID: in.TraceID, UserID: user.ID,
			Stage: domain.StagePipelineError, Decision: domain.DecisionError,
			Reason: "message_store_failed", Detail: errDetail(err)})
		return fmt.Errorf("store message: %w", err)
	}
	p.event(ctx, domain.TraceEvent{TraceID: in.TraceID, UserID: user.ID, MessageID: messageID,
		Stage: domain.StageMessageStored, Decision: domain.DecisionOK,
		Detail: repository.Detail(map[string]any{"type": string(in.MessageType), "chars": len(in.Content())})})
	log.Info("incoming message stored",
		zap.Int64("message_id", messageID),
		zap.String("type", string(in.MessageType)),
		logger.Preview("text", in.Content()))

	// Media metadata is always recorded; the binary is never fetched implicitly.
	if in.MediaID != "" {
		if _, err := p.trace.MediaAsset(ctx, domain.MediaAsset{
			UserID: user.ID, MessageID: messageID, MediaID: in.MediaID,
			MimeType: in.MimeType, SHA256: in.SHA256, Filename: in.Filename,
			Caption: in.Caption, Voice: in.Voice,
		}); err != nil {
			log.Warn("store media metadata failed", zap.Error(err))
		}
	}

	// ------------------------------------------- 4. context for this decision
	facts, err := p.trace.Facts(ctx, user.ID)
	if err != nil {
		log.Warn("load user facts failed", zap.Error(err))
		facts = map[string]string{}
	}

	// A phone number typed into the chat is captured wherever it appears.
	if extracted, ok := ExtractPhone(in.Content()); ok && extracted != user.PhoneNumber {
		if err := p.users.SetPhone(ctx, user.ID, extracted); err != nil {
			log.Warn("store extracted phone failed", zap.Error(err))
		} else {
			user.PhoneNumber = extracted
		}
	}

	aiCallsToday, err := p.aiLog.CountByUserSince(ctx, user.ID, startOfDay())
	if err != nil {
		log.Warn("count ai calls failed", zap.Error(err))
	}

	// ------------------------------------------------- 5. gate: spend tokens?
	gateResult := p.gate.Evaluate(GateInput{
		Text:         in.Content(),
		MessageType:  in.MessageType,
		State:        user.CurrentState,
		AICallsToday: aiCallsToday,
	})
	gateDecision := domain.DecisionSkipAI
	if gateResult.CallAI {
		gateDecision = domain.DecisionCallAI
	}
	p.event(ctx, domain.TraceEvent{TraceID: in.TraceID, UserID: user.ID, MessageID: messageID,
		Stage: domain.StageGate, Decision: gateDecision, Reason: gateResult.Reason,
		Detail: repository.Detail(map[string]any{
			"triggers":       gateResult.Trigger.Codes,
			"phrases":        gateResult.Trigger.Phrases,
			"ai_calls_today": aiCallsToday,
		})})
	log.Info("gate evaluated",
		zap.Bool("call_ai", gateResult.CallAI),
		zap.String("reason", gateResult.Reason),
		zap.Strings("triggers", gateResult.Trigger.Codes))

	// ------------------------------------------------------- 6. classify
	var (
		classification domain.AIClassification
		aiFailed       bool
	)
	if gateResult.CallAI {
		classification, aiFailed = p.classify(ctx, log, user, messageID, in)
	}

	// ------------------------------------------------------- 7. decide
	knownService := user.DetectedService
	decision := ShouldRespond(DecisionInput{
		TriggerMatched:      gateResult.Trigger.Matched,
		Trigger:             gateResult.Trigger,
		AICalled:            gateResult.CallAI,
		AIFailed:            aiFailed,
		AI:                  classification,
		State:               user.CurrentState,
		MinConfidence:       p.cfg.MinConfidence,
		HasPhone:            user.PhoneNumber != "",
		ClarifyAlreadyAsked: facts[domain.FactClarifyAsked] != "",
		KnownService:        knownService,
	})

	decisionLabel := domain.DecisionSilent
	if decision.Respond {
		decisionLabel = domain.DecisionRespond
	}
	p.event(ctx, domain.TraceEvent{TraceID: in.TraceID, UserID: user.ID, MessageID: messageID,
		Stage: domain.StageDecision, Decision: decisionLabel, Reason: decision.Reason,
		Detail: repository.Detail(map[string]any{
			"action":     string(decision.Action),
			"service":    decision.Service,
			"intent":     string(classification.Intent),
			"confidence": classification.Confidence,
			"next_state": string(decision.NextState),
		})})
	log.Info("response decision",
		zap.Bool("respond", decision.Respond),
		zap.String("reason", decision.Reason),
		zap.String("action", string(decision.Action)),
		zap.String("intent", string(classification.Intent)),
		zap.Float64("confidence", classification.Confidence))

	if err := p.messages.MarkProcessed(ctx, messageID, gateResult.CallAI && !aiFailed,
		string(classification.Intent), classification.Confidence, decision.Respond); err != nil {
		log.Warn("mark message processed failed", zap.Error(err))
	}

	// Language and service are learned even when the bot stays silent.
	p.rememberContext(ctx, log, user, classification, decision, messageID, in.TraceID)

	if !decision.Respond {
		p.event(ctx, domain.TraceEvent{TraceID: in.TraceID, UserID: user.ID, MessageID: messageID,
			Stage: domain.StagePipelineDone, Decision: domain.DecisionSilent,
			Reason: decision.Reason, DurationMS: time.Since(started).Milliseconds()})
		return nil
	}

	// ------------------------------------------------------- 8. reply
	lang := replyLanguage(classification.Language, user.Language)
	text := p.composer.Compose(decision, lang, classification.ClarificationQuestion)
	if strings.TrimSpace(text) == "" {
		p.event(ctx, domain.TraceEvent{TraceID: in.TraceID, UserID: user.ID, MessageID: messageID,
			Stage: domain.StageReplyBuilt, Decision: domain.DecisionSilent,
			Reason: "composer produced no text"})
		return nil
	}
	p.event(ctx, domain.TraceEvent{TraceID: in.TraceID, UserID: user.ID, MessageID: messageID,
		Stage: domain.StageReplyBuilt, Decision: domain.DecisionOK,
		Reason: string(decision.Action),
		Detail: repository.Detail(map[string]any{"language": string(lang), "chars": len(text)})})

	if err := p.send(ctx, log, user, messageID, in.TraceID, text); err != nil {
		// A failed send must not lose the lead: the qualification below still runs.
		log.Error("reply delivery failed", zap.Error(err))
	}

	// ---------------------------------------------- 9. advance the state machine
	p.advanceState(ctx, log, user, decision, messageID, in.TraceID)
	p.recordReplyFacts(ctx, log, user.ID, decision, messageID)

	// ------------------------------------------------------- 10. qualify lead
	p.qualifyLead(ctx, log, user, classification, decision, gateResult, in, lang, facts)

	p.event(ctx, domain.TraceEvent{TraceID: in.TraceID, UserID: user.ID, MessageID: messageID,
		Stage: domain.StagePipelineDone, Decision: domain.DecisionRespond,
		Reason: decision.Reason, DurationMS: time.Since(started).Milliseconds()})
	return nil
}

// classify calls the model and records the interaction whatever the outcome.
func (p *Pipeline) classify(ctx context.Context, log *zap.Logger, user *domain.User, messageID int64, in domain.InboundMessage) (domain.AIClassification, bool) {
	history := p.history(ctx, log, user.ID, messageID)
	facts, _ := p.trace.Facts(ctx, user.ID)

	p.event(ctx, domain.TraceEvent{TraceID: in.TraceID, UserID: user.ID, MessageID: messageID,
		Stage: domain.StageAIRequested, Decision: domain.DecisionCallAI,
		Detail: repository.Detail(map[string]any{"history_messages": len(history)})})

	result, err := p.ai.ClassifyMessage(ctx, domain.AIInput{
		Text:            in.Content(),
		History:         history,
		CurrentState:    user.CurrentState,
		DetectedService: user.DetectedService,
		KnownLanguage:   user.Language,
		KnownFacts:      facts,
		Services:        p.catalog.All(),
	})

	record := &domain.AIInteraction{
		UserID:           user.ID,
		MessageID:        messageID,
		TraceID:          in.TraceID,
		Model:            result.Model,
		InputTokens:      result.InputTokens,
		OutputTokens:     result.OutputTokens,
		Intent:           string(result.Intent),
		ServiceCode:      result.ServiceCode,
		Confidence:       result.Confidence,
		RawResponse:      result.RawResponse,
		ProcessingTimeMS: result.ProcessingTimeMS,
	}
	if err != nil {
		record.Error = err.Error()
	}
	if _, logErr := p.aiLog.Create(ctx, record); logErr != nil {
		log.Warn("store ai interaction failed", zap.Error(logErr))
	}

	if err != nil {
		p.event(ctx, domain.TraceEvent{TraceID: in.TraceID, UserID: user.ID, MessageID: messageID,
			Stage: domain.StageAIFailed, Decision: domain.DecisionError,
			Reason: "classification failed", Detail: errDetail(err),
			DurationMS: result.ProcessingTimeMS})
		log.Error("openai classification failed", zap.Error(err))
		return domain.AIClassification{}, true
	}

	p.event(ctx, domain.TraceEvent{TraceID: in.TraceID, UserID: user.ID, MessageID: messageID,
		Stage: domain.StageAICompleted, Decision: domain.DecisionOK,
		DurationMS: result.ProcessingTimeMS,
		Detail: repository.Detail(map[string]any{
			"intent":        string(result.Intent),
			"service":       result.ServiceCode,
			"confidence":    result.Confidence,
			"lead_score":    result.LeadScore,
			"language":      string(result.Language),
			"input_tokens":  result.InputTokens,
			"output_tokens": result.OutputTokens,
		})})
	return result, false
}

// history builds the trimmed conversation context. Only recent user and bot
// turns are included: no identifiers, no internal state, no other conversations.
func (p *Pipeline) history(ctx context.Context, log *zap.Logger, userID, currentMessageID int64) []domain.AIContextMessage {
	if p.cfg.ContextMessages == 0 {
		return nil
	}
	recent, err := p.messages.RecentByUser(ctx, userID, p.cfg.ContextMessages+1)
	if err != nil {
		log.Warn("load conversation history failed", zap.Error(err))
		return nil
	}

	out := make([]domain.AIContextMessage, 0, len(recent))
	for _, m := range recent {
		// The current message is passed separately as the final user turn.
		if m.ID == currentMessageID {
			continue
		}
		role := "user"
		if m.Direction == domain.DirectionOutgoing {
			role = "assistant"
		}
		out = append(out, domain.AIContextMessage{Role: role, Text: m.Content()})
	}
	if len(out) > p.cfg.ContextMessages {
		out = out[len(out)-p.cfg.ContextMessages:]
	}
	return out
}

// send delivers the reply, stores it as an outgoing message and records the
// delivery outcome.
func (p *Pipeline) send(ctx context.Context, log *zap.Logger, user *domain.User, replyTo int64, traceID, text string) error {
	outgoing := &domain.Message{
		UserID:      user.ID,
		TraceID:     traceID,
		MessageType: domain.MessageText,
		Text:        text,
		Direction:   domain.DirectionOutgoing,
		Processed:   true,
	}
	outgoingID, err := p.messages.Create(ctx, outgoing)
	if err != nil {
		log.Warn("store outgoing message failed", zap.Error(err))
	}

	if p.cfg.DryRun {
		log.Info("dry run: reply not sent", logger.Preview("text", text))
		p.recordDelivery(ctx, log, domain.Delivery{
			UserID: user.ID, MessageID: outgoingID, TraceID: traceID,
			Recipient: user.WhatsAppUserID, Kind: domain.DeliveryKindReply,
			Status: domain.DeliverySent, Attempts: 0, ProviderMessageID: "dry-run",
		})
		return nil
	}

	res, sendErr := p.wa.SendText(ctx, user.WhatsAppUserID, text)
	delivery := domain.Delivery{
		UserID: user.ID, MessageID: outgoingID, TraceID: traceID,
		Recipient: user.WhatsAppUserID, Kind: domain.DeliveryKindReply, Attempts: 1,
	}
	if sendErr != nil {
		delivery.Status = domain.DeliveryFailed
		delivery.Error = sendErr.Error()
		p.recordDelivery(ctx, log, delivery)
		p.event(ctx, domain.TraceEvent{TraceID: traceID, UserID: user.ID, MessageID: outgoingID,
			Stage: domain.StageReplyFailed, Decision: domain.DecisionError,
			Reason: "whatsapp send failed", Detail: errDetail(sendErr)})
		return sendErr
	}

	delivery.Status = domain.DeliverySent
	delivery.ProviderMessageID = res.MessageID
	p.recordDelivery(ctx, log, delivery)
	if outgoingID != 0 && res.MessageID != "" {
		if err := p.messages.SetProviderID(ctx, outgoingID, res.MessageID); err != nil {
			log.Warn("attach provider message id failed", zap.Error(err))
		}
	}
	p.event(ctx, domain.TraceEvent{TraceID: traceID, UserID: user.ID, MessageID: outgoingID,
		Stage: domain.StageReplySent, Decision: domain.DecisionOK})
	log.Info("reply sent", logger.Preview("text", text))
	return nil
}

// rememberContext stores what was learned about the customer even when no
// reply is sent, so the next message starts from better information.
func (p *Pipeline) rememberContext(ctx context.Context, log *zap.Logger, user *domain.User,
	cls domain.AIClassification, decision Decision, messageID int64, traceID string) {

	if cls.Language.Valid() {
		user.Language = cls.Language
	}
	if decision.Service != "" {
		user.DetectedService = decision.Service
	} else if cls.ServiceCode != "" {
		user.DetectedService = cls.ServiceCode
	}

	if err := p.users.UpdateQualification(ctx, user); err != nil {
		log.Warn("update user context failed", zap.Error(err))
	}

	for key, value := range cls.Facts {
		if value == "" {
			continue
		}
		if err := p.trace.SetFact(ctx, domain.UserFact{
			UserID: user.ID, Key: key, Value: value, Source: "ai", MessageID: messageID,
		}); err != nil {
			log.Warn("store fact failed", zap.String("key", key), zap.Error(err))
		}
	}
}

// advanceState moves the conversation forward and records the transition.
func (p *Pipeline) advanceState(ctx context.Context, log *zap.Logger, user *domain.User,
	decision Decision, messageID int64, traceID string) {

	if decision.NextState == "" || decision.NextState == user.CurrentState {
		return
	}
	from := user.CurrentState
	if err := p.users.SetState(ctx, user.ID, decision.NextState); err != nil {
		log.Warn("set state failed", zap.Error(err))
		return
	}
	user.CurrentState = decision.NextState

	if err := p.trace.StateTransition(ctx, domain.StateTransition{
		UserID: user.ID, TraceID: traceID, FromState: from, ToState: decision.NextState,
		Reason: decision.Reason, MessageID: messageID,
	}); err != nil {
		log.Warn("record state transition failed", zap.Error(err))
	}
	p.event(ctx, domain.TraceEvent{TraceID: traceID, UserID: user.ID, MessageID: messageID,
		Stage: domain.StageStateChanged, Decision: domain.DecisionOK,
		Detail: repository.Detail(map[string]any{"from": string(from), "to": string(decision.NextState)})})
}

// recordReplyFacts remembers what the bot has already said, so it never repeats
// the same menu, clarification or contact request.
func (p *Pipeline) recordReplyFacts(ctx context.Context, log *zap.Logger, userID int64, decision Decision, messageID int64) {
	var key string
	switch decision.Action {
	case ActionServiceMenu:
		key = domain.FactMenuSent
	case ActionClarify:
		key = domain.FactClarifyAsked
	case ActionAskContact:
		key = domain.FactContactAsked
	default:
		return
	}
	if err := p.trace.SetFact(ctx, domain.UserFact{
		UserID: userID, Key: key, Value: time.Now().UTC().Format(time.RFC3339),
		Source: "system", MessageID: messageID,
	}); err != nil {
		log.Warn("store reply fact failed", zap.String("key", key), zap.Error(err))
	}
}

// qualifyLead records or updates the lead and alerts Diana once it qualifies.
func (p *Pipeline) qualifyLead(ctx context.Context, log *zap.Logger, user *domain.User,
	cls domain.AIClassification, decision Decision, gate GateResult,
	in domain.InboundMessage, lang domain.Language, facts map[string]string) {

	service := decision.Service
	if service == "" {
		service = user.DetectedService
	}

	incoming, err := p.messages.CountIncoming(ctx, user.ID)
	if err != nil {
		log.Warn("count incoming messages failed", zap.Error(err))
	}

	// Refresh facts: the model may have added some during this turn.
	if latest, err := p.trace.Facts(ctx, user.ID); err == nil {
		facts = latest
	}

	verdict := p.qualify.Qualify(QualificationInput{
		Decision:      decision,
		AI:            cls,
		AICalled:      gate.CallAI,
		Service:       service,
		Language:      lang,
		HasPhone:      user.PhoneNumber != "",
		Facts:         facts,
		LastUserText:  in.Content(),
		IncomingCount: incoming,
	})
	if !verdict.Track {
		return
	}

	source := in.Source
	if source == "" {
		source = p.cfg.DefaultSource
	}

	lead, err := p.leads.Upsert(ctx, &domain.Lead{
		UserID:               user.ID,
		ServiceCode:          service,
		ServiceName:          p.catalog.Name(service, lang),
		Language:             lang,
		PhoneNumber:          user.PhoneNumber,
		LeadScore:            verdict.Score,
		Status:               verdict.Status,
		Source:               source,
		QualificationSummary: verdict.Summary,
	})
	if err != nil {
		log.Error("upsert lead failed", zap.Error(err))
		p.event(ctx, domain.TraceEvent{TraceID: in.TraceID, UserID: user.ID,
			Stage: domain.StagePipelineError, Decision: domain.DecisionError,
			Reason: "lead_upsert_failed", Detail: errDetail(err)})
		return
	}

	stage := domain.StageLeadUpdated
	if lead.CreatedAt.Equal(lead.UpdatedAt) {
		stage = domain.StageLeadCreated
	}
	p.event(ctx, domain.TraceEvent{TraceID: in.TraceID, UserID: user.ID,
		Stage: stage, Decision: domain.DecisionOK,
		Detail: repository.Detail(map[string]any{
			"lead_id": lead.ID, "status": string(lead.Status),
			"score": lead.LeadScore, "service": lead.ServiceCode,
		})})

	user.LeadScore = verdict.Score
	user.IsLead = verdict.Qualified
	if err := p.users.UpdateQualification(ctx, user); err != nil {
		log.Warn("update user lead fields failed", zap.Error(err))
	}

	log.Info("lead recorded",
		zap.Int64("lead_id", lead.ID),
		zap.String("status", string(lead.Status)),
		zap.Float64("score", lead.LeadScore),
		zap.String("service", lead.ServiceCode))

	// Diana is alerted exactly once per lead.
	if verdict.Qualified && lead.NotifiedAt == nil {
		p.notify(ctx, log, user, lead, in.TraceID)
	}
}

// notify sends the lead alert to Diana.
func (p *Pipeline) notify(ctx context.Context, log *zap.Logger, user *domain.User, lead *domain.Lead, traceID string) {
	recipient := p.cfg.NotifyRecipient
	if recipient == "" {
		log.Warn("no notification recipient configured, lead alert skipped",
			zap.Int64("lead_id", lead.ID))
		return
	}

	body := NotificationText(p.catalog, user, lead)
	notification := domain.Notification{
		LeadID: lead.ID, UserID: user.ID, TraceID: traceID,
		Channel: "whatsapp", Recipient: recipient, Body: body,
	}

	if p.cfg.DryRun {
		notification.Status = domain.DeliverySent
		p.recordNotification(ctx, log, notification)
		log.Info("dry run: lead notification not sent", zap.Int64("lead_id", lead.ID))
		if err := p.leads.MarkNotified(ctx, lead.ID); err != nil {
			log.Warn("mark lead notified failed", zap.Error(err))
		}
		return
	}

	res, err := p.wa.SendText(ctx, recipient, body)
	if err != nil {
		notification.Status = domain.DeliveryFailed
		notification.Error = err.Error()
		p.recordNotification(ctx, log, notification)
		p.recordDelivery(ctx, log, domain.Delivery{
			UserID: user.ID, TraceID: traceID, Recipient: recipient,
			Kind: domain.DeliveryKindNotification, Status: domain.DeliveryFailed,
			Attempts: 1, Error: err.Error(),
		})
		p.event(ctx, domain.TraceEvent{TraceID: traceID, UserID: user.ID,
			Stage: domain.StageNotifyFailed, Decision: domain.DecisionError,
			Reason: "lead alert failed", Detail: errDetail(err)})
		log.Error("lead notification failed", zap.Int64("lead_id", lead.ID), zap.Error(err))
		return
	}

	notification.Status = domain.DeliverySent
	p.recordNotification(ctx, log, notification)
	p.recordDelivery(ctx, log, domain.Delivery{
		UserID: user.ID, TraceID: traceID, Recipient: recipient,
		Kind: domain.DeliveryKindNotification, Status: domain.DeliverySent,
		Attempts: 1, ProviderMessageID: res.MessageID,
	})
	if err := p.leads.MarkNotified(ctx, lead.ID); err != nil {
		log.Warn("mark lead notified failed", zap.Error(err))
	}
	p.event(ctx, domain.TraceEvent{TraceID: traceID, UserID: user.ID,
		Stage: domain.StageNotifySent, Decision: domain.DecisionOK,
		Detail: repository.Detail(map[string]any{"lead_id": lead.ID})})
	log.Info("lead notification sent", zap.Int64("lead_id", lead.ID))
}

// ------------------------------------------------------------------ helpers

func (p *Pipeline) event(ctx context.Context, e domain.TraceEvent) {
	// Tracing must never break message processing.
	if err := p.trace.Event(ctx, e); err != nil {
		p.log.Warn("write trace event failed", zap.String("stage", e.Stage), zap.Error(err))
	}
}

func (p *Pipeline) recordDelivery(ctx context.Context, log *zap.Logger, d domain.Delivery) {
	if _, err := p.trace.Delivery(ctx, d); err != nil {
		log.Warn("record delivery failed", zap.Error(err))
	}
}

func (p *Pipeline) recordNotification(ctx context.Context, log *zap.Logger, n domain.Notification) {
	if _, err := p.trace.Notification(ctx, n); err != nil {
		log.Warn("record notification failed", zap.Error(err))
	}
}

// replyLanguage prefers the language detected in the current message, falling
// back to the one remembered from earlier turns.
func replyLanguage(detected, known domain.Language) domain.Language {
	if detected.Valid() {
		return detected
	}
	return known.OrDefault()
}

func startOfDay() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

func errDetail(err error) string {
	if err == nil {
		return ""
	}
	var msg string
	if errors.Is(err, context.DeadlineExceeded) {
		msg = "timeout: " + err.Error()
	} else {
		msg = err.Error()
	}
	return repository.Detail(map[string]any{"error": msg})
}
