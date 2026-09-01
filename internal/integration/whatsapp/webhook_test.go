package whatsapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"lawyer-bot/internal/domain"
)

func TestParseTextMessage(t *testing.T) {
	body := []byte(`{
	  "object": "whatsapp_business_account",
	  "entry": [{
	    "id": "1234",
	    "changes": [{
	      "field": "messages",
	      "value": {
	        "messaging_product": "whatsapp",
	        "metadata": {"display_phone_number": "77010000000", "phone_number_id": "999"},
	        "contacts": [{"wa_id": "77015551234", "profile": {"name": "Аида"}}],
	        "messages": [{
	          "from": "77015551234",
	          "id": "wamid.HBgL",
	          "timestamp": "1700000000",
	          "type": "text",
	          "text": {"body": "Какие у вас услуги?"}
	        }]
	      }
	    }]
	  }]
	}`)

	got, err := ParseWebhook(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 message, got %d", len(got))
	}

	m := got[0]
	if m.WhatsAppUserID != "77015551234" {
		t.Errorf("user id = %q", m.WhatsAppUserID)
	}
	if m.PhoneNumber != "77015551234" {
		t.Errorf("phone = %q", m.PhoneNumber)
	}
	if m.DisplayName != "Аида" {
		t.Errorf("display name = %q, want the contact profile name", m.DisplayName)
	}
	if m.WhatsAppMessageID != "wamid.HBgL" {
		t.Errorf("message id = %q", m.WhatsAppMessageID)
	}
	if m.MessageType != domain.MessageText {
		t.Errorf("type = %q, want text", m.MessageType)
	}
	if m.Text != "Какие у вас услуги?" {
		t.Errorf("text = %q", m.Text)
	}
	if m.Timestamp.IsZero() {
		t.Error("timestamp should be parsed")
	}
}

func TestParseMediaMessages(t *testing.T) {
	cases := []struct {
		name     string
		json     string
		wantType domain.MessageType
		wantCap  string
	}{
		{
			name:     "image with caption",
			json:     `{"from":"7701","id":"m1","type":"image","image":{"id":"media-1","mime_type":"image/jpeg","sha256":"abc","caption":"Вот договор"}}`,
			wantType: domain.MessageImage,
			wantCap:  "Вот договор",
		},
		{
			name:     "voice message",
			json:     `{"from":"7701","id":"m2","type":"audio","audio":{"id":"media-2","mime_type":"audio/ogg","voice":true}}`,
			wantType: domain.MessageVoice,
		},
		{
			name:     "plain audio",
			json:     `{"from":"7701","id":"m3","type":"audio","audio":{"id":"media-3","mime_type":"audio/mpeg","voice":false}}`,
			wantType: domain.MessageAudio,
		},
		{
			name:     "document",
			json:     `{"from":"7701","id":"m4","type":"document","document":{"id":"media-4","filename":"contract.pdf","mime_type":"application/pdf"}}`,
			wantType: domain.MessageDocument,
		},
		{
			name:     "video",
			json:     `{"from":"7701","id":"m5","type":"video","video":{"id":"media-5","mime_type":"video/mp4"}}`,
			wantType: domain.MessageVideo,
		},
		{
			name:     "sticker",
			json:     `{"from":"7701","id":"m6","type":"sticker","sticker":{"id":"media-6","mime_type":"image/webp"}}`,
			wantType: domain.MessageSticker,
		},
		{
			name:     "location",
			json:     `{"from":"7701","id":"m7","type":"location","location":{"latitude":43.2,"longitude":76.9,"name":"Офис","address":"Алматы"}}`,
			wantType: domain.MessageLocation,
			wantCap:  "Офис Алматы",
		},
		{
			name:     "contact card",
			json:     `{"from":"7701","id":"m8","type":"contacts","contacts":[{"name":{"formatted_name":"Диана"},"phones":[{"phone":"+77015551234"}]}]}`,
			wantType: domain.MessageContact,
			wantCap:  "Диана +77015551234",
		},
		{
			name:     "interactive button reply",
			json:     `{"from":"7701","id":"m9","type":"interactive","interactive":{"type":"button_reply","button_reply":{"id":"b1","title":"Товарный знак"}}}`,
			wantType: domain.MessageButton,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"entry":[{"changes":[{"value":{"messages":[` + tc.json + `]}}]}]}`)
			got, err := ParseWebhook(body)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("want 1 message, got %d", len(got))
			}
			if got[0].MessageType != tc.wantType {
				t.Errorf("type = %q, want %q", got[0].MessageType, tc.wantType)
			}
			if tc.wantCap != "" && got[0].Caption != tc.wantCap {
				t.Errorf("caption = %q, want %q", got[0].Caption, tc.wantCap)
			}
		})
	}
}

// Delivery receipts must not be mistaken for customer messages: reacting to
// them would mean the bot starts a conversation.
func TestStatusCallbackYieldsNoMessages(t *testing.T) {
	body := []byte(`{
	  "entry": [{"changes": [{"value": {
	    "statuses": [{"id": "wamid.x", "status": "delivered"}]
	  }}]}]
	}`)

	got, err := ParseWebhook(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("status callbacks must produce no messages, got %d", len(got))
	}
}

func TestAdReferralBecomesLeadSource(t *testing.T) {
	body := []byte(`{"entry":[{"changes":[{"value":{"messages":[
	  {"from":"7701","id":"m1","type":"text","text":{"body":"Нужен юрист"},
	   "referral":{"source_type":"ad","source_id":"camp-1","headline":"Юридические услуги"}}
	]}}]}]}`)

	got, err := ParseWebhook(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got[0].Source != domain.SourceAdvertising {
		t.Fatalf("source = %q, want %q", got[0].Source, domain.SourceAdvertising)
	}
}

func TestParseGreenAPITextMessage(t *testing.T) {
	body := []byte(`{
	  "typeWebhook": "incomingMessageReceived",
	  "timestamp": 1700000000,
	  "idMessage": "green-msg-1",
	  "senderData": {
	    "chatId": "77015551234@c.us",
	    "sender": "77015551234@c.us",
	    "senderName": "Аида"
	  },
	  "messageData": {
	    "typeMessage": "textMessage",
	    "textMessageData": {"textMessage": "Нужен юрист по договору"}
	  }
	}`)

	got, err := ParseWebhook(body)
	if err != nil {
		t.Fatalf("parse green api payload: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 message, got %d", len(got))
	}
	m := got[0]
	if m.WhatsAppUserID != "77015551234@c.us" {
		t.Fatalf("user id = %q", m.WhatsAppUserID)
	}
	if m.PhoneNumber != "77015551234" {
		t.Fatalf("phone = %q", m.PhoneNumber)
	}
	if m.WhatsAppMessageID != "green-msg-1" {
		t.Fatalf("message id = %q", m.WhatsAppMessageID)
	}
	if m.MessageType != domain.MessageText || m.Text != "Нужен юрист по договору" {
		t.Fatalf("message = %q %q", m.MessageType, m.Text)
	}
}

func TestParseGreenAPINonIncomingEventYieldsNoMessages(t *testing.T) {
	got, err := ParseWebhook([]byte(`{"typeWebhook":"outgoingMessageReceived"}`))
	if err != nil {
		t.Fatalf("parse green api non incoming: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("non incoming event should not produce messages, got %d", len(got))
	}
}

func TestMalformedPayloadIsRejected(t *testing.T) {
	if _, err := ParseWebhook([]byte(`{not json`)); err == nil {
		t.Fatal("malformed JSON should return an error")
	}
}

func TestSignatureVerification(t *testing.T) {
	secret := "app-secret"
	body := []byte(`{"entry":[]}`)

	// Computed with the same HMAC the provider uses.
	valid := signature(secret, body)
	if !VerifySignature(secret, body, valid) {
		t.Fatal("a correct signature should verify")
	}
	if VerifySignature(secret, body, "sha256=deadbeef") {
		t.Fatal("a wrong signature must be rejected")
	}
	if VerifySignature(secret, body, "") {
		t.Fatal("a missing signature must be rejected when a secret is configured")
	}
	if VerifySignature(secret, []byte(`{"entry":[1]}`), valid) {
		t.Fatal("a tampered body must be rejected")
	}
	// With no secret configured, verification is disabled for local development.
	if !VerifySignature("", body, "") {
		t.Fatal("verification should be skipped when no secret is configured")
	}
}

func signature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
