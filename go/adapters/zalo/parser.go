package zalo

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/openclaw/agent-platform/internal/gateway/channels"
)

// userMessageEvents is the set of Zalo OA event_name values that carry a
// user-originated message we should forward. Other event names (follow,
// unfollow, oa_send_*, …) are explicitly out of scope here.
var userMessageEvents = map[string]struct{}{
	"user_send_text":     {},
	"user_send_image":    {},
	"user_send_audio":    {},
	"user_send_video":    {},
	"user_send_file":     {},
	"user_send_link":     {},
	"user_send_sticker":  {},
	"user_send_gif":      {},
	"user_send_location": {},
}

// ParseEvent decodes a Zalo OA webhook body into a NormalizedMessage.
//
// channel_id format: "zalo:<oa_id>:<sender_id>".
// thread_id is the sender id (Zalo OA conversations are 1:1 between an end
// user and the OA).
//
// MessageID prefers message.msg_id; falls back to "<timestamp>:<sender_id>" so
// the gateway always has a deterministic idempotency key.
func ParseEvent(body []byte, oaID string) (*channels.NormalizedMessage, error) {
	var event map[string]any
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&event); err != nil {
		return nil, fmt.Errorf("zalo event is not valid JSON: %w", err)
	}
	if event == nil {
		return nil, fmt.Errorf("zalo event must be a JSON object")
	}
	// json.NewDecoder may decode []/null into non-map; guard the obvious
	// non-object cases.
	if _, ok := any(event).(map[string]any); !ok {
		return nil, fmt.Errorf("zalo event must be a JSON object")
	}

	eventName, _ := event["event_name"].(string)
	if eventName == "" {
		return nil, fmt.Errorf("zalo event missing 'event_name'")
	}
	if _, ok := userMessageEvents[eventName]; !ok {
		return nil, fmt.Errorf("zalo event_name %q is not a handled user message", eventName)
	}

	sender, _ := event["sender"].(map[string]any)
	if sender == nil {
		return nil, fmt.Errorf("zalo event missing 'sender'")
	}
	rawSenderID, hasSenderID := sender["id"]
	if !hasSenderID {
		return nil, fmt.Errorf("zalo event missing 'sender.id'")
	}
	senderID := stringifyJSONScalar(rawSenderID)
	if senderID == "" {
		return nil, fmt.Errorf("zalo event 'sender.id' is empty")
	}

	var recipientID string
	if rec, ok := event["recipient"].(map[string]any); ok {
		if rid, ok := rec["id"]; ok {
			recipientID = stringifyJSONScalar(rid)
		}
	}

	message, _ := event["message"].(map[string]any)
	if message == nil {
		message = map[string]any{}
	}

	text := extractText(message)
	attachments := extractAttachments(message)
	msgID, err := extractMsgID(message, event)
	if err != nil {
		return nil, err
	}

	receivedAt := parseTimestamp(event["timestamp"])

	payload := map[string]any{
		"text":        nullableString(text),
		"event_name":  eventName,
		"sender":      map[string]any{"id": senderID},
		"attachments": attachments,
		"raw":         event,
	}
	if recipientID != "" {
		payload["recipient"] = map[string]any{"id": recipientID}
	} else {
		payload["recipient"] = nil
	}

	return &channels.NormalizedMessage{
		MessageID:   msgID,
		ChannelID:   fmt.Sprintf("zalo:%s:%s", oaID, senderID),
		ThreadID:    senderID,
		Text:        text,
		Payload:     payload,
		SenderID:    senderID,
		Attachments: attachments,
		ReceivedAt:  receivedAt,
	}, nil
}

// ---------------------------------------------------------------------------
// helpers

func extractText(message map[string]any) string {
	if t, ok := message["text"].(string); ok && t != "" {
		return t
	}
	return ""
}

func extractAttachments(message map[string]any) []map[string]any {
	out := []map[string]any{}
	raw, ok := message["attachments"].([]any)
	if !ok {
		return out
	}
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		kind, ok := entry["type"].(string)
		if !ok {
			continue
		}
		payload, _ := entry["payload"].(map[string]any)
		if payload == nil {
			payload = map[string]any{}
		}
		descriptor := map[string]any{
			"kind": normalizeKind(kind),
		}
		if url, ok := payload["url"].(string); ok && url != "" {
			descriptor["url"] = url
		}
		if thumb, ok := payload["thumbnail"].(string); ok && thumb != "" {
			descriptor["thumbnail"] = thumb
		}
		if desc, ok := payload["description"].(string); ok && desc != "" {
			descriptor["description"] = desc
		}
		for _, key := range []string{"name", "size", "duration", "lat", "long"} {
			if value, ok := payload[key]; ok && value != nil {
				descriptor[key] = unwrapJSONNumber(value)
			}
		}
		descriptor["raw"] = entry
		out = append(out, descriptor)
	}
	return out
}

func normalizeKind(zaloKind string) string {
	low := strings.ToLower(zaloKind)
	switch low {
	case "image", "photo":
		return "photo"
	case "audio", "voice":
		return "audio"
	case "video":
		return "video"
	case "file", "document":
		return "document"
	case "link":
		return "link"
	case "sticker":
		return "sticker"
	case "gif":
		return "gif"
	case "location":
		return "location"
	default:
		return low
	}
}

func extractMsgID(message map[string]any, event map[string]any) (string, error) {
	if v, ok := message["msg_id"]; ok && v != nil {
		s := stringifyJSONScalar(v)
		if s != "" {
			return s, nil
		}
	}
	timestamp := event["timestamp"]
	timestampStr := stringifyJSONScalar(timestamp)
	if timestampStr == "" {
		return "", fmt.Errorf("zalo event missing both message.msg_id and timestamp")
	}
	sender, _ := event["sender"].(map[string]any)
	senderID := ""
	if sender != nil {
		if v, ok := sender["id"]; ok {
			senderID = stringifyJSONScalar(v)
		}
	}
	return fmt.Sprintf("%s:%s", timestampStr, senderID), nil
}

func parseTimestamp(raw any) string {
	var ms int64
	switch v := raw.(type) {
	case json.Number:
		s := v.String()
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			ms = i
		} else {
			return time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
		}
	case string:
		if !allDigitsZ(v) {
			return time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
		}
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			ms = i
		} else {
			return time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
		}
	case int:
		ms = int64(v)
	case int64:
		ms = v
	case float64:
		ms = int64(v)
	default:
		return time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	}
	seconds := ms / 1000
	nanos := (ms % 1000) * int64(time.Millisecond)
	t := time.Unix(seconds, nanos).UTC()
	return t.Format("2006-01-02T15:04:05Z")
}

func allDigitsZ(s string) bool {
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

// stringifyJSONScalar renders a json.Number / string / int / bool into a
// canonical string. Returns "" for nil or unsupported types.
func stringifyJSONScalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case int:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	return ""
}

func unwrapJSONNumber(v any) any {
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return i
		}
		if f, err := n.Float64(); err == nil {
			return f
		}
		return n.String()
	}
	return v
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------------------------------------------------------------------------
// chunking — same algorithm as the Telegram adapter; kept independent so the
// two channel packages don't pull each other in.

// ChunkText splits text into <= limit-char chunks, preferring (in order)
// paragraph (\n\n), newline, sentence ('. '), and finally a hard slice when no
// graceful boundary remains. Returns an empty slice for empty text.
func ChunkText(text string, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("limit must be > 0")
	}
	if text == "" {
		return []string{}, nil
	}
	if len(text) <= limit {
		return []string{text}, nil
	}
	chunks := splitWithSeparator(text, limit, "\n\n")
	chunks = refineOversized(chunks, limit, "\n")
	chunks = refineOversized(chunks, limit, ". ")
	chunks = refineOversized(chunks, limit, "")
	return chunks, nil
}

func splitWithSeparator(text string, limit int, sep string) []string {
	parts := strings.Split(text, sep)
	var out []string
	buf := ""
	for _, part := range parts {
		var candidate string
		if buf == "" {
			candidate = part
		} else {
			candidate = buf + sep + part
		}
		if len(candidate) <= limit {
			buf = candidate
			continue
		}
		if buf != "" {
			out = append(out, buf)
		}
		buf = part
	}
	if buf != "" {
		out = append(out, buf)
	}
	return out
}

func refineOversized(chunks []string, limit int, sep string) []string {
	var refined []string
	for _, chunk := range chunks {
		if len(chunk) <= limit {
			refined = append(refined, chunk)
			continue
		}
		if sep == "" {
			for i := 0; i < len(chunk); i += limit {
				end := i + limit
				if end > len(chunk) {
					end = len(chunk)
				}
				refined = append(refined, chunk[i:end])
			}
			continue
		}
		refined = append(refined, splitWithSeparator(chunk, limit, sep)...)
	}
	return refined
}
