package logger

import (
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New builds the application logger. Production uses structured JSON, other
// environments use a human readable console encoder.
//
// Callers must never log secrets (API keys, access tokens) or raw personal data
// beyond what is required for support: helpers in this package exist for that.
func New(level, env string) (*zap.Logger, error) {
	var cfg zap.Config
	if strings.EqualFold(env, "production") {
		cfg = zap.NewProductionConfig()
	} else {
		cfg = zap.NewDevelopmentConfig()
		cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	}

	cfg.Level = zap.NewAtomicLevelAt(parseLevel(level))
	cfg.EncoderConfig.TimeKey = "ts"
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	return cfg.Build()
}

func parseLevel(level string) zapcore.Level {
	l, err := zapcore.ParseLevel(strings.ToLower(strings.TrimSpace(level)))
	if err != nil {
		return zapcore.InfoLevel
	}
	return l
}

// MaskPhone reduces a phone number to a form that is useful in logs but is not
// a full identifier, e.g. "+77015551234" -> "+7701***1234".
func MaskPhone(phone string) string {
	if phone == "" {
		return ""
	}
	runes := []rune(phone)
	if len(runes) <= 6 {
		return strings.Repeat("*", len(runes))
	}
	head := 4
	tail := 4
	if len(runes) < head+tail+1 {
		head, tail = 2, 2
	}
	return string(runes[:head]) + "***" + string(runes[len(runes)-tail:])
}

// Phone is a zap field carrying a masked phone number.
func Phone(key, phone string) zap.Field {
	return zap.String(key, MaskPhone(phone))
}

// Preview truncates free-form user text so logs stay small and do not become a
// second copy of the conversation database.
func Preview(key, text string) zap.Field {
	const max = 80
	t := strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	runes := []rune(t)
	if len(runes) > max {
		t = string(runes[:max]) + "…"
	}
	return zap.String(key, t)
}
