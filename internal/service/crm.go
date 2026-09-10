package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"lawyer-bot/internal/domain"
	"lawyer-bot/internal/repository"
)

// CRMService is the operational layer behind the Admin CRM.
//
// Every consultant action goes through here rather than through the repository
// directly, because each one has to do three things atomically from the
// operator's point of view: change the client's state, stop or start automation
// consistently with that change, and leave an audit entry.
type CRMService struct {
	clients  *repository.CRMRepository
	messages *repository.MessageRepository
	notes    *repository.NoteRepository
	audit    *repository.AuditRepository
	jobs     *repository.FollowUpRepository
	admins   *repository.AdminRepository
	follow   *FollowUpService
	sender   *Messenger
	media    *MediaStore
	catalog  *Catalog
	log      *zap.Logger
}

// CRMDeps groups the collaborators.
type CRMDeps struct {
	Clients  *repository.CRMRepository
	Messages *repository.MessageRepository
	Notes    *repository.NoteRepository
	Audit    *repository.AuditRepository
	Jobs     *repository.FollowUpRepository
	Admins   *repository.AdminRepository
	FollowUp *FollowUpService
	Sender   *Messenger
	Media    *MediaStore
	Catalog  *Catalog
	Logger   *zap.Logger
}

// NewCRMService builds the CRM service.
func NewCRMService(deps CRMDeps) *CRMService {
	log := deps.Logger
	if log == nil {
		log = zap.NewNop()
	}
	return &CRMService{
		clients:  deps.Clients,
		messages: deps.Messages,
		notes:    deps.Notes,
		audit:    deps.Audit,
		jobs:     deps.Jobs,
		admins:   deps.Admins,
		follow:   deps.FollowUp,
		sender:   deps.Sender,
		media:    deps.Media,
		catalog:  deps.Catalog,
		log:      log,
	}
}

// Actor identifies the consultant performing an action.
type Actor struct {
	ID     int64
	Name   string
	Role   domain.AdminRole
	IPHash string
}

// ErrForbidden is returned when a role may not perform an action.
var ErrForbidden = errors.New("forbidden")

// ErrConflict is returned when a state change lost a race with another actor.
var ErrConflict = errors.New("state changed concurrently")

func (s *CRMService) requireWrite(a Actor) error {
	if !a.Role.CanWrite() {
		return ErrForbidden
	}
	return nil
}

// Client loads one CRM client.
func (s *CRMService) Client(ctx context.Context, id int64) (*domain.CRMClient, error) {
	return s.clients.GetClient(ctx, id)
}

// TakeOver hands a conversation to a consultant and stops all automation for it.
//
// The mode switch is a compare-and-swap against the current mode, so two
// consultants pressing "Take over" at the same moment cannot both win. Every
// pending follow-up is cancelled in the same call: a nudge must never arrive
// while a human is typing.
func (s *CRMService) TakeOver(ctx context.Context, a Actor, clientID int64) (*domain.CRMClient, error) {
	if err := s.requireWrite(a); err != nil {
		return nil, err
	}
	client, err := s.clients.GetClient(ctx, clientID)
	if err != nil {
		return nil, err
	}
	if client.Mode == domain.ModeHuman {
		// Already human-owned: make the call idempotent rather than an error.
		return client, nil
	}
	ok, err := s.clients.SetMode(ctx, clientID, domain.ModeHuman, a.ID, client.Mode)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrConflict
	}
	s.follow.CancelFor(ctx, clientID, "consultant took over the conversation")
	s.systemEvent(ctx, clientID, fmt.Sprintf("Консультант %s взял диалог в работу", displayName(a)))
	s.record(ctx, a, domain.AuditTakeover, "client", clientID, "conversation switched to human mode")
	return s.clients.GetClient(ctx, clientID)
}

// ResumeAI returns a conversation to automatic handling.
func (s *CRMService) ResumeAI(ctx context.Context, a Actor, clientID int64) (*domain.CRMClient, error) {
	if err := s.requireWrite(a); err != nil {
		return nil, err
	}
	client, err := s.clients.GetClient(ctx, clientID)
	if err != nil {
		return nil, err
	}
	if client.Blocked {
		return nil, errors.New("unblock the client before resuming automation")
	}
	if _, err := s.clients.SetMode(ctx, clientID, domain.ModeAI, a.ID, ""); err != nil {
		return nil, err
	}
	s.systemEvent(ctx, clientID, fmt.Sprintf("Консультант %s вернул диалог ассистенту", displayName(a)))
	s.record(ctx, a, domain.AuditResumeAI, "client", clientID, "conversation switched to ai mode")
	return s.clients.GetClient(ctx, clientID)
}

// PauseAI stops automation without assigning a human owner.
func (s *CRMService) PauseAI(ctx context.Context, a Actor, clientID int64) (*domain.CRMClient, error) {
	if err := s.requireWrite(a); err != nil {
		return nil, err
	}
	if _, err := s.clients.SetMode(ctx, clientID, domain.ModePaused, a.ID, ""); err != nil {
		return nil, err
	}
	s.follow.CancelFor(ctx, clientID, "automation paused")
	s.systemEvent(ctx, clientID, fmt.Sprintf("Автоответы приостановлены (%s)", displayName(a)))
	s.record(ctx, a, domain.AuditPauseAI, "client", clientID, "automation paused")
	return s.clients.GetClient(ctx, clientID)
}

// Block stops every automatic interaction with a client. History is preserved.
func (s *CRMService) Block(ctx context.Context, a Actor, clientID int64, reason string) (*domain.CRMClient, error) {
	if err := s.requireWrite(a); err != nil {
		return nil, err
	}
	if err := s.clients.SetBlocked(ctx, clientID, true, a.ID, reason); err != nil {
		return nil, err
	}
	s.follow.CancelFor(ctx, clientID, "client blocked")
	s.systemEvent(ctx, clientID, fmt.Sprintf("Клиент заблокирован (%s)", displayName(a)))
	s.record(ctx, a, domain.AuditBlock, "client", clientID, "blocked: "+truncateRunes(reason, 200))
	return s.clients.GetClient(ctx, clientID)
}

// Unblock restores a client to normal automatic handling.
func (s *CRMService) Unblock(ctx context.Context, a Actor, clientID int64) (*domain.CRMClient, error) {
	if err := s.requireWrite(a); err != nil {
		return nil, err
	}
	if err := s.clients.SetBlocked(ctx, clientID, false, a.ID, ""); err != nil {
		return nil, err
	}
	s.systemEvent(ctx, clientID, fmt.Sprintf("Клиент разблокирован (%s)", displayName(a)))
	s.record(ctx, a, domain.AuditUnblock, "client", clientID, "unblocked")
	return s.clients.GetClient(ctx, clientID)
}

// Assign links a client to a consultant. consultantID of zero unassigns.
func (s *CRMService) Assign(ctx context.Context, a Actor, clientID, consultantID int64) (*domain.CRMClient, error) {
	if err := s.requireWrite(a); err != nil {
		return nil, err
	}
	if consultantID != 0 {
		consultant, err := s.admins.GetAdmin(ctx, consultantID)
		if err != nil {
			return nil, fmt.Errorf("consultant not found: %w", err)
		}
		if !consultant.Active {
			return nil, errors.New("consultant account is disabled")
		}
	}
	if err := s.clients.Assign(ctx, clientID, consultantID, a.ID); err != nil {
		return nil, err
	}
	action, detail := domain.AuditAssign, fmt.Sprintf("assigned to admin %d", consultantID)
	if consultantID == 0 {
		action, detail = domain.AuditUnassign, "assignment cleared"
	}
	s.record(ctx, a, action, "client", clientID, detail)
	return s.clients.GetClient(ctx, clientID)
}

// SetStatus moves a client through the pipeline by hand.
//
// Moving into a terminal status also stops every scheduled follow-up, which is
// the behaviour requirement 27 asks for: a closed lead stops being nudged.
func (s *CRMService) SetStatus(ctx context.Context, a Actor, clientID int64, status domain.CRMStatus, reason string) (*domain.CRMClient, error) {
	if err := s.requireWrite(a); err != nil {
		return nil, err
	}
	if !ManualStatusAllowed(status) {
		return nil, fmt.Errorf("status %q cannot be set manually", status)
	}
	if err := s.clients.SetStatus(ctx, clientID, status, reason); err != nil {
		return nil, err
	}
	if status.Terminal() {
		s.follow.CancelFor(ctx, clientID, "lead closed: "+string(status))
		s.systemEvent(ctx, clientID, fmt.Sprintf("Статус изменён на «%s» (%s)", StatusLabel(status, domain.LangRU), displayName(a)))
	}
	s.record(ctx, a, domain.AuditStatusChange, "client", clientID,
		fmt.Sprintf("status=%s reason=%s", status, truncateRunes(reason, 200)))
	return s.clients.GetClient(ctx, clientID)
}

// UpdateClient applies a manual edit to the client card.
func (s *CRMService) UpdateClient(ctx context.Context, a Actor, clientID int64, patch repository.ClientPatch) (*domain.CRMClient, error) {
	if err := s.requireWrite(a); err != nil {
		return nil, err
	}
	if patch.Status != nil && !ManualStatusAllowed(*patch.Status) {
		return nil, fmt.Errorf("status %q cannot be set manually", *patch.Status)
	}
	if patch.Service != nil && *patch.Service != "" && !s.catalog.Has(*patch.Service) {
		return nil, fmt.Errorf("unknown service %q", *patch.Service)
	}
	if err := s.clients.UpdateClient(ctx, clientID, patch); err != nil {
		return nil, err
	}
	s.record(ctx, a, domain.AuditClientUpdate, "client", clientID, describePatch(patch))
	return s.clients.GetClient(ctx, clientID)
}

// SendMessage delivers a consultant's message through the shared outbound layer.
//
// Sending from the CRM implicitly takes the conversation over. That is the
// safest default: the moment a human speaks, the assistant must stop, and a
// consultant should never have to remember to press a button first.
func (s *CRMService) SendMessage(ctx context.Context, a Actor, clientID int64, text string, media *StoredMedia) (*domain.Message, error) {
	if err := s.requireWrite(a); err != nil {
		return nil, err
	}
	client, err := s.clients.GetClient(ctx, clientID)
	if err != nil {
		return nil, err
	}
	if client.Blocked {
		return nil, errors.New("client is blocked; unblock before sending")
	}
	text = strings.TrimSpace(text)
	if text == "" && media == nil {
		return nil, errors.New("message is empty")
	}
	if len([]rune(text)) > 4000 {
		return nil, errors.New("message is too long")
	}

	// A human is speaking: automation stops before the message leaves.
	if client.Mode != domain.ModeHuman {
		if _, err := s.clients.SetMode(ctx, clientID, domain.ModeHuman, a.ID, ""); err != nil {
			return nil, err
		}
		s.record(ctx, a, domain.AuditTakeover, "client", clientID, "implicit takeover on manual message")
	}
	s.follow.CancelFor(ctx, clientID, "consultant replied")

	out := Outbound{
		UserID:    client.ID,
		Recipient: client.WhatsAppUserID,
		Sender:    domain.SenderConsultant,
		AdminID:   a.ID,
		Kind:      "manual",
		Text:      text,
	}
	if media != nil {
		out.MediaPath = media.Path
		out.MediaName = media.DisplayName
		out.MediaMime = media.MimeType
		out.MediaSize = media.Size
		out.MediaType = media.Kind
	}

	res, sendErr := s.sender.Send(ctx, out)
	detail := "text"
	if media != nil {
		detail = "media " + media.MimeType
	}
	s.record(ctx, a, domain.AuditManualMessage, "client", clientID, detail)

	if res.MessageID == 0 {
		if sendErr != nil {
			return nil, sendErr
		}
		return nil, errors.New("message was not stored")
	}
	stored, err := s.messages.GetByID(ctx, res.MessageID)
	if err != nil {
		return nil, err
	}
	return stored, sendErr
}

// -------------------------------------------------------------------- notes

// AddNote records a private consultant note.
func (s *CRMService) AddNote(ctx context.Context, a Actor, clientID int64, body string) (*domain.InternalNote, error) {
	if err := s.requireWrite(a); err != nil {
		return nil, err
	}
	note, err := s.notes.Create(ctx, clientID, a.ID, body)
	if err != nil {
		return nil, err
	}
	s.record(ctx, a, domain.AuditNoteCreate, "client", clientID, fmt.Sprintf("note %d", note.ID))
	return note, nil
}

// UpdateNote edits a note. Consultants may only edit their own.
func (s *CRMService) UpdateNote(ctx context.Context, a Actor, noteID int64, body string) (*domain.InternalNote, error) {
	if err := s.requireWrite(a); err != nil {
		return nil, err
	}
	note, err := s.notes.Get(ctx, noteID)
	if err != nil {
		return nil, err
	}
	if note.AdminID != a.ID && !a.Role.CanAdminister() {
		return nil, ErrForbidden
	}
	if err := s.notes.Update(ctx, noteID, body); err != nil {
		return nil, err
	}
	s.record(ctx, a, domain.AuditNoteUpdate, "client", note.UserID, fmt.Sprintf("note %d", noteID))
	return s.notes.Get(ctx, noteID)
}

// DeleteNote soft-deletes a note.
func (s *CRMService) DeleteNote(ctx context.Context, a Actor, noteID int64) error {
	if err := s.requireWrite(a); err != nil {
		return err
	}
	note, err := s.notes.Get(ctx, noteID)
	if err != nil {
		return err
	}
	if note.AdminID != a.ID && !a.Role.CanAdminister() {
		return ErrForbidden
	}
	if err := s.notes.Delete(ctx, noteID); err != nil {
		return err
	}
	s.record(ctx, a, domain.AuditNoteDelete, "client", note.UserID, fmt.Sprintf("note %d", noteID))
	return nil
}

// --------------------------------------------------------------- follow-ups

// CancelFollowUp drops one scheduled nudge.
func (s *CRMService) CancelFollowUp(ctx context.Context, a Actor, jobID int64) error {
	if err := s.requireWrite(a); err != nil {
		return err
	}
	if err := s.jobs.Cancel(ctx, jobID, "cancelled by "+displayName(a)); err != nil {
		return err
	}
	s.record(ctx, a, domain.AuditFollowUpCancel, "follow_up", jobID, "cancelled")
	return nil
}

// RescheduleFollowUp moves a nudge to a new time.
func (s *CRMService) RescheduleFollowUp(ctx context.Context, a Actor, jobID int64, at time.Time) error {
	if err := s.requireWrite(a); err != nil {
		return err
	}
	if at.Before(time.Now().Add(-time.Minute)) {
		return errors.New("follow-up time must be in the future")
	}
	if err := s.jobs.Reschedule(ctx, jobID, at); err != nil {
		return err
	}
	s.record(ctx, a, domain.AuditFollowUpResched, "follow_up", jobID, at.UTC().Format(time.RFC3339))
	return nil
}

// ------------------------------------------------------------------ helpers

// systemEvent writes a visible system line into the conversation, so the CRM
// chat shows who took over and when. It is stored, never sent to WhatsApp.
func (s *CRMService) systemEvent(ctx context.Context, clientID int64, text string) {
	if _, err := s.messages.Create(ctx, &domain.Message{
		UserID:      clientID,
		MessageType: domain.MessageText,
		Text:        text,
		Direction:   domain.DirectionOutgoing,
		SenderType:  domain.SenderSystem,
		Processed:   true,
		// Never delivered: system events exist for the CRM timeline only.
		DeliveryStatus: "internal",
	}); err != nil {
		s.log.Warn("store system event failed", zap.Error(err))
	}
}

func (s *CRMService) record(ctx context.Context, a Actor, action, entity string, entityID int64, detail string) {
	if s.audit == nil {
		return
	}
	if err := s.audit.Record(ctx, domain.AuditLog{
		AdminID: a.ID, Action: action, Entity: entity, EntityID: entityID,
		Detail: detail, IPHash: a.IPHash,
	}); err != nil {
		s.log.Warn("write audit entry failed", zap.String("action", action), zap.Error(err))
	}
}

func displayName(a Actor) string {
	if strings.TrimSpace(a.Name) != "" {
		return a.Name
	}
	return fmt.Sprintf("#%d", a.ID)
}

func describePatch(p repository.ClientPatch) string {
	var parts []string
	if p.DisplayName != nil {
		parts = append(parts, "name")
	}
	if p.Language != nil {
		parts = append(parts, "language="+string(*p.Language))
	}
	if p.Service != nil {
		parts = append(parts, "service="+*p.Service)
	}
	if p.Status != nil {
		parts = append(parts, "status="+string(*p.Status))
	}
	if p.Tags != nil {
		parts = append(parts, "tags")
	}
	if p.NextAction != nil {
		parts = append(parts, "next_action")
	}
	if p.CloseReason != nil {
		parts = append(parts, "close_reason")
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, ", ")
}

// StatusLabel renders a pipeline status in the requested language. Kazakh and
// Russian labels are what the CRM and the exports display.
func StatusLabel(status domain.CRMStatus, lang domain.Language) string {
	if table, ok := statusLabels[status]; ok {
		return tr(lang, table)
	}
	return string(status)
}

var statusLabels = map[domain.CRMStatus]map[domain.Language]string{
	domain.CRMNew:                  {domain.LangRU: "Новый", domain.LangKK: "Жаңа", domain.LangEN: "New"},
	domain.CRMAIProcessing:         {domain.LangRU: "Обработка ассистентом", domain.LangKK: "Ассистент өңдеуде", domain.LangEN: "AI processing"},
	domain.CRMNeedsQualification:   {domain.LangRU: "Нужна квалификация", domain.LangKK: "Біліктілік қажет", domain.LangEN: "Needs qualification"},
	domain.CRMQualified:            {domain.LangRU: "Квалифицирован", domain.LangKK: "Біліктілігі расталған", domain.LangEN: "Qualified"},
	domain.CRMWaitingForClient:     {domain.LangRU: "Ждём клиента", domain.LangKK: "Клиентті күтудеміз", domain.LangEN: "Waiting for client"},
	domain.CRMNeedsConsultant:      {domain.LangRU: "Нужен консультант", domain.LangKK: "Кеңесші қажет", domain.LangEN: "Needs consultant"},
	domain.CRMConsultantProcessing: {domain.LangRU: "В работе у консультанта", domain.LangKK: "Кеңесшінің жұмысында", domain.LangEN: "Consultant processing"},
	domain.CRMConsultationSet:      {domain.LangRU: "Консультация назначена", domain.LangKK: "Кеңес белгіленді", domain.LangEN: "Consultation scheduled"},
	domain.CRMInProgress:           {domain.LangRU: "В работе", domain.LangKK: "Жұмыста", domain.LangEN: "In progress"},
	domain.CRMWon:                  {domain.LangRU: "Успешно закрыт", domain.LangKK: "Сәтті аяқталды", domain.LangEN: "Won"},
	domain.CRMClosed:               {domain.LangRU: "Закрыт", domain.LangKK: "Жабылды", domain.LangEN: "Closed"},
	domain.CRMLost:                 {domain.LangRU: "Потерян", domain.LangKK: "Жоғалды", domain.LangEN: "Lost"},
	domain.CRMBlocked:              {domain.LangRU: "Заблокирован", domain.LangKK: "Бұғатталған", domain.LangEN: "Blocked"},
}

// ModeLabel renders a conversation mode for the CRM.
func ModeLabel(mode domain.ConversationMode, lang domain.Language) string {
	switch mode.OrDefault() {
	case domain.ModeHuman:
		return tr(lang, map[domain.Language]string{
			domain.LangRU: "Консультант", domain.LangKK: "Кеңесші", domain.LangEN: "Consultant"})
	case domain.ModePaused:
		return tr(lang, map[domain.Language]string{
			domain.LangRU: "Пауза", domain.LangKK: "Кідіріс", domain.LangEN: "Paused"})
	default:
		return tr(lang, map[domain.Language]string{
			domain.LangRU: "Ассистент", domain.LangKK: "Ассистент", domain.LangEN: "Assistant"})
	}
}
