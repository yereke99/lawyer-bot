package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"lawyer-bot/internal/domain"
	"lawyer-bot/internal/repository"
	"lawyer-bot/traits/logger"
)

// Messenger is the ONE authoritative outbound WhatsApp path.
//
// Every message that leaves this application — an automatic AI reply, an
// automatic follow-up, a lead alert, and every manual message a consultant
// sends from the Admin CRM — goes through Send. There is deliberately no second
// integration: the provider client is injected once and nothing else in the
// codebase is allowed to call it.
//
// Send is responsible for four things that used to be scattered:
//   - persisting the outgoing row in the shared conversation model;
//   - the per-chat send lock, so two producers cannot interleave;
//   - the delivery audit trail;
//   - refreshing CRM activity timestamps.
type Messenger struct {
	messages *repository.MessageRepository
	crm      *repository.CRMRepository
	trace    *repository.TraceRepository
	wa       domain.WhatsAppClient
	files    domain.WhatsAppFileSender
	log      *zap.Logger
	dryRun   bool

	order  *chatSequencer
	notify func(userID int64)
}

// MessengerConfig configures the outbound layer.
type MessengerConfig struct {
	DryRun bool
}

// MessengerDeps groups the collaborators Messenger needs.
type MessengerDeps struct {
	Messages *repository.MessageRepository
	CRM      *repository.CRMRepository
	Trace    *repository.TraceRepository
	WhatsApp domain.WhatsAppClient
	Files    domain.WhatsAppFileSender
	Logger   *zap.Logger
}

// NewMessenger builds the outbound layer.
func NewMessenger(deps MessengerDeps, cfg MessengerConfig) *Messenger {
	log := deps.Logger
	if log == nil {
		log = zap.NewNop()
	}
	files := deps.Files
	if files == nil {
		// The provider client itself may implement file sending.
		if fs, ok := deps.WhatsApp.(domain.WhatsAppFileSender); ok {
			files = fs
		}
	}
	return &Messenger{
		messages: deps.Messages,
		crm:      deps.CRM,
		trace:    deps.Trace,
		wa:       deps.WhatsApp,
		files:    files,
		log:      log,
		dryRun:   cfg.DryRun,
		order:    newChatSequencer(),
	}
}

// OnSent registers a callback fired after a message is stored, used to push the
// new message to connected CRM browsers.
func (m *Messenger) OnSent(fn func(userID int64)) { m.notify = fn }

// SupportsFiles reports whether the configured provider can send media.
func (m *Messenger) SupportsFiles() bool { return m.files != nil }

// Outbound is one message to deliver to a client.
type Outbound struct {
	UserID    int64
	Recipient string
	Sender    domain.SenderType
	AdminID   int64
	Kind      string
	TraceID   string

	Text string

	// Media, when the message carries a file. Path is a server-side path that
	// was written by the validated upload handler; it never comes from a client.
	MediaPath string
	MediaName string
	MediaMime string
	MediaSize int64
	MediaType domain.MessageType
}

// SendResult reports what happened, including the stored message row.
type SendResult struct {
	MessageID  int64
	ProviderID string
}

// ErrNoFileSupport is returned when the configured provider cannot send media.
var ErrNoFileSupport = errors.New("configured whatsapp provider cannot send files")

// Send delivers one outbound message and records everything about it.
//
// The per-chat lock is the interlock that makes requirement "AI and consultant
// never answer at once" true at the transport level: even if both decide to
// speak in the same instant, their sends are serialised and both are stored in
// the order they actually left the server.
func (m *Messenger) Send(ctx context.Context, out Outbound) (SendResult, error) {
	if out.Recipient == "" {
		return SendResult{}, errors.New("recipient is required")
	}
	if strings.TrimSpace(out.Text) == "" && out.MediaPath == "" {
		return SendResult{}, errors.New("message has neither text nor media")
	}
	if out.MediaPath != "" && m.files == nil {
		return SendResult{}, ErrNoFileSupport
	}
	if out.Sender == "" {
		out.Sender = domain.SenderAI
	}
	if out.Kind == "" {
		out.Kind = domain.DeliveryKindReply
	}
	if out.TraceID == "" {
		out.TraceID = NewTraceID()
	}

	unlock, err := m.order.Lock(ctx, out.Recipient)
	if err != nil {
		return SendResult{}, err
	}
	defer unlock()

	log := m.log.With(
		zap.String("trace_id", out.TraceID),
		zap.Int64("client_id", out.UserID),
		zap.String("source", string(out.Sender)),
		logger.Phone("recipient", out.Recipient))

	msgType := domain.MessageText
	if out.MediaPath != "" {
		msgType = out.MediaType
		if msgType == "" {
			msgType = domain.MessageDocument
		}
	}

	stored := &domain.Message{
		UserID:         out.UserID,
		TraceID:        out.TraceID,
		MessageType:    msgType,
		Direction:      domain.DirectionOutgoing,
		Processed:      true,
		SenderType:     out.Sender,
		SenderAdminID:  out.AdminID,
		MediaPath:      out.MediaPath,
		MediaMime:      out.MediaMime,
		MediaName:      out.MediaName,
		MediaSize:      out.MediaSize,
		DeliveryStatus: "pending",
	}
	if out.MediaPath == "" {
		stored.Text = out.Text
	} else {
		stored.Caption = out.Text
	}

	messageID, storeErr := m.messages.Create(ctx, stored)
	if storeErr != nil {
		log.Warn("store outgoing message failed", zap.Error(storeErr))
	}

	delivery := domain.Delivery{
		UserID: out.UserID, MessageID: messageID, TraceID: out.TraceID,
		Recipient: out.Recipient, Kind: out.Kind, Attempts: 1,
	}

	if m.dryRun {
		delivery.Status = domain.DeliverySent
		delivery.ProviderMessageID = "dry-run"
		delivery.Attempts = 0
		m.recordDelivery(ctx, log, delivery)
		m.finish(ctx, log, out.UserID, messageID, domain.DeliverySent)
		log.Info("dry run: message not sent", logger.Preview("text", out.Text))
		return SendResult{MessageID: messageID, ProviderID: "dry-run"}, nil
	}

	var (
		res      domain.SendResult
		sendErr  error
		provider = "text"
	)
	if out.MediaPath != "" {
		provider = "file"
		res, sendErr = m.files.SendFile(ctx, out.Recipient, domain.OutgoingFile{
			Path:     out.MediaPath,
			FileName: out.MediaName,
			MimeType: out.MediaMime,
			Caption:  out.Text,
			Type:     msgType,
		})
	} else {
		res, sendErr = m.wa.SendText(ctx, out.Recipient, out.Text)
	}

	if sendErr != nil {
		delivery.Status = domain.DeliveryFailed
		delivery.Error = sendErr.Error()
		m.recordDelivery(ctx, log, delivery)
		if messageID != 0 {
			if err := m.messages.SetDeliveryStatus(ctx, messageID, domain.DeliveryFailed); err != nil {
				log.Warn("set delivery status failed", zap.Error(err))
			}
		}
		m.event(ctx, domain.TraceEvent{
			TraceID: out.TraceID, UserID: out.UserID, MessageID: messageID,
			Stage: domain.StageReplyFailed, Decision: domain.DecisionError,
			Reason: "whatsapp send failed",
			Detail: repository.Detail(map[string]any{
				"error": sendErr.Error(), "transport": provider, "source": string(out.Sender)}),
		})
		log.Error("whatsapp send failed", zap.String("transport", provider), zap.Error(sendErr))
		if m.notify != nil && out.UserID != 0 {
			m.notify(out.UserID)
		}
		return SendResult{MessageID: messageID}, sendErr
	}

	delivery.Status = domain.DeliverySent
	delivery.ProviderMessageID = res.MessageID
	m.recordDelivery(ctx, log, delivery)

	if messageID != 0 && res.MessageID != "" {
		if err := m.messages.SetProviderID(ctx, messageID, res.MessageID); err != nil {
			log.Warn("attach provider message id failed", zap.Error(err))
		}
	}
	m.finish(ctx, log, out.UserID, messageID, domain.DeliverySent)
	m.event(ctx, domain.TraceEvent{
		TraceID: out.TraceID, UserID: out.UserID, MessageID: messageID,
		Stage: domain.StageReplySent, Decision: domain.DecisionOK,
		Detail: repository.Detail(map[string]any{
			"transport": provider, "source": string(out.Sender), "kind": out.Kind}),
	})
	log.Info("message sent", zap.String("transport", provider), logger.Preview("text", out.Text))
	return SendResult{MessageID: messageID, ProviderID: res.MessageID}, nil
}

// SendRaw delivers a message to an address that is not a CRM client, such as
// the lead alert sent to the company's own consultant number.
func (m *Messenger) SendRaw(ctx context.Context, recipient, text string) (domain.SendResult, error) {
	if m.dryRun {
		return domain.SendResult{MessageID: "dry-run"}, nil
	}
	unlock, err := m.order.Lock(ctx, recipient)
	if err != nil {
		return domain.SendResult{}, err
	}
	defer unlock()
	return m.wa.SendText(ctx, recipient, text)
}

func (m *Messenger) finish(ctx context.Context, log *zap.Logger, userID, messageID int64, status string) {
	if messageID != 0 {
		if err := m.messages.SetDeliveryStatus(ctx, messageID, status); err != nil {
			log.Warn("set delivery status failed", zap.Error(err))
		}
	}
	if userID != 0 && m.crm != nil {
		if err := m.crm.TouchOutbound(ctx, userID, time.Now().UTC()); err != nil {
			log.Warn("touch outbound failed", zap.Error(err))
		}
	}
	if m.notify != nil && userID != 0 {
		m.notify(userID)
	}
}

func (m *Messenger) recordDelivery(ctx context.Context, log *zap.Logger, d domain.Delivery) {
	if m.trace == nil {
		return
	}
	if _, err := m.trace.Delivery(ctx, d); err != nil {
		log.Warn("record delivery failed", zap.Error(err))
	}
}

func (m *Messenger) event(ctx context.Context, e domain.TraceEvent) {
	if m.trace == nil {
		return
	}
	if err := m.trace.Event(ctx, e); err != nil {
		m.log.Warn("write trace event failed", zap.String("stage", e.Stage), zap.Error(err))
	}
}

// MessageTypeForMime maps a validated MIME type onto the conversation model's
// message kind, so the CRM renders an image as an image and a PDF as a file.
func MessageTypeForMime(mime string, voice bool) domain.MessageType {
	mime = strings.ToLower(strings.TrimSpace(mime))
	switch {
	case voice:
		return domain.MessageVoice
	case strings.HasPrefix(mime, "image/"):
		return domain.MessageImage
	case strings.HasPrefix(mime, "video/"):
		return domain.MessageVideo
	case strings.HasPrefix(mime, "audio/"):
		return domain.MessageAudio
	default:
		return domain.MessageDocument
	}
}

// DescribeMedia renders a short label for a media message in a notification or
// a summary, where the binary itself cannot be shown.
func DescribeMedia(kind domain.MessageType, name string) string {
	if name != "" {
		return fmt.Sprintf("[%s: %s]", kind, name)
	}
	return fmt.Sprintf("[%s]", kind)
}
