package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"lawyer-bot/internal/worker"
)

// RouterConfig configures the HTTP routes.
type RouterConfig struct {
	WebhookPath string
	Version     string

	// AdminBasePath mounts the Admin CRM. Empty disables it entirely.
	AdminBasePath string
	// AdminAPI is the CRM's JSON API; AdminUI serves its single-page app.
	AdminAPI http.Handler
	AdminUI  http.Handler
}

// NewRouter wires the HTTP endpoints. When webhook is nil, the process exposes
// only operational routes; Green API native polling does not need an inbound
// WhatsApp webhook.
func NewRouter(webhook *WhatsAppHandler, pool *worker.Pool, log *zap.Logger, cfg RouterConfig) http.Handler {
	mux := http.NewServeMux()

	path := cfg.WebhookPath
	if path == "" {
		path = "/webhook/whatsapp"
	}
	if webhook != nil {
		mux.Handle(path, webhook)
	}

	// The Admin CRM is a browser-facing concern mounted on the same server. It
	// does not change the inbound WhatsApp transport in any way.
	if cfg.AdminBasePath != "" && cfg.AdminAPI != nil {
		base := strings.TrimSuffix(cfg.AdminBasePath, "/")
		mux.Handle(base+"/api/", http.StripPrefix(base, cfg.AdminAPI))
		if cfg.AdminUI != nil {
			mux.Handle(base+"/", http.StripPrefix(base, cfg.AdminUI))
			mux.Handle(base, http.RedirectHandler(base+"/", http.StatusMovedPermanently))
		}
	}

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

// Flush forwards to the real writer. Embedding an http.ResponseWriter hides the
// concrete type's Flush, so without this the CRM's server-sent event stream is
// refused as "streaming unsupported" and the live view never connects.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer, which is how
// the event stream clears its own write deadline.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
