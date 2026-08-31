package service

import (
	"context"
	"strings"
	"sync"

	"lawyer-bot/internal/domain"
)

// chatSequencer serializes work for one WhatsApp chat while allowing unrelated
// chats to move independently.
type chatSequencer struct {
	locks sync.Map
}

type chatLock struct {
	ch chan struct{}
}

func newChatSequencer() *chatSequencer {
	return &chatSequencer{}
}

func (s *chatSequencer) Lock(ctx context.Context, key string) (func(), error) {
	if s == nil || key == "" {
		return func() {}, nil
	}
	raw, _ := s.locks.LoadOrStore(key, &chatLock{ch: make(chan struct{}, 1)})
	lock := raw.(*chatLock)

	select {
	case lock.ch <- struct{}{}:
		return func() { <-lock.ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func inboundOrderKey(in domain.InboundMessage) string {
	if key := strings.TrimSpace(in.WhatsAppUserID); key != "" {
		return key
	}
	return strings.TrimSpace(in.PhoneNumber)
}
