package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"lawyer-bot/internal/domain"
)

// This file adds media to the existing Green API integration. The receive path
// (receiveNotification / deleteNotification) is untouched: nothing here changes
// how inbound messages arrive.

// SendFile uploads and sends a file through Green API sendFileByUpload.
//
// Green API decides the WhatsApp message kind from the uploaded file's own MIME
// type, so an .ogg/opus upload arrives as a voice message, an image as a photo
// and everything else as a document.
func (c *GreenClient) SendFile(ctx context.Context, recipient string, file domain.OutgoingFile) (domain.SendResult, error) {
	if strings.TrimSpace(recipient) == "" {
		return domain.SendResult{}, fmt.Errorf("recipient is required")
	}
	if strings.TrimSpace(file.Path) == "" {
		return domain.SendResult{}, fmt.Errorf("file path is required")
	}

	f, err := os.Open(file.Path)
	if err != nil {
		return domain.SendResult{}, fmt.Errorf("open outgoing file: %w", err)
	}
	defer f.Close()

	name := strings.TrimSpace(file.FileName)
	if name == "" {
		name = filepath.Base(file.Path)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("chatId", greenChatID(recipient)); err != nil {
		return domain.SendResult{}, fmt.Errorf("build upload request: %w", err)
	}
	if err := writer.WriteField("fileName", name); err != nil {
		return domain.SendResult{}, fmt.Errorf("build upload request: %w", err)
	}
	if caption := strings.TrimSpace(file.Caption); caption != "" {
		if err := writer.WriteField("caption", caption); err != nil {
			return domain.SendResult{}, fmt.Errorf("build upload request: %w", err)
		}
	}

	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		return domain.SendResult{}, fmt.Errorf("build upload request: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return domain.SendResult{}, fmt.Errorf("read outgoing file: %w", err)
	}
	if err := writer.Close(); err != nil {
		return domain.SendResult{}, fmt.Errorf("finalise upload request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.mediaEndpoint("sendFileByUpload"), bytes.NewReader(body.Bytes()))
	if err != nil {
		return domain.SendResult{}, fmt.Errorf("build green api upload: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return domain.SendResult{}, fmt.Errorf("green api upload failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return domain.SendResult{}, fmt.Errorf("read green api upload response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return domain.SendResult{}, &APIError{StatusCode: resp.StatusCode, Body: string(raw)}
	}

	var parsed struct {
		IDMessage string `json:"idMessage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return domain.SendResult{}, fmt.Errorf("decode green api upload response: %w", err)
	}
	return domain.SendResult{MessageID: parsed.IDMessage}, nil
}

// DownloadFile fetches one inbound media object.
//
// Green API hands out a direct download URL in the incoming notification; when
// it is absent the downloadFile method resolves it from the message id. The
// returned reader is always closed by the caller.
func (c *GreenClient) DownloadFile(ctx context.Context, ref domain.MediaRef) (io.ReadCloser, string, error) {
	url := strings.TrimSpace(ref.URL)
	if url == "" {
		resolved, err := c.resolveDownloadURL(ctx, ref)
		if err != nil {
			return nil, "", err
		}
		url = resolved
	}
	if url == "" {
		return nil, "", fmt.Errorf("no download url for media %q", ref.MessageID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("build media download: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("media download failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, "", &APIError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	return resp.Body, resp.Header.Get("Content-Type"), nil
}

func (c *GreenClient) resolveDownloadURL(ctx context.Context, ref domain.MediaRef) (string, error) {
	if ref.ChatID == "" || ref.MessageID == "" {
		return "", nil
	}
	payload := map[string]any{
		"chatId":    greenChatID(ref.ChatID),
		"idMessage": ref.MessageID,
	}
	var parsed struct {
		DownloadURL string `json:"downloadUrl"`
	}
	if err := c.doJSON(ctx, http.MethodPost, c.endpoint("downloadFile"), payload, &parsed); err != nil {
		return "", err
	}
	return parsed.DownloadURL, nil
}

// mediaEndpoint builds a Green API media URL. Uploads go to the media host,
// which is the same instance path on a different subdomain.
func (c *GreenClient) mediaEndpoint(method string) string {
	base := c.baseURL
	if strings.Contains(base, "://api.") {
		base = strings.Replace(base, "://api.", "://media.", 1)
	}
	return fmt.Sprintf("%s/waInstance%s/%s/%s", base, c.idInstance, method, c.tokenInstance)
}
