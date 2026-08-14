package repository

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"lawyer-bot/internal/domain"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestUserUpsertCreatesThenUpdates(t *testing.T) {
	ctx := context.Background()
	repo := NewUserRepository(newTestDB(t))

	u, err := repo.Upsert(ctx, "wa-1", "77015551234", "Aidos")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if u.ID == 0 || u.WhatsAppUserID != "wa-1" {
		t.Fatalf("unexpected user: %+v", u)
	}
	if u.CurrentState != domain.StateNew {
		t.Fatalf("new user should start in state %q, got %q", domain.StateNew, u.CurrentState)
	}
	if u.FirstSeenAt.IsZero() || u.CreatedAt.IsZero() {
		t.Fatalf("timestamps did not round-trip: %+v", u)
	}

	// A later message with no display name must not erase the known one.
	again, err := repo.Upsert(ctx, "wa-1", "", "")
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if again.ID != u.ID {
		t.Fatalf("upsert created a duplicate user: %d vs %d", again.ID, u.ID)
	}
	if again.DisplayName != "Aidos" || again.PhoneNumber != "77015551234" {
		t.Fatalf("upsert erased known contact data: %+v", again)
	}
}

func TestUserNotFound(t *testing.T) {
	repo := NewUserRepository(newTestDB(t))
	_, err := repo.GetByWhatsAppID(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestMessageDeduplication(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	users := NewUserRepository(db)
	msgs := NewMessageRepository(db)

	u, err := users.Upsert(ctx, "wa-2", "770", "")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	m := &domain.Message{
		UserID:            u.ID,
		WhatsAppMessageID: "wamid.1",
		MessageType:       domain.MessageText,
		Text:              "Какие у вас услуги?",
		Direction:         domain.DirectionIncoming,
	}
	if _, err := msgs.Create(ctx, m); err != nil {
		t.Fatalf("create message: %v", err)
	}

	dup, err := msgs.ExistsByWhatsAppID(ctx, "wamid.1")
	if err != nil {
		t.Fatalf("exists: %v", err)
	}
	if !dup {
		t.Fatal("stored message should be reported as duplicate on retry")
	}

	fresh, err := msgs.ExistsByWhatsAppID(ctx, "wamid.2")
	if err != nil {
		t.Fatalf("exists: %v", err)
	}
	if fresh {
		t.Fatal("unknown message id must not be reported as duplicate")
	}
}

func TestRecentByUserReturnsChronologicalOrder(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	users := NewUserRepository(db)
	msgs := NewMessageRepository(db)

	u, _ := users.Upsert(ctx, "wa-3", "770", "")
	base := time.Now().UTC().Add(-time.Hour)
	for i, text := range []string{"first", "second", "third"} {
		if _, err := msgs.Create(ctx, &domain.Message{
			UserID:      u.ID,
			MessageType: domain.MessageText,
			Text:        text,
			Direction:   domain.DirectionIncoming,
			CreatedAt:   base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	got, err := msgs.RecentByUser(ctx, u.ID, 2)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 messages, got %d", len(got))
	}
	if got[0].Text != "second" || got[1].Text != "third" {
		t.Fatalf("want the newest two in chronological order, got %q, %q", got[0].Text, got[1].Text)
	}
}

func TestLeadUpsertKeepsSingleOpenLead(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	users := NewUserRepository(db)
	leads := NewLeadRepository(db)

	u, _ := users.Upsert(ctx, "wa-4", "77015551234", "Diana's client")

	first, err := leads.Upsert(ctx, &domain.Lead{
		UserID:      u.ID,
		ServiceCode: domain.ServicePrivacyPolicy,
		ServiceName: "Политика конфиденциальности",
		Language:    domain.LangRU,
		PhoneNumber: "77015551234",
		LeadScore:   0.6,
		Status:      domain.LeadQualifying,
		Source:      domain.SourceWhatsApp,
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	second, err := leads.Upsert(ctx, &domain.Lead{
		UserID:               u.ID,
		LeadScore:            0.9,
		Status:               domain.LeadQualified,
		QualificationSummary: "Нужна политика для мобильного приложения",
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	if second.ID != first.ID {
		t.Fatalf("second qualification created a duplicate lead: %d vs %d", second.ID, first.ID)
	}
	if second.ServiceCode != domain.ServicePrivacyPolicy {
		t.Fatalf("empty service code overwrote a known one: %q", second.ServiceCode)
	}
	if second.LeadScore != 0.9 {
		t.Fatalf("lead score should rise to 0.9, got %v", second.LeadScore)
	}
	if second.Status != domain.LeadQualified {
		t.Fatalf("status should advance to qualified, got %q", second.Status)
	}
}

func TestTraceChainIsRecorded(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	users := NewUserRepository(db)
	trace := NewTraceRepository(db)

	u, _ := users.Upsert(ctx, "wa-5", "770", "")

	stages := []string{domain.StageMessageStored, domain.StageGate, domain.StageDecision}
	for _, s := range stages {
		if err := trace.Event(ctx, domain.TraceEvent{
			TraceID:  "trace-1",
			UserID:   u.ID,
			Stage:    s,
			Decision: domain.DecisionOK,
			Detail:   Detail(map[string]any{"stage": s}),
		}); err != nil {
			t.Fatalf("trace event: %v", err)
		}
	}

	got, err := trace.ByTraceID(ctx, "trace-1")
	if err != nil {
		t.Fatalf("by trace id: %v", err)
	}
	if len(got) != len(stages) {
		t.Fatalf("want %d events, got %d", len(stages), len(got))
	}
	for i, s := range stages {
		if got[i].Stage != s {
			t.Fatalf("event %d: want stage %q, got %q", i, s, got[i].Stage)
		}
	}
}

func TestFactsRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	users := NewUserRepository(db)
	trace := NewTraceRepository(db)

	u, _ := users.Upsert(ctx, "wa-6", "770", "")

	if err := trace.SetFact(ctx, domain.UserFact{UserID: u.ID, Key: domain.FactPlatform, Value: "mobile_app", Source: "ai"}); err != nil {
		t.Fatalf("set fact: %v", err)
	}
	// Re-setting the same key must update rather than duplicate.
	if err := trace.SetFact(ctx, domain.UserFact{UserID: u.ID, Key: domain.FactPlatform, Value: "website", Source: "ai"}); err != nil {
		t.Fatalf("update fact: %v", err)
	}

	facts, err := trace.Facts(ctx, u.ID)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if len(facts) != 1 || facts[domain.FactPlatform] != "website" {
		t.Fatalf("unexpected facts: %+v", facts)
	}
}

func TestAIUsageAggregation(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	users := NewUserRepository(db)
	ai := NewAIInteractionRepository(db)

	u, _ := users.Upsert(ctx, "wa-7", "770", "")

	for i := 0; i < 3; i++ {
		if _, err := ai.Create(ctx, &domain.AIInteraction{
			UserID:       u.ID,
			Model:        "test-model",
			InputTokens:  100,
			OutputTokens: 20,
			Intent:       string(domain.IntentServiceInquiry),
		}); err != nil {
			t.Fatalf("create interaction: %v", err)
		}
	}

	n, err := ai.CountByUserSince(ctx, u.ID, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("want 3 calls, got %d", n)
	}

	usage, err := ai.UsageSince(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if usage.Calls != 3 || usage.InputTokens != 300 || usage.OutputTokens != 60 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
}
