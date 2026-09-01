package whatsapp

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"lawyer-bot/internal/domain"
)

// webhookPayload mirrors the WhatsApp Cloud API webhook body. Only the fields
// the bot actually uses are declared; unknown fields are ignored so provider
// additions never break parsing.
type webhookPayload struct {
	Object string `json:"object"`
	Entry  []struct {
		ID      string `json:"id"`
		Changes []struct {
			Field string `json:"field"`
			Value struct {
				MessagingProduct string `json:"messaging_product"`
				Metadata         struct {
					DisplayPhoneNumber string `json:"display_phone_number"`
					PhoneNumberID      string `json:"phone_number_id"`
				} `json:"metadata"`
				Contacts []struct {
					WaID    string `json:"wa_id"`
					Profile struct {
						Name string `json:"name"`
					} `json:"profile"`
				} `json:"contacts"`
				Messages []webhookMessage `json:"messages"`
				Statuses []struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"statuses"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

type webhookMessage struct {
	From      string `json:"from"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`

	Text struct {
		Body string `json:"body"`
	} `json:"text"`

	Image    *mediaObject `json:"image"`
	Video    *mediaObject `json:"video"`
	Audio    *mediaObject `json:"audio"`
	Document *mediaObject `json:"document"`
	Sticker  *mediaObject `json:"sticker"`

	Location *struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Name      string  `json:"name"`
		Address   string  `json:"address"`
	} `json:"location"`

	Contacts []struct {
		Name struct {
			FormattedName string `json:"formatted_name"`
		} `json:"name"`
		Phones []struct {
			Phone string `json:"phone"`
			WaID  string `json:"wa_id"`
		} `json:"phones"`
	} `json:"contacts"`

	Button *struct {
		Text    string `json:"text"`
		Payload string `json:"payload"`
	} `json:"button"`

	Interactive *struct {
		Type        string `json:"type"`
		ButtonReply *struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"button_reply"`
		ListReply *struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"list_reply"`
	} `json:"interactive"`

	Referral *struct {
		SourceType string `json:"source_type"`
		SourceID   string `json:"source_id"`
		Headline   string `json:"headline"`
	} `json:"referral"`
}

type mediaObject struct {
	ID       string `json:"id"`
	MimeType string `json:"mime_type"`
	SHA256   string `json:"sha256"`
	Caption  string `json:"caption"`
	Filename string `json:"filename"`
	Voice    bool   `json:"voice"`
}

type greenWebhookPayload struct {
	TypeWebhook  string `json:"typeWebhook"`
	InstanceData struct {
		IDInstance   int64  `json:"idInstance"`
		WID          string `json:"wid"`
		TypeInstance string `json:"typeInstance"`
	} `json:"instanceData"`
	Timestamp  int64  `json:"timestamp"`
	IDMessage  string `json:"idMessage"`
	SenderData struct {
		ChatID            string `json:"chatId"`
		Sender            string `json:"sender"`
		ChatName          string `json:"chatName"`
		SenderName        string `json:"senderName"`
		SenderContactName string `json:"senderContactName"`
	} `json:"senderData"`
	MessageData struct {
		TypeMessage     string `json:"typeMessage"`
		TextMessageData struct {
			TextMessage string `json:"textMessage"`
		} `json:"textMessageData"`
		ExtendedTextMessageData struct {
			Text string `json:"text"`
		} `json:"extendedTextMessageData"`
		FileMessageData struct {
			DownloadURL string `json:"downloadUrl"`
			Caption     string `json:"caption"`
			FileName    string `json:"fileName"`
			MimeType    string `json:"mimeType"`
		} `json:"fileMessageData"`
		LocationMessageData struct {
			NameLocation string `json:"nameLocation"`
			Address      string `json:"address"`
		} `json:"locationMessageData"`
		ContactMessageData struct {
			DisplayName string `json:"displayName"`
			VCard       string `json:"vcard"`
		} `json:"contactMessageData"`
	} `json:"messageData"`
}

// ParseWebhook converts a raw webhook body into provider-independent messages.
//
// Delivery-status callbacks and other non-message events yield an empty slice:
// the bot only ever reacts to real incoming messages.
func ParseWebhook(body []byte) ([]domain.InboundMessage, error) {
	var probe struct {
		TypeWebhook string `json:"typeWebhook"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("decode webhook payload: %w", err)
	}
	if probe.TypeWebhook != "" {
		return parseGreenWebhook(body)
	}

	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode webhook payload: %w", err)
	}

	var out []domain.InboundMessage
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			value := change.Value

			// Contact profiles arrive alongside the messages.
			names := make(map[string]string, len(value.Contacts))
			for _, c := range value.Contacts {
				names[c.WaID] = c.Profile.Name
			}

			for _, m := range value.Messages {
				out = append(out, convertMessage(m, names[m.From]))
			}
		}
	}
	return out, nil
}

func parseGreenWebhook(body []byte) ([]domain.InboundMessage, error) {
	var payload greenWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode green api webhook payload: %w", err)
	}
	if payload.TypeWebhook != "incomingMessageReceived" {
		return nil, nil
	}
	return []domain.InboundMessage{convertGreenMessage(payload)}, nil
}

func convertMessage(m webhookMessage, displayName string) domain.InboundMessage {
	in := domain.InboundMessage{
		WhatsAppUserID:    m.From,
		PhoneNumber:       m.From, // wa_id is the customer's phone number in E.164 digits
		DisplayName:       displayName,
		WhatsAppMessageID: m.ID,
		Timestamp:         parseTimestamp(m.Timestamp),
		Source:            domain.SourceWhatsApp,
	}

	if m.Referral != nil && m.Referral.SourceType != "" {
		in.Source = referralSource(m.Referral.SourceType)
	}

	switch m.Type {
	case "text":
		in.MessageType = domain.MessageText
		in.Text = m.Text.Body

	case "image":
		in.MessageType = domain.MessageImage
		applyMedia(&in, m.Image)
	case "video":
		in.MessageType = domain.MessageVideo
		applyMedia(&in, m.Video)
	case "audio":
		in.MessageType = domain.MessageAudio
		if m.Audio != nil && m.Audio.Voice {
			in.MessageType = domain.MessageVoice
		}
		applyMedia(&in, m.Audio)
	case "document":
		in.MessageType = domain.MessageDocument
		applyMedia(&in, m.Document)
	case "sticker":
		in.MessageType = domain.MessageSticker
		applyMedia(&in, m.Sticker)

	case "location":
		in.MessageType = domain.MessageLocation
		if m.Location != nil {
			in.Caption = joinNonEmpty(" ", m.Location.Name, m.Location.Address)
		}

	case "contacts":
		in.MessageType = domain.MessageContact
		if len(m.Contacts) > 0 {
			c := m.Contacts[0]
			phone := ""
			if len(c.Phones) > 0 {
				phone = c.Phones[0].Phone
			}
			in.Caption = joinNonEmpty(" ", c.Name.FormattedName, phone)
		}

	case "button":
		in.MessageType = domain.MessageButton
		if m.Button != nil {
			in.Text = m.Button.Text
		}

	case "interactive":
		in.MessageType = domain.MessageButton
		if m.Interactive != nil {
			switch {
			case m.Interactive.ButtonReply != nil:
				in.Text = m.Interactive.ButtonReply.Title
			case m.Interactive.ListReply != nil:
				in.Text = m.Interactive.ListReply.Title
			}
		}

	default:
		in.MessageType = domain.MessageUnknown
	}

	return in
}

func convertGreenMessage(m greenWebhookPayload) domain.InboundMessage {
	displayName := firstNonEmpty(m.SenderData.SenderContactName, m.SenderData.SenderName, m.SenderData.ChatName)
	chatID := firstNonEmpty(m.SenderData.ChatID, m.SenderData.Sender)
	in := domain.InboundMessage{
		WhatsAppUserID:    chatID,
		PhoneNumber:       greenPhoneNumber(firstNonEmpty(m.SenderData.Sender, chatID)),
		DisplayName:       displayName,
		WhatsAppMessageID: m.IDMessage,
		Timestamp:         time.Unix(m.Timestamp, 0).UTC(),
		Source:            domain.SourceWhatsApp,
	}
	if m.Timestamp == 0 {
		in.Timestamp = time.Now().UTC()
	}

	switch m.MessageData.TypeMessage {
	case "textMessage":
		in.MessageType = domain.MessageText
		in.Text = m.MessageData.TextMessageData.TextMessage
	case "extendedTextMessage", "quotedMessage":
		in.MessageType = domain.MessageText
		in.Text = m.MessageData.ExtendedTextMessageData.Text
	case "imageMessage":
		in.MessageType = domain.MessageImage
		applyGreenFile(&in, m)
	case "videoMessage":
		in.MessageType = domain.MessageVideo
		applyGreenFile(&in, m)
	case "audioMessage":
		in.MessageType = domain.MessageAudio
		applyGreenFile(&in, m)
	case "documentMessage":
		in.MessageType = domain.MessageDocument
		applyGreenFile(&in, m)
	case "stickerMessage":
		in.MessageType = domain.MessageSticker
		applyGreenFile(&in, m)
	case "locationMessage":
		in.MessageType = domain.MessageLocation
		in.Caption = joinNonEmpty(" ", m.MessageData.LocationMessageData.NameLocation,
			m.MessageData.LocationMessageData.Address)
	case "contactMessage":
		in.MessageType = domain.MessageContact
		in.Caption = joinNonEmpty(" ", m.MessageData.ContactMessageData.DisplayName,
			m.MessageData.ContactMessageData.VCard)
	default:
		in.MessageType = domain.MessageUnknown
	}

	return in
}

func applyMedia(in *domain.InboundMessage, m *mediaObject) {
	if m == nil {
		return
	}
	in.MediaID = m.ID
	in.MimeType = m.MimeType
	in.SHA256 = m.SHA256
	in.Caption = m.Caption
	in.Filename = m.Filename
	in.Voice = m.Voice
}

func applyGreenFile(in *domain.InboundMessage, m greenWebhookPayload) {
	in.MediaID = m.IDMessage
	in.MimeType = m.MessageData.FileMessageData.MimeType
	in.Caption = m.MessageData.FileMessageData.Caption
	in.Filename = m.MessageData.FileMessageData.FileName
}

func referralSource(sourceType string) string {
	switch sourceType {
	case "ad":
		return domain.SourceAdvertising
	case "post":
		return domain.SourceInstagram
	default:
		return domain.SourceWhatsApp
	}
}

func parseTimestamp(ts string) time.Time {
	if ts == "" {
		return time.Now().UTC()
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return time.Now().UTC()
	}
	return time.Unix(sec, 0).UTC()
}

func joinNonEmpty(sep string, parts ...string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += sep
		}
		out += p
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func greenPhoneNumber(value string) string {
	value = strings.TrimSpace(value)
	if before, _, ok := strings.Cut(value, "@"); ok {
		value = before
	}

	var b strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
