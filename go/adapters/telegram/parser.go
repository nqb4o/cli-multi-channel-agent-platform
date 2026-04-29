package telegram

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/openclaw/agent-platform/internal/gateway/channels"
)

// messageKeys ranks the Telegram update fields that carry a message we should
// surface to the runtime. The first match wins.
var messageKeys = []string{"message", "edited_message", "channel_post", "edited_channel_post"}

// ParseUpdate decodes a Telegram webhook body into a NormalizedMessage.
//
// channel_id format: "tg:<bot_id>:<chat_id>".
// thread_id is "<chat_id>" for plain chats, "<chat_id>:<topic_id>" for forum-
// topic messages (when both message_thread_id is set and is_topic_message is
// true — Telegram sets message_thread_id for plain replies too).
//
// Returns an error with a non-nil ValueError-equivalent for malformed bodies;
// the gateway translates those into 400s.
func ParseUpdate(body []byte, botID string) (*channels.NormalizedMessage, error) {
	var update map[string]any
	if err := json.Unmarshal(body, &update); err != nil {
		return nil, fmt.Errorf("telegram update is not valid JSON: %w", err)
	}
	if update == nil {
		return nil, fmt.Errorf("telegram update must be a JSON object")
	}

	updateID, ok := numericInt64(update["update_id"])
	if !ok {
		return nil, fmt.Errorf("telegram update missing integer 'update_id'")
	}

	msg, msgKind := extractMessage(update)
	if msg == nil {
		return nil, fmt.Errorf("telegram update has no message variant we handle (expected one of %v)", messageKeys)
	}

	chat, _ := msg["chat"].(map[string]any)
	if chat == nil {
		return nil, fmt.Errorf("telegram message is missing 'chat'")
	}
	chatIDValue, hasChatID := chat["id"]
	if !hasChatID {
		return nil, fmt.Errorf("telegram message is missing 'chat.id'")
	}
	chatID := stringifyID(chatIDValue)
	if chatID == "" {
		return nil, fmt.Errorf("telegram message has empty 'chat.id'")
	}

	threadID := chatID
	if topicID := topicID(msg); topicID != "" {
		threadID = chatID + ":" + topicID
	}

	text := extractText(msg)
	senderID := senderID(msg)
	senderName := senderName(msg)
	attachments := extractAttachments(msg)
	cmd := extractCommand(text)

	receivedAt := isoUTCFromUnix(msg["date"])

	payload := map[string]any{
		"text":    nullableString(text),
		"photo":   filterByKind(attachments, "photo"),
		"command": nullableString(cmd),
		"kind":    msgKind,
		"sender":  map[string]any{"user_id": nullableString(senderID), "name": nullableString(senderName)},
		"chat": map[string]any{
			"id":       chatID,
			"type":     chat["type"],
			"title":    chat["title"],
			"username": chat["username"],
		},
		"raw": msg,
	}
	if audio := firstByKind(attachments, "audio"); audio != nil {
		payload["audio"] = audio
	}

	return &channels.NormalizedMessage{
		MessageID:   strconv.FormatInt(updateID, 10),
		ChannelID:   fmt.Sprintf("tg:%s:%s", botID, chatID),
		ThreadID:    threadID,
		Text:        text,
		Payload:     payload,
		SenderID:    senderID,
		Attachments: attachments,
		ReceivedAt:  receivedAt,
	}, nil
}

// ---------------------------------------------------------------------------
// helpers

func extractMessage(update map[string]any) (map[string]any, string) {
	for _, key := range messageKeys {
		if candidate, ok := update[key].(map[string]any); ok {
			return candidate, key
		}
	}
	return nil, ""
}

// topicID returns the forum topic id only when this is actually a forum-topic
// message. Telegram sets message_thread_id on plain replies too, which is why
// we additionally check is_topic_message.
func topicID(msg map[string]any) string {
	tid, ok := numericInt64(msg["message_thread_id"])
	if !ok {
		return ""
	}
	if isTopic, _ := msg["is_topic_message"].(bool); !isTopic {
		return ""
	}
	return strconv.FormatInt(tid, 10)
}

func extractText(msg map[string]any) string {
	if t, ok := msg["text"].(string); ok && t != "" {
		return t
	}
	if c, ok := msg["caption"].(string); ok && c != "" {
		return c
	}
	return ""
}

func senderID(msg map[string]any) string {
	if user, ok := msg["from"].(map[string]any); ok {
		if uid, ok := numericInt64(user["id"]); ok {
			return strconv.FormatInt(uid, 10)
		}
	}
	if sc, ok := msg["sender_chat"].(map[string]any); ok {
		if cid, ok := numericInt64(sc["id"]); ok {
			return strconv.FormatInt(cid, 10)
		}
	}
	return ""
}

func senderName(msg map[string]any) string {
	if user, ok := msg["from"].(map[string]any); ok {
		first, _ := user["first_name"].(string)
		last, _ := user["last_name"].(string)
		full := strings.TrimSpace(strings.Join(joinNonEmpty(first, last), " "))
		if full != "" {
			return full
		}
		if u, ok := user["username"].(string); ok && u != "" {
			return u
		}
	}
	if sc, ok := msg["sender_chat"].(map[string]any); ok {
		if title, ok := sc["title"].(string); ok && title != "" {
			return title
		}
	}
	return ""
}

func joinNonEmpty(parts ...string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// extractAttachments returns the per-message attachments list. Photos surface
// as a single record carrying every PhotoSize so callers can pick a resolution.
func extractAttachments(msg map[string]any) []map[string]any {
	var out []map[string]any

	if rawPhotos, ok := msg["photo"].([]any); ok && len(rawPhotos) > 0 {
		var sizes []map[string]any
		for _, item := range rawPhotos {
			size, ok := item.(map[string]any)
			if !ok {
				continue
			}
			sizes = append(sizes, map[string]any{
				"file_id":        size["file_id"],
				"file_unique_id": size["file_unique_id"],
				"width":          size["width"],
				"height":         size["height"],
				"file_size":      size["file_size"],
			})
		}
		if len(sizes) > 0 {
			out = append(out, map[string]any{
				"kind":    "photo",
				"mime":    "image/jpeg",
				"sizes":   sizes,
				"file_id": sizes[len(sizes)-1]["file_id"], // largest
			})
		}
	}

	type kindMime struct {
		kind, mime string
	}
	for _, km := range []kindMime{
		{"audio", ""}, {"voice", "audio/ogg"}, {"video", ""}, {"document", ""}, {"animation", ""},
	} {
		item, ok := msg[km.kind].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := item["file_id"]; !ok {
			continue
		}
		entry := map[string]any{
			"kind":           km.kind,
			"file_id":        item["file_id"],
			"file_unique_id": item["file_unique_id"],
			"file_size":      item["file_size"],
		}
		if mime, ok := item["mime_type"].(string); ok && mime != "" {
			entry["mime"] = mime
		} else if km.mime != "" {
			entry["mime"] = km.mime
		} else {
			entry["mime"] = nil
		}
		if name, ok := item["file_name"].(string); ok && name != "" {
			entry["file_name"] = name
		}
		if dur, ok := item["duration"]; ok && dur != nil {
			entry["duration"] = dur
		}
		out = append(out, entry)
	}

	return out
}

func extractCommand(text string) string {
	if text == "" || !strings.HasPrefix(text, "/") {
		return ""
	}
	head := text
	if i := strings.IndexAny(head, " \t\n"); i >= 0 {
		head = head[:i]
	}
	if i := strings.IndexByte(head, '@'); i >= 0 {
		head = head[:i]
	}
	if len(head) <= 1 {
		return ""
	}
	return head
}

// ---------------------------------------------------------------------------
// json number / id coercion helpers

// numericInt64 accepts int / int64 / float64 (encoding/json default) values.
func numericInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case float64:
		// JSON numbers without a fractional part round-trip cleanly.
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return i, true
		}
		// Some IDs may not survive Int64(); fall back to string parse.
		if i, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
			return i, true
		}
	}
	return 0, false
}

// stringifyID renders a JSON-decoded chat-id (possibly a float64) without
// scientific notation. Telegram chat ids can be negative supergroups like
// -1001234567890 which are exact in float64 but format ugly otherwise.
func stringifyID(v any) string {
	switch n := v.(type) {
	case string:
		return n
	case int:
		return strconv.FormatInt(int64(n), 10)
	case int64:
		return strconv.FormatInt(n, 10)
	case float64:
		if n == float64(int64(n)) {
			return strconv.FormatInt(int64(n), 10)
		}
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return ""
}

func isoUTCFromUnix(v any) string {
	if ts, ok := numericInt64(v); ok {
		return time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05Z")
	}
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func filterByKind(items []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, it := range items {
		if k, _ := it["kind"].(string); k == kind {
			out = append(out, it)
		}
	}
	if out == nil {
		return []map[string]any{}
	}
	return out
}

func firstByKind(items []map[string]any, kind string) map[string]any {
	for _, it := range items {
		if k, _ := it["kind"].(string); k == kind {
			return it
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// chunking

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

// refineOversized walks chunks and re-splits any that exceed limit. An empty
// sep means "hard slice at the limit boundary".
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
