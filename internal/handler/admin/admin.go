// Package admin exposes the Admin CRM HTTP API.
//
// Design notes:
//   - every route except login is behind session authentication, and every
//     unsafe method additionally requires a matching CSRF token;
//   - the browser never talks to WhatsApp or OpenAI: provider credentials are
//     read from the process environment and stay server-side;
//   - a client ID arriving from the browser is always resolved through the
//     repository and checked, never trusted as an access grant.
package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"lawyer-bot/internal/domain"
	"lawyer-bot/internal/repository"
	"lawyer-bot/internal/service"
)

// Cookie and header names.
const (
	sessionCookie = "lb_admin_session"
	csrfCookie    = "lb_csrf"
	csrfHeader    = "X-CSRF-Token"
)

// API is the Admin CRM HTTP surface.
type API struct {
	auth     *service.AuthService
	crm      *service.CRMService
	clients  *repository.CRMRepository
	messages *repository.MessageRepository
	notes    *repository.NoteRepository
	jobs     *repository.FollowUpRepository
	admins   *repository.AdminRepository
	audit    *repository.AuditRepository
	aiLog    *repository.AIInteractionRepository
	settings *repository.SettingsRepository
	export   *service.ExportService
	media    *service.MediaStore
	catalog  *service.Catalog
	hub      *service.EventHub
	follow   *service.FollowUpService
	log      *zap.Logger
	cfg      Config
}

// Config configures the API.
type Config struct {
	BasePath      string
	SecureCookies bool
	SessionTTL    time.Duration
	MaxUploadSize int64
	FollowUp      service.FollowUpConfig
	Models        ModelInfo
	Version       string
}

// ModelInfo is the non-secret description of the AI configuration shown in
// settings. It deliberately carries no keys.
type ModelInfo struct {
	ReplyModel      string `json:"reply_model"`
	ClassifierModel string `json:"classifier_model"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	ContextMessages int    `json:"context_messages"`
	AgentReplies    bool   `json:"agent_replies"`
	MinConfidence   float64
	DryRun          bool
}

// Deps groups the API's collaborators.
type Deps struct {
	Auth     *service.AuthService
	CRM      *service.CRMService
	Clients  *repository.CRMRepository
	Messages *repository.MessageRepository
	Notes    *repository.NoteRepository
	Jobs     *repository.FollowUpRepository
	Admins   *repository.AdminRepository
	Audit    *repository.AuditRepository
	AILog    *repository.AIInteractionRepository
	Settings *repository.SettingsRepository
	Export   *service.ExportService
	Media    *service.MediaStore
	Catalog  *service.Catalog
	Hub      *service.EventHub
	FollowUp *service.FollowUpService
	Logger   *zap.Logger
}

// New builds the Admin CRM API.
func New(deps Deps, cfg Config) *API {
	log := deps.Logger
	if log == nil {
		log = zap.NewNop()
	}
	if cfg.BasePath == "" {
		cfg.BasePath = "/admin"
	}
	if cfg.MaxUploadSize <= 0 {
		cfg.MaxUploadSize = 32 << 20
	}
	return &API{
		auth: deps.Auth, crm: deps.CRM, clients: deps.Clients, messages: deps.Messages,
		notes: deps.Notes, jobs: deps.Jobs, admins: deps.Admins, audit: deps.Audit,
		aiLog: deps.AILog, settings: deps.Settings, export: deps.Export, media: deps.Media,
		catalog: deps.Catalog, hub: deps.Hub, follow: deps.FollowUp, log: log, cfg: cfg,
	}
}

// Routes returns the API mux, already wrapped in the auth middleware.
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()

	// Public: login is the only unauthenticated endpoint.
	mux.HandleFunc("POST /api/auth/login", a.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", a.protected(a.handleLogout))
	mux.HandleFunc("GET /api/auth/me", a.protected(a.handleMe))
	mux.HandleFunc("POST /api/auth/password", a.protected(a.handleChangePassword))

	mux.HandleFunc("GET /api/dashboard", a.protected(a.handleDashboard))
	mux.HandleFunc("GET /api/analytics", a.protected(a.handleAnalytics))

	mux.HandleFunc("GET /api/clients", a.protected(a.handleListClients))
	mux.HandleFunc("GET /api/clients/{id}", a.protected(a.handleGetClient))
	mux.HandleFunc("PATCH /api/clients/{id}", a.protected(a.handlePatchClient))
	mux.HandleFunc("GET /api/clients/{id}/messages", a.protected(a.handleMessages))
	mux.HandleFunc("POST /api/clients/{id}/messages", a.protected(a.handleSendMessage))
	mux.HandleFunc("POST /api/clients/{id}/read", a.protected(a.handleMarkRead))

	mux.HandleFunc("POST /api/clients/{id}/takeover", a.protected(a.clientAction(actionTakeover)))
	mux.HandleFunc("POST /api/clients/{id}/resume-ai", a.protected(a.clientAction(actionResumeAI)))
	mux.HandleFunc("POST /api/clients/{id}/pause-ai", a.protected(a.clientAction(actionPauseAI)))
	mux.HandleFunc("POST /api/clients/{id}/block", a.protected(a.clientAction(actionBlock)))
	mux.HandleFunc("POST /api/clients/{id}/unblock", a.protected(a.clientAction(actionUnblock)))
	mux.HandleFunc("POST /api/clients/{id}/assign", a.protected(a.clientAction(actionAssign)))
	mux.HandleFunc("POST /api/clients/{id}/status", a.protected(a.clientAction(actionStatus)))

	mux.HandleFunc("GET /api/clients/{id}/notes", a.protected(a.handleListNotes))
	mux.HandleFunc("POST /api/clients/{id}/notes", a.protected(a.handleCreateNote))
	mux.HandleFunc("PATCH /api/notes/{id}", a.protected(a.handleUpdateNote))
	mux.HandleFunc("DELETE /api/notes/{id}", a.protected(a.handleDeleteNote))

	mux.HandleFunc("GET /api/follow-ups", a.protected(a.handleListFollowUps))
	mux.HandleFunc("POST /api/follow-ups/{id}/cancel", a.protected(a.handleCancelFollowUp))
	mux.HandleFunc("POST /api/follow-ups/{id}/reschedule", a.protected(a.handleRescheduleFollowUp))

	mux.HandleFunc("GET /api/consultants", a.protected(a.handleListConsultants))
	mux.HandleFunc("POST /api/consultants", a.protected(a.handleCreateConsultant))
	mux.HandleFunc("PATCH /api/consultants/{id}", a.protected(a.handleUpdateConsultant))

	mux.HandleFunc("GET /api/services", a.protected(a.handleServices))
	mux.HandleFunc("GET /api/settings", a.protected(a.handleSettings))
	mux.HandleFunc("GET /api/audit", a.protected(a.handleAudit))
	mux.HandleFunc("GET /api/search", a.protected(a.handleSearch))
	mux.HandleFunc("GET /api/export", a.protected(a.handleExport))
	mux.HandleFunc("GET /api/media/{id}", a.protected(a.handleMedia))
	mux.HandleFunc("GET /api/events", a.protected(a.handleEvents))

	return mux
}

// ------------------------------------------------------------- middleware

type ctxKey string

const actorKey ctxKey = "admin.actor"

// protected requires a valid session and, for unsafe methods, a CSRF token.
func (a *API) protected(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			unauthorized(w)
			return
		}
		admin, session, err := a.auth.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			a.clearCookies(w)
			unauthorized(w)
			return
		}

		// CSRF: an unsafe request must echo the session's own token. A
		// cross-site form cannot read it, so it cannot forge the request.
		if !safeMethod(r.Method) {
			token := r.Header.Get(csrfHeader)
			if token == "" {
				token = r.FormValue("csrf_token")
			}
			if !service.ConstantTimeEquals(token, session.CSRFToken) {
				writeJSON(w, http.StatusForbidden, errorBody("csrf token mismatch"))
				return
			}
		}

		actor := service.Actor{
			ID: admin.ID, Name: admin.Name, Role: admin.Role, IPHash: clientKey(r),
		}
		ctx := context.WithValue(r.Context(), actorKey, actor)
		ctx = context.WithValue(ctx, ctxKey("admin.session"), session.CSRFToken)
		next(w, r.WithContext(ctx))
	}
}

func actorFrom(ctx context.Context) service.Actor {
	if a, ok := ctx.Value(actorKey).(service.Actor); ok {
		return a
	}
	return service.Actor{}
}

func safeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// clientKey hashes the caller's address for rate limiting and audit, so no raw
// IP address is ever stored.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	sum := sha256.Sum256([]byte(host))
	return hex.EncodeToString(sum[:16])
}

// ------------------------------------------------------------------- auth

func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body, 1<<16); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid request"))
		return
	}

	session, err := a.auth.Login(r.Context(), body.Email, body.Password, clientKey(r))
	switch {
	case errors.Is(err, service.ErrRateLimited):
		writeJSON(w, http.StatusTooManyRequests, errorBody("too many attempts, please wait"))
		return
	case errors.Is(err, service.ErrAccountLocked):
		writeJSON(w, http.StatusTooManyRequests, errorBody("account temporarily locked"))
		return
	case err != nil:
		// One message for every credential problem: the API never reveals
		// whether an account exists.
		writeJSON(w, http.StatusUnauthorized, errorBody("invalid email or password"))
		return
	}

	a.setCookies(w, session)
	writeJSON(w, http.StatusOK, map[string]any{
		"user":       publicAdmin(session.Admin),
		"csrf_token": session.CSRFToken,
	})
}

func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r.Context())
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if err := a.auth.Logout(r.Context(), cookie.Value, actor.ID, clientKey(r)); err != nil {
			a.log.Warn("logout failed", zap.Error(err))
		}
	}
	a.clearCookies(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) handleMe(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r.Context())
	admin, err := a.admins.GetAdmin(r.Context(), actor.ID)
	if err != nil {
		unauthorized(w)
		return
	}
	csrf, _ := r.Context().Value(ctxKey("admin.session")).(string)
	writeJSON(w, http.StatusOK, map[string]any{
		"user":       publicAdmin(*admin),
		"csrf_token": csrf,
		"version":    a.cfg.Version,
		"media_send": a.media != nil,
	})
}

func (a *API) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r.Context())
	var body struct {
		Current string `json:"current_password"`
		Next    string `json:"new_password"`
	}
	if err := decodeJSON(r, &body, 1<<16); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid request"))
		return
	}
	if err := a.auth.ChangePassword(r.Context(), actor.ID, body.Current, body.Next); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	a.clearCookies(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) setCookies(w http.ResponseWriter, session *service.Session) {
	// HttpOnly: JavaScript can never read the session token, so an XSS bug
	// cannot exfiltrate it. SameSite=Lax blocks cross-site submission.
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: session.Token, Path: "/",
		HttpOnly: true, Secure: a.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
		Expires: session.ExpiresAt, MaxAge: int(time.Until(session.ExpiresAt).Seconds()),
	})
	// The CSRF cookie is deliberately readable by the page's own script, which
	// is how the double-submit check works.
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookie, Value: session.CSRFToken, Path: "/",
		HttpOnly: false, Secure: a.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
		Expires: session.ExpiresAt, MaxAge: int(time.Until(session.ExpiresAt).Seconds()),
	})
}

func (a *API) clearCookies(w http.ResponseWriter) {
	for _, name := range []string{sessionCookie, csrfCookie} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", HttpOnly: name == sessionCookie,
			Secure: a.cfg.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: -1,
		})
	}
}

func publicAdmin(u domain.AdminUser) map[string]any {
	return map[string]any{
		"id":         u.ID,
		"email":      u.Email,
		"name":       u.Name,
		"role":       string(u.Role),
		"active":     u.Active,
		"last_login": u.LastLoginAt,
	}
}

// --------------------------------------------------------------- responses

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func errorBody(message string) map[string]any {
	return map[string]any{"error": message}
}

func unauthorized(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, errorBody("authentication required"))
}

func decodeJSON(r *http.Request, target any, max int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, max))
	dec.DisallowUnknownFields()
	return dec.Decode(target)
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// writeServiceError maps a service-layer error onto an HTTP status without
// leaking internals.
func (a *API) writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrForbidden):
		writeJSON(w, http.StatusForbidden, errorBody("your role cannot perform this action"))
	case errors.Is(err, service.ErrConflict):
		writeJSON(w, http.StatusConflict, errorBody("another consultant changed this conversation, reload and retry"))
	case errors.Is(err, repository.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorBody("not found"))
	case errors.Is(err, service.ErrNoFileSupport):
		writeJSON(w, http.StatusBadRequest, errorBody("the configured WhatsApp provider cannot send files"))
	case errors.Is(err, service.ErrUnsupportedMedia):
		writeJSON(w, http.StatusUnsupportedMediaType, errorBody("this file type is not allowed"))
	case errors.Is(err, service.ErrMediaTooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, errorBody("file is too large"))
	default:
		a.log.Error("admin request failed", zap.Error(err))
		writeJSON(w, http.StatusBadRequest, errorBody(err.Error()))
	}
}
