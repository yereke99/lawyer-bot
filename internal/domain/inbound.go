package domain

import (
	"context"
	"io"
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

	// Media metadata. MediaURL is the provider's own download link when it
	// supplies one; the binary is fetched by the media service, never inline in
	// the message parser.
	MediaID  string
	MediaURL string
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

// OutgoingFile is a validated, server-stored file ready to be sent to a client.
// Path always points at a file the application wrote itself; it is never a value
// that came from a browser or from the model.
type OutgoingFile struct {
	Path     string
	FileName string
	MimeType string
	Caption  string
	Type     MessageType
}

// WhatsAppFileSender is the optional media half of a provider integration.
// A provider that cannot upload files simply does not implement it, and the
// Admin CRM reports media sending as unavailable instead of failing silently.
type WhatsAppFileSender interface {
	SendFile(ctx context.Context, recipient string, file OutgoingFile) (SendResult, error)
}

// WhatsAppFileFetcher is the optional inbound media half: it resolves a stored
// media reference into bytes the application can persist.
type WhatsAppFileFetcher interface {
	DownloadFile(ctx context.Context, ref MediaRef) (io.ReadCloser, string, error)
}

// MediaRef is everything a provider needs to fetch one inbound media object.
type MediaRef struct {
	MediaID   string
	URL       string
	ChatID    string
	MessageID string
	MimeType  string
}
