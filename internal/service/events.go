package service

import (
	"sync"
	"sync/atomic"
	"time"
)

// EventHub broadcasts CRM changes to connected admin browsers.
//
// This is strictly an internal, admin-side concern: it has nothing to do with
// how WhatsApp messages arrive. The inbound transport stays exactly what it was
// — Green API native polling — and this hub only pushes what already happened
// to whoever has the CRM open.
//
// Delivery is best effort and never blocks a producer: a subscriber whose
// buffer is full simply misses an event and re-syncs on its next poll of the
// conversation, which is the correct trade for a UI notification.
type EventHub struct {
	mu          sync.RWMutex
	subscribers map[uint64]*subscriber
	nextID      atomic.Uint64
}

type subscriber struct {
	ch chan CRMEvent
	// client scopes a subscriber to one conversation; zero means every change.
	client int64
}

// CRMEvent is one change worth telling the CRM about.
type CRMEvent struct {
	Type     string    `json:"type"`
	ClientID int64     `json:"client_id,omitempty"`
	At       time.Time `json:"at"`
}

// Event types.
const (
	EventClientChanged = "client.changed"
	EventPing          = "ping"
)

// NewEventHub builds an EventHub.
func NewEventHub() *EventHub {
	return &EventHub{subscribers: make(map[uint64]*subscriber)}
}

// Subscribe registers a listener. clientID of zero receives every event.
// The returned cancel function must always be called.
func (h *EventHub) Subscribe(clientID int64) (<-chan CRMEvent, func()) {
	id := h.nextID.Add(1)
	sub := &subscriber{ch: make(chan CRMEvent, 16), client: clientID}

	h.mu.Lock()
	h.subscribers[id] = sub
	h.mu.Unlock()

	return sub.ch, func() {
		h.mu.Lock()
		if existing, ok := h.subscribers[id]; ok {
			delete(h.subscribers, id)
			close(existing.ch)
		}
		h.mu.Unlock()
	}
}

// ClientChanged announces that one conversation changed.
func (h *EventHub) ClientChanged(clientID int64) {
	h.publish(CRMEvent{Type: EventClientChanged, ClientID: clientID, At: time.Now().UTC()})
}

func (h *EventHub) publish(event CRMEvent) {
	if h == nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, sub := range h.subscribers {
		if sub.client != 0 && sub.client != event.ClientID {
			continue
		}
		select {
		case sub.ch <- event:
		default:
			// Slow consumer: drop rather than stall the pipeline.
		}
	}
}

// Subscribers reports the number of connected listeners, for health output.
func (h *EventHub) Subscribers() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subscribers)
}
