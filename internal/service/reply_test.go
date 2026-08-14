package service

import (
	"strings"
	"testing"

	"lawyer-bot/internal/domain"
)

func TestComposerAnswersInTheCustomersLanguage(t *testing.T) {
	c := NewComposer(NewCatalog())
	decision := Decision{Respond: true, Action: ActionServiceMenu}

	ru := c.Compose(decision, domain.LangRU, "")
	if !strings.Contains(ru, "Товарный знак") {
		t.Fatalf("Russian menu missing expected item:\n%s", ru)
	}

	kk := c.Compose(decision, domain.LangKK, "")
	if !strings.Contains(kk, "Тауар белгісі") {
		t.Fatalf("Kazakh menu missing expected item:\n%s", kk)
	}

	en := c.Compose(decision, domain.LangEN, "")
	if !strings.Contains(en, "Trademark") {
		t.Fatalf("English menu missing expected item:\n%s", en)
	}

	// An unknown language falls back to Russian rather than failing.
	if c.Compose(decision, domain.LangUnknown, "") != ru {
		t.Fatal("unknown language should fall back to the Russian template")
	}
}

func TestServiceInfoStatesCostDependsOnComplexity(t *testing.T) {
	c := NewComposer(NewCatalog())
	decision := Decision{Respond: true, Action: ActionServiceInfo, Service: domain.ServicePrivacyPolicy}

	ru := c.Compose(decision, domain.LangRU, "")
	if !strings.Contains(ru, "Стоимость зависит от сложности") {
		t.Fatalf("Russian reply must explain that cost depends on complexity:\n%s", ru)
	}
	if !strings.Contains(ru, "Диане") {
		t.Fatalf("Russian reply must offer the handoff to Diana:\n%s", ru)
	}

	kk := c.Compose(decision, domain.LangKK, "")
	if !strings.Contains(kk, "күрделілігіне") {
		t.Fatalf("Kazakh reply must explain that cost depends on complexity:\n%s", kk)
	}

	en := c.Compose(decision, domain.LangEN, "")
	if !strings.Contains(en, "depends on the complexity") {
		t.Fatalf("English reply must explain that cost depends on complexity:\n%s", en)
	}
}

// No template may contain a number that reads as a price.
func TestNoTemplateContainsAPrice(t *testing.T) {
	c := NewComposer(NewCatalog())
	actions := []ReplyAction{ActionServiceMenu, ActionClarify, ActionServiceInfo, ActionAskContact, ActionHandoff}
	langs := []domain.Language{domain.LangRU, domain.LangKK, domain.LangEN}

	for _, action := range actions {
		for _, lang := range langs {
			text := c.Compose(Decision{Respond: true, Action: action, Service: domain.ServicePrivacyPolicy}, lang, "")
			if containsPrice(text) {
				t.Errorf("action %q in %q contains price-like content:\n%s", action, lang, text)
			}
		}
	}
}

func TestSanitizeQuestionRejectsInventedPrices(t *testing.T) {
	rejected := []string{
		"",
		"Это не вопрос.",
		"Стоимость составляет 50000 тг, подойдёт?",
		"Цена 30000 тенге, оформляем?",
		"The price is 200 USD, shall we proceed?",
		strings.Repeat("очень длинный вопрос ", 20) + "?",
	}
	for _, q := range rejected {
		if _, ok := SanitizeQuestion(q); ok {
			t.Errorf("question %q should have been rejected", q)
		}
	}

	accepted := []string{
		"Политика нужна для сайта или мобильного приложения?",
		"Приложение уже запущено или в разработке?",
		"Is the app already live or still in development?",
		"Белгіні Қазақстанда тіркеу керек пе?",
	}
	for _, q := range accepted {
		got, ok := SanitizeQuestion(q)
		if !ok {
			t.Errorf("question %q should have been accepted", q)
			continue
		}
		if got != q {
			t.Errorf("accepted question was altered: %q -> %q", q, got)
		}
	}
}

func TestClarifyPrefersModelQuestionThenFallsBackToCatalog(t *testing.T) {
	c := NewComposer(NewCatalog())
	decision := Decision{Respond: true, Action: ActionClarify, Service: domain.ServiceMobileAppUserAgreement}

	// A valid model question is used verbatim.
	modelQ := "Приложение уже запущено или находится в разработке?"
	got := c.Compose(decision, domain.LangRU, modelQ)
	if !strings.Contains(got, modelQ) {
		t.Fatalf("a valid model question should be used:\n%s", got)
	}

	// A question carrying a price is discarded in favour of the catalog's own.
	got = c.Compose(decision, domain.LangRU, "Готовы заплатить 100000 тг?")
	if strings.Contains(got, "100000") {
		t.Fatalf("a price from the model must never reach the customer:\n%s", got)
	}
	if !strings.Contains(got, "приложение уже запущено") && !strings.Contains(got, "Подскажите") {
		t.Fatalf("expected the catalog clarification as fallback:\n%s", got)
	}
}

func TestNotificationContainsEverythingDianaNeeds(t *testing.T) {
	catalog := NewCatalog()
	user := &domain.User{DisplayName: "Аида", PhoneNumber: "77015551234"}
	lead := &domain.Lead{
		ServiceCode:          domain.ServicePrivacyPolicy,
		ServiceName:          catalog.Name(domain.ServicePrivacyPolicy, domain.LangRU),
		Language:             domain.LangRU,
		PhoneNumber:          "77015551234",
		LeadScore:            0.86,
		Source:               domain.SourceWhatsApp,
		QualificationSummary: "Нужна политика конфиденциальности для мобильного приложения",
	}

	got := NotificationText(catalog, user, lead)
	for _, want := range []string{
		"Аида",
		"+77015551234",
		"RU",
		"Политика конфиденциальности",
		"мобильного приложения",
		"0.86",
		domain.SourceWhatsApp,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("notification is missing %q:\n%s", want, got)
		}
	}
}

func TestUnknownActionProducesSilence(t *testing.T) {
	c := NewComposer(NewCatalog())
	if got := c.Compose(Decision{Respond: false, Action: ActionNone}, domain.LangRU, ""); got != "" {
		t.Fatalf("ActionNone must produce no text, got %q", got)
	}
}
