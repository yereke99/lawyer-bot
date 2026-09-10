package handler

import (
	"lawyer-bot/internal/domain"

	"go.uber.org/zap"
)

func privateWhatsAppMessages(messages []domain.InboundMessage, log *zap.Logger) []domain.InboundMessage {
	if len(messages) == 0 {
		return nil
	}
	out := make([]domain.InboundMessage, 0, len(messages))
	for _, msg := range messages {
		if domain.IsPrivateWhatsAppChat(msg.WhatsAppUserID) {
			out = append(out, msg)
			continue
		}
		reason := "non_private_chat"
		if domain.IsGroupWhatsAppChat(msg.WhatsAppUserID) {
			reason = "group_chat"
		}
		log.Info("ignored incoming whatsapp message from non-private chat",
			zap.String("reason", reason),
			zap.String("chat_id", msg.WhatsAppUserID),
			zap.String("provider_message_id", msg.WhatsAppMessageID))
	}
	return out
}
