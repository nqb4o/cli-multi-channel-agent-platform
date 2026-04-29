package zalo

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/openclaw/agent-platform/internal/gateway/channels"
)

// signatureHeader is the canonical case-folded form of Zalo's signature header.
const signatureHeader = "X-Zevent-Signature"

// Adapter implements channels.ChannelAdapter for a single Zalo Official
// Account.
type Adapter struct {
	cfg       Config
	api       *OaAPI
	refresher *TokenRefresher

	// ownsRefresher tracks whether the adapter built the refresher and is
	// responsible for stopping it.
	ownsRefresher bool
}

// NewOptions allows tests to inject a refresher; production callers pass nil.
type NewOptions struct {
	HTTPClient *http.Client
	Alert      AlertCallback
	Refresher  *TokenRefresher
}

// New constructs an Adapter from a fully-formed Config. opts may be nil.
func New(cfg Config, opts *NewOptions) (*Adapter, error) {
	cfg = cfg.withDefaults()
	if opts == nil {
		opts = &NewOptions{}
	}

	a := &Adapter{cfg: cfg}
	if opts.Refresher != nil {
		a.refresher = opts.Refresher
		a.ownsRefresher = false
	} else {
		refresher, err := a.buildRefresher(opts.Alert)
		if err != nil {
			return nil, err
		}
		a.refresher = refresher
		a.ownsRefresher = true
	}

	timeout := time.Duration(cfg.RequestTimeoutSeconds * float64(time.Second))
	a.api = NewOaAPI(a.refresher.TokenProvider(), cfg.APIBase, cfg.OAuthBase, opts.HTTPClient, timeout)
	return a, nil
}

// Type returns the channel type id used in the gateway registry.
func (a *Adapter) Type() string { return "zalo" }

// Config returns a copy of the static configuration.
func (a *Adapter) Config() Config { return a.cfg }

// API exposes the underlying OaAPI for tests / advanced wiring.
func (a *Adapter) API() *OaAPI { return a.api }

// Refresher exposes the underlying token refresher.
func (a *Adapter) Refresher() *TokenRefresher { return a.refresher }

// SwapRefresher replaces the bound refresher. Test helper for forcing the
// adapter into specific state-machine states without driving the real loop.
func (a *Adapter) SwapRefresher(refresher *TokenRefresher, owned bool) {
	a.refresher = refresher
	a.ownsRefresher = owned
	timeout := time.Duration(a.cfg.RequestTimeoutSeconds * float64(time.Second))
	// Rebuild api so it sees the new refresher's token provider.
	if existing := a.api; existing != nil {
		a.api = NewOaAPI(refresher.TokenProvider(), a.cfg.APIBase, a.cfg.OAuthBase, existing.client, timeout)
	}
}

// Close stops the owned token refresher (if any). Safe to call multiple times.
func (a *Adapter) Close() {
	if a.ownsRefresher && a.refresher != nil {
		a.refresher.Stop()
	}
}

// StartTokenRefresher starts the background refresh loop. Idempotent.
func (a *Adapter) StartTokenRefresher(ctx context.Context) {
	a.refresher.Start(ctx)
}

// StopTokenRefresher stops the background refresh loop. Idempotent.
func (a *Adapter) StopTokenRefresher() {
	a.refresher.Stop()
}

// VerifySignature compares X-ZEvent-Signature against HMAC-SHA256(app_secret,
// body). Strips an optional "sha256=" prefix and lower-cases for compare.
func (a *Adapter) VerifySignature(headers http.Header, body []byte) bool {
	provided := headers.Get(signatureHeader)
	if provided == "" {
		for k, v := range headers {
			if strings.EqualFold(k, signatureHeader) && len(v) > 0 {
				provided = v[0]
				break
			}
		}
	}
	if provided == "" {
		return false
	}
	provided = strings.TrimSpace(provided)
	if low := strings.ToLower(provided); strings.HasPrefix(low, "sha256=") {
		provided = provided[len("sha256="):]
	}
	provided = strings.ToLower(provided)

	mac := hmac.New(sha256.New, []byte(a.cfg.AppSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(provided), []byte(expected))
}

// ParseIncoming wraps ParseEvent with the bound OA id.
func (a *Adapter) ParseIncoming(body []byte) (*channels.NormalizedMessage, error) {
	return ParseEvent(body, a.cfg.OAID)
}

// SendOutgoing delivers a reply to the OA → user conversation.
//
// thread_id is the Zalo user id (per ParseEvent). channel_id is opaque here.
//
// opts recognized:
//
//	"attachments" -> []map[string]any with {kind, url|file_id, caption?}
//
// Long text > cfg.TextChunkLimit is chunked at paragraph / newline / sentence
// boundaries before being sent as multiple sequential /v3.0/oa/message/cs
// calls.
//
// Errors:
//
//   - *SendDisabledError when the refresher is in failure-disabled state.
//   - *MessagingWindowExceededError when the OA tries to reply outside 24h.
//   - *APIError for any other OA Open API failure.
func (a *Adapter) SendOutgoing(ctx context.Context, channelID, threadID, text string, opts map[string]any) error {
	if a.refresher.SendDisabled() {
		return &SendDisabledError{
			OAID:                a.cfg.OAID,
			ConsecutiveFailures: a.refresher.ConsecutiveFailures(),
		}
	}
	if threadID == "" {
		return errors.New("thread_id (zalo user_id) must be non-empty")
	}

	var attachments []map[string]any
	if raw, ok := opts["attachments"]; ok {
		attachments = coerceAttachments(raw)
	}
	for _, att := range attachments {
		if err := a.sendAttachment(ctx, threadID, att); err != nil {
			// Bubble messaging-window errors verbatim — see Python adapter
			// (it explicitly re-raises that type and swallows the rest).
			if _, isWindow := err.(*MessagingWindowExceededError); isWindow {
				return err
			}
			// All other API errors are intentionally swallowed; the Python
			// adapter logs them and continues.
		}
	}

	if text == "" {
		return nil
	}

	chunks, err := ChunkText(text, a.cfg.TextChunkLimit)
	if err != nil {
		return err
	}
	for _, chunk := range chunks {
		if _, err := a.api.SendTextCS(ctx, threadID, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (a *Adapter) sendAttachment(ctx context.Context, recipientUserID string, att map[string]any) error {
	kind, ok := att["kind"].(string)
	if !ok || kind == "" {
		return nil
	}
	zaloType := zaloAttachmentType(kind)
	url := stringOrEmpty(att["url"])
	if url == "" {
		url = stringOrEmpty(att["file_id"])
	}
	if url == "" {
		return nil
	}
	payload := map[string]any{"url": url}
	caption := stringOrEmpty(att["caption"])
	_, err := a.api.SendAttachmentCS(ctx, recipientUserID, zaloType, payload, caption)
	return err
}

// zaloAttachmentType maps our canonical kind → Zalo's wire type. Unknown kinds
// pass through unchanged so callers can inject Zalo-specific values.
func zaloAttachmentType(kind string) string {
	switch kind {
	case "photo":
		return "image"
	case "document":
		return "file"
	default:
		return kind
	}
}

func coerceAttachments(raw any) []map[string]any {
	switch v := raw.(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

func stringOrEmpty(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// buildRefresher constructs a TokenRefresher whose ``refresh`` callable closes
// over the adapter's API. The closure is set up lazily because the API client
// is constructed *after* the refresher (it pulls tokens from the refresher).
func (a *Adapter) buildRefresher(alert AlertCallback) (*TokenRefresher, error) {
	cfg := a.cfg
	refreshFn := func(ctx context.Context, refreshToken string) (map[string]any, error) {
		// Read api lazily so the closure picks up the post-init pointer.
		if a.api == nil {
			return nil, errors.New("api not initialised")
		}
		return a.api.RefreshAccessToken(ctx, refreshToken, cfg.AppID, cfg.AppSecret)
	}
	return NewTokenRefresher(
		cfg.OAAccessToken,
		cfg.OARefreshToken,
		cfg.OAID,
		refreshFn,
		cfg.TokenValiditySeconds,
		cfg.TokenRefreshLeadS,
		cfg.TokenFailureDisableS,
		alert,
		nil, // real clock
		nil, // real sleep
	)
}

// ---------------------------------------------------------------------------
// self-registration helper

// RegistryLike is the minimal surface Register touches in a registry.
type RegistryLike interface {
	Register(channelType string, adapter channels.ChannelAdapter) error
}

// Register builds an adapter from cfg and registers it under "zalo" in the
// supplied registry.
func Register(reg RegistryLike, cfg Config) (*Adapter, error) {
	if reg == nil {
		return nil, errors.New("registry must be non-nil")
	}
	adapter, err := New(cfg, nil)
	if err != nil {
		return nil, err
	}
	if err := reg.Register("zalo", adapter); err != nil {
		return nil, err
	}
	return adapter, nil
}
