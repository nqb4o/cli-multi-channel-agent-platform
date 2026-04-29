package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openclaw/agent-platform/internal/gateway/channels"
)

// ---------------------------------------------------------------------------
// helpers

const testWebhookSecret = "test-webhook-secret-token"

func newTestConfig(apiBase string) Config {
	return Config{
		BotToken:       testBotID + ":AAFAKE-bot-token-for-tests",
		WebhookSecret:  testWebhookSecret,
		BotID:          testBotID,
		APIBase:        apiBase,
		TextChunkLimit: 4096,
	}
}

// botAPIServer collects the requests made against /bot<token>/<method>. Each
// call hands back the chosen response (defaulting to {ok: true, result: {}}).
type botAPIServer struct {
	t        *testing.T
	mu       sync.Mutex
	calls    map[string][]*recordedCall
	override map[string]http.HandlerFunc
}

type recordedCall struct {
	Method      string
	URL         string
	Body        []byte
	ContentType string
}

func newBotAPIServer(t *testing.T) (*httptest.Server, *botAPIServer) {
	state := &botAPIServer{
		t:        t,
		calls:    map[string][]*recordedCall{},
		override: map[string]http.HandlerFunc{},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		// URL path is /bot<token>/<method>
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		method := parts[len(parts)-1]
		state.mu.Lock()
		state.calls[method] = append(state.calls[method], &recordedCall{
			Method:      method,
			URL:         r.URL.String(),
			Body:        body,
			ContentType: r.Header.Get("Content-Type"),
		})
		override := state.override[method]
		state.mu.Unlock()
		if override != nil {
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			override(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	return server, state
}

func (s *botAPIServer) Calls(method string) []*recordedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*recordedCall(nil), s.calls[method]...)
}

func (s *botAPIServer) SetHandler(method string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.override[method] = h
}

// ---------------------------------------------------------------------------
// VerifySignature

func TestVerifySignature_AcceptsCorrectSecret(t *testing.T) {
	adapter := New(newTestConfig(DefaultAPIBase), nil)
	headers := http.Header{}
	headers.Set("X-Telegram-Bot-Api-Secret-Token", testWebhookSecret)
	assert.True(t, adapter.VerifySignature(headers, []byte("{}")))
}

func TestVerifySignature_RejectsWrongSecret(t *testing.T) {
	adapter := New(newTestConfig(DefaultAPIBase), nil)
	headers := http.Header{}
	headers.Set("X-Telegram-Bot-Api-Secret-Token", "totally-wrong-token")
	assert.False(t, adapter.VerifySignature(headers, []byte("{}")))
}

func TestVerifySignature_RejectsMissingHeader(t *testing.T) {
	adapter := New(newTestConfig(DefaultAPIBase), nil)
	assert.False(t, adapter.VerifySignature(http.Header{}, []byte("{}")))
}

func TestVerifySignature_HeaderCaseInsensitive(t *testing.T) {
	adapter := New(newTestConfig(DefaultAPIBase), nil)
	// http.Header.Set canonicalises automatically, but a non-canonical
	// raw map (e.g. lowercased by an upstream proxy) must still verify.
	headers := http.Header{
		"x-telegram-bot-api-secret-token": []string{testWebhookSecret},
	}
	assert.True(t, adapter.VerifySignature(headers, []byte("{}")))
}

// ---------------------------------------------------------------------------
// ParseIncoming — smoke wraps the parser

func TestParseIncoming_Text(t *testing.T) {
	body := loadFixture(t, "update_text.json")
	adapter := New(newTestConfig(DefaultAPIBase), nil)
	msg, err := adapter.ParseIncoming(body)
	require.NoError(t, err)
	assert.Equal(t, "tg:"+testBotID+":7741148933", msg.ChannelID)
	assert.Equal(t, "7741148933", msg.ThreadID)
	assert.Equal(t, "/start hello", msg.Text)
}

func TestParseIncoming_Photo(t *testing.T) {
	body := loadFixture(t, "update_photo.json")
	adapter := New(newTestConfig(DefaultAPIBase), nil)
	msg, err := adapter.ParseIncoming(body)
	require.NoError(t, err)
	var photos []map[string]any
	for _, a := range msg.Attachments {
		if k, _ := a["kind"].(string); k == "photo" {
			photos = append(photos, a)
		}
	}
	assert.Len(t, photos, 1)
}

func TestParseIncoming_ForumTopic(t *testing.T) {
	body := loadFixture(t, "update_forum_topic.json")
	adapter := New(newTestConfig(DefaultAPIBase), nil)
	msg, err := adapter.ParseIncoming(body)
	require.NoError(t, err)
	assert.Equal(t, "-1001234567890:17", msg.ThreadID)
}

// ---------------------------------------------------------------------------
// SendOutgoing happy paths

func TestSendOutgoing_TextHitsSendMessage(t *testing.T) {
	server, state := newBotAPIServer(t)
	defer server.Close()
	cfg := newTestConfig(server.URL)
	adapter := New(cfg, server.Client())

	err := adapter.SendOutgoing(
		context.Background(),
		"tg:"+cfg.BotID+":7741148933",
		"7741148933",
		"Hello back!",
		nil,
	)
	require.NoError(t, err)
	calls := state.Calls("sendMessage")
	require.Len(t, calls, 1)
	var body map[string]any
	require.NoError(t, json.Unmarshal(calls[0].Body, &body))
	assert.Equal(t, "7741148933", body["chat_id"])
	assert.Equal(t, "Hello back!", body["text"])
	_, hasThread := body["message_thread_id"]
	assert.False(t, hasThread, "no forum topic → no message_thread_id")
}

func TestSendOutgoing_ForumTopicIncludesThreadID(t *testing.T) {
	server, state := newBotAPIServer(t)
	defer server.Close()
	cfg := newTestConfig(server.URL)
	adapter := New(cfg, server.Client())

	err := adapter.SendOutgoing(
		context.Background(),
		"tg:"+cfg.BotID+":-1001234567890",
		"-1001234567890:17",
		"topic reply",
		nil,
	)
	require.NoError(t, err)
	calls := state.Calls("sendMessage")
	require.Len(t, calls, 1)
	var body map[string]any
	require.NoError(t, json.Unmarshal(calls[0].Body, &body))
	assert.Equal(t, "-1001234567890", body["chat_id"])
	// json decodes numbers as float64 by default; compare numerically.
	assert.EqualValues(t, 17, body["message_thread_id"])
}

func TestSendOutgoing_SkipsWhenTextEmptyAndNoAttachments(t *testing.T) {
	server, state := newBotAPIServer(t)
	defer server.Close()
	cfg := newTestConfig(server.URL)
	adapter := New(cfg, server.Client())

	err := adapter.SendOutgoing(
		context.Background(), "tg:"+cfg.BotID+":1", "1", "", nil,
	)
	require.NoError(t, err)
	assert.Empty(t, state.Calls("sendMessage"))
}

func TestSendOutgoing_PhotoAttachment(t *testing.T) {
	server, state := newBotAPIServer(t)
	defer server.Close()
	cfg := newTestConfig(server.URL)
	adapter := New(cfg, server.Client())

	err := adapter.SendOutgoing(
		context.Background(),
		"tg:"+cfg.BotID+":1",
		"1",
		"caption follows",
		map[string]any{
			"attachments": []map[string]any{
				{"kind": "photo", "url": "https://x/y.jpg"},
			},
		},
	)
	require.NoError(t, err)
	assert.Len(t, state.Calls("sendPhoto"), 1)
	assert.Len(t, state.Calls("sendMessage"), 1)
}

func TestSendOutgoing_8000CharTextChunksCorrectly(t *testing.T) {
	server, state := newBotAPIServer(t)
	defer server.Close()
	cfg := newTestConfig(server.URL)
	adapter := New(cfg, server.Client())

	err := adapter.SendOutgoing(
		context.Background(), "tg:"+cfg.BotID+":1", "1",
		strings.Repeat("x", 8000), nil,
	)
	require.NoError(t, err)
	calls := state.Calls("sendMessage")
	require.GreaterOrEqual(t, len(calls), 2)
	for _, call := range calls {
		var body map[string]any
		require.NoError(t, json.Unmarshal(call.Body, &body))
		text, _ := body["text"].(string)
		assert.LessOrEqual(t, len(text), cfg.TextChunkLimit)
	}
}

func TestSendOutgoing_HugeTextUploadsAsDocument(t *testing.T) {
	server, state := newBotAPIServer(t)
	defer server.Close()
	cfg := newTestConfig(server.URL)
	adapter := New(cfg, server.Client())

	huge := strings.Repeat("z", 4096*8+1)
	err := adapter.SendOutgoing(
		context.Background(), "tg:"+cfg.BotID+":1", "1", huge, nil,
	)
	require.NoError(t, err)
	assert.Len(t, state.Calls("sendDocument"), 1)
	assert.Empty(t, state.Calls("sendMessage"))
}

func TestSendOutgoing_PassesParseModeWhenProvided(t *testing.T) {
	server, state := newBotAPIServer(t)
	defer server.Close()
	cfg := newTestConfig(server.URL)
	adapter := New(cfg, server.Client())

	err := adapter.SendOutgoing(
		context.Background(), "tg:"+cfg.BotID+":1", "1", "hello",
		map[string]any{"parse_mode": "MarkdownV2"},
	)
	require.NoError(t, err)
	calls := state.Calls("sendMessage")
	require.Len(t, calls, 1)
	var body map[string]any
	require.NoError(t, json.Unmarshal(calls[0].Body, &body))
	assert.Equal(t, "MarkdownV2", body["parse_mode"])
}

// ---------------------------------------------------------------------------
// SendOutgoing error paths

func TestSendOutgoing_RejectsEmptyThreadID(t *testing.T) {
	adapter := New(newTestConfig(DefaultAPIBase), nil)
	err := adapter.SendOutgoing(
		context.Background(), "tg:1:1", "", "hi", nil,
	)
	assert.Error(t, err)
}

func TestSendOutgoing_NonNumericTopicSuffixErrors(t *testing.T) {
	adapter := New(newTestConfig(DefaultAPIBase), nil)
	err := adapter.SendOutgoing(
		context.Background(), "tg:1:1", "abc:notnumber", "hi", nil,
	)
	assert.Error(t, err)
}

func TestSendOutgoing_PropagatesAPIErrorOnSendMessage(t *testing.T) {
	server, state := newBotAPIServer(t)
	defer server.Close()
	state.SetHandler("sendMessage", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Forbidden: bot was blocked"}`))
	})
	cfg := newTestConfig(server.URL)
	adapter := New(cfg, server.Client())

	err := adapter.SendOutgoing(
		context.Background(), "tg:"+cfg.BotID+":1", "1", "hi", nil,
	)
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// Register

type fakeRegistry struct {
	stored map[string]channels.ChannelAdapter
	err    error
}

func (f *fakeRegistry) Register(channelType string, adapter channels.ChannelAdapter) error {
	if f.err != nil {
		return f.err
	}
	if f.stored == nil {
		f.stored = map[string]channels.ChannelAdapter{}
	}
	f.stored[channelType] = adapter
	return nil
}

func TestRegister_StoresUnderTelegramKey(t *testing.T) {
	reg := &fakeRegistry{}
	adapter, err := Register(reg, newTestConfig(DefaultAPIBase))
	require.NoError(t, err)
	require.NotNil(t, adapter)
	assert.Same(t, adapter, reg.stored["telegram"])
	assert.Equal(t, "telegram", adapter.Type())
}

func TestRegister_NilRegistryErrors(t *testing.T) {
	_, err := Register(nil, newTestConfig(DefaultAPIBase))
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// Config / FromEnv

func TestFromEnv_Required(t *testing.T) {
	_, err := FromEnv(map[string]string{"TELEGRAM_BOT_TOKEN": ""})
	assert.Error(t, err)
}

func TestFromEnv_Defaults(t *testing.T) {
	cfg, err := FromEnv(map[string]string{
		"TELEGRAM_BOT_TOKEN":      "1234567890:AABBCC",
		"TELEGRAM_WEBHOOK_SECRET": "secret",
	})
	require.NoError(t, err)
	assert.Equal(t, "1234567890", cfg.BotID)
	assert.Equal(t, DefaultAPIBase, cfg.APIBase)
	assert.Equal(t, DefaultTextChunkLimit, cfg.TextChunkLimit)
}

func TestFromEnv_RejectsNonNumericBotID(t *testing.T) {
	_, err := FromEnv(map[string]string{
		"TELEGRAM_BOT_TOKEN":      "abc:def",
		"TELEGRAM_WEBHOOK_SECRET": "x",
	})
	assert.Error(t, err)
}

func TestFromEnv_RejectsOversizeChunkLimit(t *testing.T) {
	_, err := FromEnv(map[string]string{
		"TELEGRAM_BOT_TOKEN":        "1:x",
		"TELEGRAM_WEBHOOK_SECRET":   "x",
		"TELEGRAM_TEXT_CHUNK_LIMIT": "5000",
	})
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// Adapter implements channels.ChannelAdapter — compile-time assertion.

var _ channels.ChannelAdapter = (*Adapter)(nil)
