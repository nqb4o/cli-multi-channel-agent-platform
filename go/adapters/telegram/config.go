// Package telegram implements F07 — the Telegram Bot API channel adapter for
// the gateway's channel registry.
//
// Ported from adapters/channels/telegram/src/channel_telegram (Python). The
// public surface mirrors the Go ChannelAdapter contract in
// internal/gateway/channels: VerifySignature + ParseIncoming + SendOutgoing.
package telegram

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config carries the static per-bot Telegram configuration.
type Config struct {
	// BotToken is the @BotFather-issued token. Format: "<bot_id>:<secret>".
	BotToken string
	// WebhookSecret is the value Telegram echoes in the
	// X-Telegram-Bot-Api-Secret-Token header on every webhook delivery.
	WebhookSecret string
	// BotID is the numeric prefix of BotToken; used in the channel_id.
	BotID string
	// APIBase overrides the public Bot API base URL. Tests inject local URLs.
	APIBase string
	// CommandPrefix is an optional command prefix surfaced to the runtime.
	CommandPrefix string
	// TextChunkLimit is the outbound chunk size (Telegram's hard limit is 4096).
	TextChunkLimit int
	// RequestTimeoutSeconds sets the outbound HTTP timeout.
	RequestTimeoutSeconds float64
}

// DefaultAPIBase is the public Bot API endpoint.
const DefaultAPIBase = "https://api.telegram.org"

// DefaultTextChunkLimit matches Telegram's documented sendMessage limit.
const DefaultTextChunkLimit = 4096

// DefaultRequestTimeoutSeconds is the per-request HTTP timeout.
const DefaultRequestTimeoutSeconds = 30.0

// FromEnv builds a Config from environment variables.
//
// Required:
//
//	TELEGRAM_BOT_TOKEN
//	TELEGRAM_WEBHOOK_SECRET
//
// Optional:
//
//	TELEGRAM_API_BASE         (default: https://api.telegram.org)
//	TELEGRAM_COMMAND_PREFIX   (default: unset)
//	TELEGRAM_TEXT_CHUNK_LIMIT (default: 4096; must be in (0, 4096])
func FromEnv(env map[string]string) (Config, error) {
	if env == nil {
		env = osEnvMap()
	}

	token := strings.TrimSpace(env["TELEGRAM_BOT_TOKEN"])
	secret := strings.TrimSpace(env["TELEGRAM_WEBHOOK_SECRET"])
	if token == "" {
		return Config{}, errors.New("TELEGRAM_BOT_TOKEN must be set")
	}
	if secret == "" {
		return Config{}, errors.New("TELEGRAM_WEBHOOK_SECRET must be set")
	}

	parts := strings.SplitN(token, ":", 2)
	botID := parts[0]
	if botID == "" || !allDigits(botID) {
		return Config{}, errors.New(
			"TELEGRAM_BOT_TOKEN must look like '<bot_id>:<secret>' with a numeric bot id prefix",
		)
	}

	apiBase := strings.TrimRight(envOr(env, "TELEGRAM_API_BASE", DefaultAPIBase), "/")
	prefix := env["TELEGRAM_COMMAND_PREFIX"]

	chunkLimit := DefaultTextChunkLimit
	if raw := env["TELEGRAM_TEXT_CHUNK_LIMIT"]; raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("TELEGRAM_TEXT_CHUNK_LIMIT must be an integer: %w", err)
		}
		chunkLimit = v
	}
	if chunkLimit <= 0 || chunkLimit > DefaultTextChunkLimit {
		return Config{}, fmt.Errorf("TELEGRAM_TEXT_CHUNK_LIMIT must be in (0, %d]", DefaultTextChunkLimit)
	}

	return Config{
		BotToken:              token,
		WebhookSecret:         secret,
		BotID:                 botID,
		APIBase:               apiBase,
		CommandPrefix:         prefix,
		TextChunkLimit:        chunkLimit,
		RequestTimeoutSeconds: DefaultRequestTimeoutSeconds,
	}, nil
}

// withDefaults applies missing optional fields to keep callers terse.
func (c Config) withDefaults() Config {
	if c.APIBase == "" {
		c.APIBase = DefaultAPIBase
	}
	if c.TextChunkLimit == 0 {
		c.TextChunkLimit = DefaultTextChunkLimit
	}
	if c.RequestTimeoutSeconds == 0 {
		c.RequestTimeoutSeconds = DefaultRequestTimeoutSeconds
	}
	return c
}

func envOr(env map[string]string, key, def string) string {
	if v, ok := env[key]; ok && v != "" {
		return v
	}
	return def
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func osEnvMap() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		out[kv[:i]] = kv[i+1:]
	}
	return out
}
