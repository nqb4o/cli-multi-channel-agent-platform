// Package channels declares the channel adapter contract that gateway routes
// look up at request time. F07 (Telegram) and F08 (Zalo) implement this and
// register at gateway startup; the gateway itself never imports those packages.
package channels

import (
	"context"
	"net/http"
)

// NormalizedMessage is the channel-agnostic representation of one inbound user
// message. Mirrors the Python NormalizedMessage dataclass — field set is part
// of the cross-package contract.
type NormalizedMessage struct {
	// MessageID is the channel-supplied idempotency key (Telegram update_id,
	// Zalo event id, …).
	MessageID string

	// ChannelID is the stable identifier the gateway uses to look up
	// (user_id, agent_id) in the DB. Adapter chooses what this is — typically
	// the bot/account identifier.
	ChannelID string

	// ThreadID is the conversation/thread identifier within the channel
	// (Telegram chat_id, Zalo conversation id, …).
	ThreadID string

	// Text is an optional plain-text payload (empty for media-only msgs).
	Text string

	// Payload is the adapter-defined structured payload forwarded to the
	// agent runtime. The shape is opaque to F06.
	Payload map[string]any

	// SenderID is the optional source user identifier on the channel.
	SenderID string

	// Attachments is an optional list of attachment descriptors.
	Attachments []map[string]any

	// ReceivedAt is an ISO-8601 string set by the adapter (or by gateway as a
	// fallback).
	ReceivedAt string
}

// ChannelAdapter is the contract implemented by F07/F08 channel adapters.
//
// Adapters are stateless from the gateway's perspective — they may hold their
// own outbound HTTP client, but the gateway treats them as pure functions for
// signature verify + parse, and async fire-and-forget for SendOutgoing.
type ChannelAdapter interface {
	// Type is the stable channel type id, e.g. "telegram", "zalo". Used as the
	// URL path segment in POST /channels/{type}/webhook.
	Type() string

	// VerifySignature returns true iff the request is authentic. F06 returns
	// 401 on false.
	VerifySignature(headers http.Header, body []byte) bool

	// ParseIncoming decodes the raw webhook body into a NormalizedMessage.
	// Adapters MUST return a non-nil error for malformed payloads — the
	// gateway turns those into 400 responses.
	ParseIncoming(body []byte) (*NormalizedMessage, error)

	// SendOutgoing delivers an outbound message back to the channel. F06 does
	// not call this in MVP-0 (the runtime/orchestrator owns reply delivery),
	// but the contract lives here so adapters ship one cohesive interface.
	SendOutgoing(ctx context.Context, channelID, threadID, text string, opts map[string]any) error
}
