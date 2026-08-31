package service

import (
	"context"
	cryptorand "crypto/rand"
	"math/big"
	"time"

	"go.uber.org/zap"

	"lawyer-bot/internal/domain"
	"lawyer-bot/internal/repository"
)

func normalizeReplyDelayRange(minDelay, maxDelay time.Duration) (time.Duration, time.Duration) {
	if minDelay < 0 {
		minDelay = 0
	}
	if maxDelay < 0 {
		maxDelay = 0
	}
	if minDelay == 0 && maxDelay == 0 {
		return 0, 0
	}
	if maxDelay <= minDelay {
		maxDelay = minDelay + time.Millisecond
	}
	return minDelay, maxDelay
}

func (p *Pipeline) waitBeforeReply(ctx context.Context, log *zap.Logger, userID, messageID int64, traceID string) error {
	delay := randomDuration(p.cfg.ReplyDelayMin, p.cfg.ReplyDelayMax)
	if delay <= 0 {
		return nil
	}

	log.Info("waiting before automatic reply", zap.Duration("delay", delay))
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		p.event(ctx, domain.TraceEvent{
			TraceID: traceID, UserID: userID, MessageID: messageID,
			Stage: domain.StageReplyDelayed, Decision: domain.DecisionOK,
			DurationMS: delay.Milliseconds(),
			Detail:     repository.Detail(map[string]any{"delay_ms": delay.Milliseconds()}),
		})
		return nil
	}
}

func randomDuration(minDelay, maxDelay time.Duration) time.Duration {
	if minDelay <= 0 || maxDelay <= 0 {
		return 0
	}
	if maxDelay <= minDelay {
		return minDelay
	}

	span := int64(maxDelay - minDelay)
	n, err := cryptorand.Int(cryptorand.Reader, big.NewInt(span+1))
	if err != nil {
		return minDelay + time.Duration(time.Now().UnixNano()%(span+1))
	}
	return minDelay + time.Duration(n.Int64())
}
