package routes

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/openclaw/agent-platform/internal/gateway"
	pkgcrypto "github.com/openclaw/agent-platform/pkg/crypto"
)

func createAgent(t *testing.T, env *testEnv, tok string) string {
	t.Helper()
	rec := authedReq(t, env, http.MethodPost, "/agents", tok,
		map[string]string{"name": "a", "config_yaml": "x: 1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create agent: %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body["agent_id"].(string)
}

// ---------------------------------------------------------------------------
// POST /channels — encryption + auth
// ---------------------------------------------------------------------------

func TestUserChannels_RegisterEncryptsConfig(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	_, tok := env.authedToken("alice@example.test")
	agentID := createAgent(t, env, tok)

	secretToken := "super-secret-bot-token-do-not-leak"
	rec := authedReq(t, env, http.MethodPost, "/channels", tok, map[string]any{
		"type":     "telegram",
		"ext_id":   "tg:bot-x:chat-1",
		"agent_id": agentID,
		"config": map[string]any{
			"bot_token":      secretToken,
			"webhook_secret": "another-secret",
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["channel_id"].(string) == "" || body["agent_id"] != agentID {
		t.Fatalf("body=%v", body)
	}
	if body["type"] != "telegram" || body["ext_id"] != "tg:bot-x:chat-1" {
		t.Fatalf("body=%v", body)
	}

	rows := env.channelsRepo.Rows()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	blob := rows[0].ConfigEncrypted
	if len(blob) == 0 || blob[0] != 0x01 {
		t.Fatalf("blob version byte missing/wrong: %x", blob[:4])
	}
	if bytes.Contains(blob, []byte(secretToken)) {
		t.Fatal("plaintext bot token leaked into encrypted blob")
	}
	if bytes.Contains(blob, []byte("webhook_secret")) {
		t.Fatal("plaintext key name leaked into encrypted blob")
	}
}

func TestUserChannels_RegisterDecryptsCorrectly(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	_, tok := env.authedToken("alice@example.test")
	agentID := createAgent(t, env, tok)

	cfg := map[string]any{"bot_token": "abc", "extra": []any{1.0, 2.0, 3.0}}
	rec := authedReq(t, env, http.MethodPost, "/channels", tok, map[string]any{
		"type":     "telegram",
		"ext_id":   "tg:rt:chat-1",
		"agent_id": agentID,
		"config":   cfg,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}

	rows := env.channelsRepo.Rows()
	key, err := hex.DecodeString(testEncryptionKey)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := pkgcrypto.Decrypt(key, rows[0].ConfigEncrypted)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(plain, &got); err != nil {
		t.Fatal(err)
	}
	if got["bot_token"] != "abc" {
		t.Fatalf("decrypted bot_token mismatch: %v", got)
	}
}

func TestUserChannels_RegisterWithoutToken_401(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	rec := authedReq(t, env, http.MethodPost, "/channels", "", map[string]any{
		"type": "telegram", "ext_id": "tg:nope", "agent_id": "x",
		"config": map[string]any{},
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestUserChannels_RegisterCrossUserAgent_403(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	_, tok := env.authedToken("alice@example.test")
	other, _ := env.agentsRepo.Create(context.Background(), "other-user-1", "n", "y: 1")
	rec := authedReq(t, env, http.MethodPost, "/channels", tok, map[string]any{
		"type":     "telegram",
		"ext_id":   "tg:hostile:chat-1",
		"agent_id": other.AgentID,
		"config":   map[string]any{"bot_token": "x"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestUserChannels_RegisterUnknownAgent_404(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	_, tok := env.authedToken("alice@example.test")
	rec := authedReq(t, env, http.MethodPost, "/channels", tok, map[string]any{
		"type": "telegram", "ext_id": "tg:no-agent:chat-1",
		"agent_id": "nonexistent", "config": map[string]any{},
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestUserChannels_500WhenDBEncryptionKeyMissing(t *testing.T) {
	opts := defaultEnvOpts()
	opts.encryptionKey = ""
	env := newTestEnv(t, opts)
	_, tok := env.authedToken("alice@example.test")
	a, _ := env.agentsRepo.Create(context.Background(),
		// Need this agent to belong to alice for the 403 check to pass.
		// authedToken just returned the user; recreate one for them.
		"", "n", "y: 1")
	// Manually attach the agent to alice — re-create through the repo.
	user, err := env.usersRepo.GetByEmail(context.Background(), "alice@example.test")
	if err != nil || user == nil {
		t.Fatalf("user lookup: %v %v", user, err)
	}
	a, err = env.agentsRepo.Create(context.Background(), user.UserID, "n", "y: 1")
	if err != nil {
		t.Fatal(err)
	}

	rec := authedReq(t, env, http.MethodPost, "/channels", tok, map[string]any{
		"type": "telegram", "ext_id": "tg:missing-key:chat-1",
		"agent_id": a.AgentID, "config": map[string]any{"bot_token": "x"},
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "DB_ENCRYPTION_KEY") {
		t.Fatalf("missing helpful message: %s", rec.Body.String())
	}
}

func TestUserChannels_500WhenDBEncryptionKeyWrongLength(t *testing.T) {
	opts := defaultEnvOpts()
	opts.encryptionKey = strings.Repeat("ab", 16) // 16 bytes, not 32
	env := newTestEnv(t, opts)
	_, tok := env.authedToken("alice@example.test")
	user, _ := env.usersRepo.GetByEmail(context.Background(), "alice@example.test")
	a, _ := env.agentsRepo.Create(context.Background(), user.UserID, "n", "y: 1")

	rec := authedReq(t, env, http.MethodPost, "/channels", tok, map[string]any{
		"type": "telegram", "ext_id": "tg:short-key:chat-1",
		"agent_id": a.AgentID, "config": map[string]any{},
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "DB_ENCRYPTION_KEY") {
		t.Fatalf("missing helpful message: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// GET / DELETE
// ---------------------------------------------------------------------------

func TestUserChannels_ListOnlyForAuthedUser(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	user, tok := env.authedToken("alice@example.test")
	// Other user's channel.
	if _, err := env.channelsRepo.Register(
		context.Background(),
		"other-user", "telegram", "tg:other:chat-1",
		[]byte{0x01, 0, 0, 0}, "other-agent",
	); err != nil {
		t.Fatal(err)
	}

	agentID := createAgent(t, env, tok)
	authedReq(t, env, http.MethodPost, "/channels", tok, map[string]any{
		"type": "telegram", "ext_id": "tg:mine:chat-1",
		"agent_id": agentID, "config": map[string]any{"bot_token": "x"},
	})

	rec := authedReq(t, env, http.MethodGet, "/channels", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	chans := body["channels"].([]any)
	if len(chans) != 1 {
		t.Fatalf("expected 1, got %d", len(chans))
	}
	c0 := chans[0].(map[string]any)
	if c0["ext_id"] != "tg:mine:chat-1" {
		t.Fatalf("ext_id=%v", c0)
	}
	if c0["user_id"] != user.UserID {
		t.Fatalf("user_id=%v", c0)
	}
}

func TestUserChannels_DeleteOwner(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	_, tok := env.authedToken("alice@example.test")
	agentID := createAgent(t, env, tok)
	rec := authedReq(t, env, http.MethodPost, "/channels", tok, map[string]any{
		"type": "telegram", "ext_id": "tg:to-delete:chat-1",
		"agent_id": agentID, "config": map[string]any{},
	})
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	cid := body["channel_id"].(string)

	rec2 := authedReq(t, env, http.MethodDelete, "/channels/"+cid, tok, nil)
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec2.Code)
	}

	rec3 := authedReq(t, env, http.MethodGet, "/channels", tok, nil)
	var listBody map[string]any
	_ = json.Unmarshal(rec3.Body.Bytes(), &listBody)
	for _, ch := range listBody["channels"].([]any) {
		c := ch.(map[string]any)
		if c["channel_id"] == cid {
			t.Fatalf("channel still listed after delete")
		}
	}
}

func TestUserChannels_DeleteCrossUser_403(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	_, tok := env.authedToken("alice@example.test")
	other, err := env.channelsRepo.Register(
		context.Background(),
		"other-user", "telegram", "tg:hostile:chat-1",
		[]byte{0x01, 0, 0, 0}, "other-agent",
	)
	if err != nil {
		t.Fatal(err)
	}
	rec := authedReq(t, env, http.MethodDelete, "/channels/"+other.ChannelID, tok, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Co-existence with the webhook router
// ---------------------------------------------------------------------------

func TestUserChannels_LookupRoutingWorksAfterRegistration(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	env.registerTelegram()
	_, tok := env.authedToken("alice@example.test")
	agentID := createAgent(t, env, tok)
	extID := "12345"

	rec := authedReq(t, env, http.MethodPost, "/channels", tok, map[string]any{
		"type": "telegram", "ext_id": extID,
		"agent_id": agentID, "config": map[string]any{"bot_token": "x"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}
	routing, err := env.channelsRepo.LookupRouting(context.Background(), "telegram", extID)
	if err != nil {
		t.Fatal(err)
	}
	if routing == nil {
		t.Fatal("routing missing")
	}
	if routing.AgentID != agentID {
		t.Fatalf("agent_id=%q want=%q", routing.AgentID, agentID)
	}
}

// Sanity: ensure the InMemoryChannelsRepo also satisfies the
// ChannelsRepoWithListGetDelete extension at compile time. If this stops
// compiling, list/delete routes will silently degrade in tests.
var _ gateway.ChannelsRepoWithListGetDelete = (*gateway.InMemoryChannelsRepo)(nil)
