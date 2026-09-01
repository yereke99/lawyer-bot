package handler

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"lawyer-bot/internal/domain"
	"lawyer-bot/internal/integration/whatsapp"
	"lawyer-bot/internal/repository"
	"lawyer-bot/internal/service"
	"lawyer-bot/internal/worker"
)

// GreenAPINativeClient is the Green API receive/delete subset used by polling.
type GreenAPINativeClient interface {
	ReceiveNotification(ctx context.Context, timeoutSeconds int) (*whatsapp.GreenNotification, error)
	DeleteNotification(ctx context.Context, receiptID int64) error
}

// GreenAPIPoller reads Green API native notifications without a webhook.
type GreenAPIPoller struct {
	client         GreenAPINativeClient
	pipeline       *service.Pipeline
	trace          *repository.TraceRepository
	pool           *worker.Pool
	log            *zap.Logger
	storeRaw       bool
	receiveTimeout int
	retryDelay     time.Duration
}

// GreenAPIPollerConfig configures Green API native polling.
type GreenAPIPollerConfig struct {
	StoreRaw              bool
	ReceiveTimeoutSeconds int
	RetryDelay            time.Duration
}

// NewGreenAPIPoller builds the Green API native poller.
func NewGreenAPIPoller(client GreenAPINativeClient, pipeline *service.Pipeline,
	trace *repository.TraceRepository, pool *worker.Pool, log *zap.Logger,
	cfg GreenAPIPollerConfig) *GreenAPIPoller {

	if cfg.ReceiveTimeoutSeconds <= 0 {
		cfg.ReceiveTimeoutSeconds = 5
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = 5 * time.Second
	}
	if log == nil {
		log = zap.NewNop()
	}

	return &GreenAPIPoller{
		client:         client,
		pipeline:       pipeline,
		trace:          trace,
		pool:           pool,
		log:            log,
		storeRaw:       cfg.StoreRaw,
		receiveTimeout: cfg.ReceiveTimeoutSeconds,
		retryDelay:     cfg.RetryDelay,
	}
}

// Start launches polling until ctx is cancelled.
func (p *GreenAPIPoller) Start(ctx context.Context) {
	go p.loop(ctx)
	p.log.Info("green api native polling started",
		zap.Int("receive_timeout_seconds", p.receiveTimeout),
		zap.Duration("retry_delay", p.retryDelay))
}

func (p *GreenAPIPoller) loop(ctx context.Context) {
	for {
		if err := ctx.Err(); err != nil {
			p.log.Info("green api native polling stopped", zap.Error(err))
			return
		}

		notification, err := p.client.ReceiveNotification(ctx, p.receiveTimeout)
		if err != nil {
			p.log.Error("green api receive notification failed", zap.Error(err))
			if !p.waitRetry(ctx) {
				return
			}
			continue
		}
		if notification == nil {
			continue
		}

		if err := p.handleNotification(ctx, notification); err != nil {
			p.log.Error("green api notification handling failed",
				zap.Int64("receipt_id", notification.ReceiptID),
				zap.Error(err))
			if !p.waitRetry(ctx) {
				return
			}
		}
	}
}

func (p *GreenAPIPoller) handleNotification(ctx context.Context, notification *whatsapp.GreenNotification) error {
	traceID := service.NewTraceID()
	log := p.log.With(zap.String("trace_id", traceID), zap.Int64("receipt_id", notification.ReceiptID))

	payload := ""
	if p.storeRaw {
		payload = string(notification.Body)
	}

	messages, parseErr := whatsapp.ParseWebhook(notification.Body)
	if parseErr != nil {
		p.storePollingEvent(ctx, domain.WebhookEvent{
			TraceID: traceID, Provider: "greenapi", Payload: payload,
			Status: "rejected", Error: parseErr.Error(),
		})
		if err := p.client.DeleteNotification(ctx, notification.ReceiptID); err != nil {
			return fmt.Errorf("delete malformed green api notification: %w", err)
		}
		return nil
	}

	p.storePollingEvent(ctx, domain.WebhookEvent{
		TraceID: traceID, Provider: "greenapi", Payload: payload,
		MessageCount: len(messages), Status: "received",
	})

	for i, msg := range messages {
		msg.TraceID = service.NewTraceID()
		if err := p.enqueue(msg); err != nil {
			return fmt.Errorf("queue green api message %d: %w", i, err)
		}
		log.Info("green api message queued",
			zap.Int("index", i),
			zap.String("message_trace_id", msg.TraceID),
			zap.String("provider_message_id", msg.WhatsAppMessageID))
	}

	if err := p.client.DeleteNotification(ctx, notification.ReceiptID); err != nil {
		return fmt.Errorf("delete green api notification: %w", err)
	}
	return nil
}

func (p *GreenAPIPoller) enqueue(msg domain.InboundMessage) error {
	return p.pool.Submit(func(ctx context.Context) {
		if err := p.pipeline.Handle(ctx, msg); err != nil {
			p.log.Error("pipeline failed",
				zap.String("trace_id", msg.TraceID),
				zap.Error(err))
		}
	})
}

func (p *GreenAPIPoller) storePollingEvent(ctx context.Context, e domain.WebhookEvent) {
	if _, err := p.trace.WebhookEvent(ctx, e); err != nil {
		p.log.Warn("store green api polling event failed", zap.Error(err))
	}
}

func (p *GreenAPIPoller) waitRetry(ctx context.Context) bool {
	timer := time.NewTimer(p.retryDelay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
