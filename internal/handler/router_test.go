package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"lawyer-bot/internal/worker"
)

// The request logger wraps every response. Embedding an http.ResponseWriter
// hides the concrete writer's Flush, and without forwarding it the Admin CRM's
// event stream is refused as "streaming unsupported" and the live view never
// connects.
func TestRequestLoggerKeepsTheResponseFlushable(t *testing.T) {
	var flushable, unwrapped bool

	handler := requestLogger(zap.NewNop(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, flushable = w.(http.Flusher)
		_, unwrapped = w.(interface{ Unwrap() http.ResponseWriter })
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/admin/api/events", nil))

	if !flushable {
		t.Fatal("the wrapped writer must implement http.Flusher, or server-sent events break")
	}
	if !unwrapped {
		t.Fatal("the wrapped writer must expose Unwrap, or http.ResponseController cannot reach it")
	}
}

// A streaming handler mounted on the real router must be able to flush.
func TestRouterAllowsStreamingResponses(t *testing.T) {
	stream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: ping\ndata: {}\n\n"))
		flusher.Flush()
	})

	router := NewRouter(nil, worker.New(worker.Options{}), zap.NewNop(), RouterConfig{
		AdminBasePath: "/admin",
		AdminAPI:      stream,
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/api/events", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("the event stream must be accepted, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q", got)
	}
}
