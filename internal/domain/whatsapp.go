package domain

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrWhatsAppGroupChat is returned before any outbound path can send to a
	// WhatsApp group.
	ErrWhatsAppGroupChat = errors.New("whatsapp group chats are not allowed")
	// ErrWhatsAppNonPrivateChat covers every other chat kind the bot must not
	// talk to, such as broadcast lists and channels.
	ErrWhatsAppNonPrivateChat = errors.New("whatsapp chat is not a private chat")
)

// IsGroupWhatsAppChat reports whether id is a WhatsApp group chat identifier.
// Green API group chats use the @g.us suffix.
func IsGroupWhatsAppChat(id string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(id)), "@g.us")
}

// IsPrivateWhatsAppChat reports whether id is an individual WhatsApp chat
// identifier supported by the providers wired in this project.
func IsPrivateWhatsAppChat(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" || IsGroupWhatsAppChat(id) {
		return false
	}
	if strings.HasSuffix(id, "@c.us") {
		return true
	}
	if strings.Contains(id, "@") {
		return false
	}
	return true
}

// ValidatePrivateWhatsAppRecipient rejects group and non-private destinations
// before a provider request is built.
func ValidatePrivateWhatsAppRecipient(recipient string) error {
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		return fmt.Errorf("recipient is required")
	}
	if IsGroupWhatsAppChat(recipient) {
		return ErrWhatsAppGroupChat
	}
	if !IsPrivateWhatsAppChat(recipient) {
		return fmt.Errorf("%w: %s", ErrWhatsAppNonPrivateChat, recipient)
	}
	return nil
}
