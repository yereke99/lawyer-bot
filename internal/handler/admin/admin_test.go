package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"lawyer-bot/internal/domain"
	"lawyer-bot/internal/repository"
	"lawyer-bot/internal/service"
)

// apiHarness stands up the real API over a real database, with the WhatsApp
// provider stubbed. Nothing here mocks the auth or CRM layers.
type apiHarness struct {
	server *httptest.Server
	db     *repository.DB
	client *http.Client
	csrf   string
	crm    *repository.CRMRepository
	users  *repository.UserRepository
}

type stubSender struct{ fail bool }

func (s *stubSender) SendText(context.Context, string, string) (domain.SendResult, error) {
	if s.fail {
		return domain.SendResult{}, http.ErrServerClosed
	}
	return domain.SendResult{MessageID: "wamid.stub"}, nil
}

func (s *stubSender) SendMedia(context.Context, string, string, string) (domain.SendResult, error) {
	return domain.SendResult{MessageID: "wamid.stub"}, nil
}

func (s *stubSender) SendFile(context.Context, string, domain.OutgoingFile) (domain.SendResult, error) {
	if s.fail {
		return domain.SendResult{}, http.ErrServerClosed
	}
	return domain.SendResult{MessageID: "wamid.file"}, nil
}

func newAPIHarness(t *testing.T) *apiHarness {
	t.Helper()
	ctx := context.Background()

	db, err := repository.Open(ctx, filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	users := repository.NewUserRepository(db)
	messages := repository.NewMessageRepository(db)
	clients := repository.NewCRMRepository(db)
	admins := repository.NewAdminRepository(db)
	jobs := repository.NewFollowUpRepository(db)
	notes := repository.NewNoteRepository(db)
	audit := repository.NewAuditRepository(db)
	settings := repository.NewSettingsRepository(db)
	aiLog := repository.NewAIInteractionRepository(db)
	catalog := service.NewCatalog()
	log := zap.NewNop()
	provider := &stubSender{}

	mediaStore, err := service.NewMediaStore(service.MediaConfig{
		Root: filepath.Join(t.TempDir(), "media"), MaxBytes: 1 << 20,
	}, nil)
	if err != nil {
		t.Fatalf("media store: %v", err)
	}

	messenger := service.NewMessenger(service.MessengerDeps{
		Messages: messages, CRM: clients, Trace: repository.NewTraceRepository(db),
		WhatsApp: provider, Files: provider, Logger: log,
	}, service.MessengerConfig{})

	follow := service.NewFollowUpService(service.FollowUpDeps{
		Jobs: jobs, CRM: clients, Messages: messages, Sender: messenger, Logger: log,
	}, service.FollowUpConfig{Enabled: true, Delays: []time.Duration{time.Hour}})

	auth := service.NewAuthService(admins, audit, log, service.AuthConfig{
		PBKDF2Iterations: 120_000, SessionTTL: time.Hour,
	})
	if _, err := auth.EnsureBootstrapAdmin(ctx, "admin@lawyer.kz", "Str0ngPassword", "Диана"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}

	crmService := service.NewCRMService(service.CRMDeps{
		Clients: clients, Messages: messages, Notes: notes, Audit: audit, Jobs: jobs,
		Admins: admins, FollowUp: follow, Sender: messenger, Media: mediaStore,
		Catalog: catalog, Logger: log,
	})

	api := New(Deps{
		Auth: auth, CRM: crmService, Clients: clients, Messages: messages, Notes: notes,
		Jobs: jobs, Admins: admins, Audit: audit, AILog: aiLog, Settings: settings,
		Export: service.NewExportService(clients, catalog), Media: mediaStore,
		Catalog: catalog, Hub: service.NewEventHub(), FollowUp: follow, Logger: log,
	}, Config{BasePath: "/admin", SessionTTL: time.Hour, MaxUploadSize: 1 << 20})

	server := httptest.NewServer(api.Routes())
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return &apiHarness{
		server: server, db: db, crm: clients, users: users,
		client: &http.Client{Jar: jar, Timeout: 5 * time.Second},
	}
}

func (h *apiHarness) do(t *testing.T, method, path string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if h.csrf != "" {
		req.Header.Set(csrfHeader, h.csrf)
	}

	res, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer res.Body.Close()

	var decoded map[string]any
	if strings.Contains(res.Header.Get("Content-Type"), "json") {
		_ = json.NewDecoder(res.Body).Decode(&decoded)
	}
	return res, decoded
}

func (h *apiHarness) login(t *testing.T) {
	t.Helper()
	res, body := h.do(t, http.MethodPost, "/api/auth/login",
		map[string]string{"email": "admin@lawyer.kz", "password": "Str0ngPassword"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %d %v", res.StatusCode, body)
	}
	token, _ := body["csrf_token"].(string)
	if token == "" {
		t.Fatal("login must return a CSRF token")
	}
	h.csrf = token
}

func (h *apiHarness) seedClient(t *testing.T) int64 {
	t.Helper()
	u, err := h.users.Upsert(context.Background(), "77015551234@c.us", "77015551234", "Аида")
	if err != nil {
		t.Fatalf("seed client: %v", err)
	}
	return u.ID
}

// ------------------------------------------------------------------- tests

// Scenario 15: every CRM route refuses an unauthenticated caller.
func TestEveryAdminRouteRequiresAuthentication(t *testing.T) {
	h := newAPIHarness(t)
	id := h.seedClient(t)

	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/dashboard"},
		{http.MethodGet, "/api/clients"},
		{http.MethodGet, "/api/clients/1"},
		{http.MethodGet, "/api/clients/1/messages"},
		{http.MethodPost, "/api/clients/1/messages"},
		{http.MethodPost, "/api/clients/1/takeover"},
		{http.MethodPost, "/api/clients/1/block"},
		{http.MethodGet, "/api/follow-ups"},
		{http.MethodGet, "/api/consultants"},
		{http.MethodGet, "/api/audit"},
		{http.MethodGet, "/api/export"},
		{http.MethodGet, "/api/settings"},
		{http.MethodGet, "/api/media/1"},
	}
	for _, r := range routes {
		res, _ := h.do(t, r.method, r.path, nil)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s must require authentication, got %d", r.method, r.path, res.StatusCode)
		}
	}
	_ = id
}

// Section 40: an unsafe request without a CSRF token is refused.
func TestUnsafeRequestsRequireCSRFToken(t *testing.T) {
	h := newAPIHarness(t)
	id := h.seedClient(t)
	h.login(t)

	good := h.csrf
	h.csrf = "forged-token"
	res, _ := h.do(t, http.MethodPost, "/api/clients/"+itoa(id)+"/takeover", map[string]string{})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a forged CSRF token must be refused, got %d", res.StatusCode)
	}

	h.csrf = ""
	res, _ = h.do(t, http.MethodPost, "/api/clients/"+itoa(id)+"/takeover", map[string]string{})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a missing CSRF token must be refused, got %d", res.StatusCode)
	}

	// Reads are unaffected.
	h.csrf = good
	res, _ = h.do(t, http.MethodGet, "/api/dashboard", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("an authenticated read must succeed, got %d", res.StatusCode)
	}
}

// The session cookie is HttpOnly, so a script can never read it.
func TestSessionCookieIsHttpOnly(t *testing.T) {
	h := newAPIHarness(t)

	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/api/auth/login",
		strings.NewReader(`{"email":"admin@lawyer.kz","password":"Str0ngPassword"}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer res.Body.Close()

	var session *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("login must set a session cookie")
	}
	if !session.HttpOnly {
		t.Fatal("the session cookie must be HttpOnly")
	}
	if session.SameSite != http.SameSiteLaxMode {
		t.Fatalf("the session cookie must be SameSite=Lax, got %v", session.SameSite)
	}
}

// Bad credentials never reveal whether an account exists.
func TestLoginErrorsAreIndistinguishable(t *testing.T) {
	h := newAPIHarness(t)

	_, wrongPass := h.do(t, http.MethodPost, "/api/auth/login",
		map[string]string{"email": "admin@lawyer.kz", "password": "not-the-password"})
	_, unknown := h.do(t, http.MethodPost, "/api/auth/login",
		map[string]string{"email": "ghost@lawyer.kz", "password": "not-the-password"})

	if wrongPass["error"] != unknown["error"] {
		t.Fatalf("login errors must be identical: %v vs %v", wrongPass["error"], unknown["error"])
	}
}

// Scenario 15: an identifier from the browser is validated, not trusted.
func TestUnknownClientIdIsRejected(t *testing.T) {
	h := newAPIHarness(t)
	h.login(t)

	for _, path := range []string{"/api/clients/999999", "/api/clients/999999/messages", "/api/clients/-1"} {
		res, _ := h.do(t, http.MethodGet, path, nil)
		if res.StatusCode == http.StatusOK {
			t.Fatalf("%s must not return data for a non-existent client", path)
		}
	}
}

// The settings endpoint must never expose a secret.
func TestSettingsNeverExposeSecrets(t *testing.T) {
	h := newAPIHarness(t)
	h.login(t)

	res, body := h.do(t, http.MethodGet, "/api/settings", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("settings: %d", res.StatusCode)
	}
	// The explanatory note names the environment variables on purpose; it is the
	// payload that must be free of them.
	delete(body, "note")
	raw, _ := json.Marshal(body)
	lowered := strings.ToLower(string(raw))
	for _, secret := range []string{"openai_api_key", "sk-", "green_api_token", "password_hash", "whatsapp_token", "csrf_token"} {
		if strings.Contains(lowered, secret) {
			t.Fatalf("settings leaked %q: %s", secret, raw)
		}
	}
}

// The full consultant flow over HTTP: take over, send, note, block, export.
func TestConsultantFlowOverHTTP(t *testing.T) {
	h := newAPIHarness(t)
	id := h.seedClient(t)
	h.login(t)
	path := "/api/clients/" + itoa(id)

	res, body := h.do(t, http.MethodPost, path+"/takeover", map[string]string{})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("takeover: %d %v", res.StatusCode, body)
	}
	if body["mode"] != "human" {
		t.Fatalf("takeover must switch to human mode, got %v", body["mode"])
	}

	res, body = h.do(t, http.MethodPost, path+"/messages", map[string]string{"text": "Здравствуйте!"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("send: %d %v", res.StatusCode, body)
	}
	message, _ := body["message"].(map[string]any)
	if message["sender"] != "consultant" {
		t.Fatalf("a manual message must be a consultant message, got %v", message["sender"])
	}

	res, body = h.do(t, http.MethodPost, path+"/notes", map[string]string{"body": "Перезвонить после 18:00"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("note: %d %v", res.StatusCode, body)
	}

	res, body = h.do(t, http.MethodPost, path+"/block", map[string]string{"reason": "спам"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("block: %d %v", res.StatusCode, body)
	}
	if body["blocked"] != true {
		t.Fatalf("block must be reflected in the response, got %v", body["blocked"])
	}

	// Sending to a blocked client is refused.
	res, _ = h.do(t, http.MethodPost, path+"/messages", map[string]string{"text": "ещё раз"})
	if res.StatusCode == http.StatusOK {
		t.Fatal("sending to a blocked client must fail")
	}

	// The export renders with Kazakh headers and no secrets.
	req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/api/export?format=csv&lang=kk", nil)
	export, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	defer export.Body.Close()
	if export.StatusCode != http.StatusOK {
		t.Fatalf("export status: %d", export.StatusCode)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(export.Body); err != nil {
		t.Fatalf("read export: %v", err)
	}
	if !strings.Contains(buf.String(), "Тіл") {
		t.Fatalf("the export must carry Kazakh headers: %q", buf.String())
	}
}

// Uploads outside the allow-list are refused before anything is stored.
func TestUploadRejectsDisallowedTypes(t *testing.T) {
	h := newAPIHarness(t)
	id := h.seedClient(t)
	h.login(t)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "payload.svg")
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := part.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)); err != nil {
		t.Fatalf("write part: %v", err)
	}
	_ = writer.WriteField("text", "смотрите")
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/api/clients/"+itoa(id)+"/messages", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set(csrfHeader, h.csrf)

	res, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		t.Fatal("an SVG upload must be refused")
	}
}

// Media is served with headers that stop a browser executing it.
func TestMediaResponseIsNotExecutable(t *testing.T) {
	h := newAPIHarness(t)
	id := h.seedClient(t)
	h.login(t)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "contract.pdf")
	_, _ = part.Write([]byte("%PDF-1.4\nfake contract"))
	_ = writer.Close()

	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/api/clients/"+itoa(id)+"/messages", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set(csrfHeader, h.csrf)

	res, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	var sent map[string]any
	_ = json.NewDecoder(res.Body).Decode(&sent)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("pdf upload should succeed, got %d: %v", res.StatusCode, sent)
	}

	message, _ := sent["message"].(map[string]any)
	media, _ := message["media"].(map[string]any)
	if media == nil {
		t.Fatalf("the stored message must reference its media: %v", message)
	}

	msgID := int64(message["id"].(float64))
	fetch, _ := h.do(t, http.MethodGet, "/api/media/"+itoa(msgID), nil)
	if fetch.StatusCode != http.StatusOK {
		t.Fatalf("media fetch: %d", fetch.StatusCode)
	}
	if fetch.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("media must be served with nosniff")
	}
	if !strings.Contains(fetch.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatal("media must be served sandboxed")
	}
	if !strings.Contains(fetch.Header.Get("Content-Disposition"), "contract.pdf") {
		t.Fatalf("media must keep its display name, got %q", fetch.Header.Get("Content-Disposition"))
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
