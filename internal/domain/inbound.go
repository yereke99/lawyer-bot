package domain

import (
	"context"
	"time"
)

// InboundMessage is a provider-independent incoming WhatsApp message. The
// pipeline works only with this type, so swapping the WhatsApp provider does
// not reach the business logic.
type InboundMessage struct {
	TraceID           string
	WhatsAppUserID    string
	PhoneNumber       string
	DisplayName       string
	WhatsAppMessageID string
	MessageType       MessageType
	Text              string
	Caption           string
	Timestamp         time.Time

	// Media metadata. The binary payload is never downloaded automatically.
	MediaID  string
	MimeType string
	SHA256   string
	Filename string
	Voice    bool

	// Source is the acquisition channel, taken from an ad referral when the
	// provider supplies one.
	Source string
}

// Content is the text worth classifying.
func (m InboundMessage) Content() string {
	if m.Text != "" {
		return m.Text
	}
	return m.Caption
}

// SendResult is what a provider reports back after accepting a message.
type SendResult struct {
	MessageID string
}

// WhatsAppClient abstracts the messaging provider.
//
// Note on the signature: sends return the provider message ID rather than only
// an error, because every outbound message is recorded in the delivery audit
// trail and must be linkable to the provider's own record.
type WhatsAppClient interface {
	SendText(ctx context.Context, recipient string, text string) (SendResult, error)
	SendMedia(ctx context.Context, recipient string, mediaID string, caption string) (SendResult, error)
}
