package admin

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"lawyer-bot/internal/domain"
	"lawyer-bot/internal/repository"
	"lawyer-bot/internal/service"
)

// clientFilter builds the server-side filter from query parameters.
//
// Filtering and pagination happen in SQL. The browser never receives the whole
// client table, which is what keeps the CRM fast as history grows.
func (a *API) clientFilter(r *http.Request) repository.ClientFilter {
	q := r.URL.Query()
	f := repository.ClientFilter{
		Search:    strings.TrimSpace(q.Get("q")),
		Statuses:  splitCSV(q.Get("status")),
		Services:  splitCSV(q.Get("service")),
		Languages: splitCSV(q.Get("language")),
		Modes:     splitCSV(q.Get("mode")),
		Unread:    q.Get("unread") == "1",
		Sort:      q.Get("sort"),
		Desc:      q.Get("dir") != "asc",
		Limit:     service.ParseInt(q.Get("limit"), 50),
		Offset:    service.ParseInt(q.Get("offset"), 0),
	}
	if raw := strings.TrimSpace(q.Get("assigned")); raw != "" {
		id := int64(service.ParseInt(raw, -1))
		if id >= 0 {
			f.AssignedTo = &id
		}
	}
	switch q.Get("blocked") {
	case "1", "true":
		v := true
		f.Blocked = &v
	case "0", "false":
		v := false
		f.Blocked = &v
	}
	if from, ok := parseDate(q.Get("from")); ok {
		f.From = &from
	}
	if to, ok := parseDate(q.Get("to")); ok {
		end := to.Add(24*time.Hour - time.Second)
		f.To = &end
	}
	return f
}

func (a *API) handleListClients(w http.ResponseWriter, r *http.Request) {
	filter := a.clientFilter(r)
	clients, total, err := a.clients.ListClients(r.Context(), filter)
	if err != nil {
		a.writeServiceError(w, err)
		return
	}

	items := make([]map[string]any, 0, len(clients))
	for i := range clients {
		items = append(items, a.clientSummary(&clients[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":  items,
		"total":  total,
		"limit":  filter.Limit,
		"offset": filter.Offset,
	})
}

func (a *API) handleGetClient(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid client id"))
		return
	}
	// The identifier from the browser is resolved through the repository; it is
	// never treated as an access grant on its own.
	client, err := a.clients.GetClient(r.Context(), id)
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.clientDetail(r, client))
}

func (a *API) handlePatchClient(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid client id"))
		return
	}
	var body struct {
		DisplayName *string   `json:"display_name"`
		Language    *string   `json:"language"`
		Service     *string   `json:"service"`
		Status      *string   `json:"status"`
		Tags        *[]string `json:"tags"`
		NextAction  *string   `json:"next_action"`
		CloseReason *string   `json:"close_reason"`
	}
	if err := decodeJSON(r, &body, 1<<16); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid request"))
		return
	}

	patch := repository.ClientPatch{
		DisplayName: body.DisplayName,
		Service:     body.Service,
		Tags:        body.Tags,
		NextAction:  body.NextAction,
		CloseReason: body.CloseReason,
	}
	if body.Language != nil {
		lang := domain.Language(strings.ToLower(strings.TrimSpace(*body.Language)))
		patch.Language = &lang
	}
	if body.Status != nil {
		status := domain.CRMStatus(strings.TrimSpace(*body.Status))
		patch.Status = &status
	}

	client, err := a.crm.UpdateClient(r.Context(), actorFrom(r.Context()), id, patch)
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	a.hub.ClientChanged(id)
	writeJSON(w, http.StatusOK, a.clientDetail(r, client))
}

// ------------------------------------------------------------------ actions

type clientActionKind int

const (
	actionTakeover clientActionKind = iota
	actionResumeAI
	actionPauseAI
	actionBlock
	actionUnblock
	actionAssign
	actionStatus
)

// clientAction handles the state-changing buttons of the client screen.
func (a *API) clientAction(kind clientActionKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r)
		if !ok {
			writeJSON(w, http.StatusBadRequest, errorBody("invalid client id"))
			return
		}
		var body struct {
			Reason       string `json:"reason"`
			ConsultantID int64  `json:"consultant_id"`
			Status       string `json:"status"`
		}
		if r.ContentLength > 0 {
			if err := decodeJSON(r, &body, 1<<16); err != nil {
				writeJSON(w, http.StatusBadRequest, errorBody("invalid request"))
				return
			}
		}

		actor := actorFrom(r.Context())
		var (
			client *domain.CRMClient
			err    error
		)
		switch kind {
		case actionTakeover:
			client, err = a.crm.TakeOver(r.Context(), actor, id)
		case actionResumeAI:
			client, err = a.crm.ResumeAI(r.Context(), actor, id)
		case actionPauseAI:
			client, err = a.crm.PauseAI(r.Context(), actor, id)
		case actionBlock:
			client, err = a.crm.Block(r.Context(), actor, id, body.Reason)
		case actionUnblock:
			client, err = a.crm.Unblock(r.Context(), actor, id)
		case actionAssign:
			consultantID := body.ConsultantID
			if consultantID < 0 {
				consultantID = 0
			}
			client, err = a.crm.Assign(r.Context(), actor, id, consultantID)
		case actionStatus:
			client, err = a.crm.SetStatus(r.Context(), actor, id,
				domain.CRMStatus(strings.TrimSpace(body.Status)), body.Reason)
		}
		if err != nil {
			a.writeServiceError(w, err)
			return
		}
		a.hub.ClientChanged(id)
		writeJSON(w, http.StatusOK, a.clientDetail(r, client))
	}
}

// ----------------------------------------------------------------- messages

func (a *API) handleMessages(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid client id"))
		return
	}
	if _, err := a.clients.GetClient(r.Context(), id); err != nil {
		a.writeServiceError(w, err)
		return
	}

	q := r.URL.Query()
	// after= streams only what is new, which is how the live view stays cheap.
	if raw := strings.TrimSpace(q.Get("after")); raw != "" {
		after := int64(service.ParseInt(raw, 0))
		messages, err := a.messages.SinceID(r.Context(), id, after, 200)
		if err != nil {
			a.writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": renderMessages(messages), "has_more": false})
		return
	}

	before := int64(service.ParseInt(q.Get("before"), 0))
	limit := service.ParseInt(q.Get("limit"), 50)
	messages, hasMore, err := a.messages.PageByUser(r.Context(), id, before, limit)
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":    renderMessages(messages),
		"has_more": hasMore,
	})
}

func (a *API) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid client id"))
		return
	}
	if err := a.clients.MarkRead(r.Context(), id); err != nil {
		a.writeServiceError(w, err)
		return
	}
	a.hub.ClientChanged(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSendMessage sends a consultant's message.
//
// Two request shapes are accepted: JSON for plain text, and multipart for a
// file. Either way the browser talks only to this endpoint; the WhatsApp
// credentials never leave the server.
func (a *API) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid client id"))
		return
	}
	actor := actorFrom(r.Context())

	contentType := r.Header.Get("Content-Type")
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil &&
		strings.HasPrefix(mediaType, "multipart/") {
		a.sendWithMedia(w, r, actor, id)
		return
	}

	var body struct {
		Text string `json:"text"`
	}
	if err := decodeJSON(r, &body, 1<<20); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid request"))
		return
	}
	message, err := a.crm.SendMessage(r.Context(), actor, id, body.Text, nil)
	if err != nil && message == nil {
		a.writeServiceError(w, err)
		return
	}
	a.hub.ClientChanged(id)
	a.respondSent(w, message, err)
}

func (a *API) sendWithMedia(w http.ResponseWriter, r *http.Request, actor service.Actor, clientID int64) {
	if a.media == nil {
		writeJSON(w, http.StatusBadRequest, errorBody("media sending is not configured"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, a.cfg.MaxUploadSize+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, errorBody("upload is too large"))
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("file is required"))
		return
	}
	defer file.Close()

	sniffed, err := sniffContentType(file)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("could not read the uploaded file"))
		return
	}

	// Resolving the type: the browser's own header wins, but several clients
	// send application/octet-stream for every file, so the extension and then
	// the sniffed type act as fallbacks. Whatever is resolved must still pass
	// the allow-list, and the sniffed content must agree with it: a .pdf that
	// is really an HTML page is rejected.
	declared := strings.TrimSpace(header.Header.Get("Content-Type"))
	if declared == "" || strings.EqualFold(declared, "application/octet-stream") {
		if byExt := mime.TypeByExtension(strings.ToLower(filepath.Ext(header.Filename))); byExt != "" {
			declared = byExt
		} else if sniffed != "" && !strings.EqualFold(sniffed, "application/octet-stream") {
			declared = sniffed
		}
	}
	if !compatibleTypes(declared, sniffed) {
		writeJSON(w, http.StatusUnsupportedMediaType,
			errorBody("file content does not match its declared type"))
		return
	}

	voice := r.FormValue("voice") == "1"
	stored, err := a.media.Save(file, declared, header.Filename, voice)
	if err != nil {
		a.writeServiceError(w, err)
		return
	}

	message, sendErr := a.crm.SendMessage(r.Context(), actor, clientID, r.FormValue("text"), &stored)
	if sendErr != nil && message == nil {
		// The upload is discarded when the send never produced a message row,
		// so a failed send does not leave an orphan file behind.
		if err := os.Remove(stored.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			a.log.Warn("remove orphaned upload failed", zap.Error(err))
		}
		a.writeServiceError(w, sendErr)
		return
	}
	a.hub.ClientChanged(clientID)
	a.respondSent(w, message, sendErr)
}

func (a *API) respondSent(w http.ResponseWriter, message *domain.Message, sendErr error) {
	body := map[string]any{"message": renderMessage(*message)}
	if sendErr != nil {
		// The message is stored and visible, but WhatsApp refused it. The CRM
		// shows both facts rather than pretending the send succeeded.
		body["warning"] = "saved, but WhatsApp delivery failed: " + sendErr.Error()
		writeJSON(w, http.StatusAccepted, body)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// sniffContentType reads the first bytes of an upload and rewinds it.
func sniffContentType(file io.ReadSeeker) (string, error) {
	head := make([]byte, 512)
	n, err := file.Read(head)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return http.DetectContentType(head[:n]), nil
}

// compatibleTypes reports whether a sniffed type is consistent with a declared
// one. Office and audio containers are detected as generic binary data by the
// sniffer, so an octet-stream result is accepted for a declared allow-listed
// type; a disagreement about the top-level family is not.
func compatibleTypes(declared, sniffed string) bool {
	declaredMime, _, err := service.ValidateMime(declared)
	if err != nil {
		return false
	}
	sniffed = strings.ToLower(strings.TrimSpace(sniffed))
	if i := strings.IndexByte(sniffed, ';'); i >= 0 {
		sniffed = sniffed[:i]
	}
	switch {
	case sniffed == "" || sniffed == "application/octet-stream" || sniffed == "application/zip":
		return true
	case sniffed == declaredMime:
		return true
	}
	// An HTML or script sniff is never acceptable, whatever was declared.
	if strings.Contains(sniffed, "html") || strings.Contains(sniffed, "javascript") ||
		strings.Contains(sniffed, "xml") {
		return false
	}
	return family(sniffed) == family(declaredMime)
}

func family(mimeType string) string {
	if i := strings.IndexByte(mimeType, '/'); i > 0 {
		return mimeType[:i]
	}
	return mimeType
}

// -------------------------------------------------------------------- media

// handleMedia serves a stored media file to an authenticated consultant.
func (a *API) handleMedia(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid message id"))
		return
	}
	message, err := a.messages.GetByID(r.Context(), id)
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	if message.MediaPath == "" {
		writeJSON(w, http.StatusNotFound, errorBody("this message has no stored media"))
		return
	}
	// Resolve refuses any path outside the media root, so a tampered row cannot
	// turn this endpoint into an arbitrary file reader.
	path, err := a.media.Resolve(message.MediaPath)
	if err != nil {
		a.log.Warn("resolve media failed", zap.Int64("message_id", id), zap.Error(err))
		writeJSON(w, http.StatusNotFound, errorBody("media unavailable"))
		return
	}
	file, err := os.Open(path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorBody("media unavailable"))
		return
	}
	defer file.Close()

	name := message.MediaName
	if name == "" {
		name = filepath.Base(path)
	}
	contentType := message.MediaMime
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	// Serving media inline is only safe because the allow-list excludes HTML and
	// SVG; everything that could execute is refused at upload time. The headers
	// below stop a browser from second-guessing the type anyway.
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("inline", map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "private, max-age=3600")

	info, err := file.Stat()
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorBody("media unavailable"))
		return
	}
	http.ServeContent(w, r, name, info.ModTime(), file)
}

// ---------------------------------------------------------------- rendering

func (a *API) clientSummary(c *domain.CRMClient) map[string]any {
	name := contactDisplayName(c)
	return map[string]any{
		"id":              c.ID,
		"name":            name,
		"display_name":    c.DisplayName,
		"phone":           service.FormatE164(c.PhoneNumber),
		"language":        string(c.Language.OrDefault()),
		"service":         c.DetectedService,
		"service_name":    a.catalog.Name(c.DetectedService, c.Language),
		"status":          string(c.CRMStatus),
		"status_label":    service.StatusLabel(c.CRMStatus, domain.LangRU),
		"current_state":   string(c.CurrentState),
		"mode":            string(c.Mode),
		"blocked":         c.Blocked,
		"assigned_id":     c.AssignedAdminID,
		"assigned_name":   c.AssignedAdminName,
		"unread":          c.UnreadCount,
		"last_message":    c.LastMessagePreview,
		"last_message_at": c.LastMessageAt,
		"last_activity":   c.LastInboundAt,
		"next_follow_up":  c.NextFollowUpAt,
		"created_at":      c.User.CreatedAt,
		"needs_human":     c.CRMStatus == domain.CRMNeedsConsultant,
	}
}

func (a *API) clientDetail(r *http.Request, c *domain.CRMClient) map[string]any {
	detail := a.clientSummary(c)
	detail["whatsapp_id"] = c.WhatsAppUserID
	detail["ai_enabled"] = c.AIEnabled
	detail["summary"] = c.AISummary
	detail["important_facts"] = c.ImportantFacts
	detail["next_action"] = c.NextAction
	detail["qualification_stage"] = c.QualificationStage
	detail["confidence"] = c.AIConfidence
	detail["intent"] = c.Intent
	detail["tags"] = c.Tags
	detail["close_reason"] = c.CloseReason
	detail["block_reason"] = c.BlockReason
	detail["blocked_at"] = c.BlockedAt
	detail["assigned_at"] = c.AssignedAt
	detail["first_seen_at"] = c.FirstSeenAt
	detail["last_inbound_at"] = c.LastInboundAt
	detail["last_outbound_at"] = c.LastOutboundAt
	detail["follow_up_stage"] = c.FollowUpStage
	detail["language_locked"] = c.LanguageLocked
	detail["lead_score"] = c.LeadScore

	if job, err := a.jobs.NextPendingForUser(r.Context(), c.ID); err == nil {
		detail["next_follow_up_job"] = map[string]any{
			"id": job.ID, "stage": job.Stage, "scheduled_at": job.ScheduledAt,
		}
	}
	return detail
}

func renderMessages(messages []domain.Message) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		out = append(out, renderMessage(m))
	}
	return out
}

func renderMessage(m domain.Message) map[string]any {
	body := m.Text
	if body == "" {
		body = m.Caption
	}
	sender := m.SenderOrDefault()
	item := map[string]any{
		"id":              m.ID,
		"conversation_id": m.UserID,
		"direction":       string(m.Direction),
		"direction_type":  apiDirection(m.Direction),
		"sender":          string(sender),
		"sender_type":     string(sender),
		"sender_name":     senderName(sender),
		"type":            string(m.MessageType),
		"text":            body,
		"created_at":      m.CreatedAt,
		"delivery":        m.DeliveryStatus,
		"admin_id":        m.SenderAdminID,
		"provider_id":     m.WhatsAppMessageID,
	}
	if m.MediaPath != "" {
		item["media"] = map[string]any{
			"url":  fmt.Sprintf("/admin/api/media/%d", m.ID),
			"name": m.MediaName,
			"mime": m.MediaMime,
			"size": m.MediaSize,
		}
	} else if m.MediaID != "" {
		// The provider had media but the download failed or is unsupported.
		item["media_unavailable"] = true
	}
	return item
}

func contactDisplayName(c *domain.CRMClient) string {
	if c == nil {
		return ""
	}
	if name := strings.TrimSpace(c.DisplayName); name != "" {
		return name
	}
	if phone := service.FormatE164(c.PhoneNumber); phone != "" {
		return phone
	}
	return c.WhatsAppUserID
}

func apiDirection(direction domain.Direction) string {
	if direction == domain.DirectionOutgoing {
		return "outbound"
	}
	return "inbound"
}

func senderName(sender domain.SenderType) string {
	switch sender {
	case domain.SenderClient:
		return "Customer"
	case domain.SenderAI:
		return "Bot"
	case domain.SenderConsultant:
		return "Admin"
	case domain.SenderSystem:
		return "System"
	default:
		return string(sender)
	}
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseDate(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// repositoryFilterForSearch builds a small filter for the global search box.
func repositoryFilterForSearch(query string) repository.ClientFilter {
	return repository.ClientFilter{Search: query, Limit: 25, Sort: "last_activity", Desc: true}
}
