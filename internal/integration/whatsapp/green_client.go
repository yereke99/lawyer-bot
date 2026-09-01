package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"lawyer-bot/internal/domain"
)

// GreenClient talks to Green API native HTTP methods.
type GreenClient struct {
	idInstance    string
	tokenInstance string
	baseURL       string
	httpClient    *http.Client
}

// GreenOptions configures the Green API client.
type GreenOptions struct {
	IDInstance    string
	TokenInstance string
	BaseURL       string
	Timeout       time.Duration
	HTTPClient    *http.Client
}

// GreenNotification is one item returned from Green API receiveNotification.
type GreenNotification struct {
	ReceiptID int64
	Body      json.RawMessage
}

// NewGreen builds a Green API client.
func NewGreen(opts GreenOptions) *GreenClient {
	if opts.BaseURL == "" {
		opts.BaseURL = "https://api.green-api.com"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 15 * time.Second
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: opts.Timeout}
	}
	return &GreenClient{
		idInstance:    opts.IDInstance,
		tokenInstance: opts.TokenInstance,
		baseURL:       strings.TrimRight(opts.BaseURL, "/"),
		httpClient:    httpClient,
	}
}

// SendText sends a plain text message through Green API sendMessage.
func (c *GreenClient) SendText(ctx context.Context, recipient, text string) (domain.SendResult, error) {
	if strings.TrimSpace(recipient) == "" {
		return domain.SendResult{}, fmt.Errorf("recipient is required")
	}
	if strings.TrimSpace(text) == "" {
		return domain.SendResult{}, fmt.Errorf("empty message text")
	}

	payload := map[string]any{
		"chatId":  greenChatID(recipient),
		"message": text,
	}
	var parsed struct {
		IDMessage string `json:"idMessage"`
	}
	if err := c.doJSON(ctx, http.MethodPost, c.endpoint("sendMessage"), payload, &parsed); err != nil {
		return domain.SendResult{}, err
	}
	return domain.SendResult{MessageID: parsed.IDMessage}, nil
}

// SendMedia is kept for interface completeness. The current bot only sends
// text replies and text lead notifications.
func (c *GreenClient) SendMedia(ctx context.Context, recipient, mediaID, caption string) (domain.SendResult, error) {
	return domain.SendResult{}, fmt.Errorf("green api SendMedia is not implemented")
}

// ReceiveNotification receives one item from Green API's HTTP API queue.
func (c *GreenClient) ReceiveNotification(ctx context.Context, timeoutSeconds int) (*GreenNotification, error) {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 5
	}
	path := fmt.Sprintf("%s?receiveTimeout=%d", c.endpoint("receiveNotification"), timeoutSeconds)

	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}

	var parsed struct {
		ReceiptID int64           `json:"receiptId"`
		Body      json.RawMessage `json:"body"`
		Code      string          `json:"code"`
		Message   string          `json:"message"`
		Status    string          `json:"status"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode green api notification: %w", err)
	}
	if strings.EqualFold(parsed.Status, "error") || parsed.Code != "" {
		return nil, fmt.Errorf("green api error: %s %s", parsed.Code, parsed.Message)
	}
	if parsed.ReceiptID == 0 || len(parsed.Body) == 0 || string(parsed.Body) == "null" {
		return nil, nil
	}
	return &GreenNotification{ReceiptID: parsed.ReceiptID, Body: parsed.Body}, nil
}

// DeleteNotification removes a processed notification from the Green API queue.
func (c *GreenClient) DeleteNotification(ctx context.Context, receiptID int64) error {
	if receiptID == 0 {
		return nil
	}

	var parsed struct {
		Result bool `json:"result"`
	}
	path := fmt.Sprintf("%s/%d", c.endpoint("deleteNotification"), receiptID)
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, &parsed); err != nil {
		return err
	}
	if !parsed.Result {
		return fmt.Errorf("green api notification %d was not deleted", receiptID)
	}
	return nil
}

func (c *GreenClient) endpoint(method string) string {
	return fmt.Sprintf("%s/waInstance%s/%s/%s", c.baseURL, c.idInstance, method, c.tokenInstance)
}

func (c *GreenClient) doJSON(ctx context.Context, method, url string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal green api request: %w", err)
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return fmt.Errorf("build green api request: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("green api request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read green api response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode green api response: %w", err)
	}
	return nil
}

func greenChatID(recipient string) string {
	recipient = strings.TrimSpace(recipient)
	if strings.Contains(recipient, "@") {
		return recipient
	}

	var digits strings.Builder
	for _, r := range recipient {
		if unicode.IsDigit(r) {
			digits.WriteRune(r)
		}
	}
	if digits.Len() == 0 {
		return recipient
	}
	return digits.String() + "@c.us"
}
