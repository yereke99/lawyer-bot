package openai

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"lawyer-bot/internal/domain"
)

// ---------------------------------------------------------------- wire types

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	ResponseFormat      json.RawMessage `json:"response_format,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage usage `json:"usage"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// classificationDTO is the exact shape the model is forced to return.
type classificationDTO struct {
	IsRelevant            bool    `json:"is_relevant"`
	ShouldRespond         bool    `json:"should_respond"`
	Language              string  `json:"language"`
	Intent                string  `json:"intent"`
	ServiceCode           string  `json:"service_code"`
	Confidence            float64 `json:"confidence"`
	NeedsClarification    bool    `json:"needs_clarification"`
	ClarificationQuestion string  `json:"clarification_question"`
	LeadScore             float64 `json:"lead_score"`
	Summary               string  `json:"summary"`
	Facts                 struct {
		Platform  string `json:"platform"`
		AppStatus string `json:"app_status"`
		Country   string `json:"country"`
	} `json:"facts"`
}

// ------------------------------------------------------------ prompt & schema

// systemPrompt is intentionally terse. Every extra sentence is tokens spent on
// every single message, and the model's only job here is classification.
func systemPrompt(in domain.AIInput) string {
	var b strings.Builder

	b.WriteString(`You classify incoming WhatsApp messages for a legal services company in Kazakhstan.
Return ONLY the JSON object of the given schema. You never talk to the customer directly.

Rules:
- Legal services only. Greetings, small talk, weather, jokes, or anything unrelated => is_relevant=false and intent="greeting" or "irrelevant".
- NEVER mention, estimate or invent any price, cost, amount or currency. Pricing is decided by a human.
- clarification_question: at most one short question, in the customer's own language, max 20 words, no prices. Empty string when nothing needs clarifying.
- language: "ru", "kk" or "en", detected from the customer's message.
- service_code: exactly one code from the list, or "" when unclear.
- confidence and lead_score: 0.0 to 1.0.
- summary: the customer's request in Russian, max 20 words, no prices.
- facts: fill only what the customer actually stated, otherwise "".
  platform: "mobile_app" | "website" | "" ; app_status: "launched" | "in_development" | "" ; country: free text or "".

service_code values:
`)

	codes := make([]string, 0, len(in.Services))
	for _, s := range in.Services {
		codes = append(codes, s.Code)
	}
	sort.Strings(codes)
	b.WriteString(strings.Join(codes, ", "))

	b.WriteString("\n\nintent values:\n")
	intents := make([]string, 0, len(domain.AllIntents))
	for _, i := range domain.AllIntents {
		intents = append(intents, string(i))
	}
	b.WriteString(strings.Join(intents, ", "))

	// Conversation context, kept to the few fields that change the answer.
	b.WriteString("\n\nConversation state: ")
	b.WriteString(string(in.CurrentState))
	if in.DetectedService != "" {
		b.WriteString("\nAlready identified service: ")
		b.WriteString(in.DetectedService)
	}
	if in.KnownLanguage.Valid() {
		b.WriteString("\nPreviously detected language: ")
		b.WriteString(string(in.KnownLanguage))
	}
	if len(in.KnownFacts) > 0 {
		keys := make([]string, 0, len(in.KnownFacts))
		for k := range in.KnownFacts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("\nAlready known: ")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s=%s", k, in.KnownFacts[k])
		}
		b.WriteString("\nDo not ask again about anything already known.")
	}

	return b.String()
}

// agentSystemPrompt asks the model to write the actual WhatsApp answer. The
// application has already decided that replying is allowed, so the model's job
// here is tone and wording, not policy.
func agentSystemPrompt(in domain.AIReplyInput) string {
	var b strings.Builder

	lang := in.KnownLanguage.OrDefault()
	b.WriteString(`You are the WhatsApp assistant for a legal services company in Kazakhstan.
Write the exact outgoing reply to the customer. Return plain text only.

Rules:
- Reply in the customer's language. If uncertain, use Russian.
- Be natural, concise and helpful: 1-3 short sentences, max 700 characters.
- Legal services only. Do not answer unrelated questions.
- Do not give final legal advice, legal conclusions, guarantees or promises of a result.
- NEVER mention, estimate or invent any price, cost, amount, discount, tariff, number of tenge, dollars, euros or any currency.
- Ask at most one question.
- Do not say a human has already received the lead; say you can pass it to Diana or that Diana can clarify details.
- If the action is service_menu, ask what service is needed and list the main legal-service directions briefly.
- If the action is clarify, ask one practical clarification question.
- If the action is ask_contact, ask for a phone number or preferred contact.
- If the action is service_info or handoff, acknowledge the request and offer Diana's follow-up.

Application decision context:
`)
	fmt.Fprintf(&b, "language=%s\n", lang)
	fmt.Fprintf(&b, "state=%s\n", in.CurrentState)
	fmt.Fprintf(&b, "action=%s\n", in.ReplyAction)
	fmt.Fprintf(&b, "reason=%s\n", in.DecisionReason)
	fmt.Fprintf(&b, "next_state=%s\n", in.DecisionNextState)
	fmt.Fprintf(&b, "has_phone=%t\n", in.HasPhone)

	service := firstNonEmpty(in.DecisionService, in.DetectedService, in.Classification.ServiceCode)
	if service != "" {
		fmt.Fprintf(&b, "service=%s\n", service)
	}
	if in.Classification.Intent != "" {
		fmt.Fprintf(&b, "intent=%s\n", in.Classification.Intent)
	}
	if in.Classification.Summary != "" {
		fmt.Fprintf(&b, "customer_summary=%s\n", in.Classification.Summary)
	}

	if len(in.KnownFacts) > 0 {
		keys := make([]string, 0, len(in.KnownFacts))
		for k := range in.KnownFacts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("known_facts=")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s=%s", k, in.KnownFacts[k])
		}
		b.WriteString("\n")
	}

	b.WriteString("\nService catalog:\n")
	services := append([]domain.LegalService(nil), in.Services...)
	sort.Slice(services, func(i, j int) bool { return services[i].Code < services[j].Code })
	for _, s := range services {
		fmt.Fprintf(&b, "- %s: %s", s.Code, s.Name(lang))
		if desc := s.Description(lang); desc != "" {
			fmt.Fprintf(&b, " - %s", desc)
		}
		if q := s.ClarifyQuestion(lang); q != "" {
			fmt.Fprintf(&b, " Clarification: %s", q)
		}
		b.WriteString("\n")
	}

	return b.String()
}

func agentUserPrompt(in domain.AIReplyInput, max int) string {
	var b strings.Builder
	b.WriteString("Customer message:\n")
	b.WriteString(truncate(in.Text, max))
	return b.String()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// responseFormat pins the model to a strict JSON schema, so the application
// never has to parse prose.
func responseFormat() json.RawMessage {
	return json.RawMessage(`{
  "type": "json_schema",
  "json_schema": {
    "name": "lead_classification",
    "strict": true,
    "schema": {
      "type": "object",
      "additionalProperties": false,
      "required": ["is_relevant","should_respond","language","intent","service_code","confidence","needs_clarification","clarification_question","lead_score","summary","facts"],
      "properties": {
        "is_relevant": {"type": "boolean"},
        "should_respond": {"type": "boolean"},
        "language": {"type": "string", "enum": ["ru","kk","en"]},
        "intent": {"type": "string", "enum": ["greeting","service_inquiry","consultation_request","trademark_registration","business_registration","contract_request","privacy_policy","public_offer","mobile_app_documents","website_documents","ecommerce_documents","other_legal_service","callback_request","irrelevant","unclear"]},
        "service_code": {"type": "string"},
        "confidence": {"type": "number"},
        "needs_clarification": {"type": "boolean"},
        "clarification_question": {"type": "string"},
        "lead_score": {"type": "number"},
        "summary": {"type": "string"},
        "facts": {
          "type": "object",
          "additionalProperties": false,
          "required": ["platform","app_status","country"],
          "properties": {
            "platform": {"type": "string"},
            "app_status": {"type": "string"},
            "country": {"type": "string"}
          }
        }
      }
    }
  }
}`)
}

// ------------------------------------------------------------- validation

// parseClassification decodes and validates model output. Anything unexpected
// is normalised to a safe value rather than trusted: an out-of-range score, an
// unknown intent or an invented service code must not reach the decision engine.
func parseClassification(raw string, in domain.AIInput) (domain.AIClassification, error) {
	var dto classificationDTO
	if err := json.Unmarshal([]byte(raw), &dto); err != nil {
		return domain.AIClassification{}, fmt.Errorf("decode classification: %w", err)
	}

	out := domain.AIClassification{
		IsRelevant:            dto.IsRelevant,
		ShouldRespond:         dto.ShouldRespond,
		NeedsClarification:    dto.NeedsClarification,
		ClarificationQuestion: strings.TrimSpace(dto.ClarificationQuestion),
		Summary:               strings.TrimSpace(dto.Summary),
		Confidence:            clamp01(dto.Confidence),
		LeadScore:             clamp01(dto.LeadScore),
	}

	lang := domain.Language(strings.ToLower(strings.TrimSpace(dto.Language)))
	if lang.Valid() {
		out.Language = lang
	}

	intent := domain.Intent(strings.TrimSpace(dto.Intent))
	if intent.Valid() {
		out.Intent = intent
	} else {
		out.Intent = domain.IntentUnclear
	}

	// A service code the catalog does not know is dropped, never passed on.
	code := strings.TrimSpace(dto.ServiceCode)
	if code != "" {
		for _, s := range in.Services {
			if s.Code == code {
				out.ServiceCode = code
				break
			}
		}
	}

	facts := make(map[string]string)
	if v := normaliseFact(dto.Facts.Platform); v != "" {
		facts[domain.FactPlatform] = v
	}
	if v := normaliseFact(dto.Facts.AppStatus); v != "" {
		facts[domain.FactAppStatus] = v
	}
	if v := normaliseFact(dto.Facts.Country); v != "" {
		facts[domain.FactCountry] = v
	}
	if len(facts) > 0 {
		out.Facts = facts
	}

	// An irrelevant message can never be relevant at the same time.
	if out.Intent == domain.IntentIrrelevant {
		out.IsRelevant = false
	}

	return out, nil
}

func normaliseFact(v string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "unknown" || v == "n/a" || v == "null" {
		return ""
	}
	if len(v) > 100 {
		v = v[:100]
	}
	return v
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
