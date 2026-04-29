package telegram

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testBotID = "1234567890"

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err, "load fixture %s", name)
	return body
}

// ---------------------------------------------------------------------------
// channel_id / thread_id / message_id

func TestParseUpdate_TextYieldsTGChannelID(t *testing.T) {
	body := loadFixture(t, "update_text.json")
	msg, err := ParseUpdate(body, testBotID)
	require.NoError(t, err)
	assert.Equal(t, "tg:"+testBotID+":7741148933", msg.ChannelID)
	// thread_id is chat_id for non-forum messages.
	assert.Equal(t, "7741148933", msg.ThreadID)
}

func TestParseUpdate_ExtractsTextAndMessageID(t *testing.T) {
	body := loadFixture(t, "update_text.json")
	msg, err := ParseUpdate(body, testBotID)
	require.NoError(t, err)
	assert.Equal(t, "/start hello", msg.Text)
	assert.Equal(t, "873214500", msg.MessageID)
}

func TestParseUpdate_ExtractsCommand(t *testing.T) {
	body := loadFixture(t, "update_text.json")
	msg, err := ParseUpdate(body, testBotID)
	require.NoError(t, err)
	assert.Equal(t, "/start", msg.Payload["command"])
}

func TestParseUpdate_ForumTopicAppendsTopicToThread(t *testing.T) {
	body := loadFixture(t, "update_forum_topic.json")
	msg, err := ParseUpdate(body, testBotID)
	require.NoError(t, err)
	assert.Equal(t, "tg:"+testBotID+":-1001234567890", msg.ChannelID)
	assert.Equal(t, "-1001234567890:17", msg.ThreadID)
}

func TestParseUpdate_PhotoYieldsAttachments(t *testing.T) {
	body := loadFixture(t, "update_photo.json")
	msg, err := ParseUpdate(body, testBotID)
	require.NoError(t, err)
	assert.Equal(t, "look at this", msg.Text)

	var photoAtts []map[string]any
	for _, a := range msg.Attachments {
		if k, _ := a["kind"].(string); k == "photo" {
			photoAtts = append(photoAtts, a)
		}
	}
	require.Len(t, photoAtts, 1)
	photo := photoAtts[0]
	sizes, ok := photo["sizes"].([]map[string]any)
	require.True(t, ok)
	assert.Len(t, sizes, 2)
	// file_id is the largest size's id.
	assert.Equal(t, "AgACAgIAAxkBAAIE-photo-largest", photo["file_id"])
}

// ---------------------------------------------------------------------------
// sender / received_at

func TestParseUpdate_SenderIDFromFromID(t *testing.T) {
	body := loadFixture(t, "update_text.json")
	msg, err := ParseUpdate(body, testBotID)
	require.NoError(t, err)
	assert.Equal(t, "7741148933", msg.SenderID)
}

func TestParseUpdate_ReceivedAtISO8601DerivedFromDate(t *testing.T) {
	body := loadFixture(t, "update_text.json")
	msg, err := ParseUpdate(body, testBotID)
	require.NoError(t, err)
	require.NotEmpty(t, msg.ReceivedAt)
	parsed, err := time.Parse(time.RFC3339, msg.ReceivedAt)
	require.NoError(t, err)
	assert.Equal(t, time.Date(2024, 12, 24, 0, 26, 40, 0, time.UTC), parsed.UTC())
}

// ---------------------------------------------------------------------------
// rejection paths

func TestParseUpdate_InvalidJSONRaises(t *testing.T) {
	_, err := ParseUpdate([]byte("not-json"), testBotID)
	assert.Error(t, err)
}

func TestParseUpdate_MissingMessageRaises(t *testing.T) {
	_, err := ParseUpdate([]byte(`{"update_id": 1}`), testBotID)
	assert.Error(t, err)
}

func TestParseUpdate_MissingUpdateIDRaises(t *testing.T) {
	body := []byte(`{"message": {"chat": {"id": 1}, "date": 1}}`)
	_, err := ParseUpdate(body, testBotID)
	assert.Error(t, err)
}

func TestParseUpdate_TopicIDOnlyWhenIsTopicMessageSet(t *testing.T) {
	// Telegram sets message_thread_id for plain replies too; we must only
	// treat it as a forum topic when is_topic_message is true.
	body := []byte(`{"update_id": 9, "message": {"message_id": 1, "message_thread_id": 17, "chat": {"id": 99, "type": "supergroup"}, "date": 1, "text": "hi"}}`)
	msg, err := ParseUpdate(body, testBotID)
	require.NoError(t, err)
	assert.Equal(t, "99", msg.ThreadID)
}

// ---------------------------------------------------------------------------
// chunking

func TestChunkText_8000CharsChunksUnderLimit(t *testing.T) {
	raw := ""
	for i := 0; i < 800; i++ {
		raw += "abcdefghij"
	}
	chunks, err := ChunkText(raw, 4096)
	require.NoError(t, err)
	totalLen := 0
	for _, c := range chunks {
		assert.LessOrEqual(t, len(c), 4096)
		totalLen += len(c)
	}
	assert.Equal(t, len(raw), totalLen)
	assert.GreaterOrEqual(t, len(chunks), 2)
}

func TestChunkText_PrefersParagraphBoundaries(t *testing.T) {
	filler := ""
	for i := 0; i < 100; i++ {
		filler += "filler "
	}
	text := "para one.\n\n" + filler + "\n\npara three."
	chunks, err := ChunkText(text, 200)
	require.NoError(t, err)
	for _, c := range chunks {
		assert.LessOrEqual(t, len(c), 200)
	}
	require.NotEmpty(t, chunks)
	// The first chunk should still start with the first paragraph and the
	// last chunk should still end on the last paragraph.
	assert.Contains(t, chunks[0], "para one.")
	assert.Contains(t, chunks[len(chunks)-1], "para three.")
}

func TestChunkText_ShortReturnsSingleton(t *testing.T) {
	chunks, err := ChunkText("hi", 4096)
	require.NoError(t, err)
	assert.Equal(t, []string{"hi"}, chunks)
}

func TestChunkText_EmptyReturnsEmpty(t *testing.T) {
	chunks, err := ChunkText("", 4096)
	require.NoError(t, err)
	assert.Empty(t, chunks)
}

func TestChunkText_ZeroLimitErrors(t *testing.T) {
	_, err := ChunkText("x", 0)
	assert.Error(t, err)
}
