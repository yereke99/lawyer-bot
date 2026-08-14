package domain

import "time"

// Direction distinguishes user messages from bot messages.
type Direction string

const (
	DirectionIncoming Direction = "incoming"
	DirectionOutgoing Direction = "outgoing"
)

// MessageType mirrors the WhatsApp message kinds the bot must accept.
type MessageType string

const (
	MessageText     MessageType = "text"
	MessageImage    MessageType = "image"
	MessageVideo    MessageType = "video"
	MessageAudio    MessageType = "audio"
	MessageVoice    MessageType = "voice"
	MessageDocument MessageType = "document"
	MessageSticker  MessageType = "sticker"
	MessageLocation MessageType = "location"
	MessageContact  MessageType = "contact"
	MessageButton   MessageType = "button"
	MessageUnknown  MessageType = "unknown"
)

// Analyzable reports whether the message carries text that is worth sending to
// the language model. Media is stored but never uploaded automatically.
func (t MessageType) Analyzable() bool {
	switch t {
	case MessageText, MessageButton:
		return true
	}
	return false
}

// Message is one stored WhatsApp message in either direction.
type Message struct {
	ID                int64
	UserID            int64
	WhatsAppMessageID string
	TraceID           string
	MessageType       MessageType
	Text              string
	MediaID           string
	Caption           string
	Direction         Direction
	Processed         bool
	AIProcessed       bool
	AIIntent          string
	AIConfidence      float64
	BotResponded      bool
	CreatedAt         time.Time
}

// Content returns the text the classifier should look at: the body for text
// messages, the caption for media.
func (m Message) Content() string {
	if m.Text != "" {
		return m.Text
	}
	return m.Caption
}

// MediaAsset stores metadata about received media. The binary payload is not
// downloaded by default; the record keeps the door open for future multimodal
// processing.
type MediaAsset struct {
	ID         int64
	UserID     int64
	MessageID  int64
	MediaID    string
	MimeType   string
	SHA256     string
	FileSize   int64
	Filename   string
	Caption    string
	Voice      bool
	Downloaded bool
	LocalPath  string
	CreatedAt  time.Time
}

// Delivery records the outcome of an outbound send attempt, so a failing
// WhatsApp provider never silently loses a lead.
type Delivery struct {
	ID                int64
	UserID            int64
	MessageID         int64
	TraceID           string
	Recipient         string
	Kind              string // reply | notification
	Status            string // sent | failed
	Attempts          int
	ProviderMessageID string
	Error             string
	CreatedAt         time.Time
}

// Delivery statuses.
const (
	DeliverySent   = "sent"
	DeliveryFailed = "failed"
)

// Delivery kinds.
const (
	DeliveryKindReply        = "reply"
	DeliveryKindNotification = "notification"
)

// WebhookEvent is the raw provider payload, kept for full auditability and for
// replaying traffic during debugging.
type WebhookEvent struct {
	ID           int64
	TraceID      string
	Provider     string
	Signature    string
	Payload      string
	MessageCount int
	Status       string // received | processed | rejected | duplicate
	Error        string
	CreatedAt    time.Time
}

// Notification is an alert sent to Diana or an admin.
type Notification struct {
	ID        int64
	LeadID    int64
	UserID    int64
	TraceID   string
	Channel   string // whatsapp
	Recipient string
	Body      string
	Status    string // sent | failed
	Error     string
	CreatedAt time.Time
}
