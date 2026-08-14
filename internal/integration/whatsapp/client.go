package whatsapp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"lawyer-bot/internal/domain"
)

// Client talks to the WhatsApp Cloud API (Meta Graph API).
type Client struct {
	token         string
	phoneNumberID string
	baseURL       string
	apiVersion    string
	httpClient    *http.Client
}

// Options configures the client.
type Options struct {
	Token         string
	PhoneNumberID string
	BaseURL       string
	APIVersion    string
	Timeout       time.Duration
	HTTPClient    *http.Client
}

// New builds a Client.
func New(opts Options) *Client {
	if opts.BaseURL == "" {
		opts.BaseURL = "https://graph.facebook.com"
	}
	if opts.APIVersion == "" {
		opts.APIVersion = "v21.0"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 15 * time.Second
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: opts.Timeout}
	}
	return &Client{
		token:         opts.Token,
		phoneNumberID: opts.PhoneNumberID,
		baseURL:       strings.TrimRight(opts.BaseURL, "/"),
		apiVersion:    opts.APIVersion,
		httpClient:    httpClient,
	}
}

// SendText sends a plain text message. It is only ever called in reply to an
// incoming message: nothing in this package can start a conversation on its own.
func (c *Client) SendText(ctx context.Context, recipient, text string) (domain.SendResult, error) {
	if recipient == "" {
		return domain.SendResult{}, fmt.Errorf("recipient is required")
	}
	if strings.TrimSpace(text) == "" {
		return domain.SendResult{}, fmt.Errorf("empty message text")
	}

	payload := map[string]any{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                recipient,
		"type":              "text",
		"text": map[string]any{
			"preview_url": false,
			"body":        text,
		},
	}
	return c.post(ctx, payload)
}

// SendMedia sends a previously uploaded media object with an optional caption.
func (c *Client) SendMedia(ctx context.Context, recipient, mediaID, caption string) (domain.SendResult, error) {
	if recipient == "" {
		return domain.SendResult{}, fmt.Errorf("recipient is required")
	}
	if mediaID == "" {
		return domain.SendResult{}, fmt.Errorf("media id is required")
	}

	media := map[string]any{"id": mediaID}
	if caption != "" {
		media["caption"] = caption
	}
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                recipient,
		"type":              "image",
		"image":             media,
	}
	return c.post(ctx, payload)
}

// MarkRead acknowledges an incoming message. Optional, and never required for
// the pipeline to work.
func (c *Client) MarkRead(ctx context.Context, providerMessageID string) error {
	if providerMessageID == "" {
		return nil
	}
	_, err := c.post(ctx, map[string]any{
		"messaging_product": "whatsapp",
		"status":            "read",
		"message_id":        providerMessageID,
	})
	return err
}

func (c *Client) post(ctx context.Context, payload map[string]any) (domain.SendResult, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return domain.SendResult{}, fmt.Errorf("marshal whatsapp request: %w", err)
	}

	url := fmt.Sprintf("%s/%s/%s/messages", c.baseURL, c.apiVersion, c.phoneNumberID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return domain.SendResult{}, fmt.Errorf("build whatsapp request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return domain.SendResult{}, fmt.Errorf("whatsapp request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return domain.SendResult{}, fmt.Errorf("read whatsapp response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return domain.SendResult{}, &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	var parsed struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		// The message was accepted; only the ID is unavailable.
		return domain.SendResult{}, nil
	}
	if len(parsed.Messages) > 0 {
		return domain.SendResult{MessageID: parsed.Messages[0].ID}, nil
	}
	return domain.SendResult{}, nil
}

// APIError is a non-2xx response from the provider.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	body := e.Body
	if len(body) > 400 {
		body = body[:400]
	}
	return fmt.Sprintf("whatsapp api error: status %d: %s", e.StatusCode, body)
}

// Retryable reports whether another attempt could succeed.
func (e *APIError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// VerifySignature checks Meta's X-Hub-Signature-256 header against the raw
// request body. An empty appSecret disables verification, which is only
// acceptable in local development.
func VerifySignature(appSecret string, body []byte, header string) bool {
	if appSecret == "" {
		return true
	}
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}
