package service

import (
	"strings"
	"testing"

	"lawyer-bot/internal/domain"
)

func newQualifier() *Qualifier {
	return NewQualifier(NewCatalog(), testMinConfidence)
}

func TestLeadIsNotTrackedForIrrelevantConversations(t *testing.T) {
	got := newQualifier().Qualify(QualificationInput{
		AICalled: true,
		AI: domain.AIClassification{
			IsRelevant: false,
			Intent:     domain.IntentIrrelevant,
			Confidence: 0.9,
		},
		Language: domain.LangRU,
	})
	if got.Track {
		t.Fatal("small talk must not create a lead")
	}
	if got.Qualified {
		t.Fatal("small talk must never qualify")
	}
}

func TestLeadIsTrackedWhileStillQualifying(t *testing.T) {
	got := newQualifier().Qualify(QualificationInput{
		Decision: Decision{Action: ActionClarify, NextState: domain.StateQualifying},
		AICalled: true,
		AI: domain.AIClassification{
			IsRelevant: true,
			Intent:     domain.IntentPrivacyPolicy,
			Confidence: 0.9,
			LeadScore:  0.7,
		},
		Service:       domain.ServicePrivacyPolicy,
		Language:      domain.LangRU,
		HasPhone:      true,
		IncomingCount: 1,
	})
	if !got.Track {
		t.Fatal("an in-progress legal conversation should already be recorded")
	}
	if got.Qualified {
		t.Fatal("a conversation still being clarified is not yet qualified")
	}
	if got.Status != domain.LeadQualifying {
		t.Fatalf("status = %q, want %q", got.Status, domain.LeadQualifying)
	}
}

func TestLeadQualifiesWhenEverythingIsKnown(t *testing.T) {
	got := newQualifier().Qualify(QualificationInput{
		Decision: Decision{Action: ActionServiceInfo, NextState: domain.StateReadyForDiana},
		AICalled: true,
		AI: domain.AIClassification{
			IsRelevant: true,
			Intent:     domain.IntentPrivacyPolicy,
			Confidence: 0.95,
			LeadScore:  0.9,
			Summary:    "Нужна политика конфиденциальности для приложения",
		},
		Service:       domain.ServicePrivacyPolicy,
		Language:      domain.LangRU,
		HasPhone:      true,
		Facts:         map[string]string{domain.FactPlatform: "mobile_app"},
		IncomingCount: 2,
	})
	if !got.Qualified {
		t.Fatal("service + interest + clarification + contact should qualify the lead")
	}
	if got.Status != domain.LeadQualified {
		t.Fatalf("status = %q, want %q", got.Status, domain.LeadQualified)
	}
	if got.Score < 0.7 {
		t.Fatalf("score = %v, want a high score for a complete lead", got.Score)
	}
	if !strings.Contains(got.Summary, "Политика конфиденциальности") {
		t.Fatalf("summary should name the service:\n%s", got.Summary)
	}
	if !strings.Contains(got.Summary, "мобильное приложение") {
		t.Fatalf("summary should include the known platform:\n%s", got.Summary)
	}
}

func TestLeadDoesNotQualifyWithoutContact(t *testing.T) {
	got := newQualifier().Qualify(QualificationInput{
		Decision: Decision{Action: ActionAskContact, NextState: domain.StateWaitingContact},
		AICalled: true,
		AI: domain.AIClassification{
			IsRelevant: true,
			Intent:     domain.IntentTrademark,
			Confidence: 0.95,
			LeadScore:  0.9,
		},
		Service:  domain.ServiceTrademarkRegistration,
		Language: domain.LangRU,
		HasPhone: false,
	})
	if got.Qualified {
		t.Fatal("a lead without contact information is not ready for Diana")
	}
	if !got.Track {
		t.Fatal("the opportunity should still be recorded")
	}
}

func TestLeadDoesNotQualifyBelowConfidenceThreshold(t *testing.T) {
	got := newQualifier().Qualify(QualificationInput{
		Decision: Decision{Action: ActionServiceInfo, NextState: domain.StateReadyForDiana},
		AICalled: true,
		AI: domain.AIClassification{
			IsRelevant: true,
			Intent:     domain.IntentTrademark,
			Confidence: 0.50,
			LeadScore:  0.9,
		},
		Service:  domain.ServiceTrademarkRegistration,
		Language: domain.LangRU,
		HasPhone: true,
	})
	if got.Qualified {
		t.Fatal("a low-confidence classification must not produce a qualified lead")
	}
}

// A confident model alone cannot manufacture a hot lead.
func TestModelScoreAloneCannotProduceAHotLead(t *testing.T) {
	got := newQualifier().Qualify(QualificationInput{
		Decision: Decision{Action: ActionClarify, NextState: domain.StateQualifying},
		AICalled: true,
		AI: domain.AIClassification{
			IsRelevant: true,
			Intent:     domain.IntentOtherLegal,
			Confidence: 0.40,
			LeadScore:  1.0, // the model is very excited
		},
		Language:      domain.LangRU,
		HasPhone:      false,
		IncomingCount: 1,
	})
	if got.Score > 0.65 {
		t.Fatalf("score = %v: deterministic signals must temper the model's own score", got.Score)
	}
}

func TestSummaryNeverCarriesAPrice(t *testing.T) {
	got := newQualifier().Qualify(QualificationInput{
		Decision: Decision{Action: ActionServiceInfo, NextState: domain.StateReadyForDiana},
		AICalled: true,
		AI: domain.AIClassification{
			IsRelevant: true,
			Intent:     domain.IntentTrademark,
			Confidence: 0.95,
			LeadScore:  0.9,
			Summary:    "Клиенту назвали цену 150000 тг за регистрацию",
		},
		Service:      domain.ServiceTrademarkRegistration,
		Language:     domain.LangRU,
		HasPhone:     true,
		LastUserText: "Нужна регистрация товарного знака",
	})
	if strings.Contains(got.Summary, "150000") {
		t.Fatalf("a price from the model must not reach the lead summary:\n%s", got.Summary)
	}
	if !strings.Contains(got.Summary, "Нужна регистрация") {
		t.Fatalf("the customer's own words should be used instead:\n%s", got.Summary)
	}
}

func TestCatalogCoversEveryServiceInThreeLanguages(t *testing.T) {
	catalog := NewCatalog()

	required := []string{
		domain.ServiceTrademarkRegistration, domain.ServiceBusinessRegistration,
		domain.ServiceLegalConsultation, domain.ServiceContractDrafting,
		domain.ServiceContractReview, domain.ServicePublicOffer,
		domain.ServicePrivacyPolicy, domain.ServiceWebsiteUserAgreement,
		domain.ServiceMobileAppUserAgreement, domain.ServiceEcommerceDocuments,
		domain.ServiceOnlinePlatformDocs, domain.ServicePaymentRefundPolicy,
		domain.ServiceDeliveryPolicy, domain.ServiceOtherLegalService,
	}

	for _, code := range required {
		s, ok := catalog.Get(code)
		if !ok {
			t.Errorf("catalog is missing service %q", code)
			continue
		}
		for _, lang := range []domain.Language{domain.LangRU, domain.LangKK, domain.LangEN} {
			if s.Name(lang) == "" {
				t.Errorf("service %q has no name in %q", code, lang)
			}
			if s.Description(lang) == "" {
				t.Errorf("service %q has no description in %q", code, lang)
			}
		}
		// No service ships with a fixed price: cost depends on complexity.
		if s.HasFixedPrice {
			t.Errorf("service %q ships with a fixed price, which the business rules forbid by default", code)
		}
	}
}

func TestCatalogIsExtensible(t *testing.T) {
	catalog := NewCatalog()
	before := len(catalog.All())

	catalog.Register(domain.LegalService{
		Code:   "custom_service",
		NameRU: "Новая услуга", NameKZ: "Жаңа қызмет", NameEN: "New service",
	})

	if len(catalog.All()) != before+1 {
		t.Fatal("registering a new service should extend the catalog")
	}
	if !catalog.Has("custom_service") {
		t.Fatal("the new service should be retrievable")
	}
	if got := catalog.Name("custom_service", domain.LangKK); got != "Жаңа қызмет" {
		t.Fatalf("name = %q, want the Kazakh name", got)
	}
}
