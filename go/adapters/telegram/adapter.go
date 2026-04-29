package telegram

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/openclaw/agent-platform/internal/gateway/channels"
)

// secretHeader is the lowercase form of Telegram's secret-token header.
const secretHeader = "X-Telegram-Bot-Api-Secret-Token"

// Adapter implements channels.ChannelAdapter for a single Telegram bot.
//
// Construction binds the adapter to one bot. Use FromEnv for typical wiring,
// or hand-build a Config for tests / multi-bot setups that drive their own
// registry.
type Adapter struct {
	cfg Config
	api *BotAPI
}

// New constructs an Adapter from a fully-formed Config. The HTTP client is
// optional; nil falls back to a default with cfg.RequestTimeoutSeconds.
func New(cfg Config, client *http.Client) *Adapter {
	cfg = cfg.withDefaults()
	timeout := time.Duration(cfg.RequestTimeoutSeconds * float64(time.Second))
	return &Adapter{
		cfg: cfg,
		api: NewBotAPI(cfg.BotToken, cfg.APIBase, client, timeout),
	}
}

// Type returns the channel type id used in the gateway registry.
func (a *Adapter) Type() string { return "telegram" }

// Config returns a copy of the adapter's static configuration.
func (a *Adapter) Config() Config { return a.cfg }

// VerifySignature compares the X-Telegram-Bot-Api-Secret-Token header against
// the configured secret in constant time. Telegram does not HMAC the body —
// the header echo *is* the signature.
func (a *Adapter) VerifySignature(headers http.Header, _ []byte) bool {
	provided := headers.Get(secretHeader)
	if provided == "" {
		// http.Header is case-insensitive on canonical names but callers
		// sometimes pass non-canonical maps. Walk once as a fallback.
		for k, v := range headers {
			if strings.EqualFold(k, secretHeader) && len(v) > 0 {
				provided = v[0]
				break
			}
		}
	}
	if provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare(
		[]byte(provided),
		[]byte(a.cfg.WebhookSecret),
	) == 1
}

// ParseIncoming wraps ParseUpdate with the bound bot id.
func (a *Adapter) ParseIncoming(body []byte) (*channels.NormalizedMessage, error) {
	return ParseUpdate(body, a.cfg.BotID)
}

// SendOutgoing delivers a reply. opts recognised:
//
//	"attachments" -> []map[string]any with {kind, file_id|url, caption?}
//	"parse_mode"  -> string passed through to sendMessage
//
// Long-message handling: text > cfg.TextChunkLimit is chunked at paragraph /
// newline / sentence / raw boundaries and delivered as multiple sequential
// sendMessage calls. Texts longer than 8 * limit fall back to sendDocument as
// a .txt upload.
func (a *Adapter) SendOutgoing(ctx context.Context, channelID, threadID, text string, opts map[string]any) error {
	chatID, messageThreadID, err := parseThread(threadID)
	if err != nil {
		return err
	}

	var attachments []map[string]any
	if raw, ok := opts["attachments"]; ok {
		attachments = coerceAttachments(raw)
	}
	parseMode, _ := opts["parse_mode"].(string)

	// Send any photo attachments first; the trailing text reads like a
	// caption following the photo.
	for _, att := range attachments {
		if kind, _ := att["kind"].(string); kind != "photo" {
			continue
		}
		photoRef := stringOrEmpty(att["file_id"])
		if photoRef == "" {
			photoRef = stringOrEmpty(att["url"])
		}
		if photoRef == "" {
			continue
		}
		caption := stringOrEmpty(att["caption"])
		if _, err := a.api.SendPhoto(ctx, chatID, photoRef, caption, messageThreadID); err != nil {
			// Mirror the Python adapter: log and continue. We have no
			// structured logger here, so we keep the error swallowed
			// (the gateway/observer is the right place to surface it).
		}
	}

	if text == "" {
		return nil
	}

	limit := a.cfg.TextChunkLimit
	if len(text) > limit*8 {
		_, err := a.api.SendDocument(
			ctx, chatID, "reply.txt", []byte(text),
			"", "text/plain; charset=utf-8", messageThreadID,
		)
		return err
	}

	chunks, err := ChunkText(text, limit)
	if err != nil {
		return err
	}
	for _, chunk := range chunks {
		if _, err := a.api.SendMessage(ctx, chatID, chunk, messageThreadID, parseMode); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers

// parseThread splits threadID into (chatID, messageThreadID). Forum-topic
// threads serialize as "<chat_id>:<topic_id>" per ParseUpdate; plain chats
// serialize as just chat_id.
func parseThread(threadID string) (string, int64, error) {
	if threadID == "" {
		return "", 0, errors.New("thread_id must be non-empty")
	}
	idx := strings.LastIndexByte(threadID, ':')
	if idx < 0 {
		return threadID, 0, nil
	}
	chatID := threadID[:idx]
	suffix := threadID[idx+1:]
	topic, err := strconv.ParseInt(suffix, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("thread_id %q has non-integer topic suffix", threadID)
	}
	return chatID, topic, nil
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

// ---------------------------------------------------------------------------
// self-registration helper

// RegistryLike is the minimal surface Register touches in a registry. The
// gateway's *ChannelRegistry satisfies it.
type RegistryLike interface {
	Register(channelType string, adapter channels.ChannelAdapter) error
}

// Register builds an adapter from cfg and registers it under "telegram" in the
// supplied registry. Returns the adapter so callers can later release any
// resources or call SendOutgoing in tests.
func Register(reg RegistryLike, cfg Config) (*Adapter, error) {
	if reg == nil {
		return nil, errors.New("registry must be non-nil")
	}
	adapter := New(cfg, nil)
	if err := reg.Register("telegram", adapter); err != nil {
		return nil, err
	}
	return adapter, nil
}
