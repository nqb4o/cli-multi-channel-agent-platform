package zalo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MessagingWindowExceededErrorCodes are the OA Open API error codes that map
// to "OA tried to reply outside the 24h customer-care window". Zalo's exact
// numbering has historically differed across tenants; we accept a small set.
var MessagingWindowExceededErrorCodes = map[int]struct{}{
	217: {},
	228: {},
}

// InvalidAccessTokenErrorCodes are the OA Open API codes that indicate the OA
// access token is no longer valid. The token refresher is the canonical fix.
var InvalidAccessTokenErrorCodes = map[int]struct{}{
	-216: {},
	216:  {},
	-124: {},
}

// APIError represents a Zalo OA Open API failure. ErrorCode is Zalo's numeric
// code (or nil-pointer for HTTP failures with no envelope); StatusCode is the
// HTTP status; Description is the human-readable error.
type APIError struct {
	ErrorCode   *int
	Description string
	StatusCode  int
	Payload     any
}

// Error renders the API error as a string.
func (e *APIError) Error() string {
	if e.ErrorCode != nil {
		return fmt.Sprintf("zalo api error %d: %s", *e.ErrorCode, e.Description)
	}
	return fmt.Sprintf("zalo api error (http %d): %s", e.StatusCode, e.Description)
}

// MessagingWindowExceededError signals that the OA tried to reply outside
// the 24h customer-care window. Wraps APIError so callers can either type-
// assert specifically or fall through to the general handler.
type MessagingWindowExceededError struct {
	APIError
}

// Error renders the structured message.
func (e *MessagingWindowExceededError) Error() string {
	return "zalo messaging window exceeded: " + e.APIError.Error()
}

// SendDisabledError is raised by the adapter when the token refresher is in
// the failure-disabled state. The orchestrator should surface this back to the
// user instead of silently retrying with a stale token.
type SendDisabledError struct {
	OAID                 string
	ConsecutiveFailures  int
}

// Error renders the structured message.
func (e *SendDisabledError) Error() string {
	return fmt.Sprintf(
		"zalo send disabled (oa_id=%s, consecutive_failures=%d)",
		e.OAID, e.ConsecutiveFailures,
	)
}

// Payload reproduces the Python ``payload`` map for parity with downstream
// consumers that key on ``kind``.
func (e *SendDisabledError) Payload() map[string]any {
	return map[string]any{
		"kind":                 "send_disabled",
		"oa_id":                e.OAID,
		"consecutive_failures": e.ConsecutiveFailures,
	}
}

// TokenProvider returns the current OA access token. The OaAPI calls it on
// every outbound request so a backing refresher can rotate tokens transparently.
type TokenProvider func() string

// OaAPI is a thin async-free wrapper around the Zalo OA Open API. Only the
// endpoints F08 needs are exposed.
type OaAPI struct {
	apiBase   string
	oauthBase string
	provider  TokenProvider
	client    *http.Client
	timeout   time.Duration
}

// NewOaAPI builds an OaAPI. provider must return a non-empty access token.
// A nil HTTP client falls back to a default with the requested timeout.
func NewOaAPI(provider TokenProvider, apiBase, oauthBase string, client *http.Client, timeout time.Duration) *OaAPI {
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	if oauthBase == "" {
		oauthBase = DefaultOAuthBase
	}
	if timeout == 0 {
		timeout = time.Duration(DefaultRequestTimeoutSeconds * float64(time.Second))
	}
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &OaAPI{
		apiBase:   strings.TrimRight(apiBase, "/"),
		oauthBase: strings.TrimRight(oauthBase, "/"),
		provider:  provider,
		client:    client,
		timeout:   timeout,
	}
}

// SendTextCS sends a plain-text message via /v3.0/oa/message/cs.
// recipientUserID is the Zalo user id (sender.id from the inbound event).
func (a *OaAPI) SendTextCS(ctx context.Context, recipientUserID, text string) (map[string]any, error) {
	if text == "" {
		return nil, errors.New("text must be non-empty")
	}
	body := map[string]any{
		"recipient": map[string]any{"user_id": recipientUserID},
		"message":   map[string]any{"text": text},
	}
	return a.callOA(ctx, http.MethodPost, a.apiBase+"/v3.0/oa/message/cs", body)
}

// SendAttachmentCS sends an attachment-bearing message. attachmentType is one
// of Zalo's wire values ("image", "file", "template", …).
func (a *OaAPI) SendAttachmentCS(
	ctx context.Context,
	recipientUserID string,
	attachmentType string,
	payload map[string]any,
	text string,
) (map[string]any, error) {
	message := map[string]any{
		"attachment": map[string]any{"type": attachmentType, "payload": payload},
	}
	if text != "" {
		message["text"] = text
	}
	body := map[string]any{
		"recipient": map[string]any{"user_id": recipientUserID},
		"message":   message,
	}
	return a.callOA(ctx, http.MethodPost, a.apiBase+"/v3.0/oa/message/cs", body)
}

// RefreshAccessToken refreshes the OA access token via /v4/oa/access_token.
// On success returns {access_token, refresh_token, expires_in}.
func (a *OaAPI) RefreshAccessToken(ctx context.Context, refreshToken, appID, secretKey string) (map[string]any, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("app_id", appID)

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		a.oauthBase+"/v4/oa/access_token",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("build oauth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("secret_key", secretKey)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth http: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	parsed, err := parseJSONObject(body)
	if err != nil {
		return nil, &APIError{
			ErrorCode:   nil,
			Description: fmt.Sprintf("non-json oauth response: %v", err),
			StatusCode:  resp.StatusCode,
		}
	}

	if _, ok := parsed["access_token"]; !ok || resp.StatusCode >= 400 {
		errCode := errorCodeFromAny(parsed["error"])
		desc := pickString(parsed,
			"error_description",
			"error_name",
			"message",
		)
		if desc == "" {
			desc = fmt.Sprintf("refresh failed (http %d)", resp.StatusCode)
		}
		return nil, &APIError{
			ErrorCode:   errCode,
			Description: desc,
			StatusCode:  resp.StatusCode,
			Payload:     parsed,
		}
	}
	return parsed, nil
}

// ---------------------------------------------------------------------------
// internals

func (a *OaAPI) callOA(ctx context.Context, method, url string, body map[string]any) (map[string]any, error) {
	if a.provider == nil {
		return nil, &APIError{Description: "token provider is nil", StatusCode: 0}
	}
	token := a.provider()
	if token == "" {
		return nil, &APIError{Description: "token provider returned empty token", StatusCode: 0}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("access_token", token)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return unwrapOA(resp.StatusCode, respBody)
}

// unwrapOA validates the standard {error, message, data} envelope. error == 0
// is success; any other code is a failure (with the messaging-window codes
// surfaced as a typed error).
func unwrapOA(status int, body []byte) (map[string]any, error) {
	parsed, err := parseJSONObject(body)
	if err != nil {
		return nil, &APIError{
			ErrorCode:   nil,
			Description: fmt.Sprintf("non-json response: %v", err),
			StatusCode:  status,
		}
	}
	errCode := errorCodeFromAny(parsed["error"])
	if status >= 400 || (errCode != nil && *errCode != 0) {
		desc, _ := parsed["message"].(string)
		if desc == "" {
			desc = "unknown error"
		}
		apiErr := APIError{
			ErrorCode:   errCode,
			Description: desc,
			StatusCode:  status,
			Payload:     parsed,
		}
		if errCode != nil {
			if _, isWindow := MessagingWindowExceededErrorCodes[*errCode]; isWindow {
				return nil, &MessagingWindowExceededError{APIError: apiErr}
			}
		}
		return nil, &apiErr
	}
	return parsed, nil
}

func parseJSONObject(body []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, errors.New("empty body")
	}
	var parsed any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		return nil, err
	}
	envelope, ok := parsed.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("non-object response: %v", parsed)
	}
	return envelope, nil
}

// errorCodeFromAny coerces error codes that the OA Open API serializes as
// either an int or a numeric string.
func errorCodeFromAny(v any) *int {
	switch x := v.(type) {
	case nil:
		return nil
	case int:
		return &x
	case int64:
		i := int(x)
		return &i
	case float64:
		i := int(x)
		return &i
	case json.Number:
		if i, err := x.Int64(); err == nil {
			i2 := int(i)
			return &i2
		}
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return nil
		}
		// Numeric string?
		neg := false
		if strings.HasPrefix(s, "-") {
			neg = true
			s = s[1:]
		}
		for _, r := range s {
			if r < '0' || r > '9' {
				return nil
			}
		}
		if neg {
			s = "-" + s
		}
		var i int
		_, err := fmt.Sscanf(s, "%d", &i)
		if err == nil {
			return &i
		}
	}
	return nil
}

func pickString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}
