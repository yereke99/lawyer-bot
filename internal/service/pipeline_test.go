package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"lawyer-bot/internal/domain"
	"lawyer-bot/internal/repository"
)

// ------------------------------------------------------------------- mocks

// stubAI stands in for OpenAI. Tests never touch the real API.
type stubAI struct {
	mu      sync.Mutex
	results []domain.AIClassification
	err     error
	calls   int
	inputs  []domain.AIInput
}

func (s *stubAI) ClassifyMessage(_ context.Context, in domain.AIInput) (domain.AIClassification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.inputs = append(s.inputs, in)
	idx := s.calls
	s.calls++

	if s.err != nil {
		return domain.AIClassification{Model: "stub"}, s.err
	}
	if idx >= len(s.results) {
		if len(s.results) == 0 {
			return domain.AIClassification{Model: "stub"}, nil
		}
		idx = len(s.results) - 1
	}
	out := s.results[idx]
	out.Model = "stub"
	out.InputTokens = 120
	out.OutputTokens = 30
	out.RawResponse = `{"stub":true}`
	return out, nil
}

func (s *stubAI) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// stubWhatsApp records outbound messages instead of sending them.
type stubWhatsApp struct {
	mu   sync.Mutex
	sent []sentMessage
	err  error
}

type sentMessage struct {
	To     string
	Text   string
	SentAt time.Time
}

func (s *stubWhatsApp) SendText(_ context.Context, recipient, text string) (domain.SendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return domain.SendResult{}, s.err
	}
	s.sent = append(s.sent, sentMessage{To: recipient, Text: text, SentAt: time.Now()})
	return domain.SendResult{MessageID: "wamid.out." + recipient}, nil
}

func (s *stubWhatsApp) SendMedia(_ context.Context, recipient, mediaID, caption string) (domain.SendResult, error) {
	return s.SendText(context.Background(), recipient, caption)
}

func (s *stubWhatsApp) messages() []sentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sentMessage, len(s.sent))
	copy(out, s.sent)
	return out
}

// ------------------------------------------------------------------ harness

type harness struct {
	pipeline *Pipeline
	ai       *stubAI
	wa       *stubWhatsApp
	users    *repository.UserRepository
	messages *repository.MessageRepository
	leads    *repository.LeadRepository
	trace    *repository.TraceRepository
	db       *repository.DB
}

const dianaPhone = "77009998877"

func newHarness(t *testing.T, ai *stubAI) *harness {
	t.Helper()

	db, err := repository.Open(context.Background(), filepath.Join(t.TempDir(), "pipeline.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	users := repository.NewUserRepository(db)
	messages := repository.NewMessageRepository(db)
	leads := repository.NewLeadRepository(db)
	aiLog := repository.NewAIInteractionRepository(db)
	trace := repository.NewTraceRepository(db)

	catalog := NewCatalog()
	triggers := NewTriggerSet()
	wa := &stubWhatsApp{}

	pipeline := NewPipeline(PipelineDeps{
		Users:    users,
		Messages: messages,
		Leads:    leads,
		AILog:    aiLog,
		Trace:    trace,
		AI:       ai,
		WhatsApp: wa,
		Gate: NewGate(triggers, GateConfig{
			MaxCallsPerDay:    40,
			AnalyzeUnmatched:  true,
			MinWordsUnmatched: 3,
		}),
		Catalog:  catalog,
		Composer: NewComposer(catalog),
		Qualify:  NewQualifier(catalog, testMinConfidence),
		Triggers: triggers,
		Logger:   zap.NewNop(),
	}, PipelineConfig{
		MinConfidence:   testMinConfidence,
		ContextMessages: 10,
		NotifyRecipient: dianaPhone,
		DefaultSource:   domain.SourceWhatsApp,
	})

	return &harness{
		pipeline: pipeline, ai: ai, wa: wa,
		users: users, messages: messages, leads: leads, trace: trace, db: db,
	}
}

func inbound(id, text string) domain.InboundMessage {
	return domain.InboundMessage{
		WhatsAppUserID:    "77015551234",
		PhoneNumber:       "77015551234",
		DisplayName:       "Аида",
		WhatsAppMessageID: id,
		MessageType:       domain.MessageText,
		Text:              text,
		Timestamp:         time.Now().UTC(),
		Source:            domain.SourceWhatsApp,
	}
}

func inboundFor(userID, id, text string) domain.InboundMessage {
	msg := inbound(id, text)
	msg.WhatsAppUserID = userID
	msg.PhoneNumber = userID
	return msg
}

func (h *harness) stages(t *testing.T, traceID string) []string {
	t.Helper()
	events, err := h.trace.ByTraceID(context.Background(), traceID)
	if err != nil {
		t.Fatalf("load trace: %v", err)
	}
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Stage)
	}
	return out
}

func hasStage(stages []string, want string) bool {
	for _, s := range stages {
		if s == want {
			return true
		}
	}
	return false
}

func waitUntil(t *testing.T, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

func (h *harness) outgoingCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM messages WHERE direction = ?`,
		string(domain.DirectionOutgoing)).Scan(&n); err != nil {
		t.Fatalf("count outgoing messages: %v", err)
	}
	return n
}

// --------------------------------------------------------------------- tests

// The headline requirement: a greeting is stored and traced but costs nothing
// and gets no reply.
func TestGreetingIsStoredButNeitherAnalysedNorAnswered(t *testing.T) {
	h := newHarness(t, &stubAI{})

	msg := inbound("wamid.greet", "Здравствуйте")
	msg.TraceID = "trace-greet"
	if err := h.pipeline.Handle(context.Background(), msg); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if h.ai.callCount() != 0 {
		t.Fatalf("a greeting must not reach OpenAI, got %d call(s)", h.ai.callCount())
	}
	if got := h.wa.messages(); len(got) != 0 {
		t.Fatalf("a greeting must not be answered, got %d message(s): %+v", len(got), got)
	}

	// It is still fully recorded.
	user, err := h.users.GetByWhatsAppID(context.Background(), "77015551234")
	if err != nil {
		t.Fatalf("user should exist: %v", err)
	}
	count, err := h.messages.CountIncoming(context.Background(), user.ID)
	if err != nil || count != 1 {
		t.Fatalf("incoming message should be stored, count=%d err=%v", count, err)
	}

	stages := h.stages(t, "trace-greet")
	for _, want := range []string{domain.StageMessageStored, domain.StageGate, domain.StagePipelineDone} {
		if !hasStage(stages, want) {
			t.Errorf("trace is missing stage %q, got %v", want, stages)
		}
	}
	if hasStage(stages, domain.StageAIRequested) {
		t.Errorf("trace should show no model call, got %v", stages)
	}
}

func TestSmallTalkNeverReachesTheModel(t *testing.T) {
	h := newHarness(t, &stubAI{})

	for i, text := range []string{"Как дела?", "Какая погода?", "Спасибо", "Привет"} {
		msg := inbound("wamid.chat."+string(rune('a'+i)), text)
		if err := h.pipeline.Handle(context.Background(), msg); err != nil {
			t.Fatalf("handle %q: %v", text, err)
		}
	}

	if h.ai.callCount() != 0 {
		t.Fatalf("small talk must cost zero tokens, got %d model call(s)", h.ai.callCount())
	}
	if got := h.wa.messages(); len(got) != 0 {
		t.Fatalf("small talk must not be answered, got %+v", got)
	}
}

func TestServiceInquiryReceivesTheMenu(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{{
		IsRelevant: true, ShouldRespond: true,
		Language: domain.LangRU, Intent: domain.IntentServiceInquiry,
		Confidence: 0.96, LeadScore: 0.5,
	}}}
	h := newHarness(t, ai)

	msg := inbound("wamid.inquiry", "Какие услуги вы оказываете?")
	msg.TraceID = "trace-inquiry"
	if err := h.pipeline.Handle(context.Background(), msg); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if h.ai.callCount() != 1 {
		t.Fatalf("want exactly 1 model call, got %d", h.ai.callCount())
	}
	sent := h.wa.messages()
	if len(sent) != 1 {
		t.Fatalf("want 1 reply, got %d: %+v", len(sent), sent)
	}
	if !strings.Contains(sent[0].Text, "Товарный знак") {
		t.Fatalf("expected the service menu, got:\n%s", sent[0].Text)
	}
	if sent[0].To != "77015551234" {
		t.Fatalf("reply went to %q, want the customer", sent[0].To)
	}

	user, _ := h.users.GetByWhatsAppID(context.Background(), "77015551234")
	if user.CurrentState != domain.StateWaitingIntent {
		t.Fatalf("state = %q, want %q", user.CurrentState, domain.StateWaitingIntent)
	}

	stages := h.stages(t, "trace-inquiry")
	for _, want := range []string{
		domain.StageMessageStored, domain.StageGate, domain.StageAIRequested,
		domain.StageAICompleted, domain.StageDecision, domain.StageReplyBuilt,
		domain.StageReplySent, domain.StageStateChanged,
	} {
		if !hasStage(stages, want) {
			t.Errorf("trace is missing stage %q, got %v", want, stages)
		}
	}
}

// The complete qualification journey from the specification.
func TestFullQualificationFlowNotifiesDiana(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{
		{
			IsRelevant: true, ShouldRespond: true,
			Language: domain.LangRU, Intent: domain.IntentPrivacyPolicy,
			ServiceCode: domain.ServicePrivacyPolicy, Confidence: 0.98,
			NeedsClarification:    true,
			ClarificationQuestion: "Приложение уже запущено или находится в разработке?",
			LeadScore:             0.8,
			Facts:                 map[string]string{domain.FactPlatform: "mobile_app"},
			Summary:               "Нужна политика конфиденциальности для мобильного приложения",
		},
		{
			IsRelevant: true, ShouldRespond: true,
			Language: domain.LangRU, Intent: domain.IntentPrivacyPolicy,
			ServiceCode: domain.ServicePrivacyPolicy, Confidence: 0.95,
			NeedsClarification: false, LeadScore: 0.9,
			Facts:   map[string]string{domain.FactAppStatus: "launched"},
			Summary: "Политика конфиденциальности для запущенного мобильного приложения",
		},
	}}
	h := newHarness(t, ai)
	ctx := context.Background()

	// Turn 1: the customer states the need.
	first := inbound("wamid.1", "Мне нужна политика конфиденциальности для мобильного приложения")
	if err := h.pipeline.Handle(ctx, first); err != nil {
		t.Fatalf("handle first: %v", err)
	}

	sent := h.wa.messages()
	if len(sent) != 1 {
		t.Fatalf("turn 1: want 1 reply, got %d: %+v", len(sent), sent)
	}
	if !strings.Contains(sent[0].Text, "запущено") {
		t.Fatalf("turn 1 should ask the clarification question, got:\n%s", sent[0].Text)
	}

	// Turn 2: the customer answers.
	second := inbound("wamid.2", "Уже работает")
	if err := h.pipeline.Handle(ctx, second); err != nil {
		t.Fatalf("handle second: %v", err)
	}

	sent = h.wa.messages()
	if len(sent) != 3 {
		t.Fatalf("want reply + reply + notification = 3 messages, got %d: %+v", len(sent), sent)
	}

	reply := sent[1]
	if !strings.Contains(reply.Text, "Стоимость зависит от сложности") {
		t.Fatalf("turn 2 must state that cost depends on complexity, got:\n%s", reply.Text)
	}
	if !strings.Contains(reply.Text, "Диане") {
		t.Fatalf("turn 2 must offer the handoff, got:\n%s", reply.Text)
	}

	notification := sent[2]
	if notification.To != dianaPhone {
		t.Fatalf("notification went to %q, want Diana at %q", notification.To, dianaPhone)
	}
	for _, want := range []string{"🆕", "Аида", "+77015551234", "Политика конфиденциальности"} {
		if !strings.Contains(notification.Text, want) {
			t.Errorf("notification is missing %q:\n%s", want, notification.Text)
		}
	}

	// Lead state.
	user, _ := h.users.GetByWhatsAppID(ctx, "77015551234")
	if user.CurrentState != domain.StateReadyForDiana {
		t.Fatalf("state = %q, want %q", user.CurrentState, domain.StateReadyForDiana)
	}
	if !user.IsLead {
		t.Fatal("user should be marked as a lead")
	}

	lead, err := h.leads.GetOpenByUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("lead should exist: %v", err)
	}
	if lead.Status != domain.LeadQualified {
		t.Fatalf("lead status = %q, want %q", lead.Status, domain.LeadQualified)
	}
	if lead.ServiceCode != domain.ServicePrivacyPolicy {
		t.Fatalf("lead service = %q, want %q", lead.ServiceCode, domain.ServicePrivacyPolicy)
	}
	if lead.NotifiedAt == nil {
		t.Fatal("lead should be marked as notified")
	}
	if lead.PhoneNumber != "77015551234" {
		t.Fatalf("lead phone = %q, want the WhatsApp number", lead.PhoneNumber)
	}

	// The facts extracted along the way are persisted.
	facts, _ := h.trace.Facts(ctx, user.ID)
	if facts[domain.FactPlatform] != "mobile_app" {
		t.Errorf("platform fact = %q, want mobile_app", facts[domain.FactPlatform])
	}
	if facts[domain.FactAppStatus] != "launched" {
		t.Errorf("app status fact = %q, want launched", facts[domain.FactAppStatus])
	}
}

func TestDianaIsNotifiedOnlyOnce(t *testing.T) {
	qualified := domain.AIClassification{
		IsRelevant: true, ShouldRespond: true,
		Language: domain.LangRU, Intent: domain.IntentTrademark,
		ServiceCode: domain.ServiceTrademarkRegistration, Confidence: 0.97,
		LeadScore: 0.9, Summary: "Регистрация товарного знака",
	}
	h := newHarness(t, &stubAI{results: []domain.AIClassification{qualified, qualified, qualified}})
	ctx := context.Background()

	for _, id := range []string{"wamid.a", "wamid.b", "wamid.c"} {
		if err := h.pipeline.Handle(ctx, inbound(id, "Мне нужно зарегистрировать товарный знак")); err != nil {
			t.Fatalf("handle %s: %v", id, err)
		}
	}

	notifications := 0
	for _, m := range h.wa.messages() {
		if m.To == dianaPhone {
			notifications++
		}
	}
	if notifications != 1 {
		t.Fatalf("Diana should be alerted exactly once per lead, got %d alerts", notifications)
	}
}

func TestDuplicateWebhookDeliveryIsProcessedOnce(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{{
		IsRelevant: true, ShouldRespond: true,
		Language: domain.LangRU, Intent: domain.IntentServiceInquiry, Confidence: 0.95,
	}}}
	h := newHarness(t, ai)
	ctx := context.Background()

	msg := inbound("wamid.dup", "Какие у вас услуги?")
	if err := h.pipeline.Handle(ctx, msg); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := h.pipeline.Handle(ctx, msg); err != nil {
		t.Fatalf("retry: %v", err)
	}

	if h.ai.callCount() != 1 {
		t.Fatalf("a retried webhook must not be classified twice, got %d calls", h.ai.callCount())
	}
	if got := h.wa.messages(); len(got) != 1 {
		t.Fatalf("a retried webhook must not be answered twice, got %d replies", len(got))
	}
}

func TestStateIsPersistedBeforeDelayedReplySend(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{{
		IsRelevant: true, ShouldRespond: true,
		Language: domain.LangRU, Intent: domain.IntentServiceInquiry, Confidence: 0.95,
	}}}
	h := newHarness(t, ai)
	h.pipeline.cfg.ReplyDelayMin = 80 * time.Millisecond
	h.pipeline.cfg.ReplyDelayMax = 120 * time.Millisecond

	errCh := make(chan error, 1)
	msg := inbound("wamid.delay.state", "Какие у вас услуги?")
	msg.TraceID = "trace-delay-state"
	go func() {
		errCh <- h.pipeline.Handle(context.Background(), msg)
	}()

	waitUntil(t, time.Second, func() bool {
		return h.outgoingCount(t) == 1
	})

	user, err := h.users.GetByWhatsAppID(context.Background(), "77015551234")
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	if user.CurrentState != domain.StateWaitingIntent {
		t.Fatalf("state = %q, want %q before send", user.CurrentState, domain.StateWaitingIntent)
	}
	if got := h.wa.messages(); len(got) != 0 {
		t.Fatalf("reply was sent before the delay finished: %+v", got)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("handle: %v", err)
	}
	sent := h.wa.messages()
	if len(sent) != 1 {
		t.Fatalf("want 1 delayed reply, got %d: %+v", len(sent), sent)
	}
	if !hasStage(h.stages(t, msg.TraceID), domain.StageReplyDelayed) {
		t.Fatalf("trace is missing %q", domain.StageReplyDelayed)
	}
}

func TestDuplicateWebhookDeliveryDuringReplyDelayIsProcessedOnce(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{{
		IsRelevant: true, ShouldRespond: true,
		Language: domain.LangRU, Intent: domain.IntentServiceInquiry, Confidence: 0.95,
	}}}
	h := newHarness(t, ai)
	h.pipeline.cfg.ReplyDelayMin = 80 * time.Millisecond
	h.pipeline.cfg.ReplyDelayMax = 120 * time.Millisecond

	msg := inbound("wamid.dup.delay", "Какие у вас услуги?")
	errCh := make(chan error, 1)
	go func() {
		errCh <- h.pipeline.Handle(context.Background(), msg)
	}()

	waitUntil(t, time.Second, func() bool {
		return h.outgoingCount(t) == 1
	})
	if err := h.pipeline.Handle(context.Background(), msg); err != nil {
		t.Fatalf("duplicate while first reply is delayed: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("first handle: %v", err)
	}

	if h.ai.callCount() != 1 {
		t.Fatalf("a retried webhook must not be classified twice, got %d calls", h.ai.callCount())
	}
	if got := h.wa.messages(); len(got) != 1 {
		t.Fatalf("a retried webhook must not be answered twice, got %d replies", len(got))
	}
}

func TestReplyDelaysAreIndependentAcrossChats(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{
		{IsRelevant: true, ShouldRespond: true, Language: domain.LangRU, Intent: domain.IntentServiceInquiry, Confidence: 0.95},
		{IsRelevant: true, ShouldRespond: true, Language: domain.LangRU, Intent: domain.IntentServiceInquiry, Confidence: 0.95},
	}}
	h := newHarness(t, ai)
	h.pipeline.cfg.ReplyDelayMin = 100 * time.Millisecond
	h.pipeline.cfg.ReplyDelayMax = 120 * time.Millisecond

	started := time.Now()
	errCh := make(chan error, 2)
	go func() {
		errCh <- h.pipeline.Handle(context.Background(),
			inboundFor("77015550001", "wamid.parallel.a", "Какие у вас услуги?"))
	}()
	go func() {
		errCh <- h.pipeline.Handle(context.Background(),
			inboundFor("77015550002", "wamid.parallel.b", "Какие у вас услуги?"))
	}()

	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("handle parallel message: %v", err)
		}
	}
	elapsed := time.Since(started)
	if elapsed >= 190*time.Millisecond {
		t.Fatalf("different chats appear serialized, elapsed=%s", elapsed)
	}
	if got := h.wa.messages(); len(got) != 2 {
		t.Fatalf("want 2 replies, got %d: %+v", len(got), got)
	}
}

func TestContextCancellationDuringReplyDelayStopsSend(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{{
		IsRelevant: true, ShouldRespond: true,
		Language: domain.LangRU, Intent: domain.IntentServiceInquiry, Confidence: 0.95,
	}}}
	h := newHarness(t, ai)
	h.pipeline.cfg.ReplyDelayMin = 80 * time.Millisecond
	h.pipeline.cfg.ReplyDelayMax = 120 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := h.pipeline.Handle(ctx, inbound("wamid.delay.cancel", "Какие у вас услуги?"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	if got := h.wa.messages(); len(got) != 0 {
		t.Fatalf("reply must not be sent after context cancellation, got %+v", got)
	}
}

func TestModelOutageFallsBackToTriggerMenu(t *testing.T) {
	h := newHarness(t, &stubAI{err: errors.New("openai unavailable")})
	ctx := context.Background()

	msg := inbound("wamid.down", "Какие у вас услуги?")
	msg.TraceID = "trace-down"
	if err := h.pipeline.Handle(ctx, msg); err != nil {
		t.Fatalf("the pipeline must survive an OpenAI outage: %v", err)
	}

	sent := h.wa.messages()
	if len(sent) != 1 {
		t.Fatalf("a clear trigger should still get the menu, got %d replies", len(sent))
	}
	if !strings.Contains(sent[0].Text, "Товарный знак") {
		t.Fatalf("expected the fallback menu, got:\n%s", sent[0].Text)
	}
	if !hasStage(h.stages(t, "trace-down"), domain.StageAIFailed) {
		t.Error("the outage should be visible in the trace")
	}
}

func TestModelOutageWithoutTriggerStaysSilent(t *testing.T) {
	h := newHarness(t, &stubAI{err: errors.New("openai unavailable")})

	msg := inbound("wamid.down2", "Хотел уточнить один момент по моему вопросу")
	if err := h.pipeline.Handle(context.Background(), msg); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if got := h.wa.messages(); len(got) != 0 {
		t.Fatalf("without the model and without a trigger the bot must stay silent, got %+v", got)
	}
}

func TestFailedDeliveryIsRecordedAndLeadSurvives(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{{
		IsRelevant: true, ShouldRespond: true,
		Language: domain.LangRU, Intent: domain.IntentTrademark,
		ServiceCode: domain.ServiceTrademarkRegistration, Confidence: 0.97, LeadScore: 0.9,
	}}}
	h := newHarness(t, ai)
	h.wa.err = errors.New("whatsapp is down")
	ctx := context.Background()

	if err := h.pipeline.Handle(ctx, inbound("wamid.fail", "Нужна регистрация товарного знака")); err != nil {
		t.Fatalf("a send failure must not fail the pipeline: %v", err)
	}

	failed, err := h.trace.FailedDeliveries(ctx, 10)
	if err != nil {
		t.Fatalf("failed deliveries: %v", err)
	}
	if len(failed) == 0 {
		t.Fatal("a failed send must be recorded so nothing is lost")
	}

	user, _ := h.users.GetByWhatsAppID(ctx, "77015551234")
	if _, err := h.leads.GetOpenByUser(ctx, user.ID); err != nil {
		t.Fatalf("the lead must survive a delivery failure: %v", err)
	}
}

func TestMediaWithoutCaptionIsStoredButNotAnalysed(t *testing.T) {
	h := newHarness(t, &stubAI{})
	ctx := context.Background()

	msg := inbound("wamid.img", "")
	msg.MessageType = domain.MessageImage
	msg.MediaID = "media-123"
	msg.MimeType = "image/jpeg"

	if err := h.pipeline.Handle(ctx, msg); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if h.ai.callCount() != 0 {
		t.Fatal("media must not be uploaded to the model automatically")
	}
	if got := h.wa.messages(); len(got) != 0 {
		t.Fatalf("media alone must not trigger a reply, got %+v", got)
	}

	var mediaRows int
	if err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_assets`).Scan(&mediaRows); err != nil {
		t.Fatalf("count media: %v", err)
	}
	if mediaRows != 1 {
		t.Fatalf("media metadata should be stored, got %d rows", mediaRows)
	}
}

// The model only ever sees trimmed conversation text, never internal data.
func TestModelContextIsLimitedAndClean(t *testing.T) {
	relevant := domain.AIClassification{
		IsRelevant: true, ShouldRespond: true,
		Language: domain.LangRU, Intent: domain.IntentServiceInquiry, Confidence: 0.9,
	}
	ai := &stubAI{results: []domain.AIClassification{relevant, relevant, relevant}}
	h := newHarness(t, ai)
	ctx := context.Background()

	for i, text := range []string{
		"Какие у вас услуги?",
		"А какие юридические услуги есть?",
		"Нужна консультация юриста",
	} {
		if err := h.pipeline.Handle(ctx, inbound("wamid.ctx."+string(rune('a'+i)), text)); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}

	last := ai.inputs[len(ai.inputs)-1]
	if last.Text != "Нужна консультация юриста" {
		t.Fatalf("current message = %q, want the latest customer text", last.Text)
	}
	if len(last.History) > 10 {
		t.Fatalf("history should respect the configured limit, got %d messages", len(last.History))
	}
	for _, h := range last.History {
		if h.Role != "user" && h.Role != "assistant" {
			t.Fatalf("unexpected role %q in model context", h.Role)
		}
		if h.Text == "" {
			t.Fatal("empty context message should not be sent")
		}
	}
	// The current message must not be duplicated inside the history.
	for _, h := range last.History {
		if h.Text == last.Text {
			t.Fatal("the current message must not also appear in the history")
		}
	}
}

func TestPhoneTypedInChatIsCaptured(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{{
		IsRelevant: true, ShouldRespond: true,
		Language: domain.LangRU, Intent: domain.IntentCallbackRequest, Confidence: 0.9,
	}}}
	h := newHarness(t, ai)
	ctx := context.Background()

	msg := inbound("wamid.phone", "Перезвоните мне на 8 707 111 22 33")
	msg.PhoneNumber = "77015551234"
	if err := h.pipeline.Handle(ctx, msg); err != nil {
		t.Fatalf("handle: %v", err)
	}

	user, _ := h.users.GetByWhatsAppID(ctx, "77015551234")
	if user.PhoneNumber != "77071112233" {
		t.Fatalf("phone = %q, want the number the customer typed", user.PhoneNumber)
	}
}
