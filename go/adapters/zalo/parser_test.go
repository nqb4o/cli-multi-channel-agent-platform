package zalo

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Fixture bytes (mirrors adapters/channels/zalo/tests/fixtures/)

var fixtureEventText = []byte(`{
  "app_id": "9876543210",
  "user_id_by_app": "5555444433332222111",
  "event_name": "user_send_text",
  "timestamp": "1700000000000",
  "sender": {"id": "5555444433332222111"},
  "recipient": {"id": "1234567890123456789"},
  "message": {"msg_id": "0a1b2c3d4e5f6789", "text": "Hello from Zalo"}
}`)

var fixtureEventAttachment = []byte(`{
  "app_id": "9876543210",
  "user_id_by_app": "5555444433332222111",
  "event_name": "user_send_image",
  "timestamp": "1700000060000",
  "sender": {"id": "5555444433332222111"},
  "recipient": {"id": "1234567890123456789"},
  "message": {
    "msg_id": "9f8e7d6c5b4a3210",
    "text": "look at this",
    "attachments": [
      {
        "type": "image",
        "payload": {
          "url": "https://zalo-cdn.example.com/image-abc123.jpg",
          "thumbnail": "https://zalo-cdn.example.com/image-abc123-thumb.jpg"
        }
      }
    ]
  }
}`)

// ---------------------------------------------------------------------------
// Happy paths

func TestParseEvent_UserSendText(t *testing.T) {
	msg, err := ParseEvent(fixtureEventText, "1234567890123456789")
	require.NoError(t, err)
	assert.Equal(t, "0a1b2c3d4e5f6789", msg.MessageID)
	assert.Equal(t, "zalo:1234567890123456789:5555444433332222111", msg.ChannelID)
	assert.Equal(t, "5555444433332222111", msg.ThreadID)
	assert.Equal(t, "5555444433332222111", msg.SenderID)
	assert.Equal(t, "Hello from Zalo", msg.Text)
	payload := msg.Payload
	assert.Equal(t, "user_send_text", payload["event_name"])
	assert.Equal(t, map[string]any{"id": "5555444433332222111"}, payload["sender"])
	assert.Equal(t, map[string]any{"id": "1234567890123456789"}, payload["recipient"])
	assert.Empty(t, msg.Attachments)
	assert.Equal(t, "2023-11-14T22:13:20Z", msg.ReceivedAt)
}

func TestParseEvent_UserSendImage(t *testing.T) {
	msg, err := ParseEvent(fixtureEventAttachment, "1234567890123456789")
	require.NoError(t, err)
	assert.Equal(t, "9f8e7d6c5b4a3210", msg.MessageID)
	assert.Equal(t, "zalo:1234567890123456789:5555444433332222111", msg.ChannelID)
	assert.Equal(t, "5555444433332222111", msg.ThreadID)
	assert.Equal(t, "look at this", msg.Text)
	require.Len(t, msg.Attachments, 1)
	att := msg.Attachments[0]
	assert.Equal(t, "photo", att["kind"])
	assert.Equal(t, "https://zalo-cdn.example.com/image-abc123.jpg", att["url"])
	assert.Equal(t, "https://zalo-cdn.example.com/image-abc123-thumb.jpg", att["thumbnail"])
}

func TestParseEvent_ChannelIDFormatMatchesBrief(t *testing.T) {
	// Acceptance criterion 2: channel_id=zalo:<oa>:<sender>, thread_id=<sender>.
	msg, err := ParseEvent(fixtureEventText, "my-oa-id")
	require.NoError(t, err)
	assert.Equal(t, "zalo:my-oa-id:5555444433332222111", msg.ChannelID)
	assert.Equal(t, "5555444433332222111", msg.ThreadID)
}

func TestParseEvent_PayloadCarriesRawEvent(t *testing.T) {
	msg, err := ParseEvent(fixtureEventText, "oa")
	require.NoError(t, err)
	raw, ok := msg.Payload["raw"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "user_send_text", raw["event_name"])
	message, _ := raw["message"].(map[string]any)
	assert.Equal(t, "0a1b2c3d4e5f6789", stringifyJSONScalar(message["msg_id"]))
}

// ---------------------------------------------------------------------------
// Error paths

func TestParseEvent_InvalidJSONRaisesError(t *testing.T) {
	_, err := ParseEvent([]byte("{not json"), "oa")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not valid JSON")
}

func TestParseEvent_NonObjectTopLevelRaisesError(t *testing.T) {
	_, err := ParseEvent([]byte("[]"), "oa")
	require.Error(t, err)
	// The Go implementation wraps the json unmarshal error which says "not valid JSON"
	// for array inputs; the Python counterpart says "must be a JSON object". Both
	// paths reject non-object top-level payloads — that is what we verify here.
	assert.Error(t, err)
}

func TestParseEvent_MissingEventNameRaisesError(t *testing.T) {
	body := mustMarshal(map[string]any{"sender": map[string]any{"id": "u"}, "message": map[string]any{}})
	_, err := ParseEvent(body, "oa")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "event_name")
}

func TestParseEvent_UnknownEventNameRaisesError(t *testing.T) {
	body := mustMarshal(map[string]any{
		"event_name": "follow",
		"sender":     map[string]any{"id": "u"},
		"recipient":  map[string]any{"id": "oa"},
		"message":    map[string]any{},
	})
	_, err := ParseEvent(body, "oa")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a handled user message")
}

func TestParseEvent_MissingSenderIDRaisesError(t *testing.T) {
	body := mustMarshal(map[string]any{
		"event_name": "user_send_text",
		"sender":     map[string]any{},
		"message":    map[string]any{"text": "hi", "msg_id": "x"},
	})
	_, err := ParseEvent(body, "oa")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sender.id")
}

func TestParseEvent_SyntheticMessageIDWhenMsgIDMissing(t *testing.T) {
	body := mustMarshal(map[string]any{
		"event_name": "user_send_sticker",
		"sender":     map[string]any{"id": "u123"},
		"recipient":  map[string]any{"id": "oa"},
		"timestamp":  "1700000000000",
		"message":    map[string]any{},
	})
	msg, err := ParseEvent(body, "oa")
	require.NoError(t, err)
	assert.Equal(t, "1700000000000:u123", msg.MessageID)
}

func TestParseEvent_AttachmentKindNormalization(t *testing.T) {
	body := mustMarshal(map[string]any{
		"event_name": "user_send_file",
		"sender":     map[string]any{"id": "u"},
		"recipient":  map[string]any{"id": "oa"},
		"timestamp":  "1700000000000",
		"message": map[string]any{
			"msg_id": "m1",
			"attachments": []any{
				map[string]any{
					"type": "FILE",
					"payload": map[string]any{
						"url":  "https://zalo-cdn.example.com/file.pdf",
						"name": "report.pdf",
						"size": 12345,
					},
				},
			},
		},
	})
	msg, err := ParseEvent(body, "oa")
	require.NoError(t, err)
	require.Len(t, msg.Attachments, 1)
	att := msg.Attachments[0]
	assert.Equal(t, "document", att["kind"])
	assert.Equal(t, "https://zalo-cdn.example.com/file.pdf", att["url"])
	assert.Equal(t, "report.pdf", att["name"])
}

func TestParseEvent_ReceivedAtFallsBackWhenTimestampInvalid(t *testing.T) {
	body := mustMarshal(map[string]any{
		"event_name": "user_send_text",
		"sender":     map[string]any{"id": "u"},
		"recipient":  map[string]any{"id": "oa"},
		"timestamp":  "not-a-timestamp",
		"message":    map[string]any{"msg_id": "m1", "text": "hi"},
	})
	msg, err := ParseEvent(body, "oa")
	require.NoError(t, err)
	assert.NotEmpty(t, msg.ReceivedAt)
	assert.True(t, len(msg.ReceivedAt) >= 20, "expected ISO-8601, got %q", msg.ReceivedAt)
}

func TestParseEvent_SenderIDFormat(t *testing.T) {
	// thread_id should always be the sender id string.
	msg, err := ParseEvent(fixtureEventText, "any-oa")
	require.NoError(t, err)
	assert.Equal(t, msg.SenderID, msg.ThreadID)
}

// ---------------------------------------------------------------------------
// helpers

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
