package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLoadReplyDelayDefaults(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("WHATSAPP_PROVIDER", "meta")
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
	if !cfg.LLMAgentReplies {
		t.Fatal("LLM agent replies should be enabled by default")
	}
}

func TestLoadGreenAPIConfigAllowsNativePollingWithoutMetaWebhook(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("WHATSAPP_PROVIDER", "greenapi")
	t.Setenv("OPENAI_API_KEY", "test-openai")
	t.Setenv("GREEN_API_ID_INSTANCE", "710522710085")
	t.Setenv("GREEN_API_TOKEN_INSTANCE", "test-token")
	t.Setenv("GREEN_API_API_URL", "https://7105.api.greenapi.com")
	t.Setenv("DIANA_WHATSAPP_PHONE", "77009998877")
	t.Setenv("WHATSAPP_TOKEN", "")
	t.Setenv("WHATSAPP_PHONE_NUMBER_ID", "")
	t.Setenv("WHATSAPP_VERIFY_TOKEN", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load green api config: %v", err)
	}
	if cfg.WhatsAppProvider != "greenapi" {
		t.Fatalf("provider = %q, want greenapi", cfg.WhatsAppProvider)
	}
	if cfg.GreenAPIBaseURL != "https://7105.api.greenapi.com" {
		t.Fatalf("green api url = %q", cfg.GreenAPIBaseURL)
	}
	if !cfg.GreenAPIPollingEnabled {
		t.Fatal("green api native polling should be enabled by default")
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
