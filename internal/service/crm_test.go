package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"lawyer-bot/internal/domain"
	"lawyer-bot/internal/repository"
)

// crmHarness wires the full CRM stack over a real database: the pipeline, the
// shared outbound layer, the follow-up worker and the CRM service. Only OpenAI
// and the WhatsApp provider are stubbed.
type crmHarness struct {
	*harness
	clients   *repository.CRMRepository
	jobs      *repository.FollowUpRepository
	adminRepo *repository.AdminRepository
	notes     *repository.NoteRepository
	audit     *repository.AuditRepository
	messenger *Messenger
	follow    *FollowUpService
	crm       *CRMService
	auth      *AuthService
	hub       *EventHub
}

func newCRMHarness(t *testing.T, ai *stubAI, followCfg FollowUpConfig) *crmHarness {
	t.Helper()
	base := newHarness(t, ai)

	clients := repository.NewCRMRepository(base.db)
	jobs := repository.NewFollowUpRepository(base.db)
	adminRepo := repository.NewAdminRepository(base.db)
	notes := repository.NewNoteRepository(base.db)
	audit := repository.NewAuditRepository(base.db)
	catalog := NewCatalog()
	hub := NewEventHub()

	messenger := NewMessenger(MessengerDeps{
		Messages: base.messages,
		CRM:      clients,
		Trace:    base.trace,
		WhatsApp: base.wa,
		Logger:   zap.NewNop(),
	}, MessengerConfig{})
	messenger.OnSent(hub.ClientChanged)

	follow := NewFollowUpService(FollowUpDeps{
		Jobs: jobs, CRM: clients, Messages: base.messages, Trace: base.trace,
		Settings: base.settings, Sender: messenger, Logger: zap.NewNop(),
	}, followCfg)

	auth := NewAuthService(adminRepo, audit, zap.NewNop(), AuthConfig{
		// A low work factor keeps the suite fast; production uses the default.
		PBKDF2Iterations: 120_000,
		SessionTTL:       time.Hour,
		MaxAttempts:      3,
		LockoutThreshold: 3,
		LockoutDuration:  time.Minute,
	})

	crm := NewCRMService(CRMDeps{
		Clients: clients, Messages: base.messages, Notes: notes, Audit: audit,
		Jobs: jobs, Admins: adminRepo, FollowUp: follow, Sender: messenger,
		Catalog: catalog, Logger: zap.NewNop(),
	})

	h := &crmHarness{
		harness: base, clients: clients, jobs: jobs, adminRepo: adminRepo,
		notes: notes, audit: audit, messenger: messenger, follow: follow,
		crm: crm, auth: auth, hub: hub,
	}
	// Rebuild the pipeline with the CRM collaborators attached.
	h.rebuildPipeline(t, ai, nil)
	return h
}

// rebuildPipeline wires the pipeline exactly as main.go does. A nil activation
// falls back to the deterministic legal-service triggers, which is the shipped
// default.
func (h *crmHarness) rebuildPipeline(t *testing.T, ai *stubAI, activation *Activation) {
	t.Helper()
	catalog := NewCatalog()
	triggers := NewTriggerSet()
	h.pipeline = NewPipeline(PipelineDeps{
		Users:    h.users,
		Messages: h.messages,
		Leads:    h.leads,
		AILog:    repository.NewAIInteractionRepository(h.db),
		Trace:    h.trace,
		Settings: h.settings,
		AI:       ai,
		WhatsApp: h.wa,
		Gate: NewGate(triggers, GateConfig{
			MaxCallsPerDay: 40, AnalyzeUnmatched: true, MinWordsUnmatched: 3,
		}),
		Catalog:    catalog,
		Composer:   NewComposer(catalog),
		Qualify:    NewQualifier(catalog, testMinConfidence),
		Triggers:   triggers,
		Activation: activation,
		Logger:     zap.NewNop(),
		Clients:    h.clients,
		FollowUp:   h.follow,
		Sender:     h.messenger,
		Notify:     h.hub.ClientChanged,
	}, PipelineConfig{
		MinConfidence:   testMinConfidence,
		ContextMessages: 10,
		NotifyRecipient: dianaPhone,
		DefaultSource:   domain.SourceWhatsApp,
	})
}

func testFollowUpConfig() FollowUpConfig {
	return FollowUpConfig{
		Enabled:      true,
		Delays:       []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour},
		MaxAttempts:  3,
		PollInterval: time.Hour, // the tests drive RunOnce directly
		BatchSize:    10,
		ClaimTTL:     time.Minute,
		RetryBackoff: time.Minute,
	}
}

func trademarkResult() domain.AIClassification {
	return domain.AIClassification{
		IsRelevant: true, ShouldRespond: true,
		Language: domain.LangRU, Intent: domain.IntentTrademark,
		ServiceCode: domain.ServiceTrademarkRegistration,
		Confidence:  0.95, LeadScore: 0.8,
		NeedsClarification:    true,
		ClarificationQuestion: "В Казахстане или за рубежом планируете регистрацию?",
		Summary:               "Хочет зарегистрировать товарный знак",
		QualificationStage:    domain.StageQualifying,
		LeadStatus:            string(domain.CRMNeedsQualification),
		SummaryUpdate:         "Клиент хочет зарегистрировать товарный знак на компанию",
		ImportantFacts:        []string{"ТОО «Алма»", "срок до 1 мая"},
		SuggestedFollowUp:     "уточнить страну регистрации",
	}
}

func (h *crmHarness) client(t *testing.T, waID string) *domain.CRMClient {
	t.Helper()
	c, err := h.clients.GetClientByWhatsAppID(context.Background(), waID)
	if err != nil {
		t.Fatalf("load crm client: %v", err)
	}
	return c
}

func (h *crmHarness) actor() Actor {
	return Actor{ID: 1, Name: "Диана", Role: domain.RoleConsultant}
}

// jobOutcome reads how the follow-up worker closed a job, so a test can assert
// the recorded reason and not merely the absence of a send.
func (h *crmHarness) jobOutcome(t *testing.T, jobID int64) (status, note string) {
	t.Helper()
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT status, last_error FROM follow_up_jobs WHERE id = ?`, jobID).Scan(&status, &note); err != nil {
		t.Fatalf("load follow-up job %d: %v", jobID, err)
	}
	return status, note
}

// ---------------------------------------------------------------- scenarios

// Scenarios 1, 4, 5: a new Russian message creates a client, is understood as a
// trademark request, and the CRM immediately shows the analysis.
func TestNewRussianClientIsCreatedAnalysedAndAnswered(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkResult()}}
	h := newCRMHarness(t, ai, testFollowUpConfig())
	ctx := context.Background()

	msg := inbound("wamid.new", "Здравствуйте, хочу зарегистрировать товарный знак")
	if err := h.pipeline.Handle(ctx, msg); err != nil {
		t.Fatalf("handle: %v", err)
	}

	client := h.client(t, msg.WhatsAppUserID)
	if client.PhoneNumber != "77015551234" {
		t.Fatalf("phone was not normalised: %q", client.PhoneNumber)
	}
	if client.Language != domain.LangRU {
		t.Fatalf("language must be detected as Russian, got %q", client.Language)
	}
	if client.DetectedService != domain.ServiceTrademarkRegistration {
		t.Fatalf("service must be trademark registration, got %q", client.DetectedService)
	}
	if client.AISummary == "" {
		t.Fatal("the CRM must show a short AI summary")
	}
	if len(client.ImportantFacts) == 0 {
		t.Fatal("important facts must be stored for the consultant")
	}
	if client.CRMStatus == domain.CRMNew {
		t.Fatalf("the pipeline must advance the CRM status, still %q", client.CRMStatus)
	}
	if client.QualificationStage == "" {
		t.Fatal("a qualification stage must be recorded")
	}

	// The client received exactly one reply, in Russian.
	sent := h.wa.messages()
	if len(sent) != 1 {
		t.Fatalf("expected exactly one reply, got %d", len(sent))
	}
	if strings.TrimSpace(sent[0].Text) == "" {
		t.Fatal("the reply must not be empty")
	}
}

// Scenario 3: a duplicated provider delivery is processed once.
func TestDuplicateInboundIsProcessedOnceWithCRM(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkResult()}}
	h := newCRMHarness(t, ai, testFollowUpConfig())
	ctx := context.Background()

	msg := inbound("wamid.dup", "нужен товарный знак")
	if err := h.pipeline.Handle(ctx, msg); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := h.pipeline.Handle(ctx, msg); err != nil {
		t.Fatalf("second delivery: %v", err)
	}

	if got := h.outgoingCount(t); got != 1 {
		t.Fatalf("a redelivered message must not produce a second answer, got %d", got)
	}
	client := h.client(t, msg.WhatsAppUserID)
	if client.UnreadCount != 1 {
		t.Fatalf("a duplicate must not inflate the unread badge, got %d", client.UnreadCount)
	}
}

func TestGroupInboundIsIgnoredBeforeStoreAndReply(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkResult()}}
	h := newCRMHarness(t, ai, testFollowUpConfig())
	ctx := context.Background()

	msg := inbound("green.group.1", "нужен товарный знак")
	msg.WhatsAppUserID = "120363000000000000@g.us"
	msg.PhoneNumber = "77015551234"
	msg.TraceID = "trace-group"
	if err := h.pipeline.Handle(ctx, msg); err != nil {
		t.Fatalf("handle group: %v", err)
	}

	if ai.callCount() != 0 {
		t.Fatal("a group message must not reach OpenAI")
	}
	if len(h.wa.messages()) != 0 {
		t.Fatal("a group message must not produce an outbound WhatsApp message")
	}
	var stored int
	if err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&stored); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if stored != 0 {
		t.Fatalf("a group message must not be stored as a client conversation, got %d rows", stored)
	}
	if !hasStage(h.stages(t, "trace-group"), StageWhatsAppChatGate) {
		t.Fatal("group suppression must be traced")
	}
}

func TestGlobalBotOffStoresInboundButSkipsAutomationAndAllowsManualReply(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkResult()}}
	h := newCRMHarness(t, ai, testFollowUpConfig())
	ctx := context.Background()

	if err := h.settings.SetWhatsAppBotEnabled(ctx, false, 1); err != nil {
		t.Fatalf("disable bot: %v", err)
	}

	msg := inbound("wamid.off1", "нужен товарный знак")
	msg.TraceID = "trace-bot-off"
	if err := h.pipeline.Handle(ctx, msg); err != nil {
		t.Fatalf("handle with bot off: %v", err)
	}

	client := h.client(t, msg.WhatsAppUserID)
	if client.UnreadCount != 1 {
		t.Fatalf("incoming message must still be visible in CRM, unread=%d", client.UnreadCount)
	}
	if ai.callCount() != 0 {
		t.Fatal("bot off must stop trigger/AI processing")
	}
	if len(h.wa.messages()) != 0 {
		t.Fatal("bot off must stop automatic replies")
	}
	stored, _, err := h.messages.PageByUser(ctx, client.ID, 0, 10)
	if err != nil {
		t.Fatalf("load messages: %v", err)
	}
	if len(stored) != 1 || stored[0].Direction != domain.DirectionIncoming ||
		stored[0].SenderType != domain.SenderClient {
		t.Fatalf("incoming message direction/sender wrong: %+v", stored)
	}

	manual, err := h.crm.SendMessage(ctx, h.actor(), client.ID, "Здравствуйте, я подключилась.", nil)
	if err != nil {
		t.Fatalf("manual reply must still work when bot is off: %v", err)
	}
	if manual.Direction != domain.DirectionOutgoing || manual.SenderType != domain.SenderConsultant {
		t.Fatalf("manual reply attribution wrong: %+v", manual)
	}
	if len(h.wa.messages()) != 1 {
		t.Fatal("manual reply should still be sent through WhatsApp")
	}
}

// Switching the bot back on restores automatic handling for the next message.
// Messages received while it was off are not replayed: the switch pauses the
// assistant, it does not queue work for it.
func TestAutomationResumesWhenTheBotIsSwitchedBackOn(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkResult()}}
	h := newCRMHarness(t, ai, testFollowUpConfig())
	ctx := context.Background()

	if err := h.settings.SetWhatsAppBotEnabled(ctx, false, 1); err != nil {
		t.Fatalf("disable bot: %v", err)
	}
	if err := h.pipeline.Handle(ctx, inbound("wamid.resume1", "нужен товарный знак")); err != nil {
		t.Fatalf("handle while off: %v", err)
	}
	if ai.callCount() != 0 || len(h.wa.messages()) != 0 {
		t.Fatal("nothing may be automated while the bot is off")
	}

	if err := h.settings.SetWhatsAppBotEnabled(ctx, true, 1); err != nil {
		t.Fatalf("enable bot: %v", err)
	}
	if ai.callCount() != 0 || len(h.wa.messages()) != 0 {
		t.Fatal("switching the bot on must not replay messages received while it was off")
	}

	if err := h.pipeline.Handle(ctx, inbound("wamid.resume2", "нужен товарный знак")); err != nil {
		t.Fatalf("handle after re-enabling: %v", err)
	}
	if ai.callCount() != 1 {
		t.Fatalf("the next message must be analysed again, got %d calls", ai.callCount())
	}
	if len(h.wa.messages()) != 1 {
		t.Fatalf("the next message must be answered again, got %d sends", len(h.wa.messages()))
	}
}

// A trigger the client typed with padding, line breaks and shouting still
// starts the flow: normalisation is case- and whitespace-insensitive but never
// rewrites the words themselves.
func TestPaddedAndShoutedTriggerStartsTheFlow(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"padding and case", "   НУЖЕН\n\n  Товарный   Знак  "},
		{"kazakh", "  Маған ТАУАР белгісін тіркеу керек  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ai := &stubAI{results: []domain.AIClassification{trademarkResult()}}
			h := newCRMHarness(t, ai, testFollowUpConfig())
			ctx := context.Background()

			if !NewTriggerSet().Match(tc.text).Matched {
				t.Fatalf("the deterministic filter must recognise %q", tc.text)
			}

			msg := inbound("wamid.trigger."+tc.name, tc.text)
			msg.TraceID = "trace-" + tc.name
			if err := h.pipeline.Handle(ctx, msg); err != nil {
				t.Fatalf("handle: %v", err)
			}

			if ai.callCount() != 1 {
				t.Fatalf("a valid trigger must reach the model exactly once, got %d calls", ai.callCount())
			}
			if len(h.wa.messages()) != 1 {
				t.Fatalf("a valid trigger must produce one reply, got %d", len(h.wa.messages()))
			}

			client := h.client(t, msg.WhatsAppUserID)
			if !client.CurrentState.Active() {
				t.Fatalf("the qualification flow must be initialised, state=%q", client.CurrentState)
			}

			stored, _, err := h.messages.PageByUser(ctx, client.ID, 0, 10)
			if err != nil {
				t.Fatalf("load messages: %v", err)
			}
			if len(stored) != 2 {
				t.Fatalf("the client message and the reply must both be stored, got %d", len(stored))
			}
			// The stored text keeps the client's own words, untouched.
			if stored[0].Text != tc.text {
				t.Fatalf("the incoming text must be stored verbatim, got %q", stored[0].Text)
			}
			if stored[0].Direction != domain.DirectionIncoming || stored[0].SenderType != domain.SenderClient {
				t.Fatalf("incoming attribution wrong: %s/%s", stored[0].Direction, stored[0].SenderType)
			}
			if stored[1].Direction != domain.DirectionOutgoing || stored[1].SenderType != domain.SenderAI {
				t.Fatalf("automatic reply attribution wrong: %s/%s", stored[1].Direction, stored[1].SenderType)
			}
		})
	}
}

// The shared outbound layer is the last line of defence: a group destination is
// refused before the provider client is reached and nothing is stored.
func TestOutboundLayerRefusesGroupRecipient(t *testing.T) {
	h := newCRMHarness(t, &stubAI{}, testFollowUpConfig())
	ctx := context.Background()

	_, err := h.messenger.Send(ctx, Outbound{
		Recipient: "120363000000000000@g.us",
		Sender:    domain.SenderAI,
		Text:      "Здравствуйте",
	})
	if !errors.Is(err, domain.ErrWhatsAppGroupChat) {
		t.Fatalf("the outbound layer must refuse a group destination, got %v", err)
	}
	if len(h.wa.messages()) != 0 {
		t.Fatal("nothing may reach the provider for a group destination")
	}
	if h.outgoingCount(t) != 0 {
		t.Fatal("a refused group send must not be stored as an outgoing message")
	}

	if _, err := h.messenger.SendRaw(ctx, "120363000000000000@g.us", "Здравствуйте"); !errors.Is(err, domain.ErrWhatsAppGroupChat) {
		t.Fatalf("SendRaw must refuse a group destination, got %v", err)
	}
	if len(h.wa.messages()) != 0 {
		t.Fatal("SendRaw must not reach the provider for a group destination")
	}
}

// A nudge scheduled while the bot was on must not be delivered after an
// administrator turns the bot off: the worker re-reads the switch at send time.
func TestFollowUpIsSkippedAfterTheBotIsDisabled(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkResult()}}
	cfg := testFollowUpConfig()
	cfg.Delays = []time.Duration{-time.Minute, 6 * time.Hour}
	h := newCRMHarness(t, ai, cfg)
	ctx := context.Background()

	if err := h.pipeline.Handle(ctx, inbound("wamid.fuoff1", "нужен товарный знак")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	client := h.client(t, "77015551234")
	job, err := h.jobs.NextPendingForUser(ctx, client.ID)
	if err != nil {
		t.Fatalf("a nudge should be scheduled while the bot is on: %v", err)
	}
	before := len(h.wa.messages())

	if err := h.settings.SetWhatsAppBotEnabled(ctx, false, 1); err != nil {
		t.Fatalf("disable bot: %v", err)
	}
	if err := h.follow.RunOnce(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(h.wa.messages()) != before {
		t.Fatal("a follow-up must not be sent while the bot is globally disabled")
	}
	status, note := h.jobOutcome(t, job.ID)
	if status != string(domain.FollowUpSkipped) {
		t.Fatalf("the job must be closed as skipped, got %q", status)
	}
	if !strings.Contains(note, "bot disabled") {
		t.Fatalf("the skip reason must name the global switch, got %q", note)
	}

	// Scheduling is refused too, so a disabled bot never accumulates a backlog.
	if err := h.follow.Schedule(ctx, client, 1, job.MessageID); err != nil {
		t.Fatalf("schedule while disabled: %v", err)
	}
	if _, err := h.jobs.NextPendingForUser(ctx, client.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("no nudge may be scheduled while the bot is disabled, got %v", err)
	}
}

// A legacy conversation whose WhatsApp identity is a group can never be nudged.
func TestFollowUpNeverTargetsAGroupChat(t *testing.T) {
	cfg := testFollowUpConfig()
	cfg.Delays = []time.Duration{-time.Minute}
	h := newCRMHarness(t, &stubAI{}, cfg)
	ctx := context.Background()

	group, err := h.users.Upsert(ctx, "120363000000000000@g.us", "77015551234", "Рабочий чат")
	if err != nil {
		t.Fatalf("seed group conversation: %v", err)
	}
	job, _, err := h.jobs.Schedule(ctx, domain.FollowUpJob{
		UserID: group.ID, Stage: 1, ScheduledAt: time.Now().UTC().Add(-time.Minute),
		DedupeKey: "group-follow-up-test",
	})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}

	if err := h.follow.RunOnce(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.wa.messages()) != 0 {
		t.Fatal("no scheduled message may ever be sent to a group chat")
	}
	status, note := h.jobOutcome(t, job)
	if status != string(domain.FollowUpSkipped) {
		t.Fatalf("the job must be closed as skipped, got %q", status)
	}
	if !strings.Contains(note, "non-private") {
		t.Fatalf("the skip reason must name the chat kind, got %q", note)
	}
}

// Scenario 7: once a consultant owns the conversation, the assistant is silent.
func TestAIStaysSilentDuringHumanTakeover(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkResult()}}
	h := newCRMHarness(t, ai, testFollowUpConfig())
	ctx := context.Background()

	first := inbound("wamid.h1", "нужен товарный знак")
	if err := h.pipeline.Handle(ctx, first); err != nil {
		t.Fatalf("handle: %v", err)
	}
	client := h.client(t, first.WhatsAppUserID)

	if _, err := h.crm.TakeOver(ctx, h.actor(), client.ID); err != nil {
		t.Fatalf("takeover: %v", err)
	}

	beforeCalls := ai.callCount()
	beforeSent := len(h.wa.messages())

	second := inbound("wamid.h2", "а сколько это стоит и какие документы нужны?")
	second.TraceID = "trace-takeover"
	if err := h.pipeline.Handle(ctx, second); err != nil {
		t.Fatalf("handle after takeover: %v", err)
	}

	if ai.callCount() != beforeCalls {
		t.Fatal("no token may be spent on a conversation a consultant owns")
	}
	if len(h.wa.messages()) != beforeSent {
		t.Fatal("the assistant must not answer while a consultant owns the conversation")
	}

	// The message is still stored and still raises the unread badge.
	stored, _, err := h.messages.PageByUser(ctx, client.ID, 0, 20)
	if err != nil {
		t.Fatalf("page messages: %v", err)
	}
	var found bool
	for _, m := range stored {
		if m.WhatsAppMessageID == "wamid.h2" {
			found = true
		}
	}
	if !found {
		t.Fatal("a message must be stored even when the assistant stays silent")
	}

	// The trace explains the silence.
	if !hasStage(h.stages(t, second.TraceID), StageCRMGate) {
		t.Fatal("the CRM gate decision must be traced")
	}
}

// Scenario 8: resuming lets the assistant answer again.
func TestResumeAIRestoresAutomaticReplies(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkResult()}}
	h := newCRMHarness(t, ai, testFollowUpConfig())
	ctx := context.Background()

	if err := h.pipeline.Handle(ctx, inbound("wamid.r1", "нужен товарный знак")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	client := h.client(t, "77015551234")

	if _, err := h.crm.TakeOver(ctx, h.actor(), client.ID); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if _, err := h.crm.ResumeAI(ctx, h.actor(), client.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}

	before := len(h.wa.messages())
	if err := h.pipeline.Handle(ctx, inbound("wamid.r2", "нужен договор для интернет-магазина")); err != nil {
		t.Fatalf("handle after resume: %v", err)
	}
	if len(h.wa.messages()) <= before {
		t.Fatal("the assistant must answer again after resume")
	}
}

// Scenarios 16, 18, 20: sending from the CRM takes the conversation over,
// stores the message as a consultant message and reuses the one outbound layer.
func TestConsultantMessageTakesOverAndIsStored(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkResult()}}
	h := newCRMHarness(t, ai, testFollowUpConfig())
	ctx := context.Background()

	if err := h.pipeline.Handle(ctx, inbound("wamid.c1", "нужен товарный знак")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	client := h.client(t, "77015551234")

	stored, err := h.crm.SendMessage(ctx, h.actor(), client.ID, "Здравствуйте! Меня зовут Диана.", nil)
	if err != nil {
		t.Fatalf("consultant send: %v", err)
	}
	if stored.SenderType != domain.SenderConsultant {
		t.Fatalf("a manual message must be recorded as a consultant message, got %q", stored.SenderType)
	}
	if stored.SenderAdminID != h.actor().ID {
		t.Fatalf("the author must be recorded, got %d", stored.SenderAdminID)
	}

	// It went out through the same provider client the pipeline uses.
	sent := h.wa.messages()
	if sent[len(sent)-1].Text != "Здравствуйте! Меня зовут Диана." {
		t.Fatal("the consultant message must reach WhatsApp through the shared sender")
	}

	// Sending implicitly took the conversation over.
	client = h.client(t, "77015551234")
	if client.Mode != domain.ModeHuman {
		t.Fatalf("sending from the CRM must switch to human mode, got %q", client.Mode)
	}
}

// Scenario 18: a WhatsApp failure never loses the consultant's message.
func TestConsultantMessageSurvivesProviderFailure(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkResult()}}
	h := newCRMHarness(t, ai, testFollowUpConfig())
	ctx := context.Background()

	if err := h.pipeline.Handle(ctx, inbound("wamid.f1", "нужен товарный знак")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	client := h.client(t, "77015551234")

	h.wa.err = errors.New("provider unavailable")
	stored, err := h.crm.SendMessage(ctx, h.actor(), client.ID, "Проверка связи", nil)
	if err == nil {
		t.Fatal("a provider failure must be reported to the consultant")
	}
	if stored == nil {
		t.Fatal("the message must still be stored so nothing is lost")
	}
	if stored.DeliveryStatus != domain.DeliveryFailed {
		t.Fatalf("the failure must be visible on the message, got %q", stored.DeliveryStatus)
	}
}

// Scenario 12: a blocked client is never answered automatically.
func TestBlockedClientIsNeverAnswered(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkResult()}}
	h := newCRMHarness(t, ai, testFollowUpConfig())
	ctx := context.Background()

	if err := h.pipeline.Handle(ctx, inbound("wamid.b1", "нужен товарный знак")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	client := h.client(t, "77015551234")
	if _, err := h.crm.Block(ctx, h.actor(), client.ID, "спам"); err != nil {
		t.Fatalf("block: %v", err)
	}

	before := len(h.wa.messages())
	beforeCalls := ai.callCount()
	if err := h.pipeline.Handle(ctx, inbound("wamid.b2", "нужен договор аренды срочно")); err != nil {
		t.Fatalf("handle blocked: %v", err)
	}
	if len(h.wa.messages()) != before {
		t.Fatal("a blocked client must receive nothing")
	}
	if ai.callCount() != beforeCalls {
		t.Fatal("a blocked client must not cost tokens")
	}

	// A consultant cannot send to a blocked client either.
	if _, err := h.crm.SendMessage(ctx, h.actor(), client.ID, "привет", nil); err == nil {
		t.Fatal("sending to a blocked client must be refused")
	}
}

// Scenario 13: closing a lead silences automation and stops nudges.
func TestClosingLeadStopsAutomationAndFollowUps(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkResult()}}
	h := newCRMHarness(t, ai, testFollowUpConfig())
	ctx := context.Background()

	if err := h.pipeline.Handle(ctx, inbound("wamid.k1", "нужен товарный знак")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	client := h.client(t, "77015551234")

	if _, err := h.jobs.NextPendingForUser(ctx, client.ID); err != nil {
		t.Fatalf("a follow-up should have been scheduled after the answer: %v", err)
	}

	if _, err := h.crm.SetStatus(ctx, h.actor(), client.ID, domain.CRMWon, "договор подписан"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := h.jobs.NextPendingForUser(ctx, client.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("closing must cancel every pending nudge, err=%v", err)
	}

	before := len(h.wa.messages())
	if err := h.pipeline.Handle(ctx, inbound("wamid.k2", "ещё вопрос по договору аренды")); err != nil {
		t.Fatalf("handle after close: %v", err)
	}
	if len(h.wa.messages()) != before {
		t.Fatal("a closed lead must not be answered automatically")
	}
}

// Scenario 9: a follow-up is scheduled after the assistant answers, becomes due,
// is re-validated and is sent exactly once.
func TestFollowUpIsSentOnceWhenTheClientStaysSilent(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkResult()}}
	cfg := testFollowUpConfig()
	// Due immediately, so the worker can be driven synchronously.
	cfg.Delays = []time.Duration{-time.Minute, 6 * time.Hour}
	h := newCRMHarness(t, ai, cfg)
	ctx := context.Background()

	if err := h.pipeline.Handle(ctx, inbound("wamid.fu1", "нужен товарный знак")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	before := len(h.wa.messages())

	// Two concurrent worker passes must still deliver exactly one nudge.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = h.follow.RunOnce(ctx) }()
	}
	wg.Wait()

	sent := h.wa.messages()
	if len(sent) != before+1 {
		t.Fatalf("exactly one follow-up must be sent, got %d new messages", len(sent)-before)
	}
	if !strings.Contains(sent[len(sent)-1].Text, "актуален") {
		t.Fatalf("the follow-up must be the localised first-stage text, got %q", sent[len(sent)-1].Text)
	}

	client := h.client(t, "77015551234")
	if client.CRMStatus != domain.CRMWaitingForClient {
		t.Fatalf("after a nudge the client is waiting, got %q", client.CRMStatus)
	}

	// Running again changes nothing: the job is done and the next stage is far off.
	if err := h.follow.RunOnce(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(h.wa.messages()) != before+1 {
		t.Fatal("a completed follow-up must never be re-sent")
	}
}

// Scenario 10: the follow-up is cancelled the moment the client answers.
func TestFollowUpIsCancelledWhenTheClientReplies(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkResult()}}
	h := newCRMHarness(t, ai, testFollowUpConfig())
	ctx := context.Background()

	if err := h.pipeline.Handle(ctx, inbound("wamid.fc1", "нужен товарный знак")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	client := h.client(t, "77015551234")
	stale, err := h.jobs.NextPendingForUser(ctx, client.ID)
	if err != nil {
		t.Fatalf("a nudge should be scheduled: %v", err)
	}

	// Scenario 11/12: the client answers in Kazakh; context must survive.
	ai.results = []domain.AIClassification{{
		IsRelevant: true, ShouldRespond: true,
		Language: domain.LangKK, Intent: domain.IntentTrademark,
		ServiceCode: domain.ServiceTrademarkRegistration, Confidence: 0.93,
		Summary: "Қазақстанда тіркегісі келеді", SummaryUpdate: "Клиент Қазақстанда тіркегісі келеді",
		QualificationStage: domain.StageQualified,
	}}
	if err := h.pipeline.Handle(ctx, inbound("wamid.fc2", "Қазақстанда тіркегім келеді")); err != nil {
		t.Fatalf("handle kazakh reply: %v", err)
	}

	// The nudge that was waiting for the previous message is gone. A fresh one
	// may exist, but it is anchored to the message the client just sent, so the
	// obsolete reminder can never be delivered.
	current, err := h.jobs.NextPendingForUser(ctx, client.ID)
	if err == nil && current.ID == stale.ID {
		t.Fatal("the obsolete nudge must be invalidated when the client replies")
	}
	if err == nil && current.MessageID <= stale.MessageID {
		t.Fatalf("a new nudge must anchor to the newer client message: %d vs %d",
			current.MessageID, stale.MessageID)
	}
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("next pending: %v", err)
	}

	fresh := h.client(t, "77015551234")
	if fresh.Language != domain.LangKK {
		t.Fatalf("the detected language must follow the client, got %q", fresh.Language)
	}
	if fresh.DetectedService != domain.ServiceTrademarkRegistration {
		t.Fatal("the identified service must survive the language switch")
	}
	if fresh.AISummary == "" {
		t.Fatal("the conversation summary must be preserved across turns")
	}
}

// A nudge that becomes obsolete between claiming and sending is not delivered.
func TestClaimedFollowUpIsRevalidatedBeforeSending(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkResult()}}
	cfg := testFollowUpConfig()
	cfg.Delays = []time.Duration{-time.Minute}
	h := newCRMHarness(t, ai, cfg)
	ctx := context.Background()

	if err := h.pipeline.Handle(ctx, inbound("wamid.rv1", "нужен товарный знак")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	client := h.client(t, "77015551234")

	// A consultant takes over after the job was scheduled but before it runs.
	if _, err := h.crm.TakeOver(ctx, h.actor(), client.ID); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	// Re-arm a due job directly: takeover cancelled the scheduled one, and the
	// point of this test is the check that happens after claiming.
	if _, _, err := h.jobs.Schedule(ctx, domain.FollowUpJob{
		UserID: client.ID, Stage: 1, ScheduledAt: time.Now().UTC().Add(-time.Minute),
		DedupeKey: "revalidate-test", MessageID: 0,
	}); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	before := len(h.wa.messages())
	if err := h.follow.RunOnce(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.wa.messages()) != before {
		t.Fatal("a nudge must not be sent into a conversation a consultant owns")
	}
}

// Scenario 19: two messages arriving together are serialised per client.
func TestConcurrentMessagesFromOneClientAreSerialised(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkResult()}}
	h := newCRMHarness(t, ai, testFollowUpConfig())
	ctx := context.Background()

	var wg sync.WaitGroup
	for i, text := range []string{"нужен товарный знак", "нужен договор оферты"} {
		wg.Add(1)
		go func(id int, body string) {
			defer wg.Done()
			_ = h.pipeline.Handle(ctx, inbound("wamid.par"+string(rune('a'+id)), body))
		}(i, text)
	}
	wg.Wait()

	client := h.client(t, "77015551234")
	if client.UnreadCount != 2 {
		t.Fatalf("both messages must be counted exactly once, got %d", client.UnreadCount)
	}

	stored, _, err := h.messages.PageByUser(ctx, client.ID, 0, 50)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	var inboundCount int
	for _, m := range stored {
		if m.Direction == domain.DirectionIncoming {
			inboundCount++
		}
	}
	if inboundCount != 2 {
		t.Fatalf("expected two stored inbound messages, got %d", inboundCount)
	}
}

// Scenario 17: an OpenAI outage never loses the message or sends garbage.
func TestOpenAIOutageStoresMessageAndSendsNothingWrong(t *testing.T) {
	ai := &stubAI{err: errors.New("openai unavailable")}
	h := newCRMHarness(t, ai, testFollowUpConfig())
	ctx := context.Background()

	msg := inbound("wamid.ai1", "какая сегодня погода в Астане")
	if err := h.pipeline.Handle(ctx, msg); err != nil {
		t.Fatalf("handle must not fail on an AI outage: %v", err)
	}

	client := h.client(t, msg.WhatsAppUserID)
	stored, _, err := h.messages.PageByUser(ctx, client.ID, 0, 10)
	if err != nil || len(stored) == 0 {
		t.Fatalf("the inbound message must be persisted: %d messages, err=%v", len(stored), err)
	}
	for _, m := range h.wa.messages() {
		if strings.Contains(strings.ToLower(m.Text), "error") || m.Text == "" {
			t.Fatalf("no garbage may reach the client, got %q", m.Text)
		}
	}
}

// The state machine refuses illegal transitions suggested by the model.
func TestStateMachineRejectsIllegalModelSuggestions(t *testing.T) {
	// A model cannot mark a lead won.
	if got := NextStatus(domain.CRMNeedsQualification, domain.CRMWon); got != domain.CRMNeedsQualification {
		t.Fatalf("the model must not be able to win a lead, got %q", got)
	}
	// A model cannot take a conversation away from a consultant.
	if got := NextStatus(domain.CRMConsultantProcessing, domain.CRMAIProcessing); got != domain.CRMConsultantProcessing {
		t.Fatalf("the model must not reclaim a human-owned conversation, got %q", got)
	}
	// A closed lead is not silently reopened.
	if got := NextStatus(domain.CRMClosed, domain.CRMQualified); got != domain.CRMClosed {
		t.Fatalf("a closed lead must stay closed, got %q", got)
	}
	// An unknown value is ignored rather than stored.
	if got := NextStatus(domain.CRMNew, domain.CRMStatus("hacked")); got != domain.CRMNew {
		t.Fatalf("an unknown status must be discarded, got %q", got)
	}
	// Escalation is always permitted.
	if got := NextStage(domain.StageQualified, domain.StageConsultantReq); got != domain.StageConsultantReq {
		t.Fatalf("escalation must always be allowed, got %q", got)
	}
	// Stages never move backwards on their own.
	if got := NextStage(domain.StageQualified, domain.StageIntent); got != domain.StageQualified {
		t.Fatalf("a stage must not regress, got %q", got)
	}
}

// The CRM gate is the single interlock; it must cover every stop condition.
func TestCRMGateCoversEveryStopCondition(t *testing.T) {
	cases := []struct {
		name   string
		client domain.CRMClient
		want   string
	}{
		{"blocked", domain.CRMClient{Blocked: true, AIEnabled: true, Mode: domain.ModeAI}, CRMReasonBlocked},
		{"human", domain.CRMClient{AIEnabled: true, Mode: domain.ModeHuman}, CRMReasonHumanMode},
		{"paused", domain.CRMClient{AIEnabled: true, Mode: domain.ModePaused}, CRMReasonPaused},
		{"ai off", domain.CRMClient{AIEnabled: false, Mode: domain.ModeAI}, CRMReasonAIOff},
		{"closed", domain.CRMClient{AIEnabled: true, Mode: domain.ModeAI, CRMStatus: domain.CRMLost}, CRMReasonClosed},
		{"allowed", domain.CRMClient{AIEnabled: true, Mode: domain.ModeAI, CRMStatus: domain.CRMNew}, CRMReasonOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, allowed := crmGate(&tc.client)
			if reason != tc.want {
				t.Fatalf("expected reason %q, got %q", tc.want, reason)
			}
			if allowed != (tc.want == CRMReasonOK) {
				t.Fatalf("allowed=%v for %q", allowed, tc.want)
			}
		})
	}
}

// Critical business facts must survive summary compaction.
func TestSummaryMergeKeepsCriticalFacts(t *testing.T) {
	p := &Pipeline{}
	client := &domain.CRMClient{
		AISummary: "Старое резюме",
		ImportantFacts: []string{
			"срок подачи до 1 мая",
			"консультация назначена на четверг",
			"любит синий цвет",
		},
	}
	summary, facts := p.mergeSummary(client, domain.AIClassification{
		SummaryUpdate:  "Клиент уточняет детали регистрации",
		ImportantFacts: []string{"договор уже подписан"},
	})

	if summary != "Клиент уточняет детали регистрации" {
		t.Fatalf("the newer summary must win, got %q", summary)
	}
	joined := strings.Join(facts, "|")
	for _, must := range []string{"срок подачи до 1 мая", "консультация назначена на четверг", "договор уже подписан"} {
		if !strings.Contains(joined, must) {
			t.Fatalf("critical fact %q was summarised away: %v", must, facts)
		}
	}
}

// Token strategy: the prompt carries the durable summary, not the whole history.
func TestModelReceivesCompactStateInsteadOfFullHistory(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkResult()}}
	h := newCRMHarness(t, ai, testFollowUpConfig())
	ctx := context.Background()

	if err := h.pipeline.Handle(ctx, inbound("wamid.t1", "нужен товарный знак")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if err := h.pipeline.Handle(ctx, inbound("wamid.t2", "и ещё нужен договор оферты")); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if len(ai.inputs) < 2 {
		t.Fatalf("expected at least two classifications, got %d", len(ai.inputs))
	}
	second := ai.inputs[len(ai.inputs)-1]
	if second.Summary == "" {
		t.Fatal("the second call must carry the rolling summary")
	}
	if len(second.ImportantFacts) == 0 {
		t.Fatal("the second call must carry the established facts")
	}
	if second.CRMStatus == "" || second.QualificationStage == "" {
		t.Fatal("the compact client state must reach the model")
	}
	if len(second.History) > 10 {
		t.Fatalf("history must stay a bounded window, got %d turns", len(second.History))
	}
}

// ------------------------------------------------------------------- media

// Scenario 16: media validation refuses anything that could execute.
func TestMediaValidationRejectsDangerousTypes(t *testing.T) {
	store, err := NewMediaStore(MediaConfig{Root: filepath.Join(t.TempDir(), "media"), MaxBytes: 1024}, nil)
	if err != nil {
		t.Fatalf("new media store: %v", err)
	}

	for _, bad := range []string{"image/svg+xml", "text/html", "application/x-sh", "application/javascript", ""} {
		if _, _, err := ValidateMime(bad); !errors.Is(err, ErrUnsupportedMedia) {
			t.Fatalf("%q must be rejected, err=%v", bad, err)
		}
	}
	for _, good := range []string{"image/jpeg", "application/pdf", "audio/ogg", "video/mp4"} {
		if _, _, err := ValidateMime(good); err != nil {
			t.Fatalf("%q must be accepted: %v", good, err)
		}
	}

	// A stored file gets a generated name and never keeps the client's own.
	stored, err := store.Save(bytes.NewReader([]byte("%PDF-1.4 test")), "application/pdf", "../../../etc/passwd.pdf", false)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if strings.Contains(stored.StoredName, "passwd") || strings.Contains(stored.StoredName, "..") {
		t.Fatalf("the stored name must be server-generated, got %q", stored.StoredName)
	}
	if !strings.HasPrefix(stored.Path, store.Root()) {
		t.Fatalf("the file escaped the media root: %q", stored.Path)
	}
	if info, err := os.Stat(stored.Path); err != nil {
		t.Fatalf("stat: %v", err)
	} else if info.Mode().Perm()&0o111 != 0 {
		t.Fatalf("stored media must never be executable, mode=%v", info.Mode())
	}

	// Over-sized uploads are refused rather than filling the disk.
	if _, err := store.Save(bytes.NewReader(make([]byte, 4096)), "application/pdf", "big.pdf", false); !errors.Is(err, ErrMediaTooLarge) {
		t.Fatalf("an over-sized upload must be rejected, err=%v", err)
	}
}

// Path traversal is impossible through a stored media reference.
func TestMediaResolveRefusesTraversal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "media")
	store, err := NewMediaStore(MediaConfig{Root: root}, nil)
	if err != nil {
		t.Fatalf("new media store: %v", err)
	}
	for _, attempt := range []string{"../../etc/passwd", "/etc/passwd", "a/../../../../etc/shadow", ""} {
		if _, err := store.Resolve(attempt); err == nil {
			t.Fatalf("%q must not resolve", attempt)
		}
	}
}

// -------------------------------------------------------------------- auth

// Scenario 14: authentication works, and scenario 15: it is enforced.
func TestAuthenticationLifecycle(t *testing.T) {
	h := newCRMHarness(t, &stubAI{}, testFollowUpConfig())
	ctx := context.Background()

	created, err := h.auth.EnsureBootstrapAdmin(ctx, "admin@lawyer.kz", "Str0ngPassword", "Диана")
	if err != nil || !created {
		t.Fatalf("bootstrap: created=%v err=%v", created, err)
	}
	// Running again must not reset anything.
	again, err := h.auth.EnsureBootstrapAdmin(ctx, "admin@lawyer.kz", "Different1234", "X")
	if err != nil || again {
		t.Fatalf("bootstrap must be a no-op once an account exists: created=%v err=%v", again, err)
	}

	session, err := h.auth.Login(ctx, "admin@lawyer.kz", "Str0ngPassword", "key-ok")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if session.Token == "" || session.CSRFToken == "" {
		t.Fatal("a session must carry a token and a CSRF token")
	}

	// The database stores only the digest of the token.
	if _, err := h.adminRepo.GetSession(ctx, session.Token); !errors.Is(err, repository.ErrNotFound) {
		t.Fatal("the raw session token must never be stored")
	}
	if _, err := h.adminRepo.GetSession(ctx, HashToken(session.Token)); err != nil {
		t.Fatalf("the session digest must be stored: %v", err)
	}

	admin, _, err := h.auth.Authenticate(ctx, session.Token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if admin.Email != "admin@lawyer.kz" || admin.Role != domain.RoleAdmin {
		t.Fatalf("unexpected account: %+v", admin)
	}

	// A wrong password never authenticates.
	if _, err := h.auth.Login(ctx, "admin@lawyer.kz", "wrong-password", "key-bad"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("a wrong password must be refused, err=%v", err)
	}
	// An unknown account reports the same error, revealing nothing.
	if _, err := h.auth.Login(ctx, "nobody@lawyer.kz", "whatever123", "key-bad2"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("an unknown account must look identical, err=%v", err)
	}

	// Logging out invalidates the session immediately.
	if err := h.auth.Logout(ctx, session.Token, admin.ID, "key-ok"); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, _, err := h.auth.Authenticate(ctx, session.Token); err == nil {
		t.Fatal("a logged-out session must not authenticate")
	}
}

// Login is rate limited per caller, and the limit survives a restart because it
// lives in the database.
func TestLoginIsRateLimited(t *testing.T) {
	h := newCRMHarness(t, &stubAI{}, testFollowUpConfig())
	ctx := context.Background()

	if _, err := h.auth.EnsureBootstrapAdmin(ctx, "admin@lawyer.kz", "Str0ngPassword", "Admin"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	var limited bool
	for i := 0; i < 6; i++ {
		_, err := h.auth.Login(ctx, "admin@lawyer.kz", "bad-password", "attacker")
		if errors.Is(err, ErrRateLimited) {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("repeated failures from one caller must be rate limited")
	}
	// A different caller is unaffected by the attacker's budget.
	if _, err := h.auth.Login(ctx, "admin@lawyer.kz", "Str0ngPassword", "innocent"); err != nil &&
		!errors.Is(err, ErrAccountLocked) {
		t.Fatalf("an unrelated caller must not be blocked outright: %v", err)
	}
}

// Scenario 15: a read-only role cannot change anything.
func TestReadOnlyRoleCannotWrite(t *testing.T) {
	h := newCRMHarness(t, &stubAI{results: []domain.AIClassification{trademarkResult()}}, testFollowUpConfig())
	ctx := context.Background()

	if err := h.pipeline.Handle(ctx, inbound("wamid.ro", "нужен товарный знак")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	client := h.client(t, "77015551234")
	readonly := Actor{ID: 9, Name: "Аудитор", Role: domain.RoleReadOnly}

	if _, err := h.crm.TakeOver(ctx, readonly, client.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-only must not take over, err=%v", err)
	}
	if _, err := h.crm.SendMessage(ctx, readonly, client.ID, "привет", nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-only must not send, err=%v", err)
	}
	if _, err := h.crm.Block(ctx, readonly, client.ID, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-only must not block, err=%v", err)
	}
}

// Password policy is enforced everywhere a password is set.
func TestPasswordPolicy(t *testing.T) {
	for _, weak := range []string{"", "short", "alllettersonly", "1234567890"} {
		if err := ValidatePassword(weak); !errors.Is(err, ErrWeakPassword) {
			t.Fatalf("%q must be rejected, err=%v", weak, err)
		}
	}
	if err := ValidatePassword("Str0ngPassword"); err != nil {
		t.Fatalf("a strong password must be accepted: %v", err)
	}
}

// Every consultant action leaves an audit entry, and none of them log secrets.
func TestConsultantActionsAreAudited(t *testing.T) {
	h := newCRMHarness(t, &stubAI{results: []domain.AIClassification{trademarkResult()}}, testFollowUpConfig())
	ctx := context.Background()

	if err := h.pipeline.Handle(ctx, inbound("wamid.au", "нужен товарный знак")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	client := h.client(t, "77015551234")

	if _, err := h.crm.TakeOver(ctx, h.actor(), client.ID); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if _, err := h.crm.Block(ctx, h.actor(), client.ID, "спам"); err != nil {
		t.Fatalf("block: %v", err)
	}

	entries, total, err := h.audit.List(ctx, "client", client.ID, 50, 0)
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	if total < 2 {
		t.Fatalf("expected the actions to be audited, got %d entries", total)
	}
	var sawTakeover, sawBlock bool
	for _, e := range entries {
		switch e.Action {
		case domain.AuditTakeover:
			sawTakeover = true
		case domain.AuditBlock:
			sawBlock = true
		}
		if strings.Contains(strings.ToLower(e.Detail), "password") ||
			strings.Contains(strings.ToLower(e.Detail), "token") {
			t.Fatalf("audit details must never carry secrets: %q", e.Detail)
		}
	}
	if !sawTakeover || !sawBlock {
		t.Fatalf("takeover=%v block=%v were not audited", sawTakeover, sawBlock)
	}
}

// Business hours defer a nudge instead of waking a client at night.
func TestBusinessHoursDeferNightFollowUps(t *testing.T) {
	loc := time.FixedZone("test", 5*3600)
	cfg := FollowUpConfig{
		Enabled: true, Delays: []time.Duration{time.Hour},
		BusinessHoursOnly: true, BusinessStartHour: 9, BusinessEndHour: 21, Location: loc,
	}.Normalise()

	night := time.Date(2026, 3, 10, 3, 0, 0, 0, loc)
	moved := cfg.NextSendTime(night).In(loc)
	if moved.Hour() != 9 {
		t.Fatalf("a 03:00 nudge must move to 09:00, got %v", moved)
	}

	late := time.Date(2026, 3, 10, 23, 30, 0, 0, loc)
	movedLate := cfg.NextSendTime(late).In(loc)
	if movedLate.Hour() != 9 || movedLate.Day() != 11 {
		t.Fatalf("a 23:30 nudge must move to 09:00 next day, got %v", movedLate)
	}

	midday := time.Date(2026, 3, 10, 14, 0, 0, 0, loc)
	if got := cfg.NextSendTime(midday); !got.Equal(midday) {
		t.Fatalf("a nudge inside business hours must not move, got %v", got)
	}
}

// The export never carries an internal or secret field.
func TestExportContainsBusinessFieldsOnly(t *testing.T) {
	h := newCRMHarness(t, &stubAI{results: []domain.AIClassification{trademarkResult()}}, testFollowUpConfig())
	ctx := context.Background()

	if err := h.pipeline.Handle(ctx, inbound("wamid.ex", "нужен товарный знак")); err != nil {
		t.Fatalf("handle: %v", err)
	}

	export := NewExportService(h.clients, NewCatalog())
	rows, err := export.Rows(ctx, repository.ClientFilter{}, domain.LangKK)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("expected a header and at least one client, got %d rows", len(rows))
	}
	// Kazakh headers, as the CRM requires for reports.
	if rows[0][0] != "Клиент" || rows[0][2] != "Тіл" {
		t.Fatalf("export headers must be localised: %v", rows[0])
	}

	flat := strings.ToLower(strings.Join(rows[1], " "))
	for _, forbidden := range []string{"password", "hash", "token", "sk-", "api_key"} {
		if strings.Contains(flat, forbidden) {
			t.Fatalf("the export leaked %q: %v", forbidden, rows[1])
		}
	}

	// Both formats render without error.
	var csvBuf, xlsxBuf bytes.Buffer
	if err := WriteCSV(&csvBuf, rows); err != nil {
		t.Fatalf("csv: %v", err)
	}
	if err := WriteXLSX(&xlsxBuf, rows, "CRM"); err != nil {
		t.Fatalf("xlsx: %v", err)
	}
	if !bytes.HasPrefix(csvBuf.Bytes(), []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatal("the CSV must start with a BOM so Excel reads Cyrillic correctly")
	}
	if !bytes.HasPrefix(xlsxBuf.Bytes(), []byte("PK")) {
		t.Fatal("the XLSX must be a zip container")
	}
}

// The live hub notifies only the subscriber that asked for that client.
func TestEventHubScopesBySubscriber(t *testing.T) {
	hub := NewEventHub()
	all, cancelAll := hub.Subscribe(0)
	defer cancelAll()
	one, cancelOne := hub.Subscribe(42)
	defer cancelOne()

	hub.ClientChanged(42)
	select {
	case e := <-one:
		if e.ClientID != 42 {
			t.Fatalf("wrong client: %d", e.ClientID)
		}
	case <-time.After(time.Second):
		t.Fatal("the scoped subscriber did not receive its event")
	}
	select {
	case <-all:
	case <-time.After(time.Second):
		t.Fatal("the global subscriber did not receive the event")
	}

	hub.ClientChanged(7)
	select {
	case e := <-one:
		t.Fatalf("a scoped subscriber must not receive другого client's event: %d", e.ClientID)
	case <-time.After(50 * time.Millisecond):
	}
}
