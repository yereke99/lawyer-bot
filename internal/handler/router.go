package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"lawyer-bot/internal/worker"
)

// RouterConfig configures the HTTP routes.
type RouterConfig struct {
	WebhookPath string
	Version     string
}

// NewRouter wires the HTTP endpoints. The webhook is the only route that
// accepts external traffic; the rest is operational.
func NewRouter(webhook *WhatsAppHandler, pool *worker.Pool, log *zap.Logger, cfg RouterConfig) http.Handler {
	mux := http.NewServeMux()

	path := cfg.WebhookPath
	if path == "" {
		path = "/webhook/whatsapp"
	}
	mux.Handle(path, webhook)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"version": cfg.Version,
			"queued":  pool.Pending(),
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	})

	return requestLogger(log, mux)
}

// requestLogger records method, path, status and duration for every request.
func requestLogger(log *zap.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		log.Debug("http request",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", rec.status),
			zap.Duration("duration", time.Since(start)))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
