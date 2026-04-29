package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"time"
)

// APIError represents a Telegram Bot API failure (non-2xx status, malformed
// envelope, or {ok: false} response). Callers can inspect StatusCode and
// Description to log / route on specific failure modes.
type APIError struct {
	StatusCode  int
	Description string
	Payload     any
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram api error %d: %s", e.StatusCode, e.Description)
}

// BotAPI is a thin wrapper around https://api.telegram.org/bot<token>.
//
// Only the three methods F07 needs (sendMessage, sendPhoto, sendDocument) are
// exposed. The HTTP client is owned externally so tests can swap in
// httptest.NewServer transports.
type BotAPI struct {
	base    string
	timeout time.Duration
	client  *http.Client
}

// NewBotAPI builds a BotAPI bound to bot_token, optionally overriding the API
// base URL and HTTP client. A nil client falls back to a default with the
// configured per-request timeout.
func NewBotAPI(botToken, apiBase string, client *http.Client, timeout time.Duration) *BotAPI {
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	if timeout == 0 {
		timeout = time.Duration(DefaultRequestTimeoutSeconds * float64(time.Second))
	}
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &BotAPI{
		base:    fmt.Sprintf("%s/bot%s", trimRightSlash(apiBase), botToken),
		timeout: timeout,
		client:  client,
	}
}

// SendMessage calls /sendMessage. messageThreadID and parseMode are sent only
// when non-zero / non-empty.
func (b *BotAPI) SendMessage(ctx context.Context, chatID, text string, messageThreadID int64, parseMode string) (map[string]any, error) {
	payload := map[string]any{"chat_id": chatID, "text": text}
	if messageThreadID != 0 {
		payload["message_thread_id"] = messageThreadID
	}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}
	return b.callJSON(ctx, "sendMessage", payload)
}

// SendPhoto calls /sendPhoto with an existing file_id or HTTP URL.
func (b *BotAPI) SendPhoto(ctx context.Context, chatID, photo, caption string, messageThreadID int64) (map[string]any, error) {
	payload := map[string]any{"chat_id": chatID, "photo": photo}
	if caption != "" {
		payload["caption"] = caption
	}
	if messageThreadID != 0 {
		payload["message_thread_id"] = messageThreadID
	}
	return b.callJSON(ctx, "sendPhoto", payload)
}

// SendDocument uploads bytes as a multipart-encoded document. Used as the
// fall-back path for over-long replies.
func (b *BotAPI) SendDocument(
	ctx context.Context,
	chatID string,
	filename string,
	content []byte,
	caption string,
	mimeType string,
	messageThreadID int64,
) (map[string]any, error) {
	if mimeType == "" {
		mimeType = "text/plain"
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("chat_id", chatID)
	if caption != "" {
		_ = mw.WriteField("caption", caption)
	}
	if messageThreadID != 0 {
		_ = mw.WriteField("message_thread_id", strconv.FormatInt(messageThreadID, 10))
	}
	part, err := createFormFile(mw, "document", filename, mimeType)
	if err != nil {
		return nil, fmt.Errorf("multipart create: %w", err)
	}
	if _, err := part.Write(content); err != nil {
		return nil, fmt.Errorf("multipart write: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("multipart close: %w", err)
	}

	url := b.base + "/sendDocument"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return unwrapEnvelope(resp.StatusCode, body)
}

// ---------------------------------------------------------------------------
// internals

func (b *BotAPI) callJSON(ctx context.Context, method string, payload map[string]any) (map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.base+"/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return unwrapEnvelope(resp.StatusCode, respBody)
}

// unwrapEnvelope validates the standard Telegram Bot API envelope.
func unwrapEnvelope(status int, body []byte) (map[string]any, error) {
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, &APIError{StatusCode: status, Description: fmt.Sprintf("non-json response: %v", err)}
	}
	envelope, ok := parsed.(map[string]any)
	if !ok {
		return nil, &APIError{StatusCode: status, Description: fmt.Sprintf("non-object response: %v", parsed), Payload: parsed}
	}
	okFlag, _ := envelope["ok"].(bool)
	if status >= 400 || !okFlag {
		desc, _ := envelope["description"].(string)
		if desc == "" {
			desc = "unknown error"
		}
		return nil, &APIError{StatusCode: status, Description: desc, Payload: envelope}
	}
	if result, ok := envelope["result"].(map[string]any); ok {
		return result, nil
	}
	// deleteWebhook etc. return {ok: true, result: true} — wrap so callers
	// always get a map.
	return map[string]any{"result": envelope["result"]}, nil
}

func createFormFile(mw *multipart.Writer, field, filename, mimeType string) (io.Writer, error) {
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name=%q; filename=%q`, field, filename)}
	h["Content-Type"] = []string{mimeType}
	return mw.CreatePart(h)
}

func trimRightSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
