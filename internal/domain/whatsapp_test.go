package domain

import (
	"errors"
	"testing"
)

func TestWhatsAppChatClassification(t *testing.T) {
	cases := []struct {
		id      string
		group   bool
		private bool
	}{
		{"77015551234", false, true},
		{"77015551234@c.us", false, true},
		{"77015551234@C.US", false, true},
		{"120363000000000000@g.us", true, false},
		{" 120363000000000000@G.US ", true, false},
		{"status@broadcast", false, false},
		{"120363000000000000@newsletter", false, false},
		{"", false, false},
	}
	for _, tc := range cases {
		if got := IsGroupWhatsAppChat(tc.id); got != tc.group {
			t.Errorf("IsGroupWhatsAppChat(%q) = %v, want %v", tc.id, got, tc.group)
		}
		if got := IsPrivateWhatsAppChat(tc.id); got != tc.private {
			t.Errorf("IsPrivateWhatsAppChat(%q) = %v, want %v", tc.id, got, tc.private)
		}
	}
}

func TestValidatePrivateWhatsAppRecipient(t *testing.T) {
	if err := ValidatePrivateWhatsAppRecipient("77015551234@c.us"); err != nil {
		t.Fatalf("a private chat must be accepted: %v", err)
	}
	if err := ValidatePrivateWhatsAppRecipient("120363000000000000@g.us"); !errors.Is(err, ErrWhatsAppGroupChat) {
		t.Fatalf("a group must be refused as a group, got %v", err)
	}
	if err := ValidatePrivateWhatsAppRecipient("status@broadcast"); !errors.Is(err, ErrWhatsAppNonPrivateChat) {
		t.Fatalf("a broadcast list must be refused, got %v", err)
	}
	if err := ValidatePrivateWhatsAppRecipient("   "); err == nil {
		t.Fatal("an empty recipient must be refused")
	}
}
