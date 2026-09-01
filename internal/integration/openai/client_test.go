package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lawyer-bot/internal/domain"
)

// testInput mirrors what the pipeline sends.
func testInput(text string) domain.AIInput {
	return domain.AIInput{
		Text:         text,
		CurrentState: domain.StateNew,
		Services: []domain.LegalService{
			{Code: domain.ServicePrivacyPolicy, NameRU: "Политика конфиденциальности"},
			{Code: domain.ServiceTrademarkRegistration, NameRU: "Регистрация товарного знака"},
		},
	}
}

// newStubServer stands in for the OpenAI API. No test ever calls the real one.
func newStubServer(t *testing.T, content string, capture *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization header = %q", got)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}

		body, _ := io.ReadAll(r.Body)
		if capture != nil {
			var parsed map[string]any
			if err := json.Unmarshal(body, &parsed); err != nil {
				t.Errorf("request body is not JSON: %v", err)
			}
			*capture = parsed
		}

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]any{"content": content},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 210, "completion_tokens": 40},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func newTestClient(baseURL string) *Client {
	return New(Options{
		APIKey:        "test-key",
		BaseURL:       baseURL,
		Model:         "test-model",
		MaxTokens:     300,
		MaxInputChars: 1200,
	})
}

func TestClassifyMessageParsesStructuredOutput(t *testing.T) {
	content := `{
		"is_relevant": true,
		"should_respond": true,
		"language": "ru",
		"intent": "privacy_policy",
		"service_code": "privacy_policy",
		"confidence": 0.94,
		"needs_clarification": true,
		"clarification_question": "Политика нужна для сайта или мобильного приложения?",
		"lead_score": 0.82,
		"summary": "Нужна политика конфиденциальности",
		"facts": {"platform": "mobile_app", "app_status": "", "country": ""}
	}`

	srv := newStubServer(t, content, nil)
	defer srv.Close()

	got, err := newTestClient(srv.URL).ClassifyMessage(context.Background(),
		testInput("Мне нужна политика конфиденциальности для мобильного приложения"))
	if err != nil {
		t.Fatalf("classify: %v", err)
	}

	if !got.IsRelevant {
		t.Error("is_relevant should be true")
	}
	if got.Language != domain.LangRU {
		t.Errorf("language = %q", got.Language)
	}
	if got.Intent != domain.IntentPrivacyPolicy {
		t.Errorf("intent = %q", got.Intent)
	}
	if got.ServiceCode != domain.ServicePrivacyPolicy {
		t.Errorf("service = %q", got.ServiceCode)
	}
	if got.Confidence != 0.94 {
		t.Errorf("confidence = %v", got.Confidence)
	}
	if got.LeadScore != 0.82 {
		t.Errorf("lead score = %v", got.LeadScore)
	}
	if got.Facts[domain.FactPlatform] != "mobile_app" {
		t.Errorf("platform fact = %q", got.Facts[domain.FactPlatform])
	}
	if got.InputTokens != 210 || got.OutputTokens != 40 {
		t.Errorf("token usage not captured: in=%d out=%d", got.InputTokens, got.OutputTokens)
	}
	if got.RawResponse == "" {
		t.Error("raw response should be kept for the audit log")
	}
}

// The request must pin the model to a strict schema and keep output short.
func TestRequestUsesStrictSchemaAndTokenLimit(t *testing.T) {
	var captured map[string]any
	srv := newStubServer(t, `{"is_relevant":false,"should_respond":false,"language":"ru","intent":"greeting","service_code":"","confidence":0.9,"needs_clarification":false,"clarification_question":"","lead_score":0.1,"summary":"","facts":{"platform":"","app_status":"","country":""}}`, &captured)
	defer srv.Close()

	if _, err := newTestClient(srv.URL).ClassifyMessage(context.Background(), testInput("Здравствуйте")); err != nil {
		t.Fatalf("classify: %v", err)
	}

	if captured["model"] != "test-model" {
		t.Errorf("model = %v", captured["model"])
	}
	if captured["max_completion_tokens"] != float64(300) {
		t.Errorf("max_completion_tokens = %v, want the configured limit", captured["max_completion_tokens"])
	}

	format, ok := captured["response_format"].(map[string]any)
	if !ok {
		t.Fatal("request must pin a response_format")
	}
	if format["type"] != "json_schema" {
		t.Errorf("response_format type = %v, want json_schema", format["type"])
	}
	schema, _ := format["json_schema"].(map[string]any)
	if schema["strict"] != true {
		t.Error("the schema must be strict so output is always parseable")
	}

	// The system prompt must forbid prices.
	messages, _ := captured["messages"].([]any)
	if len(messages) < 2 {
		t.Fatalf("want a system and a user message, got %d", len(messages))
	}
	system, _ := messages[0].(map[string]any)
	prompt, _ := system["content"].(string)
	if !strings.Contains(prompt, "NEVER mention, estimate or invent any price") {
		t.Errorf("system prompt must forbid prices:\n%s", prompt)
	}
	if !strings.Contains(prompt, domain.ServicePrivacyPolicy) {
		t.Error("system prompt should list the catalog service codes")
	}
}

func TestGenerateReplyUsesAgentPromptWithoutSchema(t *testing.T) {
	var captured map[string]any
	srv := newStubServer(t, "Понял, могу передать вопрос Диане для уточнения деталей.", &captured)
	defer srv.Close()

	got, err := newTestClient(srv.URL).GenerateReply(context.Background(), domain.AIReplyInput{
		Text:          "Нужна регистрация товарного знака",
		KnownLanguage: domain.LangRU,
		CurrentState:  domain.StateNew,
		ReplyAction:   "service_info",
		Services:      testInput("").Services,
		Classification: domain.AIClassification{
			Intent:      domain.IntentTrademark,
			ServiceCode: domain.ServiceTrademarkRegistration,
			Summary:     "Регистрация товарного знака",
			Confidence:  0.95,
		},
	})
	if err != nil {
		t.Fatalf("generate reply: %v", err)
	}
	if got.Text == "" {
		t.Fatal("agent reply text should be returned")
	}
	if got.InputTokens != 210 || got.OutputTokens != 40 {
		t.Errorf("token usage not captured: in=%d out=%d", got.InputTokens, got.OutputTokens)
	}
	if _, ok := captured["response_format"]; ok {
		t.Fatal("agent reply must be free text, not JSON-schema classification")
	}

	messages, _ := captured["messages"].([]any)
	if len(messages) < 2 {
		t.Fatalf("want a system and a user message, got %d", len(messages))
	}
	system, _ := messages[0].(map[string]any)
	prompt, _ := system["content"].(string)
	if !strings.Contains(prompt, "Write the exact outgoing reply") {
		t.Errorf("system prompt should describe agent reply generation:\n%s", prompt)
	}
	if !strings.Contains(prompt, "NEVER mention, estimate or invent any price") {
		t.Errorf("agent prompt must forbid prices:\n%s", prompt)
	}
}

// Model output is never trusted: unknown values are normalised away.
func TestInvalidModelOutputIsNormalised(t *testing.T) {
	content := `{
		"is_relevant": true,
		"should_respond": true,
		"language": "fr",
		"intent": "sell_them_something",
		"service_code": "made_up_service",
		"confidence": 7.5,
		"needs_clarification": false,
		"clarification_question": "",
		"lead_score": -3,
		"summary": "",
		"facts": {"platform": "unknown", "app_status": "", "country": ""}
	}`

	srv := newStubServer(t, content, nil)
	defer srv.Close()

	got, err := newTestClient(srv.URL).ClassifyMessage(context.Background(), testInput("тест"))
	if err != nil {
		t.Fatalf("classify: %v", err)
	}

	if got.Intent != domain.IntentUnclear {
		t.Errorf("an unknown intent must become %q, got %q", domain.IntentUnclear, got.Intent)
	}
	if got.ServiceCode != "" {
		t.Errorf("a service code outside the catalog must be dropped, got %q", got.ServiceCode)
	}
	if got.Language != domain.LangUnknown {
		t.Errorf("an unsupported language must be dropped, got %q", got.Language)
	}
	if got.Confidence != 1 {
		t.Errorf("confidence must be clamped to 1, got %v", got.Confidence)
	}
	if got.LeadScore != 0 {
		t.Errorf("lead score must be clamped to 0, got %v", got.LeadScore)
	}
	if got.Facts[domain.FactPlatform] != "" {
		t.Errorf(`"unknown" must not be stored as a fact, got %q`, got.Facts[domain.FactPlatform])
	}
}

func TestIrrelevantIntentForcesIsRelevantFalse(t *testing.T) {
	content := `{"is_relevant":true,"should_respond":true,"language":"ru","intent":"irrelevant","service_code":"","confidence":0.9,"needs_clarification":false,"clarification_question":"","lead_score":0.1,"summary":"","facts":{"platform":"","app_status":"","country":""}}`
	srv := newStubServer(t, content, nil)
	defer srv.Close()

	got, err := newTestClient(srv.URL).ClassifyMessage(context.Background(), testInput("как дела"))
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.IsRelevant {
		t.Fatal("an irrelevant intent cannot also be relevant")
	}
}

func TestAPIErrorIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit"}}`))
	}))
	defer srv.Close()

	_, err := newTestClient(srv.URL).ClassifyMessage(context.Background(), testInput("Нужен юрист"))
	if err == nil {
		t.Fatal("a rate limit response should be an error")
	}

	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		t.Fatalf("want *APIError, got %T", err)
	}
	if !apiErr.Retryable() {
		t.Error("429 should be retryable")
	}
	if strings.Contains(err.Error(), "test-key") {
		t.Error("the API key must never appear in an error message")
	}
}

func TestRefusalIsReportedAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"content": "", "refusal": "I cannot help with that"},
			}},
		})
	}))
	defer srv.Close()

	if _, err := newTestClient(srv.URL).ClassifyMessage(context.Background(), testInput("тест")); err == nil {
		t.Fatal("a refusal should surface as an error, not as a silent empty classification")
	}
}

// A model that rejects an explicit temperature must not break classification.
func TestTemperatureRetry(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		attempts++

		if strings.Contains(string(body), `"temperature"`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported value: 'temperature' does not support 0"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{
				"content": `{"is_relevant":true,"should_respond":true,"language":"ru","intent":"service_inquiry","service_code":"","confidence":0.9,"needs_clarification":false,"clarification_question":"","lead_score":0.5,"summary":"","facts":{"platform":"","app_status":"","country":""}}`,
			}}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	defer srv.Close()

	got, err := newTestClient(srv.URL).ClassifyMessage(context.Background(), testInput("Какие у вас услуги?"))
	if err != nil {
		t.Fatalf("classify should recover by retrying without temperature: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("want 2 attempts (rejected, then retried), got %d", attempts)
	}
	if got.Intent != domain.IntentServiceInquiry {
		t.Fatalf("intent = %q", got.Intent)
	}
}

func TestLongInputIsTruncated(t *testing.T) {
	var captured map[string]any
	srv := newStubServer(t, `{"is_relevant":false,"should_respond":false,"language":"ru","intent":"unclear","service_code":"","confidence":0.1,"needs_clarification":false,"clarification_question":"","lead_score":0,"summary":"","facts":{"platform":"","app_status":"","country":""}}`, &captured)
	defer srv.Close()

	client := New(Options{APIKey: "test-key", BaseURL: srv.URL, Model: "test-model", MaxInputChars: 50})
	if _, err := client.ClassifyMessage(context.Background(), testInput(strings.Repeat("длинный текст ", 100))); err != nil {
		t.Fatalf("classify: %v", err)
	}

	messages, _ := captured["messages"].([]any)
	last, _ := messages[len(messages)-1].(map[string]any)
	content, _ := last["content"].(string)
	if len([]rune(content)) > 50 {
		t.Fatalf("input should be truncated to the configured limit, got %d runes", len([]rune(content)))
	}
}
