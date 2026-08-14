package handler

import (
	"context"
	"crypto/subtle"
	"io"
	"net/http"

	"go.uber.org/zap"

	"lawyer-bot/internal/domain"
	"lawyer-bot/internal/integration/whatsapp"
	"lawyer-bot/internal/repository"
	"lawyer-bot/internal/service"
	"lawyer-bot/internal/worker"
)

// maxWebhookBody caps the request body the handler will read.
const maxWebhookBody = 2 << 20 // 2 MiB

// WhatsAppHandler terminates the provider webhook.
//
// It contains no business logic: it authenticates the request, stores the raw
// payload for audit, converts it to domain messages and hands each one to the
// pipeline through the worker pool.
type WhatsAppHandler struct {
	pipeline    *service.Pipeline
	trace       *repository.TraceRepository
	pool        *worker.Pool
	log         *zap.Logger
	verifyToken string
	appSecret   string
	storeRaw    bool
}

// WhatsAppHandlerConfig configures the handler.
type WhatsAppHandlerConfig struct {
	VerifyToken string
	AppSecret   string
	StoreRaw    bool
}

// NewWhatsAppHandler builds the webhook handler.
func NewWhatsAppHandler(pipeline *service.Pipeline, trace *repository.TraceRepository,
	pool *worker.Pool, log *zap.Logger, cfg WhatsAppHandlerConfig) *WhatsAppHandler {
	return &WhatsAppHandler{
		pipeline:    pipeline,
		trace:       trace,
		pool:        pool,
		log:         log,
		verifyToken: cfg.VerifyToken,
		appSecret:   cfg.AppSecret,
		storeRaw:    cfg.StoreRaw,
	}
}

// ServeHTTP routes the two webhook methods.
func (h *WhatsAppHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.verify(w, r)
	case http.MethodPost:
		h.receive(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// verify answers Meta's subscription challenge.
func (h *WhatsAppHandler) verify(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode := q.Get("hub.mode")
	token := q.Get("hub.verify_token")
	challenge := q.Get("hub.challenge")

	if mode != "subscribe" || subtle.ConstantTimeCompare([]byte(token), []byte(h.verifyToken)) != 1 {
		h.log.Warn("webhook verification rejected", zap.String("mode", mode))
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	h.log.Info("webhook verified")
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(challenge))
}

// receive accepts an event batch.
//
// The provider retries anything that is not answered quickly, so the handler
// acknowledges first and processes asynchronously. Duplicate deliveries are
// filtered by message ID inside the pipeline.
func (h *WhatsAppHandler) receive(w http.ResponseWriter, r *http.Request) {
	traceID := service.NewTraceID()
	log := h.log.With(zap.String("trace_id", traceID))

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		log.Error("read webhook body failed", zap.Error(err))
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	signature := r.Header.Get("X-Hub-Signature-256")
	if !whatsapp.VerifySignature(h.appSecret, body, signature) {
		log.Warn("webhook signature verification failed")
		h.storeEvent(r.Context(), traceID, body, signature, 0, "rejected", "invalid signature")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	messages, parseErr := whatsapp.ParseWebhook(body)
	if parseErr != nil {
		log.Error("parse webhook failed", zap.Error(parseErr))
		h.storeEvent(r.Context(), traceID, body, signature, 0, "rejected", parseErr.Error())
		// The payload is malformed; retrying will not help, so acknowledge it.
		w.WriteHeader(http.StatusOK)
		return
	}

	h.storeEvent(r.Context(), traceID, body, signature, len(messages), "received", "")

	// Acknowledge before doing any slow work.
	w.WriteHeader(http.StatusOK)

	if len(messages) == 0 {
		// Status callbacks and other non-message events end here: the bot has
		// nothing to react to.
		return
	}

	for i, msg := range messages {
		msg.TraceID = service.NewTraceID()
		if err := h.enqueue(msg); err != nil {
			log.Error("could not queue message for processing",
				zap.Int("index", i),
				zap.String("message_trace_id", msg.TraceID),
				zap.Error(err))
		}
	}
}

func (h *WhatsAppHandler) enqueue(msg domain.InboundMessage) error {
	return h.pool.Submit(func(ctx context.Context) {
		if err := h.pipeline.Handle(ctx, msg); err != nil {
			h.log.Error("pipeline failed",
				zap.String("trace_id", msg.TraceID),
				zap.Error(err))
		}
	})
}

func (h *WhatsAppHandler) storeEvent(ctx context.Context, traceID string, body []byte,
	signature string, count int, status, errMsg string) {

	payload := ""
	if h.storeRaw {
		payload = string(body)
	}
	if _, err := h.trace.WebhookEvent(ctx, domain.WebhookEvent{
		TraceID:      traceID,
		Provider:     "whatsapp",
		Signature:    signature,
		Payload:      payload,
		MessageCount: count,
		Status:       status,
		Error:        errMsg,
	}); err != nil {
		h.log.Warn("store webhook event failed", zap.Error(err))
	}
}
