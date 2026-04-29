package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openclaw/agent-platform/internal/gateway"
)

// streamFieldStr coerces a go-redis stream value (always interface{}) to the
// underlying string. Helper because tests routinely json.Unmarshal the
// "message" field which arrives as interface{} but is always a string.
func streamFieldStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// telegramUpdate builds a Telegram-shape update JSON body.
func telegramUpdate(updateID int, text string) []byte {
	upd := map[string]any{
		"update_id": updateID,
		"message": map[string]any{
			"message_id": 1,
			"date":       1714291200,
			"chat":       map[string]any{"id": 555, "type": "private"},
			"from":       map[string]any{"id": 999, "first_name": "Alice"},
			"text":       text,
		},
	}
	b, _ := json.Marshal(upd)
	return b
}

func signedHeaders() http.Header {
	h := http.Header{}
	h.Set("X-Telegram-Bot-Api-Secret-Token", testWebhookSecret)
	h.Set("Content-Type", "application/json")
	return h
}

func doWebhook(t *testing.T, env *testEnv, headers http.Header, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/channels/telegram/webhook", bytes.NewReader(body))
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	return rec
}

func TestWebhook_HappyPathEnqueues(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	env.registerTelegram()

	env.channelsRepo.SeedRouting(testTGType, testTelegramChanID, gateway.ChannelLookup{
		ChannelID: "ch-uuid-1",
		UserID:    "user-uuid-alice",
		AgentID:   "agent-uuid-1",
	})

	rec := doWebhook(t, env, signedHeaders(), telegramUpdate(100, "hello"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["status"] != "accepted" || resp["run_id"] == "" {
		t.Fatalf("body=%v", resp)
	}

	entries, err := env.redis.XRange(context.Background(), "agent:runs", "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 stream entry, got %d", len(entries))
	}
	fields := entries[0].Values
	if fields["user_id"] != "user-uuid-alice" {
		t.Fatalf("user_id=%q", fields["user_id"])
	}
	if fields["agent_id"] != "agent-uuid-1" {
		t.Fatalf("agent_id=%q", fields["agent_id"])
	}
	if fields["channel_id"] != "ch-uuid-1" {
		t.Fatalf("channel_id=%q", fields["channel_id"])
	}
	var msg map[string]any
	_ = json.Unmarshal([]byte(streamFieldStr(fields["message"])), &msg)
	if msg["text"] != "hello" {
		t.Fatalf("message text=%v", msg)
	}
}

func TestWebhook_DuplicateUpdateIDShortCircuits(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	env.registerTelegram()
	env.channelsRepo.SeedRouting(testTGType, testTelegramChanID, gateway.ChannelLookup{
		ChannelID: "ch-1", UserID: "alice", AgentID: "agent-1",
	})

	body := telegramUpdate(200, "hello")
	r1 := doWebhook(t, env, signedHeaders(), body)
	r2 := doWebhook(t, env, signedHeaders(), body)

	if r1.Code != http.StatusOK || r2.Code != http.StatusOK {
		t.Fatalf("statuses %d,%d", r1.Code, r2.Code)
	}
	var second map[string]string
	_ = json.Unmarshal(r2.Body.Bytes(), &second)
	if second["status"] != "duplicate" {
		t.Fatalf("expected duplicate, got %v", second)
	}
	entries, _ := env.redis.XRange(context.Background(), "agent:runs", "-", "+").Result()
	if len(entries) != 1 {
		t.Fatalf("duplicate must not enqueue twice; got %d entries", len(entries))
	}
}

func TestWebhook_BadSignatureReturns401(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	env.registerTelegram()

	h := http.Header{}
	h.Set("X-Telegram-Bot-Api-Secret-Token", "wrong-secret")
	h.Set("Content-Type", "application/json")
	rec := doWebhook(t, env, h, telegramUpdate(1, "hi"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestWebhook_MissingSignatureReturns401(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	env.registerTelegram()
	h := http.Header{"Content-Type": []string{"application/json"}}
	rec := doWebhook(t, env, h, telegramUpdate(1, "hi"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestWebhook_UnknownChannelType_404(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	req := httptest.NewRequest(http.MethodPost, "/channels/no-such/webhook", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestWebhook_RoutingMissReturns404(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	env.registerTelegram()
	rec := doWebhook(t, env, signedHeaders(), telegramUpdate(300, "hi"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "channel not registered") {
		t.Fatalf("missing 'channel not registered': %s", rec.Body.String())
	}
}

func TestWebhook_MalformedBody_400(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	env.registerTelegram()
	bad, _ := json.Marshal(map[string]any{"foo": "bar"})
	rec := doWebhook(t, env, signedHeaders(), bad)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestWebhook_InvalidJSON_400(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	env.registerTelegram()
	rec := doWebhook(t, env, signedHeaders(), []byte("not json"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestWebhook_LongTextRoundTrips(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	env.registerTelegram()
	env.channelsRepo.SeedRouting(testTGType, testTelegramChanID, gateway.ChannelLookup{
		ChannelID: "ch-1", UserID: "alice", AgentID: "agent-1",
	})
	big := strings.Repeat("abc ", 1024)
	rec := doWebhook(t, env, signedHeaders(), telegramUpdate(400, big))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	entries, _ := env.redis.XRange(context.Background(), "agent:runs", "-", "+").Result()
	if len(entries) != 1 {
		t.Fatalf("len=%d", len(entries))
	}
	var msg map[string]any
	_ = json.Unmarshal([]byte(streamFieldStr(entries[0].Values["message"])), &msg)
	if msg["text"] != big {
		t.Fatalf("text round-trip failed (len=%d)", len(big))
	}
}

func TestWebhook_VerifyPanicReturns401(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	a := env.registerTelegram()
	a.verifyErr = true
	rec := doWebhook(t, env, signedHeaders(), telegramUpdate(1, "x"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on verifier panic, got %d", rec.Code)
	}
}

func TestWebhook_PayloadIncludesAdapterFields(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	env.registerTelegram()
	env.channelsRepo.SeedRouting(testTGType, testTelegramChanID, gateway.ChannelLookup{
		ChannelID: "c-1", UserID: "alice", AgentID: "ag",
	})
	rec := doWebhook(t, env, signedHeaders(), telegramUpdate(500, "hello"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	entries, _ := env.redis.XRange(context.Background(), "agent:runs", "-", "+").Result()
	if len(entries) != 1 {
		t.Fatal("len mismatch")
	}
	var msg map[string]any
	_ = json.Unmarshal([]byte(streamFieldStr(entries[0].Values["message"])), &msg)
	if msg["text"] != "hello" {
		t.Fatalf("text setdefault: %v", msg)
	}
	if msg["sender_id"] != "999" {
		t.Fatalf("sender_id setdefault: %v", msg)
	}
	if _, ok := msg["raw_update_id"]; !ok {
		t.Fatal("adapter payload key dropped")
	}
}

// Smoke check that the helper context type assertion still compiles.
var _ context.Context = context.Background()
