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
	"lawyer-bot/internal/handler"
	"lawyer-bot/internal/integration/openai"
	"lawyer-bot/internal/integration/whatsapp"
	"lawyer-bot/internal/repository"
	"lawyer-bot/internal/service"
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
		zap.String("model", cfg.OpenAIModel),
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

	// -------------------------------------------------------- integrations
	aiClient := openai.New(openai.Options{
		APIKey:        cfg.OpenAIAPIKey,
		BaseURL:       cfg.OpenAIBaseURL,
		Model:         cfg.OpenAIModel,
		MaxTokens:     cfg.OpenAIMaxOutputTokens,
		MaxInputChars: cfg.OpenAIMaxInputChars,
		Timeout:       cfg.OpenAITimeout(),
	})

	waClient := whatsapp.New(whatsapp.Options{
		Token:         cfg.WhatsAppToken,
		PhoneNumberID: cfg.WhatsAppPhoneNumberID,
		BaseURL:       cfg.WhatsAppAPIBaseURL,
		APIVersion:    cfg.WhatsAppAPIVersion,
		Timeout:       cfg.WhatsAppTimeout(),
	})

	// ------------------------------------------------------------ services
	catalog := service.NewCatalog()
	triggers := service.NewTriggerSet()
	gate := service.NewGate(triggers, service.GateConfig{
		MaxCallsPerDay:    cfg.AIMaxCallsPerDay,
		AnalyzeUnmatched:  cfg.AIAnalyzeUnmatched,
		MinWordsUnmatched: cfg.AIMinWordsUnmatched,
	})

	pipeline := service.NewPipeline(service.PipelineDeps{
		Users:    users,
		Messages: messages,
		Leads:    leads,
		AILog:    aiLog,
		Trace:    trace,
		AI:       aiClient,
		WhatsApp: waClient,
		Gate:     gate,
		Catalog:  catalog,
		Composer: service.NewComposer(catalog),
		Qualify:  service.NewQualifier(catalog, cfg.AIMinConfidence),
		Triggers: triggers,
		Logger:   log,
	}, service.PipelineConfig{
		MinConfidence:   cfg.AIMinConfidence,
		ContextMessages: cfg.OpenAIContextMessages,
		NotifyRecipient: cfg.NotificationRecipient(),
		DefaultSource:   cfg.DefaultLeadSrc,
		DryRun:          cfg.DryRun,
	})

	// --------------------------------------------------------- worker pool
	pool := worker.New(worker.Options{
		Workers:   cfg.WorkerCount,
		QueueSize: cfg.QueueSize,
		// One job must outlive a slow OpenAI call plus the WhatsApp send.
		JobTimeout: cfg.OpenAITimeout() + cfg.WhatsAppTimeout() + 15*time.Second,
		Logger:     log,
	})
	pool.Start(context.WithoutCancel(ctx))

	// ---------------------------------------------------------------- http
	webhook := handler.NewWhatsAppHandler(pipeline, trace, pool, log, handler.WhatsAppHandlerConfig{
		VerifyToken: cfg.WhatsAppVerifyToken,
		AppSecret:   cfg.WhatsAppAppSecret,
		StoreRaw:    cfg.TraceRawPayload,
	})
	if cfg.WhatsAppAppSecret == "" {
		log.Warn("WHATSAPP_APP_SECRET is not set: webhook signatures are not verified")
	}

	router := handler.NewRouter(webhook, pool, log, handler.RouterConfig{
		WebhookPath: cfg.WebhookPath,
		Version:     version,
	})

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
		log.Info("http server listening",
			zap.String("addr", cfg.HTTPAddr),
			zap.String("webhook_path", cfg.WebhookPath))
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
