package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"lawyer-bot/internal/domain"
)

// Client is a thin OpenAI Chat Completions client. The service layer uses it in
// two steps: first for structured classification, then for a customer-facing
// agent reply only after deterministic application rules allow a response.
type Client struct {
	apiKey     string
	baseURL    string
	model      string
	maxTokens  int
	maxInput   int
	httpClient *http.Client
}

// Options configures the client.
type Options struct {
	APIKey        string
	BaseURL       string
	Model         string
	MaxTokens     int
	MaxInputChars int
	Timeout       time.Duration
	HTTPClient    *http.Client
}

// New builds a Client.
func New(opts Options) *Client {
	if opts.BaseURL == "" {
		opts.BaseURL = "https://api.openai.com/v1"
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = 300
	}
	if opts.MaxInputChars <= 0 {
		opts.MaxInputChars = 1200
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 20 * time.Second
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: opts.Timeout}
	}
	return &Client{
		apiKey:     opts.APIKey,
		baseURL:    strings.TrimRight(opts.BaseURL, "/"),
		model:      opts.Model,
		maxTokens:  opts.MaxTokens,
		maxInput:   opts.MaxInputChars,
		httpClient: httpClient,
	}
}

// ClassifyMessage implements domain.AIClient.
func (c *Client) ClassifyMessage(ctx context.Context, in domain.AIInput) (domain.AIClassification, error) {
	started := time.Now()

	reqBody := c.buildRequest(in)
	raw, usage, err := c.call(ctx, reqBody)
	if err != nil {
		return domain.AIClassification{
			Model:            c.model,
			ProcessingTimeMS: time.Since(started).Milliseconds(),
		}, err
	}

	result, err := parseClassification(raw, in)
	result.Model = c.model
	result.InputTokens = usage.PromptTokens
	result.OutputTokens = usage.CompletionTokens
	result.RawResponse = raw
	result.ProcessingTimeMS = time.Since(started).Milliseconds()
	if err != nil {
		return result, err
	}
	return result, nil
}

// GenerateReply implements domain.AIReplyClient.
func (c *Client) GenerateReply(ctx context.Context, in domain.AIReplyInput) (domain.AIReply, error) {
	started := time.Now()

	reqBody := c.buildReplyRequest(in)
	raw, usage, err := c.call(ctx, reqBody)
	out := domain.AIReply{
		Text:             strings.TrimSpace(raw),
		Model:            c.model,
		InputTokens:      usage.PromptTokens,
		OutputTokens:     usage.CompletionTokens,
		RawResponse:      raw,
		ProcessingTimeMS: time.Since(started).Milliseconds(),
	}
	if err != nil {
		return out, err
	}
	if out.Text == "" {
		return out, fmt.Errorf("openai returned empty agent reply")
	}
	return out, nil
}

func (c *Client) buildRequest(in domain.AIInput) chatRequest {
	messages := make([]chatMessage, 0, len(in.History)+3)
	messages = append(messages, chatMessage{Role: "system", Content: systemPrompt(in)})

	for _, h := range in.History {
		role := "user"
		if h.Role == "assistant" {
			role = "assistant"
		}
		messages = append(messages, chatMessage{Role: role, Content: truncate(h.Text, c.maxInput/2)})
	}

	messages = append(messages, chatMessage{
		Role:    "user",
		Content: truncate(in.Text, c.maxInput),
	})

	temp := 0.0
	return chatRequest{
		Model:               c.model,
		Messages:            messages,
		MaxCompletionTokens: c.maxTokens,
		Temperature:         &temp,
		ResponseFormat:      responseFormat(),
	}
}

func (c *Client) buildReplyRequest(in domain.AIReplyInput) chatRequest {
	messages := make([]chatMessage, 0, len(in.History)+3)
	messages = append(messages, chatMessage{Role: "system", Content: agentSystemPrompt(in)})

	for _, h := range in.History {
		role := "user"
		if h.Role == "assistant" {
			role = "assistant"
		}
		messages = append(messages, chatMessage{Role: role, Content: truncate(h.Text, c.maxInput/2)})
	}

	messages = append(messages, chatMessage{
		Role:    "user",
		Content: agentUserPrompt(in, c.maxInput),
	})

	temp := 0.7
	return chatRequest{
		Model:               c.model,
		Messages:            messages,
		MaxCompletionTokens: c.maxTokens,
		Temperature:         &temp,
	}
}

func (c *Client) call(ctx context.Context, body chatRequest) (string, usage, error) {
	raw, u, err := c.do(ctx, body)
	if err == nil {
		return raw, u, nil
	}
	// Some models reject an explicit temperature. Retry once with the model's
	// own default rather than failing the whole classification.
	if body.Temperature != nil && isUnsupportedParam(err, "temperature") {
		body.Temperature = nil
		return c.do(ctx, body)
	}
	return raw, u, err
}

func (c *Client) do(ctx context.Context, body chatRequest) (string, usage, error) {
	var u usage

	payload, err := json.Marshal(body)
	if err != nil {
		return "", u, fmt.Errorf("marshal openai request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", u, fmt.Errorf("build openai request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", u, fmt.Errorf("openai request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", u, fmt.Errorf("read openai response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// The body may contain the API key only if the caller put it there; the
		// error text below is provider-generated and safe to log.
		return "", u, &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	var parsed chatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", u, fmt.Errorf("decode openai response: %w", err)
	}
	u = parsed.Usage

	if len(parsed.Choices) == 0 {
		return "", u, fmt.Errorf("openai returned no choices")
	}
	choice := parsed.Choices[0]
	if choice.Message.Refusal != "" {
		return "", u, fmt.Errorf("openai refused the request: %s", choice.Message.Refusal)
	}
	content := strings.TrimSpace(choice.Message.Content)
	if content == "" {
		return "", u, fmt.Errorf("openai returned empty content")
	}
	return content, u, nil
}

// APIError is a non-200 response from the API.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	body := e.Body
	if len(body) > 400 {
		body = body[:400]
	}
	return fmt.Sprintf("openai api error: status %d: %s", e.StatusCode, body)
}

// Retryable reports whether another attempt could succeed.
func (e *APIError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

func isUnsupportedParam(err error, param string) bool {
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		return false
	}
	return strings.Contains(apiErr.Body, param)
}

func asAPIError(err error, target **APIError) bool {
	for err != nil {
		if e, ok := err.(*APIError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
