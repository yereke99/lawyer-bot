package repository

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lawyer-bot/internal/domain"
)

// newCRMTestDB gives every test its own migrated database.
func newCRMTestDB(t *testing.T) (*DB, *CRMRepository, *UserRepository) {
	t.Helper()
	db := newTestDB(t)
	return db, NewCRMRepository(db), NewUserRepository(db)
}

func mustClient(t *testing.T, users *UserRepository, waID, phone, name string) *domain.User {
	t.Helper()
	u, err := users.Upsert(context.Background(), waID, phone, name)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	return u
}

// Scenario 20: the CRM migration must be safe to run repeatedly on a database
// that already holds production rows.
func TestCRMMigrationIsIdempotentAndPreservesData(t *testing.T) {
	ctx := context.Background()
	db, crm, users := newCRMTestDB(t)

	u := mustClient(t, users, "wa-migrate", "77015550000", "Айгүл")
	msgs := NewMessageRepository(db)
	if _, err := msgs.Create(ctx, &domain.Message{
		UserID: u.ID, WhatsAppMessageID: "m-1", MessageType: domain.MessageText,
		Text: "нужен товарный знак", Direction: domain.DirectionIncoming,
	}); err != nil {
		t.Fatalf("store message: %v", err)
	}

	// Running both migrations again must change nothing.
	for i := 0; i < 3; i++ {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("re-run base migration %d: %v", i, err)
		}
		if err := db.MigrateCRM(ctx); err != nil {
			t.Fatalf("re-run crm migration %d: %v", i, err)
		}
	}

	client, err := crm.GetClient(ctx, u.ID)
	if err != nil {
		t.Fatalf("load client after migration: %v", err)
	}
	if client.DisplayName != "Айгүл" || client.PhoneNumber != "77015550000" {
		t.Fatalf("existing client data was altered: %+v", client)
	}
	if client.CRMStatus != domain.CRMNew {
		t.Fatalf("expected default status new, got %q", client.CRMStatus)
	}
	if client.Mode != domain.ModeAI || !client.AIEnabled {
		t.Fatalf("legacy rows must default to AI mode, got mode=%q enabled=%v", client.Mode, client.AIEnabled)
	}

	stored, _, err := msgs.PageByUser(ctx, u.ID, 0, 10)
	if err != nil {
		t.Fatalf("page messages: %v", err)
	}
	if len(stored) != 1 || stored[0].Text != "нужен товарный знак" {
		t.Fatalf("message history was not preserved: %+v", stored)
	}
	// The backfill must classify historical rows so the CRM renders them.
	if stored[0].SenderOrDefault() != domain.SenderClient {
		t.Fatalf("inbound history should be attributed to the client, got %q", stored[0].SenderType)
	}
}

// Scenario 2: a returning client is found, never duplicated.
func TestExistingClientIsFoundByWhatsAppIdentity(t *testing.T) {
	ctx := context.Background()
	_, crm, users := newCRMTestDB(t)

	first := mustClient(t, users, "77015551234@c.us", "77015551234", "Ерлан")
	second := mustClient(t, users, "77015551234@c.us", "77015551234", "Ерлан Е.")
	if first.ID != second.ID {
		t.Fatalf("a second message created a duplicate client: %d vs %d", first.ID, second.ID)
	}

	client, err := crm.GetClientByWhatsAppID(ctx, "77015551234@c.us")
	if err != nil {
		t.Fatalf("lookup by whatsapp id: %v", err)
	}
	if client.ID != first.ID {
		t.Fatalf("lookup returned the wrong client")
	}
}

// Scenario 6/7: taking over is a compare-and-swap, so two consultants racing
// for the same conversation cannot both win.
func TestTakeoverIsAtomicUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	_, crm, users := newCRMTestDB(t)
	u := mustClient(t, users, "wa-race", "77010000001", "Race")

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(adminID int64) {
			defer wg.Done()
			ok, err := crm.SetMode(ctx, u.ID, domain.ModeHuman, adminID, domain.ModeAI)
			if err != nil {
				return
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(int64(i + 1))
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("exactly one consultant must win the takeover, got %d", wins)
	}
	client, err := crm.GetClient(ctx, u.ID)
	if err != nil {
		t.Fatalf("load client: %v", err)
	}
	if client.Mode != domain.ModeHuman || client.AIEnabled {
		t.Fatalf("takeover must disable automation, got mode=%q ai=%v", client.Mode, client.AIEnabled)
	}
	if !client.AutomationAllowed() == false {
		t.Fatal("automation must not be allowed after takeover")
	}
}

// Scenario 8: resuming returns the conversation to automatic handling.
func TestResumeAIRestoresAutomation(t *testing.T) {
	ctx := context.Background()
	_, crm, users := newCRMTestDB(t)
	u := mustClient(t, users, "wa-resume", "77010000002", "Resume")

	if _, err := crm.SetMode(ctx, u.ID, domain.ModeHuman, 1, ""); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if _, err := crm.SetMode(ctx, u.ID, domain.ModeAI, 1, ""); err != nil {
		t.Fatalf("resume: %v", err)
	}

	client, _ := crm.GetClient(ctx, u.ID)
	if !client.AutomationAllowed() {
		t.Fatalf("automation must be allowed after resume: mode=%q ai=%v status=%q",
			client.Mode, client.AIEnabled, client.CRMStatus)
	}
}

// Scenario 12: a blocked client stops all automation and keeps their history.
func TestBlockStopsAutomationAndKeepsHistory(t *testing.T) {
	ctx := context.Background()
	db, crm, users := newCRMTestDB(t)
	u := mustClient(t, users, "wa-block", "77010000003", "Blocked")

	msgs := NewMessageRepository(db)
	if _, err := msgs.Create(ctx, &domain.Message{
		UserID: u.ID, WhatsAppMessageID: "b-1", MessageType: domain.MessageText,
		Text: "старое сообщение", Direction: domain.DirectionIncoming,
	}); err != nil {
		t.Fatalf("store message: %v", err)
	}

	if err := crm.SetBlocked(ctx, u.ID, true, 7, "спам"); err != nil {
		t.Fatalf("block: %v", err)
	}
	client, _ := crm.GetClient(ctx, u.ID)
	if !client.Blocked || client.AutomationAllowed() {
		t.Fatalf("blocked client must not be automated: %+v", client)
	}
	if client.CRMStatus != domain.CRMBlocked || client.BlockedBy != 7 || client.BlockReason != "спам" {
		t.Fatalf("block metadata was not persisted: %+v", client)
	}
	if client.BlockedAt == nil {
		t.Fatal("block timestamp must be recorded")
	}

	history, _, err := msgs.PageByUser(ctx, u.ID, 0, 10)
	if err != nil || len(history) != 1 {
		t.Fatalf("blocking must never delete history: %d messages, err=%v", len(history), err)
	}

	if err := crm.SetBlocked(ctx, u.ID, false, 7, ""); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	client, _ = crm.GetClient(ctx, u.ID)
	if client.Blocked || !client.AutomationAllowed() {
		t.Fatalf("unblock must restore automation: %+v", client)
	}
}

// Scenario 13: a closed lead is terminal for automation.
func TestClosedLeadStopsAutomation(t *testing.T) {
	ctx := context.Background()
	_, crm, users := newCRMTestDB(t)
	u := mustClient(t, users, "wa-closed", "77010000004", "Closed")

	if err := crm.SetStatus(ctx, u.ID, domain.CRMWon, "договор подписан"); err != nil {
		t.Fatalf("set status: %v", err)
	}
	client, _ := crm.GetClient(ctx, u.ID)
	if client.AutomationAllowed() {
		t.Fatal("a won lead must not receive automatic messages")
	}
	if client.CloseReason != "договор подписан" {
		t.Fatalf("close reason was not stored: %q", client.CloseReason)
	}
}

// Follow-up scheduling is idempotent and claiming is exclusive (scenario 11).
func TestFollowUpSchedulingIsIdempotentAndClaimIsExclusive(t *testing.T) {
	ctx := context.Background()
	db, _, users := newCRMTestDB(t)
	jobs := NewFollowUpRepository(db)
	u := mustClient(t, users, "wa-follow", "77010000005", "Follow")

	job := domain.FollowUpJob{
		UserID: u.ID, Stage: 1, ScheduledAt: time.Now().UTC().Add(-time.Minute),
		DedupeKey: FollowUpDedupeKey(u.ID, 1, 42), MessageID: 42,
	}
	if _, created, err := jobs.Schedule(ctx, job); err != nil || !created {
		t.Fatalf("first schedule must create a job: created=%v err=%v", created, err)
	}
	// The same logical nudge, scheduled again, must not produce a second job.
	if _, created, err := jobs.Schedule(ctx, job); err != nil || created {
		t.Fatalf("duplicate schedule must be a no-op: created=%v err=%v", created, err)
	}

	// Two workers claim concurrently; exactly one may get the job.
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed int
	)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			token := "worker-" + strings.Repeat("x", n+1)
			got, err := jobs.Claim(ctx, token, time.Now().UTC(), 10, time.Hour)
			if err != nil {
				return
			}
			mu.Lock()
			claimed += len(got)
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if claimed != 1 {
		t.Fatalf("a due job must be claimed exactly once, got %d claims", claimed)
	}
}

// Scenario 10: a client's reply cancels every outstanding nudge.
func TestClientReplyCancelsPendingFollowUps(t *testing.T) {
	ctx := context.Background()
	db, _, users := newCRMTestDB(t)
	jobs := NewFollowUpRepository(db)
	u := mustClient(t, users, "wa-cancel", "77010000006", "Cancel")

	for stage := 1; stage <= 3; stage++ {
		if _, _, err := jobs.Schedule(ctx, domain.FollowUpJob{
			UserID: u.ID, Stage: stage, ScheduledAt: time.Now().UTC().Add(time.Hour),
			DedupeKey: FollowUpDedupeKey(u.ID, stage, 99), MessageID: 99,
		}); err != nil {
			t.Fatalf("schedule stage %d: %v", stage, err)
		}
	}

	n, err := jobs.CancelPendingForUser(ctx, u.ID, "client replied")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if n != 3 {
		t.Fatalf("every pending nudge must be cancelled, got %d", n)
	}
	if _, err := jobs.NextPendingForUser(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no pending nudge should remain, got err=%v", err)
	}
	// A cancelled job is never claimable again.
	got, err := jobs.Claim(ctx, "worker", time.Now().UTC().Add(2*time.Hour), 10, time.Hour)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("cancelled jobs must not be claimable, got %d", len(got))
	}
}

// A stale claim is recovered, so a worker crash cannot block a nudge forever.
func TestStaleClaimIsRecovered(t *testing.T) {
	ctx := context.Background()
	db, _, users := newCRMTestDB(t)
	jobs := NewFollowUpRepository(db)
	u := mustClient(t, users, "wa-stale", "77010000007", "Stale")

	if _, _, err := jobs.Schedule(ctx, domain.FollowUpJob{
		UserID: u.ID, Stage: 1, ScheduledAt: time.Now().UTC().Add(-time.Hour),
		DedupeKey: FollowUpDedupeKey(u.ID, 1, 1), MessageID: 1,
	}); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	first, err := jobs.Claim(ctx, "crashed-worker", time.Now().UTC(), 5, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("initial claim failed: %d jobs, err=%v", len(first), err)
	}
	// Nothing may reclaim it while the claim is fresh.
	again, _ := jobs.Claim(ctx, "other", time.Now().UTC(), 5, time.Minute)
	if len(again) != 0 {
		t.Fatal("a fresh claim must not be stolen")
	}
	// After the TTL passes, another worker may recover it.
	recovered, err := jobs.Claim(ctx, "healthy-worker", time.Now().UTC().Add(2*time.Minute), 5, time.Minute)
	if err != nil || len(recovered) != 1 {
		t.Fatalf("a stale claim must be recoverable: %d jobs, err=%v", len(recovered), err)
	}
}

// Server-side filtering must actually narrow the result set.
func TestClientListFiltersServerSide(t *testing.T) {
	ctx := context.Background()
	_, crm, users := newCRMTestDB(t)

	a := mustClient(t, users, "wa-f1", "77011111111", "Асхат")
	b := mustClient(t, users, "wa-f2", "77022222222", "Борис")
	if err := crm.SetStatus(ctx, a.ID, domain.CRMQualified, ""); err != nil {
		t.Fatalf("status: %v", err)
	}
	if err := crm.SetStatus(ctx, b.ID, domain.CRMLost, "не отвечает"); err != nil {
		t.Fatalf("status: %v", err)
	}

	got, total, err := crm.ListClients(ctx, ClientFilter{Statuses: []string{string(domain.CRMQualified)}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("status filter did not narrow the list: total=%d items=%d", total, len(got))
	}

	// Phone search must tolerate the formatting a consultant would type.
	got, _, err = crm.ListClients(ctx, ClientFilter{Search: "+7 702 222-22-22"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("phone search failed: %d results", len(got))
	}
}

// AI state is stored, and empty values never erase what is already known.
func TestApplyAIStateMergesRatherThanOverwrites(t *testing.T) {
	ctx := context.Background()
	_, crm, users := newCRMTestDB(t)
	u := mustClient(t, users, "wa-state", "77010000008", "State")

	if err := crm.ApplyAIState(ctx, u.ID, AIState{
		Status: domain.CRMQualified, QualificationStage: domain.StageQualified,
		Intent: "trademark_registration", Service: "trademark_registration",
		Confidence: 0.91, Summary: "Хочет зарегистрировать бренд",
		ImportantFacts: []string{"ТОО «Алма»", "срок до 1 мая"},
		Language:       domain.LangRU,
	}); err != nil {
		t.Fatalf("apply state: %v", err)
	}

	// A later, less certain turn returns empty strings; nothing may be lost.
	if err := crm.ApplyAIState(ctx, u.ID, AIState{Confidence: 0.2}); err != nil {
		t.Fatalf("apply empty state: %v", err)
	}

	client, _ := crm.GetClient(ctx, u.ID)
	if client.AISummary != "Хочет зарегистрировать бренд" {
		t.Fatalf("summary was erased: %q", client.AISummary)
	}
	if len(client.ImportantFacts) != 2 {
		t.Fatalf("important facts were erased: %v", client.ImportantFacts)
	}
	if client.DetectedService != "trademark_registration" || client.CRMStatus != domain.CRMQualified {
		t.Fatalf("qualification was lost: service=%q status=%q", client.DetectedService, client.CRMStatus)
	}
}

// A manually set language is never overwritten by automatic detection.
func TestManualLanguageOverridesDetection(t *testing.T) {
	ctx := context.Background()
	_, crm, users := newCRMTestDB(t)
	u := mustClient(t, users, "wa-lang", "77010000009", "Lang")

	kk := domain.LangKK
	if err := crm.UpdateClient(ctx, u.ID, ClientPatch{Language: &kk}); err != nil {
		t.Fatalf("set language: %v", err)
	}
	if err := crm.ApplyAIState(ctx, u.ID, AIState{Language: domain.LangRU}); err != nil {
		t.Fatalf("apply state: %v", err)
	}

	client, _ := crm.GetClient(ctx, u.ID)
	if client.Language != domain.LangKK {
		t.Fatalf("manual language must win over detection, got %q", client.Language)
	}
	if !client.LanguageLocked {
		t.Fatal("manual language must set the lock flag")
	}
}

// Unread counting drives the CRM badge and must clear on read.
func TestUnreadCounterTracksInboundAndClears(t *testing.T) {
	ctx := context.Background()
	_, crm, users := newCRMTestDB(t)
	u := mustClient(t, users, "wa-unread", "77010000010", "Unread")

	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		if err := crm.TouchInbound(ctx, u.ID, now); err != nil {
			t.Fatalf("touch inbound: %v", err)
		}
	}
	client, _ := crm.GetClient(ctx, u.ID)
	if client.UnreadCount != 3 {
		t.Fatalf("expected 3 unread, got %d", client.UnreadCount)
	}
	if client.LastInboundAt == nil {
		t.Fatal("inbound activity must be recorded")
	}

	if err := crm.MarkRead(ctx, u.ID); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	client, _ = crm.GetClient(ctx, u.ID)
	if client.UnreadCount != 0 {
		t.Fatalf("unread badge must clear, got %d", client.UnreadCount)
	}
}

// The dashboard aggregates without scanning per client.
func TestDashboardCounts(t *testing.T) {
	ctx := context.Background()
	_, crm, users := newCRMTestDB(t)

	a := mustClient(t, users, "wa-d1", "77010000011", "A")
	b := mustClient(t, users, "wa-d2", "77010000012", "B")
	if err := crm.SetStatus(ctx, a.ID, domain.CRMNeedsConsultant, ""); err != nil {
		t.Fatalf("status: %v", err)
	}
	if err := crm.SetBlocked(ctx, b.ID, true, 1, "спам"); err != nil {
		t.Fatalf("block: %v", err)
	}

	board, err := crm.Dashboard(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	if board.ByStatus[string(domain.CRMNeedsConsultant)] != 1 {
		t.Fatalf("needs-consultant count is wrong: %v", board.ByStatus)
	}
	if board.Blocked != 1 {
		t.Fatalf("blocked count is wrong: %d", board.Blocked)
	}
	if board.NewToday != 2 {
		t.Fatalf("new-today count is wrong: %d", board.NewToday)
	}
}

// Message paging must be bounded and chronological.
func TestMessagePagingIsBoundedAndOrdered(t *testing.T) {
	ctx := context.Background()
	db, _, users := newCRMTestDB(t)
	msgs := NewMessageRepository(db)
	u := mustClient(t, users, "wa-page", "77010000013", "Page")

	for i := 0; i < 10; i++ {
		if _, err := msgs.Create(ctx, &domain.Message{
			UserID: u.ID, MessageType: domain.MessageText,
			Text: string(rune('a' + i)), Direction: domain.DirectionIncoming,
		}); err != nil {
			t.Fatalf("store message %d: %v", i, err)
		}
	}

	page, hasMore, err := msgs.PageByUser(ctx, u.ID, 0, 4)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if len(page) != 4 || !hasMore {
		t.Fatalf("expected a bounded page with more available, got %d hasMore=%v", len(page), hasMore)
	}
	// A page is returned oldest-first so the chat renders top to bottom.
	if page[0].ID > page[len(page)-1].ID {
		t.Fatal("page must be in chronological order")
	}
	// The newest page must end at the newest message.
	if page[len(page)-1].Text != "j" {
		t.Fatalf("the first page must be the newest messages, got %q", page[len(page)-1].Text)
	}
}

// Notes are private CRM data with an author and a soft delete.
func TestInternalNotesLifecycle(t *testing.T) {
	ctx := context.Background()
	db, _, users := newCRMTestDB(t)
	notes := NewNoteRepository(db)
	u := mustClient(t, users, "wa-note", "77010000014", "Note")

	note, err := notes.Create(ctx, u.ID, 5, "Клиент просил перезвонить после 18:00")
	if err != nil {
		t.Fatalf("create note: %v", err)
	}
	list, err := notes.ListByClient(ctx, u.ID, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list notes: %d, err=%v", len(list), err)
	}
	if list[0].AdminID != 5 {
		t.Fatalf("note author was not recorded: %d", list[0].AdminID)
	}

	if err := notes.Delete(ctx, note.ID); err != nil {
		t.Fatalf("delete note: %v", err)
	}
	list, _ = notes.ListByClient(ctx, u.ID, 10)
	if len(list) != 0 {
		t.Fatalf("deleted note must not be listed, got %d", len(list))
	}
	if _, err := notes.Get(ctx, note.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted note must not be readable, err=%v", err)
	}
}

// The analytics series must survive a timestamp the driver wrote in a layout
// SQLite's date() cannot parse. This is the regression for a scan that failed
// with "converting NULL to string is unsupported" and took the whole analytics
// screen down.
func TestDailyNewClientsToleratesUnparseableTimestamps(t *testing.T) {
	ctx := context.Background()
	db, crm, users := newCRMTestDB(t)

	good := mustClient(t, users, "wa-day-good", "77010000020", "Good")

	// Layouts the driver has produced across versions, plus one value SQLite's
	// date() rejects outright.
	layouts := []string{
		"2026-09-08 10:00:00.999719903+00:00", // modernc: space separator, 9 fractional digits
		"2026-09-08T11:00:00.196816+00:00",    // T separator, 6 fractional digits
		"2026-09-09 12:00:00+00:00",           // no fractional part
		"not-a-timestamp",                     // date() returns NULL for this
	}
	for i, raw := range layouts {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO users (whatsapp_user_id, phone_number, display_name, language,
				current_state, detected_service, lead_score, is_lead,
				first_seen_at, last_seen_at, created_at, updated_at)
			VALUES (?, '', '', '', 'new', '', 0, 0, ?, ?, ?, ?)`,
			fmt.Sprintf("wa-day-%d", i), raw, raw, raw, raw); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	series, err := crm.DailyNewClients(ctx, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("daily new clients must not fail on an odd timestamp: %v", err)
	}

	byDay := map[string]int{}
	for _, point := range series {
		if len(point.Day) != 10 {
			t.Fatalf("a series point must be a calendar day, got %q", point.Day)
		}
		byDay[point.Day] = point.Count
	}
	if byDay["2026-09-08"] != 2 {
		t.Fatalf("both 8 September rows must be counted regardless of layout: %v", byDay)
	}
	if byDay["2026-09-09"] != 1 {
		t.Fatalf("the 9 September row is missing: %v", byDay)
	}
	// The unparseable row is skipped, never surfaced as an empty bucket.
	if _, ok := byDay[""]; ok {
		t.Fatalf("a malformed timestamp must not become an empty day: %v", byDay)
	}
	_ = good
}

// The bot switch is a durable setting. It defaults to on so an existing
// deployment keeps answering after the migration, and it survives a restart.
func TestWhatsAppBotSwitchIsPersistedAndDefaultsToOn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "settings.db")

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	settings := NewSettingsRepository(db)

	enabled, err := settings.WhatsAppBotEnabled(ctx)
	if err != nil {
		t.Fatalf("read default: %v", err)
	}
	if !enabled {
		t.Fatal("an unset switch must read as enabled")
	}

	if err := settings.SetWhatsAppBotEnabled(ctx, false, 7); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if enabled, err = settings.WhatsAppBotEnabled(ctx); err != nil || enabled {
		t.Fatalf("the switch must read back as disabled: %v %v", enabled, err)
	}
	all, err := settings.All(ctx)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if all[SettingWhatsAppBotEnabled] != "false" {
		t.Fatalf("the raw setting must be stored, got %q", all[SettingWhatsAppBotEnabled])
	}
	db.Close()

	// Reopening the same database is what a restart looks like.
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer reopened.Close()
	if enabled, err = NewSettingsRepository(reopened).WhatsAppBotEnabled(ctx); err != nil || enabled {
		t.Fatalf("the switch must survive a restart: %v %v", enabled, err)
	}

	if err := NewSettingsRepository(reopened).SetWhatsAppBotEnabled(ctx, true, 7); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if enabled, err = NewSettingsRepository(reopened).WhatsAppBotEnabled(ctx); err != nil || !enabled {
		t.Fatalf("the switch must be re-enablable: %v %v", enabled, err)
	}
}
