package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every changeable value of the application.
// Nothing here may ever be hardcoded in the source: all values are read from
// the process environment (optionally seeded from a .env file).
type Config struct {
	// Runtime
	Env      string
	LogLevel string

	// HTTP server exposing health checks and, in Meta mode, the WhatsApp webhook
	HTTPAddr        string
	WebhookPath     string
	HTTPReadTimeout time.Duration

	// OpenAI
	OpenAIAPIKey          string
	OpenAIBaseURL         string
	OpenAIModel           string
	OpenAIClassifierModel string
	OpenAIMaxOutputTokens int
	OpenAIContextMessages int
	OpenAITimeoutSeconds  int
	OpenAIMaxInputChars   int
	LLMAgentReplies       bool

	// AI decision thresholds and token budget
	AIMinConfidence     float64
	AIMaxCallsPerDay    int
	AIAnalyzeUnmatched  bool
	AIMinWordsUnmatched int

	// WhatsApp (Meta Cloud API by default)
	WhatsAppProvider      string
	WhatsAppToken         string
	WhatsAppPhoneNumberID string
	WhatsAppVerifyToken   string
	WhatsAppAppSecret     string
	WhatsAppAPIBaseURL    string
	WhatsAppAPIVersion    string
	WhatsAppTimeoutSecond int
	WhatsAppReplyDelayMin time.Duration
	WhatsAppReplyDelayMax time.Duration

	// Green API native polling
	GreenAPIIDInstance            string
	GreenAPITokenInstance         string
	GreenAPIBaseURL               string
	GreenAPIPollingEnabled        bool
	GreenAPIReceiveTimeoutSeconds int
	GreenAPIRetryDelaySeconds     int

	// Lead handoff target
	DianaWhatsAppPhone  string
	DianaWhatsAppUserID string

	// Storage
	SQLitePath string

	// Admins
	AdminTelegramIDs []int64

	// Processing
	WorkerCount int
	QueueSize   int

	// Behaviour switches
	DryRun          bool   // process everything but never actually send a WhatsApp message
	DefaultLeadSrc  string // source recorded on leads when nothing else is known
	TraceRawPayload bool   // persist raw webhook bodies for full auditability

	// ----------------------------------------------------------- Admin CRM
	AdminEnabled       bool
	AdminBasePath      string
	AdminBootstrapMail string
	AdminBootstrapPass string
	AdminBootstrapName string
	AdminSessionHours  int
	AdminSecureCookies bool
	AdminPBKDF2Iter    int
	AdminMaxAttempts   int
	AdminLockoutTries  int
	AdminLockoutMins   int

	// Media storage for CRM conversations.
	MediaPath       string
	MediaMaxSizeMB  int
	MediaDownloadIn bool

	// Follow-up automation.
	FollowUpEnabled       bool
	FollowUpDelays        []time.Duration
	FollowUpMaxAttempts   int
	FollowUpPollSeconds   int
	FollowUpBatchSize     int
	FollowUpClaimTTLMins  int
	FollowUpRetryMins     int
	FollowUpBusinessOnly  bool
	FollowUpBusinessStart int
	FollowUpBusinessEnd   int
	FollowUpTimezone      string
}

// Load reads configuration from the environment. If a .env file is present in
// the working directory (or at ENV_FILE) it is loaded first, without ever
// overriding variables that are already set in the real environment.
func Load() (*Config, error) {
	if err := loadDotEnv(getenv("ENV_FILE", ".env")); err != nil {
		return nil, fmt.Errorf("load env file: %w", err)
	}

	cfg := &Config{
		Env:      getenv("APP_ENV", "development"),
		LogLevel: getenv("LOG_LEVEL", "info"),

		HTTPAddr:        getenv("HTTP_ADDR", ":8080"),
		WebhookPath:     getenv("WEBHOOK_PATH", "/webhook/whatsapp"),
		HTTPReadTimeout: time.Duration(getenvInt("HTTP_READ_TIMEOUT_SECONDS", 15)) * time.Second,

		OpenAIAPIKey:          os.Getenv("OPENAI_API_KEY"),
		OpenAIBaseURL:         getenv("OPENAI_BASE_URL", "https://api.openai.com/v1"),
		OpenAIModel:           getenv("OPENAI_MODEL", "gpt-4o-mini"),
		OpenAIClassifierModel: getenv("OPENAI_CLASSIFIER_MODEL", ""),
		OpenAIMaxOutputTokens: getenvInt("OPENAI_MAX_OUTPUT_TOKENS", 300),
		OpenAIContextMessages: getenvInt("OPENAI_CONTEXT_MESSAGES", 10),
		OpenAITimeoutSeconds:  getenvInt("OPENAI_TIMEOUT_SECONDS", 20),
		OpenAIMaxInputChars:   getenvInt("OPENAI_MAX_INPUT_CHARS", 1200),
		LLMAgentReplies:       getenvBool("LLM_AGENT_REPLIES", true),

		AIMinConfidence:     getenvFloat("AI_MIN_CONFIDENCE", 0.75),
		AIMaxCallsPerDay:    getenvInt("AI_MAX_CALLS_PER_USER_PER_DAY", 40),
		AIAnalyzeUnmatched:  getenvBool("AI_ANALYZE_UNMATCHED", true),
		AIMinWordsUnmatched: getenvInt("AI_MIN_WORDS_UNMATCHED", 3),

		WhatsAppProvider:      strings.ToLower(getenv("WHATSAPP_PROVIDER", "meta")),
		WhatsAppToken:         os.Getenv("WHATSAPP_TOKEN"),
		WhatsAppPhoneNumberID: os.Getenv("WHATSAPP_PHONE_NUMBER_ID"),
		WhatsAppVerifyToken:   os.Getenv("WHATSAPP_VERIFY_TOKEN"),
		WhatsAppAppSecret:     os.Getenv("WHATSAPP_APP_SECRET"),
		WhatsAppAPIBaseURL:    getenv("WHATSAPP_API_BASE_URL", "https://graph.facebook.com"),
		WhatsAppAPIVersion:    getenv("WHATSAPP_API_VERSION", "v21.0"),
		WhatsAppTimeoutSecond: getenvInt("WHATSAPP_TIMEOUT_SECONDS", 15),
		WhatsAppReplyDelayMin: time.Duration(getenvInt("WHATSAPP_BOT_REPLY_DELAY_MIN_MS", 1500)) * time.Millisecond,
		WhatsAppReplyDelayMax: time.Duration(getenvInt("WHATSAPP_BOT_REPLY_DELAY_MAX_MS", 3000)) * time.Millisecond,

		GreenAPIIDInstance:            getenvFirst([]string{"GREEN_API_ID_INSTANCE", "GREEN_API_INSTANCE"}, ""),
		GreenAPITokenInstance:         getenvFirst([]string{"GREEN_API_TOKEN_INSTANCE", "GREEN_API_TOKEN"}, ""),
		GreenAPIBaseURL:               getenvFirst([]string{"GREEN_API_API_URL", "GREEN_API_BASE_URL"}, "https://api.green-api.com"),
		GreenAPIPollingEnabled:        getenvBool("GREEN_API_POLLING_ENABLED", true),
		GreenAPIReceiveTimeoutSeconds: getenvInt("GREEN_API_RECEIVE_TIMEOUT_SECONDS", 5),
		GreenAPIRetryDelaySeconds:     getenvInt("GREEN_API_RETRY_DELAY_SECONDS", 5),

		DianaWhatsAppPhone:  os.Getenv("DIANA_WHATSAPP_PHONE"),
		DianaWhatsAppUserID: os.Getenv("DIANA_WHATSAPP_USER_ID"),

		SQLitePath: getenv("SQLITE_PATH", "data/lawyer-bot.db"),

		AdminTelegramIDs: getenvInt64Slice("ADMIN_TELEGRAM_IDS"),

		WorkerCount: getenvInt("WORKER_COUNT", 4),
		QueueSize:   getenvInt("QUEUE_SIZE", 256),

		DryRun:          getenvBool("DRY_RUN", false),
		DefaultLeadSrc:  getenv("DEFAULT_LEAD_SOURCE", "whatsapp"),
		TraceRawPayload: getenvBool("TRACE_RAW_PAYLOAD", true),

		AdminEnabled:       getenvBool("ADMIN_ENABLED", true),
		AdminBasePath:      getenv("ADMIN_BASE_PATH", "/admin"),
		AdminBootstrapMail: strings.TrimSpace(os.Getenv("ADMIN_EMAIL")),
		AdminBootstrapPass: os.Getenv("ADMIN_PASSWORD"),
		AdminBootstrapName: getenv("ADMIN_NAME", "Administrator"),
		AdminSessionHours:  getenvInt("ADMIN_SESSION_HOURS", 12),
		AdminSecureCookies: getenvBool("ADMIN_SECURE_COOKIES", false),
		AdminPBKDF2Iter:    getenvInt("ADMIN_PBKDF2_ITERATIONS", 600000),
		AdminMaxAttempts:   getenvInt("ADMIN_LOGIN_MAX_ATTEMPTS", 10),
		AdminLockoutTries:  getenvInt("ADMIN_LOCKOUT_THRESHOLD", 8),
		AdminLockoutMins:   getenvInt("ADMIN_LOCKOUT_MINUTES", 15),

		MediaPath:       getenv("MEDIA_PATH", "data/media"),
		MediaMaxSizeMB:  getenvInt("MEDIA_MAX_SIZE_MB", 32),
		MediaDownloadIn: getenvBool("MEDIA_DOWNLOAD_INBOUND", true),

		FollowUpEnabled:       getenvBool("FOLLOWUP_ENABLED", true),
		FollowUpDelays:        getenvDurations("FOLLOWUP_DELAYS", []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour}),
		FollowUpMaxAttempts:   getenvInt("FOLLOWUP_MAX_ATTEMPTS", 3),
		FollowUpPollSeconds:   getenvInt("FOLLOWUP_POLL_SECONDS", 60),
		FollowUpBatchSize:     getenvInt("FOLLOWUP_BATCH_SIZE", 20),
		FollowUpClaimTTLMins:  getenvInt("FOLLOWUP_CLAIM_TTL_MINUTES", 5),
		FollowUpRetryMins:     getenvInt("FOLLOWUP_RETRY_MINUTES", 10),
		FollowUpBusinessOnly:  getenvBool("FOLLOWUP_BUSINESS_HOURS_ONLY", true),
		FollowUpBusinessStart: getenvInt("FOLLOWUP_BUSINESS_START_HOUR", 9),
		FollowUpBusinessEnd:   getenvInt("FOLLOWUP_BUSINESS_END_HOUR", 21),
		FollowUpTimezone:      getenv("FOLLOWUP_TIMEZONE", "Asia/Almaty"),
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate reports configuration that would make the bot misbehave at runtime.
func (c *Config) Validate() error {
	var problems []string

	if c.OpenAIAPIKey == "" {
		problems = append(problems, "OPENAI_API_KEY is required")
	}
	if c.OpenAIModel == "" {
		problems = append(problems, "OPENAI_MODEL is required")
	}
	switch c.WhatsAppProvider {
	case "meta":
		if c.WhatsAppToken == "" {
			problems = append(problems, "WHATSAPP_TOKEN is required")
		}
		if c.WhatsAppPhoneNumberID == "" {
			problems = append(problems, "WHATSAPP_PHONE_NUMBER_ID is required")
		}
		if c.WhatsAppVerifyToken == "" {
			problems = append(problems, "WHATSAPP_VERIFY_TOKEN is required")
		}
	case "greenapi":
		if c.GreenAPIIDInstance == "" {
			problems = append(problems, "GREEN_API_ID_INSTANCE is required")
		}
		if c.GreenAPITokenInstance == "" {
			problems = append(problems, "GREEN_API_TOKEN_INSTANCE is required")
		}
		if c.GreenAPIBaseURL == "" {
			problems = append(problems, "GREEN_API_API_URL is required")
		}
		if c.GreenAPIReceiveTimeoutSeconds < 5 || c.GreenAPIReceiveTimeoutSeconds > 60 {
			problems = append(problems, "GREEN_API_RECEIVE_TIMEOUT_SECONDS must be between 5 and 60")
		}
		if c.GreenAPIRetryDelaySeconds < 1 {
			problems = append(problems, "GREEN_API_RETRY_DELAY_SECONDS must be >= 1")
		}
	default:
		problems = append(problems, "WHATSAPP_PROVIDER must be meta or greenapi")
	}
	if c.WhatsAppReplyDelayMin <= 0 {
		problems = append(problems, "WHATSAPP_BOT_REPLY_DELAY_MIN_MS must be > 0")
	}
	if c.WhatsAppReplyDelayMax <= 0 {
		problems = append(problems, "WHATSAPP_BOT_REPLY_DELAY_MAX_MS must be > 0")
	}
	if c.WhatsAppReplyDelayMax <= c.WhatsAppReplyDelayMin {
		problems = append(problems, "WHATSAPP_BOT_REPLY_DELAY_MAX_MS must be greater than WHATSAPP_BOT_REPLY_DELAY_MIN_MS")
	}
	if c.SQLitePath == "" {
		problems = append(problems, "SQLITE_PATH is required")
	}
	if c.AIMinConfidence < 0 || c.AIMinConfidence > 1 {
		problems = append(problems, "AI_MIN_CONFIDENCE must be between 0 and 1")
	}
	if c.OpenAIContextMessages < 0 {
		problems = append(problems, "OPENAI_CONTEXT_MESSAGES must be >= 0")
	}
	if c.WorkerCount < 1 {
		problems = append(problems, "WORKER_COUNT must be >= 1")
	}
	if c.QueueSize < 1 {
		problems = append(problems, "QUEUE_SIZE must be >= 1")
	}
	if c.DianaWhatsAppPhone == "" && c.DianaWhatsAppUserID == "" {
		problems = append(problems, "DIANA_WHATSAPP_PHONE or DIANA_WHATSAPP_USER_ID is required to hand off leads")
	}
	if c.AdminEnabled {
		if !strings.HasPrefix(c.AdminBasePath, "/") {
			problems = append(problems, "ADMIN_BASE_PATH must start with /")
		}
		if c.AdminSessionHours < 1 {
			problems = append(problems, "ADMIN_SESSION_HOURS must be >= 1")
		}
		if c.AdminPBKDF2Iter < 100000 {
			problems = append(problems, "ADMIN_PBKDF2_ITERATIONS must be >= 100000")
		}
		if c.MediaMaxSizeMB < 1 || c.MediaMaxSizeMB > 512 {
			problems = append(problems, "MEDIA_MAX_SIZE_MB must be between 1 and 512")
		}
		// A bootstrap password is only used when no account exists yet, but a
		// weak one must never be accepted even then.
		if c.AdminBootstrapPass != "" && len([]rune(c.AdminBootstrapPass)) < 10 {
			problems = append(problems, "ADMIN_PASSWORD must be at least 10 characters")
		}
		if c.AdminBootstrapMail != "" && !strings.Contains(c.AdminBootstrapMail, "@") {
			problems = append(problems, "ADMIN_EMAIL must be a valid email address")
		}
	}
	if c.FollowUpEnabled {
		if len(c.FollowUpDelays) == 0 {
			problems = append(problems, "FOLLOWUP_DELAYS must list at least one duration")
		}
		for _, d := range c.FollowUpDelays {
			if d <= 0 {
				problems = append(problems, "FOLLOWUP_DELAYS must contain positive durations")
				break
			}
		}
		if c.FollowUpBusinessOnly && (c.FollowUpBusinessStart < 0 || c.FollowUpBusinessEnd > 24 ||
			c.FollowUpBusinessEnd <= c.FollowUpBusinessStart) {
			problems = append(problems, "FOLLOWUP_BUSINESS_START_HOUR must be lower than FOLLOWUP_BUSINESS_END_HOUR (0-24)")
		}
		if _, err := time.LoadLocation(c.FollowUpTimezone); err != nil {
			problems = append(problems, "FOLLOWUP_TIMEZONE is not a valid IANA timezone")
		}
	}

	if len(problems) > 0 {
		return errors.New("invalid configuration: " + strings.Join(problems, "; "))
	}
	return nil
}

// OpenAITimeout returns the per-request OpenAI timeout.
func (c *Config) OpenAITimeout() time.Duration {
	return time.Duration(c.OpenAITimeoutSeconds) * time.Second
}

// WhatsAppTimeout returns the per-request WhatsApp timeout.
func (c *Config) WhatsAppTimeout() time.Duration {
	return time.Duration(c.WhatsAppTimeoutSecond) * time.Second
}

// NotificationRecipient is the WhatsApp address that receives lead alerts.
func (c *Config) NotificationRecipient() string {
	if c.DianaWhatsAppUserID != "" {
		return c.DianaWhatsAppUserID
	}
	return c.DianaWhatsAppPhone
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func getenvFirst(keys []string, def string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return def
}

func getenvInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getenvFloat(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func getenvBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// getenvDurations parses a comma-separated duration list such as "1h,6h,24h".
// Follow-up timing is configuration, never a constant in the business logic.
func getenvDurations(key string, def []time.Duration) []time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	parts := strings.Split(raw, ",")
	out := make([]time.Duration, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		d, err := time.ParseDuration(p)
		if err != nil || d <= 0 {
			return def
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return def
	}
	return out
}

// AdminSessionTTL is how long an admin session survives without activity.
func (c *Config) AdminSessionTTL() time.Duration {
	return time.Duration(c.AdminSessionHours) * time.Hour
}

// MediaMaxBytes is the accepted upload size in bytes.
func (c *Config) MediaMaxBytes() int64 {
	return int64(c.MediaMaxSizeMB) << 20
}

// FollowUpLocation resolves the configured business-hours timezone.
func (c *Config) FollowUpLocation() *time.Location {
	loc, err := time.LoadLocation(c.FollowUpTimezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

func getenvInt64Slice(key string) []int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// loadDotEnv seeds the environment from a KEY=VALUE file. Missing files are not
// an error: production deployments usually inject real environment variables.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if key == "" {
			continue
		}
		// Real environment always wins over the file.
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return sc.Err()
}
