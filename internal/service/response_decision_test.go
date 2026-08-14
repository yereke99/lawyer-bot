package service

import (
	"testing"

	"lawyer-bot/internal/domain"
)

const testMinConfidence = 0.75

func TestNoModelCallMeansSilence(t *testing.T) {
	got := ShouldRespond(DecisionInput{
		AICalled:      false,
		State:         domain.StateNew,
		MinConfidence: testMinConfidence,
	})
	if got.Respond {
		t.Fatal("a message the gate skipped must never receive a reply")
	}
	if got.Reason != ReasonNoAIAndNoTrigger {
		t.Fatalf("reason = %q, want %q", got.Reason, ReasonNoAIAndNoTrigger)
	}
}

func TestGreetingFromNewUserIsIgnored(t *testing.T) {
	got := ShouldRespond(DecisionInput{
		AICalled: true,
		AI: domain.AIClassification{
			IsRelevant: true,
			Intent:     domain.IntentGreeting,
			Language:   domain.LangRU,
			Confidence: 0.99,
		},
		State:         domain.StateNew,
		MinConfidence: testMinConfidence,
	})
	if got.Respond {
		t.Fatalf("a bare greeting must not be answered, got action %q", got.Action)
	}
	if got.Reason != ReasonGreetingOnly {
		t.Fatalf("reason = %q, want %q", got.Reason, ReasonGreetingOnly)
	}
}

func TestIrrelevantMessageIsIgnored(t *testing.T) {
	got := ShouldRespond(DecisionInput{
		AICalled: true,
		AI: domain.AIClassification{
			IsRelevant: false,
			Intent:     domain.IntentIrrelevant,
			Confidence: 0.95,
		},
		State:         domain.StateNew,
		MinConfidence: testMinConfidence,
	})
	if got.Respond {
		t.Fatal("an irrelevant message must not be answered")
	}
}

// Even inside an active flow, an off-topic question gets no answer.
func TestUnrelatedQuestionInActiveFlowIsIgnored(t *testing.T) {
	got := ShouldRespond(DecisionInput{
		AICalled: true,
		AI: domain.AIClassification{
			IsRelevant: false,
			Intent:     domain.IntentIrrelevant,
			Confidence: 0.9,
		},
		State:         domain.StateQualifying,
		MinConfidence: testMinConfidence,
	})
	if got.Respond {
		t.Fatal("an unrelated question must not be answered even mid-qualification")
	}
}

func TestServiceInquiryGetsTheMenu(t *testing.T) {
	got := ShouldRespond(DecisionInput{
		TriggerMatched: true,
		AICalled:       true,
		AI: domain.AIClassification{
			IsRelevant: true,
			Intent:     domain.IntentServiceInquiry,
			Language:   domain.LangRU,
			Confidence: 0.96,
		},
		State:         domain.StateNew,
		MinConfidence: testMinConfidence,
	})
	if !got.Respond {
		t.Fatalf("a service inquiry must be answered, reason %q", got.Reason)
	}
	if got.Action != ActionServiceMenu {
		t.Fatalf("action = %q, want %q", got.Action, ActionServiceMenu)
	}
	if got.NextState != domain.StateWaitingIntent {
		t.Fatalf("next state = %q, want %q", got.NextState, domain.StateWaitingIntent)
	}
}

func TestIdentifiedServiceAsksOneClarification(t *testing.T) {
	got := ShouldRespond(DecisionInput{
		TriggerMatched: true,
		AICalled:       true,
		AI: domain.AIClassification{
			IsRelevant:            true,
			Intent:                domain.IntentPrivacyPolicy,
			ServiceCode:           domain.ServicePrivacyPolicy,
			Language:              domain.LangRU,
			Confidence:            0.98,
			NeedsClarification:    true,
			ClarificationQuestion: "Политика нужна для сайта или мобильного приложения?",
		},
		State:         domain.StateWaitingIntent,
		HasPhone:      true,
		MinConfidence: testMinConfidence,
	})
	if !got.Respond || got.Action != ActionClarify {
		t.Fatalf("want a clarification, got respond=%v action=%q", got.Respond, got.Action)
	}
	if got.Service != domain.ServicePrivacyPolicy {
		t.Fatalf("service = %q, want %q", got.Service, domain.ServicePrivacyPolicy)
	}
}

func TestClarificationIsNeverAskedTwice(t *testing.T) {
	got := ShouldRespond(DecisionInput{
		AICalled: true,
		AI: domain.AIClassification{
			IsRelevant:         true,
			Intent:             domain.IntentPrivacyPolicy,
			ServiceCode:        domain.ServicePrivacyPolicy,
			Confidence:         0.98,
			NeedsClarification: true,
		},
		State:               domain.StateQualifying,
		HasPhone:            true,
		ClarifyAlreadyAsked: true,
		MinConfidence:       testMinConfidence,
	})
	if got.Action == ActionClarify {
		t.Fatal("the same clarification must not be asked a second time")
	}
	if got.Action != ActionServiceInfo {
		t.Fatalf("action = %q, want %q", got.Action, ActionServiceInfo)
	}
	if got.NextState != domain.StateReadyForDiana {
		t.Fatalf("next state = %q, want %q", got.NextState, domain.StateReadyForDiana)
	}
}

func TestMissingPhoneTriggersContactRequest(t *testing.T) {
	got := ShouldRespond(DecisionInput{
		AICalled: true,
		AI: domain.AIClassification{
			IsRelevant:  true,
			Intent:      domain.IntentTrademark,
			ServiceCode: domain.ServiceTrademarkRegistration,
			Confidence:  0.93,
		},
		State:         domain.StateQualifying,
		HasPhone:      false,
		MinConfidence: testMinConfidence,
	})
	if got.Action != ActionAskContact {
		t.Fatalf("action = %q, want %q", got.Action, ActionAskContact)
	}
	if got.NextState != domain.StateWaitingContact {
		t.Fatalf("next state = %q, want %q", got.NextState, domain.StateWaitingContact)
	}
}

func TestLowConfidenceNewUserStaysSilent(t *testing.T) {
	got := ShouldRespond(DecisionInput{
		TriggerMatched: false,
		AICalled:       true,
		AI: domain.AIClassification{
			IsRelevant:  true,
			Intent:      domain.IntentUnclear,
			Confidence:  0.40,
			ServiceCode: "",
		},
		State:         domain.StateNew,
		MinConfidence: testMinConfidence,
	})
	if got.Respond {
		t.Fatal("the bot must not guess at a stranger on low confidence")
	}
	if got.Reason != ReasonLowConfidenceNew {
		t.Fatalf("reason = %q, want %q", got.Reason, ReasonLowConfidenceNew)
	}
}

func TestLowConfidenceInActiveFlowAsksForClarity(t *testing.T) {
	got := ShouldRespond(DecisionInput{
		AICalled: true,
		AI: domain.AIClassification{
			IsRelevant: true,
			Intent:     domain.IntentUnclear,
			Confidence: 0.40,
		},
		State:         domain.StateQualifying,
		MinConfidence: testMinConfidence,
	})
	if !got.Respond || got.Action != ActionClarify {
		t.Fatalf("an active conversation deserves one clarification, got respond=%v action=%q",
			got.Respond, got.Action)
	}
}

// When OpenAI is down, deterministic triggers keep the bot useful without
// letting it answer messages it does not understand.
func TestModelFailureFallsBackToTriggers(t *testing.T) {
	withTrigger := ShouldRespond(DecisionInput{
		TriggerMatched: true,
		AICalled:       true,
		AIFailed:       true,
		State:          domain.StateNew,
		MinConfidence:  testMinConfidence,
	})
	if !withTrigger.Respond || withTrigger.Action != ActionServiceMenu {
		t.Fatalf("a clear legal trigger should still get the menu, got %+v", withTrigger)
	}
	if withTrigger.Reason != ReasonAIFallbackTrigger {
		t.Fatalf("reason = %q, want %q", withTrigger.Reason, ReasonAIFallbackTrigger)
	}

	withoutTrigger := ShouldRespond(DecisionInput{
		TriggerMatched: false,
		AICalled:       true,
		AIFailed:       true,
		State:          domain.StateNew,
		MinConfidence:  testMinConfidence,
	})
	if withoutTrigger.Respond {
		t.Fatal("with no model and no trigger the bot must stay silent rather than guess")
	}
}

func TestSmallTalkAfterHandoffIsIgnored(t *testing.T) {
	got := ShouldRespond(DecisionInput{
		AICalled: true,
		AI: domain.AIClassification{
			IsRelevant:  true,
			Intent:      domain.IntentPrivacyPolicy,
			ServiceCode: domain.ServicePrivacyPolicy,
			Confidence:  0.9,
		},
		State:         domain.StateReadyForDiana,
		KnownService:  domain.ServicePrivacyPolicy,
		HasPhone:      true,
		MinConfidence: testMinConfidence,
	})
	if got.Respond {
		t.Fatalf("after handoff the bot should stop talking, got action %q", got.Action)
	}
	if got.Reason != ReasonAlreadyHandedOff {
		t.Fatalf("reason = %q, want %q", got.Reason, ReasonAlreadyHandedOff)
	}
}

func TestNewServiceAfterHandoffReopensTheConversation(t *testing.T) {
	got := ShouldRespond(DecisionInput{
		AICalled: true,
		AI: domain.AIClassification{
			IsRelevant:  true,
			Intent:      domain.IntentTrademark,
			ServiceCode: domain.ServiceTrademarkRegistration,
			Confidence:  0.95,
		},
		State:         domain.StateReadyForDiana,
		KnownService:  domain.ServicePrivacyPolicy,
		HasPhone:      true,
		MinConfidence: testMinConfidence,
	})
	if !got.Respond {
		t.Fatal("a genuinely new request after handoff should be picked up")
	}
	if got.Service != domain.ServiceTrademarkRegistration {
		t.Fatalf("service = %q, want %q", got.Service, domain.ServiceTrademarkRegistration)
	}
}

// The model's own should_respond flag must never be the deciding factor.
func TestModelCannotForceAReply(t *testing.T) {
	got := ShouldRespond(DecisionInput{
		AICalled: true,
		AI: domain.AIClassification{
			IsRelevant:    false,
			ShouldRespond: true, // the model insists
			Intent:        domain.IntentIrrelevant,
			Confidence:    1.0,
		},
		State:         domain.StateNew,
		MinConfidence: testMinConfidence,
	})
	if got.Respond {
		t.Fatal("should_respond from the model must not override the application decision")
	}
}

func TestServiceFromIntent(t *testing.T) {
	cases := map[domain.Intent]string{
		domain.IntentTrademark:     domain.ServiceTrademarkRegistration,
		domain.IntentPrivacyPolicy: domain.ServicePrivacyPolicy,
		domain.IntentBusinessReg:   domain.ServiceBusinessRegistration,
		domain.IntentGreeting:      "",
		domain.IntentIrrelevant:    "",
		domain.IntentUnclear:       "",
	}
	for intent, want := range cases {
		if got := ServiceFromIntent(intent); got != want {
			t.Errorf("ServiceFromIntent(%q) = %q, want %q", intent, got, want)
		}
	}
}
