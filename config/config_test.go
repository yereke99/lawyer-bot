package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLoadReplyDelayDefaults(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("OPENAI_API_KEY", "test-openai")
	t.Setenv("WHATSAPP_TOKEN", "test-whatsapp")
	t.Setenv("WHATSAPP_PHONE_NUMBER_ID", "phone-id")
	t.Setenv("WHATSAPP_VERIFY_TOKEN", "verify")
	t.Setenv("DIANA_WHATSAPP_PHONE", "77009998877")
	t.Setenv("WHATSAPP_BOT_REPLY_DELAY_MIN_MS", "")
	t.Setenv("WHATSAPP_BOT_REPLY_DELAY_MAX_MS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.WhatsAppReplyDelayMin != 1500*time.Millisecond {
		t.Fatalf("min reply delay = %s, want 1500ms", cfg.WhatsAppReplyDelayMin)
	}
	if cfg.WhatsAppReplyDelayMax != 3000*time.Millisecond {
		t.Fatalf("max reply delay = %s, want 3000ms", cfg.WhatsAppReplyDelayMax)
	}
}

func TestValidateRejectsFixedPositiveReplyDelay(t *testing.T) {
	cfg := &Config{
		OpenAIAPIKey:          "test-openai",
		OpenAIModel:           "gpt-4o-mini",
		WhatsAppToken:         "test-whatsapp",
		WhatsAppPhoneNumberID: "phone-id",
		WhatsAppVerifyToken:   "verify",
		SQLitePath:            "test.db",
		AIMinConfidence:       0.75,
		WorkerCount:           1,
		QueueSize:             1,
		DianaWhatsAppPhone:    "77009998877",
		WhatsAppReplyDelayMin: 2 * time.Second,
		WhatsAppReplyDelayMax: 2 * time.Second,
	}

	if err := cfg.Validate(); err == nil {
		t.Fatal("fixed positive reply delay should be rejected")
	}
}
