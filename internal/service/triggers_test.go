package service

import (
	"testing"

	"lawyer-bot/internal/domain"
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"Какие у вас услуги?": "какие у вас услуги",
		"  ПРИВЕТ!!!  ":       "привет",
		"Ёлка":                "елка",
		"Сәлеметсіз бе, көмек керек": "сәлеметсіз бе көмек керек",
		"I need a privacy-policy":    "i need a privacy policy",
		"":                           "",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTriggerMatching(t *testing.T) {
	ts := NewTriggerSet()

	cases := []struct {
		name string
		text string
		want string // expected trigger category
	}{
		// Russian phrases from the specification.
		{"ru service inquiry", "Чем можете помочь?", TriggerServiceInquiry},
		{"ru services", "Какие услуги вы оказываете?", TriggerServiceInquiry},
		{"ru consultation", "Мне нужна консультация", TriggerConsultation},
		{"ru lawyer help", "Нужна помощь юриста", TriggerLegalAssistance},
		{"ru legal services", "Какие юридические услуги есть?", TriggerLegalAssistance},
		{"ru trademark", "Мне нужно зарегистрировать товарный знак", TriggerTrademark},
		{"ru privacy policy", "Мне нужна политика конфиденциальности", TriggerPrivacyPolicy},
		{"ru contract", "Нужен договор для бизнеса", TriggerContracts},
		{"ru pricing", "Сколько стоит регистрация?", TriggerPricing},

		// Kazakh phrases from the specification.
		{"kk how help", "Сіздер қалай көмектесе аласыздар?", TriggerServiceInquiry},
		{"kk services", "Қандай қызметтер көрсетесіздер?", TriggerServiceInquiry},
		{"kk legal services", "Қандай заңгерлік қызметтер бар?", TriggerLegalAssistance},
		{"kk advice", "Маған кеңес керек", TriggerConsultation},
		{"kk lawyer help", "Заңгердің көмегі керек", TriggerLegalAssistance},
		{"kk trademark", "Маған тауар белгісін тіркеу керек", TriggerTrademark},

		// English.
		{"en services", "What services do you offer?", TriggerServiceInquiry},
		{"en privacy", "I need a privacy policy for my app", TriggerPrivacyPolicy},
		{"en lawyer", "I need a lawyer", TriggerLegalAssistance},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := ts.Match(tc.text)
			if !m.Matched {
				t.Fatalf("%q should match a trigger, got none", tc.text)
			}
			if !m.Has(tc.want) {
				t.Fatalf("%q should match %q, got %v", tc.text, tc.want, m.Codes)
			}
		})
	}
}

// The specification requires semantic variants to be recognised as relevant.
// The deterministic layer catches the common ones; the rest is the model's job.
func TestTriggerMatchesSemanticVariants(t *testing.T) {
	ts := NewTriggerSet()
	variants := []string{
		"Маған заңгер керек",
		"мне нужен юрист",
		"хочу зарегистрировать тоо",
		"нужна помощь с договором",
	}
	for _, v := range variants {
		if !ts.Match(v).Matched {
			t.Errorf("variant %q should be recognised by the deterministic filter", v)
		}
	}
}

func TestNonLegalMessagesDoNotMatch(t *testing.T) {
	ts := NewTriggerSet()
	irrelevant := []string{
		"Здравствуйте",
		"Привет",
		"Как дела?",
		"Какая сегодня погода?",
		"Сәлеметсіз бе",
		"Рахмет",
		"Hello",
		"How are you?",
		"Спасибо большое",
	}
	for _, text := range irrelevant {
		if m := ts.Match(text); m.Matched {
			t.Errorf("%q must not match any legal trigger, got %v", text, m.Codes)
		}
	}
}

func TestOffTopicAndSmallTalkDetection(t *testing.T) {
	ts := NewTriggerSet()

	offTopic := []string{"Как дела?", "какая погода сегодня", "how are you", "расскажи анекдот"}
	for _, text := range offTopic {
		if !ts.IsOffTopic(text) {
			t.Errorf("%q should be detected as off topic", text)
		}
	}

	smallTalk := []string{"Здравствуйте", "привет", "Спасибо!", "Сәлеметсіз бе", "hello", "Ок, спасибо"}
	for _, text := range smallTalk {
		if !ts.IsSmallTalkOnly(text) {
			t.Errorf("%q should be detected as small talk only", text)
		}
	}

	// A greeting combined with a real request is not small talk.
	mixed := []string{"Здравствуйте, нужен юрист", "Привет, какие у вас услуги?"}
	for _, text := range mixed {
		if ts.IsSmallTalkOnly(text) {
			t.Errorf("%q carries a real request and must not be treated as small talk", text)
		}
	}
}

// The gate is the token budget guard: this is the test that proves small talk
// never reaches OpenAI.
func TestGateKeepsSmallTalkAwayFromTheModel(t *testing.T) {
	gate := NewGate(NewTriggerSet(), GateConfig{MaxCallsPerDay: 40, AnalyzeUnmatched: true, MinWordsUnmatched: 3})

	cases := []struct {
		name   string
		text   string
		state  domain.ConversationState
		callAI bool
		reason string
	}{
		{"greeting", "Здравствуйте", domain.StateNew, false, GateReasonSmallTalkOnly},
		{"how are you", "Как дела?", domain.StateNew, false, GateReasonOffTopic},
		{"weather", "Какая погода в Алматы?", domain.StateNew, false, GateReasonOffTopic},
		{"weather in flow", "Какая погода?", domain.StateQualifying, false, GateReasonOffTopic},
		{"thanks", "Спасибо", domain.StateQualifying, false, GateReasonSmallTalkOnly},
		{"kazakh greeting", "Сәлеметсіз бе", domain.StateNew, false, GateReasonSmallTalkOnly},
		{"short unmatched", "ага понятно", domain.StateNew, false, GateReasonSmallTalkOnly},

		{"service inquiry", "Какие у вас услуги?", domain.StateNew, true, GateReasonTriggerMatched},
		{"greeting plus request", "Здравствуйте, нужен юрист", domain.StateNew, true, GateReasonTriggerMatched},
		{"trademark", "Мне нужно зарегистрировать товарный знак", domain.StateNew, true, GateReasonTriggerMatched},
		{"flow answer", "Уже работает", domain.StateQualifying, true, GateReasonActiveFlow},
		{"flow answer kk", "Әзірленуде", domain.StateServiceIdentified, true, GateReasonActiveFlow},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gate.Evaluate(GateInput{
				Text:        tc.text,
				MessageType: domain.MessageText,
				State:       tc.state,
			})
			if got.CallAI != tc.callAI {
				t.Fatalf("%q in state %q: CallAI = %v (reason %q), want %v",
					tc.text, tc.state, got.CallAI, got.Reason, tc.callAI)
			}
			if tc.reason != "" && got.Reason != tc.reason {
				t.Fatalf("%q: reason = %q, want %q", tc.text, got.Reason, tc.reason)
			}
		})
	}
}

func TestGateRespectsTokenBudget(t *testing.T) {
	gate := NewGate(NewTriggerSet(), GateConfig{MaxCallsPerDay: 5, AnalyzeUnmatched: true})

	got := gate.Evaluate(GateInput{
		Text:         "Мне нужна регистрация товарного знака",
		MessageType:  domain.MessageText,
		State:        domain.StateNew,
		AICallsToday: 5,
	})
	if got.CallAI {
		t.Fatal("gate must stop calling the model once the per-user daily budget is spent")
	}
	if got.Reason != GateReasonBudgetExceeded {
		t.Fatalf("reason = %q, want %q", got.Reason, GateReasonBudgetExceeded)
	}
}

func TestGateSkipsMediaWithoutCaption(t *testing.T) {
	gate := NewGate(NewTriggerSet(), GateConfig{AnalyzeUnmatched: true})

	got := gate.Evaluate(GateInput{Text: "", MessageType: domain.MessageImage, State: domain.StateNew})
	if got.CallAI {
		t.Fatal("media without a caption must never be sent to the model")
	}
	if got.Reason != GateReasonMediaOnly {
		t.Fatalf("reason = %q, want %q", got.Reason, GateReasonMediaOnly)
	}
}

func TestGateStrictModeSkipsUnmatched(t *testing.T) {
	strict := NewGate(NewTriggerSet(), GateConfig{AnalyzeUnmatched: false})

	got := strict.Evaluate(GateInput{
		Text:        "Хочу обсудить один вопрос по моему проекту",
		MessageType: domain.MessageText,
		State:       domain.StateNew,
	})
	if got.CallAI {
		t.Fatal("strict mode must not spend tokens on unmatched messages")
	}
	if got.Reason != GateReasonUnmatchedPolicy {
		t.Fatalf("reason = %q, want %q", got.Reason, GateReasonUnmatchedPolicy)
	}
}
