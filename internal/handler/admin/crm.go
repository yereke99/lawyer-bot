package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"lawyer-bot/internal/domain"
	"lawyer-bot/internal/service"
)

// ---------------------------------------------------------------- dashboard

func (a *API) handleDashboard(w http.ResponseWriter, r *http.Request) {
	since := startOfDayUTC()
	board, err := a.clients.Dashboard(r.Context(), since)
	if err != nil {
		a.writeServiceError(w, err)
		return
	}

	usage, err := a.aiLog.UsageSince(r.Context(), since)
	if err != nil {
		a.log.Warn("load ai usage failed", zap.Error(err))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"by_status":            board.ByStatus,
		"new_today":            board.NewToday,
		"messages_today":       board.MessagesToday,
		"inbound_today":        board.InboundToday,
		"active_dialogs":       board.ActiveDialogs,
		"ai_conversations":     board.AIConversations,
		"human_conversations":  board.HumanDialogs,
		"scheduled_follow_ups": board.ScheduledFollows,
		"unread":               board.Unread,
		"blocked":              board.Blocked,
		"statuses":             statusCatalog(),
		"ai_usage": map[string]any{
			"calls":         usage.Calls,
			"input_tokens":  usage.InputTokens,
			"output_tokens": usage.OutputTokens,
		},
	})
}

func (a *API) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	days := service.ParseInt(r.URL.Query().Get("days"), 30)
	if days < 1 || days > 365 {
		days = 30
	}
	since := startOfDayUTC().AddDate(0, 0, -days)

	daily, err := a.clients.DailyNewClients(r.Context(), since)
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	services, err := a.clients.ServiceBreakdown(r.Context(), 15)
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	usage, err := a.aiLog.UsageSince(r.Context(), since)
	if err != nil {
		a.log.Warn("load ai usage failed", zap.Error(err))
	}

	named := make([]map[string]any, 0, len(services))
	for _, s := range services {
		named = append(named, map[string]any{
			"service": s.Service,
			"name":    a.catalog.Name(s.Service, domain.LangRU),
			"count":   s.Count,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"days":     days,
		"daily":    daily,
		"services": named,
		"ai_usage": map[string]any{
			"calls":         usage.Calls,
			"input_tokens":  usage.InputTokens,
			"output_tokens": usage.OutputTokens,
		},
	})
}

// -------------------------------------------------------------------- notes

func (a *API) handleListNotes(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid client id"))
		return
	}
	notes, err := a.notes.ListByClient(r.Context(), id, 200)
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": renderNotes(notes)})
}

func (a *API) handleCreateNote(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid client id"))
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if err := decodeJSON(r, &body, 1<<16); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid request"))
		return
	}
	note, err := a.crm.AddNote(r.Context(), actorFrom(r.Context()), id, body.Body)
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, renderNote(*note))
}

func (a *API) handleUpdateNote(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid note id"))
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if err := decodeJSON(r, &body, 1<<16); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid request"))
		return
	}
	note, err := a.crm.UpdateNote(r.Context(), actorFrom(r.Context()), id, body.Body)
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, renderNote(*note))
}

func (a *API) handleDeleteNote(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid note id"))
		return
	}
	if err := a.crm.DeleteNote(r.Context(), actorFrom(r.Context()), id); err != nil {
		a.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func renderNotes(notes []domain.InternalNote) []map[string]any {
	out := make([]map[string]any, 0, len(notes))
	for _, n := range notes {
		out = append(out, renderNote(n))
	}
	return out
}

func renderNote(n domain.InternalNote) map[string]any {
	return map[string]any{
		"id":         n.ID,
		"client_id":  n.UserID,
		"author_id":  n.AdminID,
		"author":     n.AdminName,
		"body":       n.Body,
		"created_at": n.CreatedAt,
		"updated_at": n.UpdatedAt,
	}
}

// --------------------------------------------------------------- follow-ups

func (a *API) handleListFollowUps(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	statuses := splitCSV(q.Get("status"))
	if len(statuses) == 0 {
		statuses = []string{string(domain.FollowUpPending)}
	}
	items, total, err := a.jobs.List(r.Context(), statuses,
		service.ParseInt(q.Get("limit"), 50), service.ParseInt(q.Get("offset"), 0))
	if err != nil {
		a.writeServiceError(w, err)
		return
	}

	rendered := make([]map[string]any, 0, len(items))
	for _, job := range items {
		rendered = append(rendered, map[string]any{
			"id":           job.ID,
			"client_id":    job.UserID,
			"client_name":  job.ClientName,
			"client_phone": service.FormatE164(job.ClientPhone),
			"language":     job.Language,
			"crm_status":   job.CRMStatus,
			"stage":        job.Stage,
			"scheduled_at": job.ScheduledAt,
			"status":       string(job.Status),
			"attempts":     job.Attempts,
			"last_error":   job.LastError,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rendered, "total": total})
}

func (a *API) handleCancelFollowUp(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid job id"))
		return
	}
	if err := a.crm.CancelFollowUp(r.Context(), actorFrom(r.Context()), id); err != nil {
		a.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) handleRescheduleFollowUp(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid job id"))
		return
	}
	var body struct {
		ScheduledAt string `json:"scheduled_at"`
		InMinutes   int    `json:"in_minutes"`
	}
	if err := decodeJSON(r, &body, 1<<16); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid request"))
		return
	}

	var at time.Time
	switch {
	case strings.TrimSpace(body.ScheduledAt) != "":
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(body.ScheduledAt))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("scheduled_at must be RFC3339"))
			return
		}
		at = parsed
	case body.InMinutes > 0:
		at = time.Now().UTC().Add(time.Duration(body.InMinutes) * time.Minute)
	default:
		writeJSON(w, http.StatusBadRequest, errorBody("provide scheduled_at or in_minutes"))
		return
	}

	if err := a.crm.RescheduleFollowUp(r.Context(), actorFrom(r.Context()), id, at); err != nil {
		a.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "scheduled_at": at.UTC()})
}

// -------------------------------------------------------------- consultants

func (a *API) handleListConsultants(w http.ResponseWriter, r *http.Request) {
	admins, err := a.admins.ListAdmins(r.Context())
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	items := make([]map[string]any, 0, len(admins))
	for _, u := range admins {
		items = append(items, publicAdmin(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *API) handleCreateConsultant(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r.Context())
	if !actor.Role.CanAdminister() {
		writeJSON(w, http.StatusForbidden, errorBody("only an administrator can create accounts"))
		return
	}
	var body struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := decodeJSON(r, &body, 1<<16); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid request"))
		return
	}
	role := domain.AdminRole(strings.TrimSpace(body.Role))
	if !role.Valid() {
		role = domain.RoleConsultant
	}
	created, err := a.auth.CreateAccount(r.Context(), body.Email, body.Password, body.Name, role)
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	a.recordAudit(r, actor, domain.AuditSettingsChange, "admin", created.ID, "account created")
	writeJSON(w, http.StatusOK, publicAdmin(*created))
}

func (a *API) handleUpdateConsultant(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r.Context())
	if !actor.Role.CanAdminister() {
		writeJSON(w, http.StatusForbidden, errorBody("only an administrator can change accounts"))
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid account id"))
		return
	}
	var body struct {
		Name     *string `json:"name"`
		Role     *string `json:"role"`
		Active   *bool   `json:"active"`
		Password *string `json:"password"`
	}
	if err := decodeJSON(r, &body, 1<<16); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid request"))
		return
	}

	current, err := a.admins.GetAdmin(r.Context(), id)
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	name, role := current.Name, current.Role
	if body.Name != nil {
		name = *body.Name
	}
	if body.Role != nil {
		candidate := domain.AdminRole(strings.TrimSpace(*body.Role))
		if !candidate.Valid() {
			writeJSON(w, http.StatusBadRequest, errorBody("unknown role"))
			return
		}
		role = candidate
	}
	if err := a.admins.UpdateAdmin(r.Context(), id, name, role); err != nil {
		a.writeServiceError(w, err)
		return
	}
	if body.Active != nil {
		// Disabling an account must not leave its sessions alive.
		if err := a.admins.SetAdminActive(r.Context(), id, *body.Active); err != nil {
			a.writeServiceError(w, err)
			return
		}
		if !*body.Active {
			if err := a.admins.DeleteSessionsForAdmin(r.Context(), id); err != nil {
				a.log.Warn("drop sessions of disabled account failed", zap.Error(err))
			}
		}
	}
	if body.Password != nil && *body.Password != "" {
		if err := a.auth.ResetPassword(r.Context(), id, *body.Password); err != nil {
			a.writeServiceError(w, err)
			return
		}
	}
	a.recordAudit(r, actor, domain.AuditSettingsChange, "admin", id, "account updated")

	updated, err := a.admins.GetAdmin(r.Context(), id)
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, publicAdmin(*updated))
}

// ----------------------------------------------------- services & settings

func (a *API) handleServices(w http.ResponseWriter, r *http.Request) {
	services := a.catalog.All()
	items := make([]map[string]any, 0, len(services))
	for _, s := range services {
		items = append(items, map[string]any{
			"code":        s.Code,
			"name_ru":     s.NameRU,
			"name_kk":     s.NameKZ,
			"name_en":     s.NameEN,
			"description": s.DescriptionRU,
			"clarify_ru":  s.ClarifyRU,
			"clarify_kk":  s.ClarifyKZ,
			"fixed_price": s.HasFixedPrice,
			"price":       s.Price(domain.LangRU),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleSettings exposes the operational configuration.
//
// It deliberately returns no secret: no OpenAI key, no WhatsApp token, no
// instance credentials. Those live in the process environment and have no route.
func (a *API) handleSettings(w http.ResponseWriter, r *http.Request) {
	payload, err := a.settingsPayload(r.Context())
	if err != nil {
		a.log.Warn("load settings failed", zap.Error(err))
	}
	writeJSON(w, http.StatusOK, payload)
}

func (a *API) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r.Context())
	if !actor.Role.CanAdminister() {
		writeJSON(w, http.StatusForbidden, errorBody("only an administrator can change settings"))
		return
	}

	var body struct {
		WhatsAppBotEnabled *bool `json:"whatsapp_bot_enabled"`
		WhatsApp           *struct {
			BotEnabled *bool `json:"bot_enabled"`
		} `json:"whatsapp"`
	}
	if err := decodeJSON(r, &body, 1<<16); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid request"))
		return
	}

	var changed []string
	if body.WhatsAppBotEnabled != nil {
		if err := a.settings.SetWhatsAppBotEnabled(r.Context(), *body.WhatsAppBotEnabled, actor.ID); err != nil {
			a.writeServiceError(w, err)
			return
		}
		changed = append(changed, fmt.Sprintf("whatsapp_bot_enabled=%t", *body.WhatsAppBotEnabled))
	}
	if body.WhatsApp != nil && body.WhatsApp.BotEnabled != nil {
		if err := a.settings.SetWhatsAppBotEnabled(r.Context(), *body.WhatsApp.BotEnabled, actor.ID); err != nil {
			a.writeServiceError(w, err)
			return
		}
		changed = append(changed, fmt.Sprintf("whatsapp_bot_enabled=%t", *body.WhatsApp.BotEnabled))
	}
	if len(changed) == 0 {
		writeJSON(w, http.StatusBadRequest, errorBody("no supported setting provided"))
		return
	}

	a.recordAudit(r, actor, domain.AuditSettingsChange, "settings", 0, strings.Join(changed, ", "))
	payload, err := a.settingsPayload(r.Context())
	if err != nil {
		a.log.Warn("load settings failed after update", zap.Error(err))
	}
	writeJSON(w, http.StatusOK, payload)
}

func (a *API) settingsPayload(ctx context.Context) (map[string]any, error) {
	stored, err := a.settings.All(ctx)
	if err != nil {
		stored = map[string]string{}
	}
	botEnabled, botErr := a.settings.WhatsAppBotEnabled(ctx)
	if botErr != nil {
		err = botErr
	}
	cfg := a.cfg.FollowUp

	delays := make([]string, 0, len(cfg.Delays))
	for _, d := range cfg.Delays {
		delays = append(delays, d.String())
	}

	return map[string]any{
		"ai": map[string]any{
			"reply_model":       a.cfg.Models.ReplyModel,
			"classifier_model":  a.cfg.Models.ClassifierModel,
			"max_output_tokens": a.cfg.Models.MaxOutputTokens,
			"context_messages":  a.cfg.Models.ContextMessages,
			"agent_replies":     a.cfg.Models.AgentReplies,
			"min_confidence":    a.cfg.Models.MinConfidence,
			"dry_run":           a.cfg.Models.DryRun,
		},
		"follow_up": map[string]any{
			"enabled":             cfg.Enabled,
			"delays":              delays,
			"max_attempts":        cfg.MaxAttempts,
			"business_hours_only": cfg.BusinessHoursOnly,
			"business_start":      cfg.BusinessStartHour,
			"business_end":        cfg.BusinessEndHour,
			"timezone":            cfg.Location.String(),
		},
		"crm": map[string]any{
			"statuses":      statusCatalog(),
			"session_hours": int(a.cfg.SessionTTL.Hours()),
			"max_upload_mb": a.cfg.MaxUploadSize / (1 << 20),
			"media_enabled": a.media != nil,
		},
		"whatsapp": map[string]any{
			"bot_enabled":       botEnabled,
			"connection_status": "unknown",
			"connection_note":   "Provider connection status is not exposed by this runtime API.",
		},
		"stored": stored,
		"note":   "Secrets (OPENAI_API_KEY, Green API credentials) are read from the server environment and are never exposed here.",
	}, err
}

func (a *API) handleAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	entity := strings.TrimSpace(q.Get("entity"))
	entityID := int64(service.ParseInt(q.Get("entity_id"), 0))

	entries, total, err := a.audit.List(r.Context(), entity, entityID,
		service.ParseInt(q.Get("limit"), 100), service.ParseInt(q.Get("offset"), 0))
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	items := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		items = append(items, map[string]any{
			"id":         e.ID,
			"admin_id":   e.AdminID,
			"admin":      e.AdminName,
			"action":     e.Action,
			"entity":     e.Entity,
			"entity_id":  e.EntityID,
			"detail":     e.Detail,
			"created_at": e.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

// ------------------------------------------------------------------ search

// handleSearch runs a plain indexed search. It never calls the language model.
func (a *API) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeJSON(w, http.StatusOK, map[string]any{"clients": []any{}, "messages": []any{}})
		return
	}

	clients, _, err := a.clients.ListClients(r.Context(), repositoryFilterForSearch(query))
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	clientItems := make([]map[string]any, 0, len(clients))
	for i := range clients {
		clientItems = append(clientItems, a.clientSummary(&clients[i]))
	}

	messages, err := a.messages.SearchText(r.Context(), query, 25)
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"clients": clientItems, "messages": messages})
}

// ------------------------------------------------------------------ export

func (a *API) handleExport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	format := strings.ToLower(strings.TrimSpace(q.Get("format")))
	if format == "" {
		format = "csv"
	}
	lang := domain.Language(strings.ToLower(strings.TrimSpace(q.Get("lang"))))
	if !lang.Valid() {
		lang = domain.LangKK
	}

	rows, err := a.export.Rows(r.Context(), a.clientFilter(r), lang)
	if err != nil {
		a.writeServiceError(w, err)
		return
	}

	actor := actorFrom(r.Context())
	a.recordAudit(r, actor, domain.AuditExport, "crm", 0,
		fmt.Sprintf("format=%s lang=%s rows=%d", format, lang, len(rows)-1))

	name := service.ExportFileName(format)
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	w.Header().Set("Cache-Control", "no-store")

	if format == "xlsx" {
		w.Header().Set("Content-Type",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		if err := service.WriteXLSX(w, rows, "CRM"); err != nil {
			a.log.Error("write xlsx export failed", zap.Error(err))
		}
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	if err := service.WriteCSV(w, rows); err != nil {
		a.log.Error("write csv export failed", zap.Error(err))
	}
}

// ------------------------------------------------------------------ events

// handleEvents is the CRM live stream.
//
// This is Server-Sent Events between the browser and this server only. It has
// no relationship to how WhatsApp messages are received: the inbound transport
// remains Green API native polling.
func (a *API) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorBody("streaming unsupported"))
		return
	}
	clientID := int64(service.ParseInt(r.URL.Query().Get("client_id"), 0))

	// An event stream outlives the server's write timeout by design, so the
	// deadline is cleared for this one response. Without it the browser is
	// disconnected every thirty seconds and the live view flaps.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		a.log.Debug("event stream write deadline not clearable", zap.Error(err))
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events, cancel := a.hub.Subscribe(clientID)
	defer cancel()

	// A periodic comment keeps proxies from closing an idle stream.
	keepAlive := time.NewTicker(25 * time.Second)
	defer keepAlive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case event, open := <-events:
			if !open {
				return
			}
			payload, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ------------------------------------------------------------------ helpers

func (a *API) recordAudit(r *http.Request, actor service.Actor, action, entity string, entityID int64, detail string) {
	if a.audit == nil {
		return
	}
	if err := a.audit.Record(r.Context(), domain.AuditLog{
		AdminID: actor.ID, Action: action, Entity: entity, EntityID: entityID,
		Detail: detail, IPHash: actor.IPHash,
	}); err != nil {
		a.log.Warn("write audit entry failed", zap.Error(err))
	}
}

func statusCatalog() []map[string]any {
	out := make([]map[string]any, 0, len(domain.AllCRMStatuses))
	for _, s := range domain.AllCRMStatuses {
		out = append(out, map[string]any{
			"code":     string(s),
			"label":    service.StatusLabel(s, domain.LangRU),
			"label_kk": service.StatusLabel(s, domain.LangKK),
			"terminal": s.Terminal(),
		})
	}
	return out
}

func startOfDayUTC() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}
