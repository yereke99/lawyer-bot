package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"lawyer-bot/internal/domain"
)

// The configured funnel trigger. Everything in this file is written against the
// real phrase a campaign hands out, not a synthetic one.
const funnelTrigger = "Сәлеметсіз бе! Тауар белгісін тіркегім келеді"

// permanentSendError is a provider refusal the transport layer must not retry,
// such as a rejected recipient.
type permanentSendError struct{}

func (permanentSendError) Error() string   { return "provider rejected the message" }
func (permanentSendError) Retryable() bool { return false }

func trademarkClassification() domain.AIClassification {
	return domain.AIClassification{
		IsRelevant: true, ShouldRespond: true,
		Language: domain.LangKK, Intent: domain.IntentTrademark,
		ServiceCode: domain.ServiceTrademarkRegistration,
		Confidence:  0.93, LeadScore: 0.8,
	}
}

// funnelHarness is the production wiring: CRM stores, the shared outbound layer
// and an explicit activation configuration.
func funnelHarness(t *testing.T, ai *stubAI, strict bool) *crmHarness {
	t.Helper()
	h := newCRMHarness(t, ai, testFollowUpConfig())
	h.rebuildPipeline(t, ai, NewActivation([]string{funnelTrigger}, NewTriggerSet(), strict))
	return h
}

func (h *crmHarness) sessionActive(t *testing.T, waID string) bool {
	t.Helper()
	user, err := h.users.GetByWhatsAppID(context.Background(), waID)
	if err != nil {
		return false
	}
	return user.BotSessionActive()
}

// sentTo counts what actually reached one chat. The lead alert to the company's
// own consultant number is a separate, deliberate recipient and never counts as
// an answer to the customer.
func (h *crmHarness) sentTo(chatID string) int {
	n := 0
	for _, m := range h.wa.messages() {
		if m.To == chatID {
			n++
		}
	}
	return n
}

func privateInbound(waID, id, text string) domain.InboundMessage {
	msg := inbound(id, text)
	msg.WhatsAppUserID = waID
	msg.PhoneNumber = strings.TrimSuffix(waID, "@c.us")
	return msg
}

// Case A — an unrelated private message before the trigger costs nothing and is
// never answered. The WhatsApp number is also a personal one.
func TestPrivateMessageBeforeTriggerIsIgnored(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkClassification()}}
	h := funnelHarness(t, ai, false)

	for i, text := range []string{"Привет", "Ты где?", "Я тебя вчера весь вечер ждал"} {
		msg := privateInbound("77015550001@c.us", fmt.Sprintf("wamid.personal.%d", i), text)
		if err := h.pipeline.Handle(context.Background(), msg); err != nil {
			t.Fatalf("handle %q: %v", text, err)
		}
	}

	if h.ai.callCount() != 0 {
		t.Fatalf("a contact outside the funnel must not reach the model, got %d call(s)", h.ai.callCount())
	}
	if got := h.wa.messages(); len(got) != 0 {
		t.Fatalf("a contact outside the funnel must receive nothing, got %+v", got)
	}
	if h.sessionActive(t, "77015550001@c.us") {
		t.Fatal("an unrelated message must not open a funnel session")
	}
}

// Case B — the configured trigger activates the customer, and the first answer
// actually reaches WhatsApp at their own chat id.
func TestTriggerActivatesSessionAndDeliversFirstReply(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkClassification()}}
	h := funnelHarness(t, ai, true)

	const chatID = "77015550002@c.us"
	msg := privateInbound(chatID, "wamid.trigger", funnelTrigger)
	msg.TraceID = "trace-activation"
	if err := h.pipeline.Handle(context.Background(), msg); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if !h.sessionActive(t, chatID) {
		t.Fatal("the trigger must open a funnel session")
	}
	if h.ai.callCount() != 1 {
		t.Fatalf("the trigger must reach the model exactly once, got %d", h.ai.callCount())
	}

	sent := h.wa.messages()
	if len(sent) == 0 {
		t.Fatal("the customer received no answer")
	}
	if sent[0].To != chatID {
		t.Fatalf("the answer went to %q instead of the originating chat %q", sent[0].To, chatID)
	}

	stages := h.stages(t, "trace-activation")
	for _, want := range []string{StageSessionOpened, domain.StageAIRequested, domain.StageReplySent} {
		if !hasStage(stages, want) {
			t.Errorf("trace is missing stage %q, got %v", want, stages)
		}
	}

	// The CRM sees the conversation, not just the raw message.
	client, err := h.clients.GetClientByWhatsAppID(context.Background(), chatID)
	if err != nil {
		t.Fatalf("crm contact must exist: %v", err)
	}
	if client.LastOutboundAt == nil {
		t.Fatal("the CRM must record the outgoing reply on the client record")
	}
	if !strings.HasPrefix(client.BotTrigger, ActivationPhrase) {
		t.Fatalf("the activating trigger must be recorded, got %q", client.BotTrigger)
	}
}

// Case C — after activation the conversation continues, including on a message
// that carries no legal keyword at all.
func TestActivatedCustomerKeepsTalkingToTheModel(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkClassification()}}
	h := funnelHarness(t, ai, true)

	const chatID = "77015550003@c.us"
	first := privateInbound(chatID, "wamid.c1", funnelTrigger)
	if err := h.pipeline.Handle(context.Background(), first); err != nil {
		t.Fatalf("handle trigger: %v", err)
	}
	replies := len(h.wa.messages())

	// A bare answer to the assistant's question: no keyword, and small talk by
	// every keyword heuristic in the project.
	second := privateInbound(chatID, "wamid.c2", "Иә, рахмет")
	second.TraceID = "trace-followup"
	if err := h.pipeline.Handle(context.Background(), second); err != nil {
		t.Fatalf("handle follow-up: %v", err)
	}

	if h.ai.callCount() != 2 {
		t.Fatalf("an active customer's message must reach the model, got %d call(s)", h.ai.callCount())
	}
	sent := h.wa.messages()
	if len(sent) <= replies {
		t.Fatal("an active customer received no answer to their second message")
	}
	if last := sent[len(sent)-1]; last.To != chatID {
		t.Fatalf("the answer went to %q instead of %q", last.To, chatID)
	}

	// One conversation, one contact.
	client, err := h.clients.GetClientByWhatsAppID(context.Background(), chatID)
	if err != nil {
		t.Fatalf("crm contact: %v", err)
	}
	history, _, err := h.messages.PageByUser(context.Background(), client.ID, 0, 50)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	if len(history) < 4 {
		t.Fatalf("both turns must be stored in one conversation, got %d rows", len(history))
	}

	// The model was given the earlier turns, not an empty context.
	h.ai.mu.Lock()
	lastInput := h.ai.inputs[len(h.ai.inputs)-1]
	h.ai.mu.Unlock()
	if len(lastInput.History) == 0 {
		t.Fatal("the second call must carry the conversation history")
	}
}

// Case D — a group chat never activates the bot, even sending the exact trigger.
func TestGroupChatNeverActivatesTheBot(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkClassification()}}
	h := funnelHarness(t, ai, false)

	msg := privateInbound("77015550004-1600000000@g.us", "wamid.group", funnelTrigger)
	msg.WhatsAppUserID = "77015550004-1600000000@g.us"
	msg.TraceID = "trace-group"
	if err := h.pipeline.Handle(context.Background(), msg); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if h.ai.callCount() != 0 {
		t.Fatalf("a group message must never reach the model, got %d call(s)", h.ai.callCount())
	}
	if got := h.wa.messages(); len(got) != 0 {
		t.Fatalf("a group must never receive a message, got %+v", got)
	}
	if !hasStage(h.stages(t, "trace-group"), StageWhatsAppChatGate) {
		t.Error("the group rejection must be traced")
	}
}

// Case F — a duplicate provider delivery of the same message produces exactly
// one answer.
func TestDuplicateDeliveryAnswersOnce(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkClassification()}}
	h := funnelHarness(t, ai, true)

	const chatID = "77015550005@c.us"
	msg := privateInbound(chatID, "wamid.dup", funnelTrigger)
	for i := 0; i < 3; i++ {
		if err := h.pipeline.Handle(context.Background(), msg); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}

	if h.ai.callCount() != 1 {
		t.Fatalf("a duplicate delivery must not be classified again, got %d call(s)", h.ai.callCount())
	}
	if got := h.sentTo(chatID); got != 1 {
		t.Fatalf("a duplicate delivery must not be answered twice, got %d messages", got)
	}
}

// Case G — two customers writing at the same time stay isolated.
func TestConcurrentCustomersStayIsolated(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkClassification()}}
	h := funnelHarness(t, ai, true)

	chats := []string{"77015550006@c.us", "77015550007@c.us"}
	var wg sync.WaitGroup
	for i, chatID := range chats {
		wg.Add(1)
		go func(i int, chatID string) {
			defer wg.Done()
			msg := privateInbound(chatID, fmt.Sprintf("wamid.par.%d", i), funnelTrigger)
			if err := h.pipeline.Handle(context.Background(), msg); err != nil {
				t.Errorf("handle %s: %v", chatID, err)
			}
		}(i, chatID)
	}
	wg.Wait()

	seen := map[string]int{}
	for _, m := range h.wa.messages() {
		seen[m.To]++
	}
	for _, chatID := range chats {
		if seen[chatID] != 1 {
			t.Fatalf("customer %s must receive exactly one answer, got %d", chatID, seen[chatID])
		}
	}

	// Neither customer's messages leaked into the other's context.
	h.ai.mu.Lock()
	defer h.ai.mu.Unlock()
	for _, in := range h.ai.inputs {
		if len(in.History) != 0 {
			t.Fatalf("a first message must not carry another customer's history: %+v", in.History)
		}
	}
}

// Case H — the model answered but WhatsApp refused. The generated reply is kept
// and marked failed; nothing calls the model a second time.
func TestFailedDeliveryKeepsTheGeneratedReply(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkClassification()}}
	h := funnelHarness(t, ai, true)
	h.wa.err = permanentSendError{}

	const chatID = "77015550008@c.us"
	msg := privateInbound(chatID, "wamid.faildelivery", funnelTrigger)
	msg.TraceID = "trace-faildelivery"
	if err := h.pipeline.Handle(context.Background(), msg); err != nil {
		t.Fatalf("a delivery failure must not fail the pipeline: %v", err)
	}

	if h.ai.callCount() != 1 {
		t.Fatalf("a transport failure must not re-run the model, got %d call(s)", h.ai.callCount())
	}

	client, err := h.clients.GetClientByWhatsAppID(context.Background(), chatID)
	if err != nil {
		t.Fatalf("crm contact: %v", err)
	}
	history, _, err := h.messages.PageByUser(context.Background(), client.ID, 0, 50)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	var outgoing *domain.Message
	for i := range history {
		if history[i].Direction == domain.DirectionOutgoing {
			outgoing = &history[i]
		}
	}
	if outgoing == nil {
		t.Fatal("the generated reply must be preserved even when delivery fails")
	}
	if outgoing.DeliveryStatus != domain.DeliveryFailed {
		t.Fatalf("the failed reply must be visible as failed, got %q", outgoing.DeliveryStatus)
	}
	if !hasStage(h.stages(t, "trace-faildelivery"), domain.StageReplyFailed) {
		t.Error("a delivery failure must be traced")
	}
}

// Case J — a group that exists as a contact is never a destination for the AI
// funnel, and the outbound layer refuses it at the transport boundary.
func TestOutboundLayerRefusesGroupsAndBroadcasts(t *testing.T) {
	h := funnelHarness(t, &stubAI{}, false)

	for _, recipient := range []string{
		"77015550009-1600000000@g.us",
		"status@broadcast",
		"",
	} {
		if _, err := h.messenger.Send(context.Background(), Outbound{
			UserID: 1, Recipient: recipient, Text: "тест",
		}); err == nil {
			t.Fatalf("the outbound layer must refuse %q", recipient)
		}
	}
	if got := h.wa.messages(); len(got) != 0 {
		t.Fatalf("nothing may reach the provider, got %+v", got)
	}
}

// Strict mode is the tightest configuration: only the configured phrases open a
// session, and a legal keyword on its own does not.
func TestStrictModeOnlyActivatesOnConfiguredPhrases(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkClassification()}}
	h := funnelHarness(t, ai, true)

	msg := privateInbound("77015550010@c.us", "wamid.keyword", "Здравствуйте, мне нужен юрист по договорам")
	if err := h.pipeline.Handle(context.Background(), msg); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if h.ai.callCount() != 0 || len(h.wa.messages()) != 0 {
		t.Fatal("strict mode must ignore a message that is not a configured trigger")
	}
}

// Formatting a customer actually produces must still activate the funnel.
func TestTriggerMatchingToleratesRealWorldFormatting(t *testing.T) {
	activation := NewActivation([]string{funnelTrigger}, NewTriggerSet(), true)

	accepted := []string{
		funnelTrigger,
		"  сәлеметсіз бе! тауар белгісін тіркегім келеді  ",
		"СӘЛЕМЕТСІЗ БЕ!!!  ТАУАР   БЕЛГІСІН\nТІРКЕГІМ КЕЛЕДІ",
		"Сәлеметсіз бе! Тауар белгісін тіркегім келеді. Қашан бастаймыз?",
	}
	for _, text := range accepted {
		if m := activation.Match(text, domain.MessageText); !m.Matched {
			t.Errorf("this must activate the funnel: %q", text)
		}
	}

	rejected := []string{
		"Сәлеметсіз бе",
		"Тауар белгісі деген не?",
		"Привет",
		"",
	}
	for _, text := range rejected {
		if m := activation.Match(text, domain.MessageText); m.Matched {
			t.Errorf("this must not activate the funnel: %q", text)
		}
	}

	// Media without a caption can never be an activation trigger.
	if m := activation.Match(funnelTrigger, domain.MessageImage); m.Matched {
		t.Error("a non-analysable message type must not activate the funnel")
	}
}

// A repeated trigger keeps the original session rather than restarting it.
func TestRepeatedTriggerDoesNotRestartTheSession(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkClassification()}}
	h := funnelHarness(t, ai, true)

	const chatID = "77015550011@c.us"
	first := privateInbound(chatID, "wamid.again.1", funnelTrigger)
	if err := h.pipeline.Handle(context.Background(), first); err != nil {
		t.Fatalf("handle: %v", err)
	}
	user, err := h.users.GetByWhatsAppID(context.Background(), chatID)
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	opened := *user.BotActivatedAt

	time.Sleep(2 * time.Millisecond)
	second := privateInbound(chatID, "wamid.again.2", funnelTrigger)
	if err := h.pipeline.Handle(context.Background(), second); err != nil {
		t.Fatalf("handle: %v", err)
	}
	user, err = h.users.GetByWhatsAppID(context.Background(), chatID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if !user.BotActivatedAt.Equal(opened) {
		t.Fatalf("a repeated trigger must not move the activation time: %v -> %v", opened, *user.BotActivatedAt)
	}
}

// A contact who never entered the funnel is never nudged.
func TestFollowUpNeverTargetsAContactOutsideTheFunnel(t *testing.T) {
	h := funnelHarness(t, &stubAI{}, true)
	ctx := context.Background()

	user, err := h.users.Upsert(ctx, "77015550012@c.us", "77015550012", "Стороннний")
	if err != nil {
		t.Fatalf("create contact: %v", err)
	}
	client, err := h.clients.GetClient(ctx, user.ID)
	if err != nil {
		t.Fatalf("load client: %v", err)
	}
	if err := h.follow.Schedule(ctx, client, 1, 0); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if job, err := h.jobs.NextPendingForUser(ctx, user.ID); err == nil {
		t.Fatalf("no nudge may be scheduled for a contact outside the funnel: %+v", job)
	}
}

// Case E, end to end — a consultant answers from the CRM. The message reaches
// only that customer, the assistant stops talking, and the customer's next
// message does not restart it behind the consultant's back.
func TestConsultantReplyStopsTheAssistant(t *testing.T) {
	ai := &stubAI{results: []domain.AIClassification{trademarkClassification()}}
	h := funnelHarness(t, ai, true)
	ctx := context.Background()

	const chatID = "77015550013@c.us"
	if err := h.pipeline.Handle(ctx, privateInbound(chatID, "wamid.e1", funnelTrigger)); err != nil {
		t.Fatalf("handle trigger: %v", err)
	}
	client, err := h.clients.GetClientByWhatsAppID(ctx, chatID)
	if err != nil {
		t.Fatalf("crm contact: %v", err)
	}

	actor := Actor{ID: 1, Name: "Диана", Role: domain.RoleConsultant}
	stored, err := h.crm.SendMessage(ctx, actor, client.ID, "Здравствуйте, я подключилась.", nil)
	if err != nil {
		t.Fatalf("consultant send: %v", err)
	}
	if stored.SenderType != domain.SenderConsultant || stored.Direction != domain.DirectionOutgoing {
		t.Fatalf("a manual message must be stored as an outgoing consultant message: %+v", stored)
	}

	callsBefore := h.ai.callCount()
	sentBefore := h.sentTo(chatID)

	// The customer writes again. The consultant owns the conversation now.
	if err := h.pipeline.Handle(ctx, privateInbound(chatID, "wamid.e2", "Жақсы, күтемін")); err != nil {
		t.Fatalf("handle follow-up: %v", err)
	}
	if h.ai.callCount() != callsBefore {
		t.Fatal("the assistant must not answer a conversation a consultant took over")
	}
	if h.sentTo(chatID) != sentBefore {
		t.Fatal("the assistant must not send while a consultant owns the conversation")
	}

	// Nothing reached any other chat.
	for _, m := range h.wa.messages() {
		if m.To != chatID && m.To != dianaPhone {
			t.Fatalf("an unrelated chat received a message: %q", m.To)
		}
	}
}
