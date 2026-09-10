package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lawyer-bot/internal/domain"
)

func TestGreenClientSendTextUsesNativeSendMessage(t *testing.T) {
	var captured map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/waInstance123/sendMessage/token" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"idMessage":"out-1"}`))
	}))
	defer srv.Close()

	client := NewGreen(GreenOptions{IDInstance: "123", TokenInstance: "token", BaseURL: srv.URL})
	got, err := client.SendText(context.Background(), "77015551234", "Здравствуйте")
	if err != nil {
		t.Fatalf("send text: %v", err)
	}
	if got.MessageID != "out-1" {
		t.Fatalf("message id = %q", got.MessageID)
	}
	if captured["chatId"] != "77015551234@c.us" {
		t.Fatalf("chatId = %q", captured["chatId"])
	}
	if captured["message"] != "Здравствуйте" {
		t.Fatalf("message = %q", captured["message"])
	}
}

// A group destination is refused inside the client, before a request is built.
// The provider must never see it, so the test server fails if it is reached.
func TestGreenClientRejectsGroupRecipientBeforeAnyRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the provider was contacted for a group destination: %s", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewGreen(GreenOptions{IDInstance: "123", TokenInstance: "token", BaseURL: srv.URL})
	group := "120363000000000000@g.us"

	if _, err := client.SendText(context.Background(), group, "Здравствуйте"); !errors.Is(err, domain.ErrWhatsAppGroupChat) {
		t.Fatalf("SendText must refuse a group recipient, got %v", err)
	}

	path := filepath.Join(t.TempDir(), "offer.pdf")
	if err := os.WriteFile(path, []byte("%PDF-1.4"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := client.SendFile(context.Background(), group, domain.OutgoingFile{
		Path: path, FileName: "offer.pdf", MimeType: "application/pdf",
	}); !errors.Is(err, domain.ErrWhatsAppGroupChat) {
		t.Fatalf("SendFile must refuse a group recipient, got %v", err)
	}

	// A broadcast list is not a private chat either.
	if _, err := client.SendText(context.Background(), "status@broadcast", "Здравствуйте"); !errors.Is(err, domain.ErrWhatsAppNonPrivateChat) {
		t.Fatalf("SendText must refuse a broadcast destination, got %v", err)
	}
}

func TestGreenClientReceiveAndDeleteNotification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/waInstance123/receiveNotification/token":
			if r.URL.Query().Get("receiveTimeout") != "7" {
				t.Fatalf("receiveTimeout = %q", r.URL.Query().Get("receiveTimeout"))
			}
			_, _ = w.Write([]byte(`{"receiptId":42,"body":{"typeWebhook":"incomingMessageReceived","idMessage":"in-1"}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/waInstance123/deleteNotification/token/42":
			_, _ = w.Write([]byte(`{"result":true}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()

	client := NewGreen(GreenOptions{IDInstance: "123", TokenInstance: "token", BaseURL: srv.URL})
	notification, err := client.ReceiveNotification(context.Background(), 7)
	if err != nil {
		t.Fatalf("receive notification: %v", err)
	}
	if notification == nil || notification.ReceiptID != 42 {
		t.Fatalf("notification = %+v", notification)
	}
	if !strings.Contains(string(notification.Body), `"idMessage":"in-1"`) {
		t.Fatalf("body = %s", notification.Body)
	}
	if err := client.DeleteNotification(context.Background(), notification.ReceiptID); err != nil {
		t.Fatalf("delete notification: %v", err)
	}
}

func TestGreenClientReceiveNotificationEmptyQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`null`))
	}))
	defer srv.Close()

	client := NewGreen(GreenOptions{IDInstance: "123", TokenInstance: "token", BaseURL: srv.URL})
	notification, err := client.ReceiveNotification(context.Background(), 5)
	if err != nil {
		t.Fatalf("receive notification: %v", err)
	}
	if notification != nil {
		t.Fatalf("empty queue should return nil, got %+v", notification)
	}
}
