package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"lawyer-bot/internal/domain"
	"lawyer-bot/internal/integration/whatsapp"
	"lawyer-bot/internal/repository"
	"lawyer-bot/internal/service"
	"lawyer-bot/internal/worker"
)

const (
	testVerifyToken = "verify-me"
	testAppSecret   = "app-secret"
)

// recordingAI counts classifications without contacting OpenAI.
type recordingAI struct {
	mu    sync.Mutex
	calls int
}

func (r *recordingAI) ClassifyMessage(context.Context, domain.AIInput) (domain.AIClassification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return domain.AIClassification{
		IsRelevant: true, ShouldRespond: true,
		Language: domain.LangRU, Intent: domain.IntentServiceInquiry, Confidence: 0.95,
	}, nil
}

// count reports the number of classifications. The pipeline runs on a worker
// goroutine, so the counter must be read under the same lock that writes it.
func (r *recordingAI) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// recordingWA records outbound sends without contacting WhatsApp.
type recordingWA struct {
	mu   sync.Mutex
	sent []string
}

func (r *recordingWA) SendText(_ context.Context, recipient, text string) (domain.SendResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, recipient)
	return domain.SendResult{MessageID: "wamid.out"}, nil
}

func (r *recordingWA) SendMedia(ctx context.Context, recipient, mediaID, caption string) (domain.SendResult, error) {
	return r.SendText(ctx, recipient, caption)
}

func (r *recordingWA) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}

type testServer struct {
	handler  *WhatsAppHandler
	pipeline *service.Pipeline
	pool     *worker.Pool
	ai       *recordingAI
	wa       *recordingWA
	trace    *repository.TraceRepository
	db       *repository.DB
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()

	db, err := repository.Open(context.Background(), filepath.Join(t.TempDir(), "handler.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	catalog := service.NewCatalog()
	triggers := service.NewTriggerSet()
	trace := repository.NewTraceRepository(db)
	ai := &recordingAI{}
	wa := &recordingWA{}

	pipeline := service.NewPipeline(service.PipelineDeps{
		Users:    repository.NewUserRepository(db),
		Messages: repository.NewMessageRepository(db),
		Leads:    repository.NewLeadRepository(db),
		AILog:    repository.NewAIInteractionRepository(db),
		Trace:    trace,
		AI:       ai,
		WhatsApp: wa,
		Gate: service.NewGate(triggers, service.GateConfig{
			MaxCallsPerDay: 40, AnalyzeUnmatched: true, MinWordsUnmatched: 3,
		}),
		Catalog:  catalog,
		Composer: service.NewComposer(catalog),
		Qualify:  service.NewQualifier(catalog, 0.75),
		Triggers: triggers,
		Logger:   zap.NewNop(),
	}, service.PipelineConfig{
		MinConfidence:   0.75,
		ContextMessages: 10,
		NotifyRecipient: "77009998877",
	})

	pool := worker.New(worker.Options{Workers: 2, QueueSize: 16, JobTimeout: 10 * time.Second})
	pool.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = pool.Shutdown(ctx)
	})

	handler := NewWhatsAppHandler(pipeline, trace, pool, zap.NewNop(), WhatsAppHandlerConfig{
		VerifyToken: testVerifyToken,
		AppSecret:   testAppSecret,
		StoreRaw:    true,
	})

	return &testServer{
		handler: handler, pipeline: pipeline, pool: pool,
		ai: ai, wa: wa, trace: trace, db: db,
	}
}

// storedMessages counts everything the pipeline persisted, in either direction.
func (ts *testServer) storedMessages(t *testing.T) int {
	t.Helper()
	var n int
	if err := ts.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return n
}

// post sends a signed webhook and waits for asynchronous processing to settle.
func (ts *testServer) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/webhook/whatsapp", strings.NewReader(body))
	mac := hmac.New(sha256.New, []byte(testAppSecret))
	mac.Write([]byte(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))

	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)
	ts.drain(t)
	return rec
}

func (ts *testServer) drain(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ts.pool.Pending() == 0 {
			time.Sleep(60 * time.Millisecond)
			if ts.pool.Pending() == 0 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background processing did not finish in time")
}

func textWebhook(id, text string) string {
	return `{"object":"whatsapp_business_account","entry":[{"changes":[{"field":"messages","value":{
		"contacts":[{"wa_id":"77015551234","profile":{"name":"Аида"}}],
		"messages":[{"from":"77015551234","id":"` + id + `","timestamp":"1700000000","type":"text","text":{"body":"` + text + `"}}]
	}}]}]}`
}

func greenWebhook(chatID, sender, id, text string) string {
	return `{"typeWebhook":"incomingMessageReceived","instanceData":{"idInstance":1,"wid":"77000000000@c.us"},
		"timestamp":1700000000,"idMessage":"` + id + `",
		"senderData":{"chatId":"` + chatID + `","sender":"` + sender + `","senderName":"Аида","chatName":"Юристы"},
		"messageData":{"typeMessage":"textMessage","textMessageData":{"textMessage":"` + text + `"}}}`
}

// A group message carries a perfectly valid trigger. It must still be dropped
// at the ingress: nothing stored, no model call, no reply.
func TestGroupMessageIsAcknowledgedButNeverProcessed(t *testing.T) {
	ts := newTestServer(t)

	rec := ts.post(t, greenWebhook("120363000000000000@g.us", "77015551234@c.us",
		"green.group.1", "Мне нужна консультация юриста"))

	if rec.Code != http.StatusOK {
		t.Fatalf("the provider must still be acknowledged, got %d", rec.Code)
	}
	if ts.ai.count() != 0 {
		t.Fatal("a group message must never reach the model")
	}
	if ts.wa.count() != 0 {
		t.Fatal("a group message must never produce a reply")
	}
	if n := ts.storedMessages(t); n != 0 {
		t.Fatalf("a group message must not open a client conversation, got %d stored rows", n)
	}

	// The very same text in a private chat is processed normally, so the guard
	// is about the chat kind and not about parsing.
	ts.post(t, greenWebhook("77015551234@c.us", "77015551234@c.us",
		"green.private.1", "Мне нужна консультация юриста"))
	if ts.ai.count() != 1 {
		t.Fatalf("a private message with the same text must be analysed, got %d calls", ts.ai.count())
	}
	if ts.wa.count() != 1 {
		t.Fatalf("a private message with the same text must be answered, got %d sends", ts.wa.count())
	}
}

// Calling the pipeline directly with a group chat is safe too: the guard does
// not live in the HTTP layer alone.
func TestPipelineCalledDirectlyWithAGroupChatDoesNothing(t *testing.T) {
	ts := newTestServer(t)

	err := ts.pipeline.Handle(context.Background(), domain.InboundMessage{
		WhatsAppUserID:    "120363000000000000@g.us",
		PhoneNumber:       "77015551234",
		WhatsAppMessageID: "green.group.direct",
		MessageType:       domain.MessageText,
		Text:              "Мне нужна консультация юриста",
		Timestamp:         time.Now().UTC(),
		Source:            domain.SourceWhatsApp,
	})
	if err != nil {
		t.Fatalf("a group message must be ignored, not fail: %v", err)
	}
	if ts.ai.count() != 0 || ts.wa.count() != 0 || ts.storedMessages(t) != 0 {
		t.Fatal("a group message must have no effect at the service layer either")
	}
}

func TestWebhookVerificationChallenge(t *testing.T) {
	ts := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet,
		"/webhook/whatsapp?hub.mode=subscribe&hub.verify_token="+testVerifyToken+"&hub.challenge=12345", nil)
	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "12345" {
		t.Fatalf("body = %q, want the challenge echoed back", rec.Body.String())
	}
}

func TestWebhookVerificationRejectsWrongToken(t *testing.T) {
	ts := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet,
		"/webhook/whatsapp?hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=12345", nil)
	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestWebhookRejectsInvalidSignature(t *testing.T) {
	ts := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/webhook/whatsapp",
		strings.NewReader(textWebhook("wamid.sig", "Какие у вас услуги?")))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")

	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if ts.ai.count() != 0 {
		t.Fatal("an unsigned payload must never reach the pipeline")
	}
}

func TestWebhookAcknowledgesAndProcesses(t *testing.T) {
	ts := newTestServer(t)

	rec := ts.post(t, textWebhook("wamid.ok", "Какие у вас услуги?"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := ts.ai.count(); got != 1 {
		t.Fatalf("want 1 classification, got %d", got)
	}
	if ts.wa.count() != 1 {
		t.Fatalf("want 1 reply, got %d", ts.wa.count())
	}
}

// The bot is reactive only: an event carrying no customer message must produce
// no outbound traffic whatsoever.
func TestStatusCallbackProducesNoOutboundMessage(t *testing.T) {
	ts := newTestServer(t)

	body := `{"entry":[{"changes":[{"value":{"statuses":[{"id":"wamid.x","status":"delivered"}]}}]}]}`
	rec := ts.post(t, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ts.ai.count() != 0 {
		t.Fatal("a delivery receipt must not be classified")
	}
	if ts.wa.count() != 0 {
		t.Fatal("a delivery receipt must never cause the bot to message anyone")
	}
}

func TestMalformedPayloadIsAcknowledgedNotRetried(t *testing.T) {
	ts := newTestServer(t)

	rec := ts.post(t, `{"entry": [`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: malformed payloads should be acknowledged so the provider stops retrying", rec.Code)
	}
	if ts.wa.count() != 0 {
		t.Fatal("a malformed payload must not produce a reply")
	}
}

func TestRawPayloadIsStoredForAudit(t *testing.T) {
	ts := newTestServer(t)
	ts.post(t, textWebhook("wamid.audit", "Нужна консультация юриста"))

	events, err := ts.trace.FailedDeliveries(context.Background(), 1)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("no delivery should have failed, got %+v", events)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	ts := newTestServer(t)

	req := httptest.NewRequest(http.MethodDelete, "/webhook/whatsapp", nil)
	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// ------------------------------------------------------- green api polling

// fakeGreenQueue replays a fixed list of notifications and records the receipts
// the poller acknowledged.
type fakeGreenQueue struct {
	mu      sync.Mutex
	pending []*whatsapp.GreenNotification
	deleted []int64
}

func (q *fakeGreenQueue) ReceiveNotification(context.Context, int) (*whatsapp.GreenNotification, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return nil, nil
	}
	next := q.pending[0]
	q.pending = q.pending[1:]
	return next, nil
}

func (q *fakeGreenQueue) DeleteNotification(_ context.Context, receiptID int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.deleted = append(q.deleted, receiptID)
	return nil
}

func (q *fakeGreenQueue) acknowledged() []int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]int64, len(q.deleted))
	copy(out, q.deleted)
	return out
}

// Polling is the production ingress. A group notification must be dropped and
// still acknowledged, or the provider queue would stall on it forever.
func TestPollingDropsGroupNotificationsAndStillAcknowledgesThem(t *testing.T) {
	ts := newTestServer(t)
	queue := &fakeGreenQueue{pending: []*whatsapp.GreenNotification{
		{ReceiptID: 1, Body: []byte(greenWebhook("120363000000000000@g.us", "77015551234@c.us",
			"poll.group.1", "Мне нужна консультация юриста"))},
		{ReceiptID: 2, Body: []byte(greenWebhook("77015551234@c.us", "77015551234@c.us",
			"poll.private.1", "Мне нужна консультация юриста"))},
	}}

	poller := NewGreenAPIPoller(queue, ts.pipeline, ts.trace, ts.pool, zap.NewNop(),
		GreenAPIPollerConfig{ReceiveTimeoutSeconds: 1, RetryDelay: time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	poller.Start(ctx)
	waitFor(t, func() bool { return len(queue.acknowledged()) == 2 })
	cancel()
	ts.drain(t)

	if ts.ai.count() != 1 {
		t.Fatalf("only the private message may be analysed, got %d calls", ts.ai.count())
	}
	if ts.wa.count() != 1 {
		t.Fatalf("only the private message may be answered, got %d sends", ts.wa.count())
	}
	if n := ts.storedMessages(t); n != 2 {
		t.Fatalf("only the private exchange may be stored, got %d rows", n)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}
