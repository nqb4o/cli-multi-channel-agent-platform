package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openclaw/agent-platform/internal/gateway"
)

func postJSON(t *testing.T, env *testEnv, path string, headers http.Header, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf []byte
	if body != nil {
		var err error
		buf, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	return rec
}

func adminAuthHeaders() http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+testAdminToken)
	return h
}

// ---------------------------------------------------------------------------
// /admin/sandboxes
// ---------------------------------------------------------------------------

func TestAdminSandboxes_HappyPath(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	rec := postJSON(t, env, "/admin/sandboxes", adminAuthHeaders(),
		map[string]string{"user_id": "alice"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["user_id"] != "alice" || body["state"] != "running" {
		t.Fatalf("body=%v", body)
	}
	if !bytes.HasPrefix([]byte(body["id"]), []byte("sb-")) {
		t.Fatalf("id=%q", body["id"])
	}
	if len(env.orch.calls) != 1 || env.orch.calls[0] != "alice" {
		t.Fatalf("orchestrator calls=%v", env.orch.calls)
	}
}

func TestAdminSandboxes_MissingBearer_401(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	rec := postJSON(t, env, "/admin/sandboxes", nil,
		map[string]string{"user_id": "alice"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAdminSandboxes_BadToken_403(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	h := http.Header{}
	h.Set("Authorization", "Bearer wrong-token")
	rec := postJSON(t, env, "/admin/sandboxes", h,
		map[string]string{"user_id": "alice"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestAdminSandboxes_MalformedBearer_401(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	h := http.Header{}
	h.Set("Authorization", "Token nope")
	rec := postJSON(t, env, "/admin/sandboxes", h,
		map[string]string{"user_id": "alice"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAdminSandboxes_FailsClosedWhenAdminTokenUnset(t *testing.T) {
	opts := defaultEnvOpts()
	opts.adminToken = ""
	env := newTestEnv(t, opts)
	rec := postJSON(t, env, "/admin/sandboxes", adminAuthHeaders(),
		map[string]string{"user_id": "alice"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestAdminSandboxes_OrchestratorStatusError_502(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	env.orch.raiseErr = &gateway.OrchestratorStatusError{StatusCode: 503, Body: "down"}
	rec := postJSON(t, env, "/admin/sandboxes", adminAuthHeaders(),
		map[string]string{"user_id": "alice"})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
}

func TestAdminSandboxes_OrchestratorTransportError_502(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	env.orch.raiseErr = &gateway.OrchestratorTransportError{
		Err: errors.New("could not reach"),
	}
	rec := postJSON(t, env, "/admin/sandboxes", adminAuthHeaders(),
		map[string]string{"user_id": "alice"})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// /admin/channels
// ---------------------------------------------------------------------------

func TestAdminChannels_HappyPath(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	rec := postJSON(t, env, "/admin/channels", adminAuthHeaders(), map[string]any{
		"user_id":      "alice",
		"agent_id":     "agent-1",
		"channel_type": "telegram",
		"ext_id":       "tg:bot-x:chat-1",
		"config":       map[string]any{"bot_token": "secret"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["user_id"] != "alice" || body["agent_id"] != "agent-1" ||
		body["channel_type"] != "telegram" || body["ext_id"] != "tg:bot-x:chat-1" {
		t.Fatalf("body=%v", body)
	}
	if body["channel_id"] == "" {
		t.Fatal("missing channel_id")
	}

	// DAL routing was persisted.
	lk, _ := env.channelsRepo.LookupRouting(context.Background(), "telegram", "tg:bot-x:chat-1")
	if lk == nil || lk.UserID != "alice" || lk.AgentID != "agent-1" {
		t.Fatalf("routing not persisted: %+v", lk)
	}
}

func TestAdminChannels_Unauthorized_401(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	rec := postJSON(t, env, "/admin/channels", nil, map[string]any{
		"user_id": "alice", "agent_id": "agent-1",
		"channel_type": "telegram", "ext_id": "tg:x", "config": map[string]any{},
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAdminChannels_ValidationError_422(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	rec := postJSON(t, env, "/admin/channels", adminAuthHeaders(),
		map[string]any{"user_id": "alice"}) // missing other required fields
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", rec.Code)
	}
}
