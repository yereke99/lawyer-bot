package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"lawyer-bot/config"
	"lawyer-bot/internal/domain"
	"lawyer-bot/internal/handler"
	"lawyer-bot/internal/handler/admin"
	"lawyer-bot/internal/integration/openai"
	"lawyer-bot/internal/integration/whatsapp"
	"lawyer-bot/internal/repository"
	"lawyer-bot/internal/service"
	"lawyer-bot/internal/web"
	"lawyer-bot/internal/worker"
	"lawyer-bot/traits/logger"
)

// version is stamped at build time: go build -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log, err := logger.New(cfg.LogLevel, cfg.Env)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = log.Sync() }()

	log.Info("starting lawyer-bot",
		zap.String("version", version),
		zap.String("env", cfg.Env),
		zap.String("whatsapp_provider", cfg.WhatsAppProvider),
		zap.String("model", cfg.OpenAIModel),
		zap.Bool("llm_agent_replies", cfg.LLMAgentReplies),
		zap.Bool("dry_run", cfg.DryRun))

	// Root context, cancelled on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ------------------------------------------------------------- storage
	db, err := repository.Open(ctx, cfg.SQLitePath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	log.Info("database ready", zap.String("path", cfg.SQLitePath))

	users := repository.NewUserRepository(db)
	messages := repository.NewMessageRepository(db)
	leads := repository.NewLeadRepository(db)
	aiLog := repository.NewAIInteractionRepository(db)
	trace := repository.NewTraceRepository(db)

	// CRM stores. They read and write the same tables as the pipeline; nothing
	// about the conversation model is duplicated for the CRM.
	crmClients := repository.NewCRMRepository(db)
	adminUsers := repository.NewAdminRepository(db)
	followUpJobs := repository.NewFollowUpRepository(db)
	notes := repository.NewNoteRepository(db)
	auditLog := repository.NewAuditRepository(db)
	settings := repository.NewSettingsRepository(db)

	// -------------------------------------------------------- integrations
	aiClient := openai.New(openai.Options{
		APIKey:  cfg.OpenAIAPIKey,
		BaseURL: cfg.OpenAIBaseURL,
		Model:   cfg.OpenAIModel,
		// Structured analysis runs on every message and can use a cheaper
		// model than the one that writes the customer-facing answer.
		ClassifierModel: cfg.OpenAIClassifierModel,
		MaxTokens:       cfg.OpenAIMaxOutputTokens,
		MaxInputChars:   cfg.OpenAIMaxInputChars,
		Timeout:         cfg.OpenAITimeout(),
	})

	var (
		waClient    domain.WhatsAppClient
		greenClient *whatsapp.GreenClient
	)
	switch cfg.WhatsAppProvider {
	case "greenapi":
		greenClient = whatsapp.NewGreen(whatsapp.GreenOptions{
			IDInstance:    cfg.GreenAPIIDInstance,
			TokenInstance: cfg.GreenAPITokenInstance,
			BaseURL:       cfg.GreenAPIBaseURL,
			Timeout:       cfg.WhatsAppTimeout(),
		})
		waClient = greenClient
	default:
		waClient = whatsapp.New(whatsapp.Options{
			Token:         cfg.WhatsAppToken,
			PhoneNumberID: cfg.WhatsAppPhoneNumberID,
			BaseURL:       cfg.WhatsAppAPIBaseURL,
			APIVersion:    cfg.WhatsAppAPIVersion,
			Timeout:       cfg.WhatsAppTimeout(),
		})
	}

	// --------------------------------------------------------------- media
	var mediaFetcher domain.WhatsAppFileFetcher
	if greenClient != nil && cfg.MediaDownloadIn {
		mediaFetcher = greenClient
	}
	mediaStore, err := service.NewMediaStore(service.MediaConfig{
		Root:     cfg.MediaPath,
		MaxBytes: cfg.MediaMaxBytes(),
	}, mediaFetcher)
	if err != nil {
		return fmt.Errorf("init media store: %w", err)
	}

	// ------------------------------------------------------------ services
	catalog := service.NewCatalog()
	triggers := service.NewTriggerSet()
	gate := service.NewGate(triggers, service.GateConfig{
		MaxCallsPerDay:    cfg.AIMaxCallsPerDay,
		AnalyzeUnmatched:  cfg.AIAnalyzeUnmatched,
		MinWordsUnmatched: cfg.AIMinWordsUnmatched,
	})

	// The CRM live stream and the single outbound layer are built before the
	// pipeline, because the pipeline sends through the latter.
	hub := service.NewEventHub()

	messenger := service.NewMessenger(service.MessengerDeps{
		Messages: messages,
		CRM:      crmClients,
		Trace:    trace,
		WhatsApp: waClient,
		Logger:   log,
	}, service.MessengerConfig{DryRun: cfg.DryRun})
	messenger.OnSent(hub.ClientChanged)

	followUps := service.NewFollowUpService(service.FollowUpDeps{
		Jobs:     followUpJobs,
		CRM:      crmClients,
		Messages: messages,
		Trace:    trace,
		Settings: settings,
		Sender:   messenger,
		Logger:   log,
	}, service.FollowUpConfig{
		Enabled:           cfg.FollowUpEnabled,
		Delays:            cfg.FollowUpDelays,
		MaxAttempts:       cfg.FollowUpMaxAttempts,
		PollInterval:      time.Duration(cfg.FollowUpPollSeconds) * time.Second,
		BatchSize:         cfg.FollowUpBatchSize,
		ClaimTTL:          time.Duration(cfg.FollowUpClaimTTLMins) * time.Minute,
		RetryBackoff:      time.Duration(cfg.FollowUpRetryMins) * time.Minute,
		BusinessHoursOnly: cfg.FollowUpBusinessOnly,
		BusinessStartHour: cfg.FollowUpBusinessStart,
		BusinessEndHour:   cfg.FollowUpBusinessEnd,
		Location:          cfg.FollowUpLocation(),
	})

	pipeline := service.NewPipeline(service.PipelineDeps{
		Users:    users,
		Messages: messages,
		Leads:    leads,
		AILog:    aiLog,
		Trace:    trace,
		Settings: settings,
		AI:       aiClient,
		WhatsApp: waClient,
		Gate:     gate,
		Catalog:  catalog,
		Composer: service.NewComposer(catalog),
		Qualify:  service.NewQualifier(catalog, cfg.AIMinConfidence),
		Triggers: triggers,
		Logger:   log,
		Clients:  crmClients,
		FollowUp: followUps,
		Media:    mediaStore,
		Sender:   messenger,
		Notify:   hub.ClientChanged,
	}, service.PipelineConfig{
		MinConfidence:   cfg.AIMinConfidence,
		ContextMessages: cfg.OpenAIContextMessages,
		NotifyRecipient: cfg.NotificationRecipient(),
		DefaultSource:   cfg.DefaultLeadSrc,
		DryRun:          cfg.DryRun,
		ReplyDelayMin:   cfg.WhatsAppReplyDelayMin,
		ReplyDelayMax:   cfg.WhatsAppReplyDelayMax,
		AgentReplies:    cfg.LLMAgentReplies,
	})

	// --------------------------------------------------------- worker pool
	pool := worker.New(worker.Options{
		Workers:   cfg.WorkerCount,
		QueueSize: cfg.QueueSize,
		// One job must outlive a slow OpenAI call, reply pacing and the
		// WhatsApp send.
		JobTimeout: cfg.OpenAITimeout() + cfg.WhatsAppReplyDelayMax + cfg.WhatsAppTimeout() + 15*time.Second,
		Logger:     log,
	})
	pool.Start(context.WithoutCancel(ctx))

	if greenClient != nil && cfg.GreenAPIPollingEnabled {
		poller := handler.NewGreenAPIPoller(greenClient, pipeline, trace, pool, log, handler.GreenAPIPollerConfig{
			StoreRaw:              cfg.TraceRawPayload,
			ReceiveTimeoutSeconds: cfg.GreenAPIReceiveTimeoutSeconds,
			RetryDelay:            time.Duration(cfg.GreenAPIRetryDelaySeconds) * time.Second,
		})
		poller.Start(ctx)
	}

	// ---------------------------------------------------------------- http
	var webhook *handler.WhatsAppHandler
	if cfg.WhatsAppProvider == "meta" {
		webhook = handler.NewWhatsAppHandler(pipeline, trace, pool, log, handler.WhatsAppHandlerConfig{
			VerifyToken: cfg.WhatsAppVerifyToken,
			AppSecret:   cfg.WhatsAppAppSecret,
			StoreRaw:    cfg.TraceRawPayload,
		})
		if cfg.WhatsAppAppSecret == "" {
			log.Warn("WHATSAPP_APP_SECRET is not set: webhook signatures are not verified")
		}
	} else {
		log.Info("whatsapp webhook disabled; green api native polling is the inbound transport")
	}

	// ----------------------------------------------------------- admin crm
	routerCfg := handler.RouterConfig{
		WebhookPath: cfg.WebhookPath,
		Version:     version,
	}
	if cfg.AdminEnabled {
		authService := service.NewAuthService(adminUsers, auditLog, log, service.AuthConfig{
			SessionTTL:       cfg.AdminSessionTTL(),
			PBKDF2Iterations: cfg.AdminPBKDF2Iter,
			MaxAttempts:      cfg.AdminMaxAttempts,
			LockoutThreshold: cfg.AdminLockoutTries,
			LockoutDuration:  time.Duration(cfg.AdminLockoutMins) * time.Minute,
			SecureCookies:    cfg.AdminSecureCookies,
		})

		created, err := authService.EnsureBootstrapAdmin(ctx,
			cfg.AdminBootstrapMail, cfg.AdminBootstrapPass, cfg.AdminBootstrapName)
		if err != nil {
			return fmt.Errorf("bootstrap admin account: %w", err)
		}
		if created {
			log.Info("first admin account created from ADMIN_EMAIL/ADMIN_PASSWORD")
		}
		authService.StartJanitor(ctx, time.Hour)

		crmService := service.NewCRMService(service.CRMDeps{
			Clients:  crmClients,
			Messages: messages,
			Notes:    notes,
			Audit:    auditLog,
			Jobs:     followUpJobs,
			Admins:   adminUsers,
			FollowUp: followUps,
			Sender:   messenger,
			Media:    mediaStore,
			Catalog:  catalog,
			Logger:   log,
		})

		adminAPI := admin.New(admin.Deps{
			Auth:     authService,
			CRM:      crmService,
			Clients:  crmClients,
			Messages: messages,
			Notes:    notes,
			Jobs:     followUpJobs,
			Admins:   adminUsers,
			Audit:    auditLog,
			AILog:    aiLog,
			Settings: settings,
			Export:   service.NewExportService(crmClients, catalog),
			Media:    mediaStore,
			Catalog:  catalog,
			Hub:      hub,
			FollowUp: followUps,
			Logger:   log,
		}, admin.Config{
			BasePath:      cfg.AdminBasePath,
			SecureCookies: cfg.AdminSecureCookies,
			SessionTTL:    cfg.AdminSessionTTL(),
			MaxUploadSize: cfg.MediaMaxBytes(),
			FollowUp:      followUps.Config(),
			Version:       version,
			Models: admin.ModelInfo{
				ReplyModel:      aiClient.Model(),
				ClassifierModel: aiClient.ClassifierModel(),
				MaxOutputTokens: cfg.OpenAIMaxOutputTokens,
				ContextMessages: cfg.OpenAIContextMessages,
				AgentReplies:    cfg.LLMAgentReplies,
				MinConfidence:   cfg.AIMinConfidence,
				DryRun:          cfg.DryRun,
			},
		})

		routerCfg.AdminBasePath = cfg.AdminBasePath
		routerCfg.AdminAPI = adminAPI.Routes()
		routerCfg.AdminUI = web.Handler(cfg.AdminBasePath)

		if !cfg.AdminSecureCookies {
			log.Warn("ADMIN_SECURE_COOKIES is false: enable it whenever the CRM is served over HTTPS")
		}
		log.Info("admin crm mounted",
			zap.String("path", cfg.AdminBasePath),
			zap.Bool("media_send", messenger.SupportsFiles()),
			zap.Bool("follow_ups", cfg.FollowUpEnabled))
	}

	// The follow-up worker is durable: its jobs live in the database, so a
	// restart never loses a scheduled nudge.
	followUps.Start(context.WithoutCancel(ctx))

	router := handler.NewRouter(webhook, pool, log, routerCfg)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: cfg.HTTPReadTimeout,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("http server listening", zap.String("addr", cfg.HTTPAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// ------------------------------------------------------------ shutdown
	select {
	case err := <-serverErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown failed", zap.Error(err))
	}
	// Drain in-flight messages so a lead in progress is not dropped.
	if err := pool.Shutdown(shutdownCtx); err != nil {
		log.Error("worker pool shutdown failed", zap.Error(err))
	}

	log.Info("stopped cleanly")
	return nil
}
