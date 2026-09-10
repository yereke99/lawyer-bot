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
)

// FollowUpConfig holds every follow-up tunable. Nothing about timing is
// hardcoded in the business logic: the delays, the attempt cap and the business
// hours all arrive from configuration.
type FollowUpConfig struct {
	Enabled bool
	// Delays[i] is how long after the last client message stage i+1 fires.
	Delays []time.Duration
	// MaxAttempts caps delivery retries for a single job.
	MaxAttempts int
	// PollInterval is how often the worker looks for due jobs.
	PollInterval time.Duration
	// BatchSize is how many due jobs one poll claims.
	BatchSize int
	// ClaimTTL is how long a claimed job may stay claimed before another worker
	// may reclaim it, which is what makes a crash mid-send recoverable.
	ClaimTTL time.Duration
	// RetryBackoff is the delay before a transient send failure is retried.
	RetryBackoff time.Duration

	// Business hours. When enabled, a nudge due outside the window is deferred
	// to the next opening rather than waking a client at 3am.
	BusinessHoursOnly bool
	BusinessStartHour int
	BusinessEndHour   int
	Location          *time.Location
}

// Normalise fills in safe defaults.
func (c FollowUpConfig) Normalise() FollowUpConfig {
	if len(c.Delays) == 0 {
		c.Delays = []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour}
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Minute
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 20
	}
	if c.ClaimTTL <= 0 {
		c.ClaimTTL = 5 * time.Minute
	}
	if c.RetryBackoff <= 0 {
		c.RetryBackoff = 10 * time.Minute
	}
	if c.Location == nil {
		c.Location = time.UTC
	}
	if c.BusinessEndHour <= c.BusinessStartHour {
		c.BusinessStartHour, c.BusinessEndHour = 9, 21
	}
	return c
}

// Stages reports how many follow-up stages are configured.
func (c FollowUpConfig) Stages() int { return len(c.Delays) }

// DelayFor returns the delay of a 1-based stage.
func (c FollowUpConfig) DelayFor(stage int) (time.Duration, bool) {
	if stage < 1 || stage > len(c.Delays) {
		return 0, false
	}
	return c.Delays[stage-1], true
}

// NextSendTime applies the business-hours rule to a computed due time.
func (c FollowUpConfig) NextSendTime(due time.Time) time.Time {
	if !c.BusinessHoursOnly {
		return due
	}
	local := due.In(c.Location)
	switch {
	case local.Hour() < c.BusinessStartHour:
		return time.Date(local.Year(), local.Month(), local.Day(),
			c.BusinessStartHour, 0, 0, 0, c.Location).UTC()
	case local.Hour() >= c.BusinessEndHour:
		next := local.Add(24 * time.Hour)
		return time.Date(next.Year(), next.Month(), next.Day(),
			c.BusinessStartHour, 0, 0, 0, c.Location).UTC()
	default:
		return due
	}
}

// FollowUpService schedules, cancels and delivers automatic follow-ups.
//
// Durability: every job is a database row. A restart loses nothing, because no
// timer is ever held in memory.
//
// Safety: a claimed job is re-validated against the live client record
// immediately before sending. Between scheduling and delivery a client may have
// replied, been blocked, been taken over by a consultant or been closed — each
// of those cancels the nudge instead of sending it.
type FollowUpService struct {
	jobs     *repository.FollowUpRepository
	crm      *repository.CRMRepository
	messages *repository.MessageRepository
	trace    *repository.TraceRepository
	settings *repository.SettingsRepository
	sender   *Messenger
	log      *zap.Logger
	cfg      FollowUpConfig
}

// FollowUpDeps groups the collaborators.
type FollowUpDeps struct {
	Jobs     *repository.FollowUpRepository
	CRM      *repository.CRMRepository
	Messages *repository.MessageRepository
	Trace    *repository.TraceRepository
	Settings *repository.SettingsRepository
	Sender   *Messenger
	Logger   *zap.Logger
}

// NewFollowUpService builds the follow-up service.
func NewFollowUpService(deps FollowUpDeps, cfg FollowUpConfig) *FollowUpService {
	log := deps.Logger
	if log == nil {
		log = zap.NewNop()
	}
	return &FollowUpService{
		jobs:     deps.Jobs,
		crm:      deps.CRM,
		messages: deps.Messages,
		trace:    deps.Trace,
		settings: deps.Settings,
		sender:   deps.Sender,
		log:      log,
		cfg:      cfg.Normalise(),
	}
}

// Config exposes the effective configuration, for the settings screen.
func (s *FollowUpService) Config() FollowUpConfig { return s.cfg }

// CancelFor invalidates every outstanding nudge for a client. This is called
// the instant a client replies, which is what stops a stale follow-up from
// arriving after the conversation moved on.
func (s *FollowUpService) CancelFor(ctx context.Context, userID int64, reason string) {
	if s == nil || s.jobs == nil {
		return
	}
	n, err := s.jobs.CancelPendingForUser(ctx, userID, reason)
	if err != nil {
		s.log.Warn("cancel follow-ups failed", zap.Int64("client_id", userID), zap.Error(err))
		return
	}
	if n > 0 {
		if err := s.crm.SetFollowUpPlan(ctx, userID, 0, nil); err != nil {
			s.log.Warn("clear follow-up plan failed", zap.Error(err))
		}
		s.log.Debug("follow-ups cancelled", zap.Int64("client_id", userID), zap.Int("count", n))
	}
}

// Schedule plans the next nudge for a client.
//
// stage is 1-based. Scheduling is idempotent through the job's dedupe key, so
// two concurrent pipeline runs for the same message cannot produce two nudges.
func (s *FollowUpService) Schedule(ctx context.Context, client *domain.CRMClient, stage int, anchorMessageID int64) error {
	if s == nil || !s.cfg.Enabled || s.jobs == nil {
		return nil
	}
	if !s.whatsappBotEnabled(ctx) {
		s.log.Info("follow-up scheduling skipped because whatsapp bot is disabled",
			zap.Int64("client_id", clientIDOf(client)))
		return nil
	}
	if client == nil || !client.AutomationAllowed() {
		return nil
	}
	delay, ok := s.cfg.DelayFor(stage)
	if !ok {
		return nil
	}

	due := s.cfg.NextSendTime(time.Now().UTC().Add(delay))
	id, created, err := s.jobs.Schedule(ctx, domain.FollowUpJob{
		UserID:      client.ID,
		Stage:       stage,
		ScheduledAt: due,
		DedupeKey:   repository.FollowUpDedupeKey(client.ID, stage, anchorMessageID),
		MessageID:   anchorMessageID,
	})
	if err != nil {
		return err
	}
	if !created {
		return nil
	}
	if err := s.crm.SetFollowUpPlan(ctx, client.ID, stage, &due); err != nil {
		s.log.Warn("store follow-up plan failed", zap.Error(err))
	}
	s.log.Info("follow-up scheduled",
		zap.Int64("client_id", client.ID), zap.Int64("job_id", id),
		zap.Int("stage", stage), zap.Time("due", due))
	return nil
}

// Start runs the worker until ctx is cancelled.
func (s *FollowUpService) Start(ctx context.Context) {
	if s == nil || !s.cfg.Enabled {
		return
	}
	go s.loop(ctx)
	s.log.Info("follow-up worker started",
		zap.Duration("poll_interval", s.cfg.PollInterval),
		zap.Int("stages", s.cfg.Stages()),
		zap.Bool("business_hours_only", s.cfg.BusinessHoursOnly))
}

func (s *FollowUpService) loop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Info("follow-up worker stopped")
			return
		case <-ticker.C:
			if err := s.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.log.Error("follow-up run failed", zap.Error(err))
			}
		}
	}
}

// RunOnce claims and processes one batch of due jobs. It is exported so tests
// and an operator-triggered run use exactly the same path as the ticker.
func (s *FollowUpService) RunOnce(ctx context.Context) error {
	token, err := claimToken()
	if err != nil {
		return err
	}
	jobs, err := s.jobs.Claim(ctx, token, time.Now().UTC(), s.cfg.BatchSize, s.cfg.ClaimTTL)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.process(ctx, job, token)
	}
	return nil
}

// process delivers a single claimed job after re-validating it.
func (s *FollowUpService) process(ctx context.Context, job domain.FollowUpJob, token string) {
	log := s.log.With(
		zap.Int64("job_id", job.ID),
		zap.Int64("client_id", job.UserID),
		zap.Int("stage", job.Stage))

	client, err := s.crm.GetClient(ctx, job.UserID)
	if err != nil {
		s.finish(ctx, log, job, token, domain.FollowUpFailed, "client lookup failed: "+err.Error())
		return
	}

	// Re-check everything that may have changed since scheduling.
	if reason, ok := s.blocked(ctx, client, job); !ok {
		s.finish(ctx, log, job, token, domain.FollowUpSkipped, reason)
		log.Info("follow-up skipped", zap.String("reason", reason))
		return
	}

	text := followUpText(client.Language, job.Stage)
	if text == "" {
		s.finish(ctx, log, job, token, domain.FollowUpSkipped, "no text configured for stage")
		return
	}

	if job.Attempts > s.cfg.MaxAttempts {
		s.finish(ctx, log, job, token, domain.FollowUpFailed, "attempt limit reached")
		log.Warn("follow-up abandoned after repeated failures")
		return
	}

	_, sendErr := s.sender.Send(ctx, Outbound{
		UserID:    client.ID,
		Recipient: client.WhatsAppUserID,
		Sender:    domain.SenderAI,
		Kind:      "follow_up",
		Text:      text,
	})
	if sendErr != nil {
		retryAt := time.Now().UTC().Add(s.cfg.RetryBackoff)
		if err := s.jobs.Release(ctx, job.ID, token, retryAt, sendErr.Error()); err != nil {
			log.Warn("release follow-up failed", zap.Error(err))
		}
		log.Error("follow-up send failed, will retry", zap.Time("retry_at", retryAt), zap.Error(sendErr))
		return
	}

	s.finish(ctx, log, job, token, domain.FollowUpSent, "")
	s.event(ctx, domain.TraceEvent{
		UserID: client.ID, TraceID: NewTraceID(),
		Stage: "follow_up_sent", Decision: domain.DecisionOK,
		Detail: repository.Detail(map[string]any{"stage": job.Stage, "job_id": job.ID}),
	})
	log.Info("follow-up sent")

	// Waiting for the client is the natural status after a nudge.
	if client.CRMStatus == domain.CRMNeedsQualification || client.CRMStatus == domain.CRMQualified ||
		client.CRMStatus == domain.CRMAIProcessing || client.CRMStatus == domain.CRMNew {
		if err := s.crm.SetStatus(ctx, client.ID, domain.CRMWaitingForClient, client.CloseReason); err != nil {
			log.Warn("set waiting status failed", zap.Error(err))
		}
	}

	// Chain the next stage from the same anchor message, so the sequence keeps
	// its idempotency key and a client reply still cancels the whole chain.
	if next := job.Stage + 1; next <= s.cfg.Stages() {
		refreshed, err := s.crm.GetClient(ctx, client.ID)
		if err == nil {
			if err := s.Schedule(ctx, refreshed, next, job.MessageID); err != nil {
				log.Warn("schedule next follow-up stage failed", zap.Error(err))
			}
		}
	} else if err := s.crm.SetFollowUpPlan(ctx, client.ID, job.Stage, nil); err != nil {
		log.Warn("clear follow-up plan failed", zap.Error(err))
	}
}

// blocked re-validates a claimed job. It returns ok=false with the reason when
// the nudge must not be delivered.
func (s *FollowUpService) blocked(ctx context.Context, client *domain.CRMClient, job domain.FollowUpJob) (string, bool) {
	switch {
	case !s.whatsappBotEnabled(ctx):
		return "whatsapp bot disabled", false
	case !domain.IsPrivateWhatsAppChat(client.WhatsAppUserID):
		return "non-private whatsapp chat", false
	case client.Blocked:
		return "client is blocked", false
	case !client.AIEnabled:
		return "ai automation disabled for client", false
	case !client.Mode.AutomationAllowed():
		return "conversation owned by a consultant", false
	case client.CRMStatus.OrDefault().Terminal():
		return "lead is closed", false
	}

	// The anchor must still be the newest client message: anything newer means
	// the client already replied and the nudge is obsolete.
	lastInbound, err := s.messages.LastInboundID(ctx, client.ID)
	if err != nil {
		return "last inbound lookup failed: " + err.Error(), false
	}
	if job.MessageID > 0 && lastInbound > job.MessageID {
		return "client already replied", false
	}
	return "", true
}

func (s *FollowUpService) whatsappBotEnabled(ctx context.Context) bool {
	if s == nil || s.settings == nil {
		return true
	}
	enabled, err := s.settings.WhatsAppBotEnabled(ctx)
	if err != nil {
		s.log.Warn("load whatsapp bot setting failed", zap.Error(err))
		return true
	}
	return enabled
}

func clientIDOf(client *domain.CRMClient) int64 {
	if client == nil {
		return 0
	}
	return client.ID
}

func (s *FollowUpService) finish(ctx context.Context, log *zap.Logger, job domain.FollowUpJob,
	token string, status domain.FollowUpStatus, note string) {

	if err := s.jobs.Complete(ctx, job.ID, token, status, note); err != nil {
		log.Warn("complete follow-up failed", zap.Error(err))
	}
	if status != domain.FollowUpSent {
		if err := s.crm.SetFollowUpPlan(ctx, job.UserID, job.Stage, nil); err != nil {
			log.Warn("clear follow-up plan failed", zap.Error(err))
		}
	}
}

func (s *FollowUpService) event(ctx context.Context, e domain.TraceEvent) {
	if s.trace == nil {
		return
	}
	if err := s.trace.Event(ctx, e); err != nil {
		s.log.Warn("write trace event failed", zap.Error(err))
	}
}

func claimToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate claim token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// followUpText is the localised nudge. The wording is deliberately short,
// carries no price and makes no promise.
func followUpText(lang domain.Language, stage int) string {
	table, ok := followUpTexts[stage]
	if !ok {
		return ""
	}
	return strings.TrimSpace(tr(lang, table))
}

var followUpTexts = map[int]map[domain.Language]string{
	1: {
		domain.LangRU: "Здравствуйте! Подскажите, ваш вопрос ещё актуален?",
		domain.LangKK: "Сәлеметсіз бе! Сұрағыңыз әлі өзекті ме?",
		domain.LangEN: "Hello! Is your question still relevant?",
	},
	2: {
		domain.LangRU: "Напоминаю о вашем обращении. Если удобно, напишите — специалист подберёт решение.",
		domain.LangKK: "Өтінішіңізді еске саламын. Ыңғайлы болса, жазыңыз — маман шешім ұсынады.",
		domain.LangEN: "A short reminder about your request. Write back and our specialist will help.",
	},
	3: {
		domain.LangRU: "Если вопрос всё ещё открыт, напишите — мы на связи. Если уже решён, дайте знать.",
		domain.LangKK: "Сұрақ әлі шешілмесе, жазыңыз — біз байланыстамыз. Шешілген болса, хабарлаңыз.",
		domain.LangEN: "If the question is still open, write to us. If it is resolved, just let us know.",
	},
}
