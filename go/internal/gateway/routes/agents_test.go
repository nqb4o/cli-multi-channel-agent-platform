package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/openclaw/agent-platform/internal/gateway"
)

func authedReq(t *testing.T, env *testEnv, method, path string, tok string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf []byte
	if body != nil {
		var err error
		buf, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(buf))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	return rec
}

func TestAgents_CreateHappyPath(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	_, tok := env.authedToken("alice@example.test")
	rec := authedReq(t, env, http.MethodPost, "/agents", tok,
		map[string]string{"name": "research-bot", "config_yaml": "name: research"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["name"] != "research-bot" || body["config_yaml"] != "name: research" {
		t.Fatalf("body=%v", body)
	}
	if body["agent_id"].(string) == "" || body["user_id"].(string) == "" {
		t.Fatalf("missing fields: %v", body)
	}
}

func TestAgents_CreateValidationError_422(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	_, tok := env.authedToken("a@b.test")
	rec := authedReq(t, env, http.MethodPost, "/agents", tok,
		map[string]string{"config_yaml": "x: 1"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", rec.Code)
	}
}

func TestAgents_CreateWithoutToken_401(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	rec := authedReq(t, env, http.MethodPost, "/agents", "",
		map[string]string{"name": "x", "config_yaml": "y: 1"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAgents_ListReturnsOnlyOwn(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	user, tok := env.authedToken("alice@example.test")
	// Other user's agent.
	if _, err := env.agentsRepo.Create(context.Background(), "other-user", "other-bot", "x: 1"); err != nil {
		t.Fatal(err)
	}
	authedReq(t, env, http.MethodPost, "/agents", tok,
		map[string]string{"name": "mine", "config_yaml": "z: 2"})

	rec := authedReq(t, env, http.MethodGet, "/agents", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	agents := body["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent for user, got %d", len(agents))
	}
	a0 := agents[0].(map[string]any)
	if a0["name"] != "mine" || a0["user_id"] != user.UserID {
		t.Fatalf("agent[0]=%v", a0)
	}
}

func TestAgents_GetOwner(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	_, tok := env.authedToken("alice@example.test")
	rec := authedReq(t, env, http.MethodPost, "/agents", tok,
		map[string]string{"name": "n", "config_yaml": "c: 1"})
	var c map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &c)
	aid := c["agent_id"].(string)

	rec2 := authedReq(t, env, http.MethodGet, "/agents/"+aid, tok, nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec2.Code)
	}
}

func TestAgents_GetCrossUser_403(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	_, tok := env.authedToken("alice@example.test")
	other, err := env.agentsRepo.Create(context.Background(), "other-user", "secret", "x: 1")
	if err != nil {
		t.Fatal(err)
	}
	rec := authedReq(t, env, http.MethodGet, "/agents/"+other.AgentID, tok, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestAgents_GetUnknown_404(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	_, tok := env.authedToken("alice@example.test")
	rec := authedReq(t, env, http.MethodGet, "/agents/nonexistent", tok, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestAgents_PatchUpdatesConfig(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	_, tok := env.authedToken("alice@example.test")
	createRec := authedReq(t, env, http.MethodPost, "/agents", tok,
		map[string]string{"name": "n", "config_yaml": "old: 1"})
	var c map[string]any
	_ = json.Unmarshal(createRec.Body.Bytes(), &c)
	aid := c["agent_id"].(string)

	rec := authedReq(t, env, http.MethodPatch, "/agents/"+aid, tok,
		map[string]string{"config_yaml": "new: 2"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["config_yaml"] != "new: 2" {
		t.Fatalf("config not updated: %v", body)
	}

	// Subsequent GET reflects the change.
	rec2 := authedReq(t, env, http.MethodGet, "/agents/"+aid, tok, nil)
	var fresh map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &fresh)
	if fresh["config_yaml"] != "new: 2" {
		t.Fatalf("GET didn't reflect update: %v", fresh)
	}
}

func TestAgents_PatchCrossUser_403(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	_, tok := env.authedToken("alice@example.test")
	other, _ := env.agentsRepo.Create(context.Background(), "other-user-2", "n", "x: 1")
	rec := authedReq(t, env, http.MethodPatch, "/agents/"+other.AgentID, tok,
		map[string]string{"config_yaml": "hacked: yes"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestAgents_DeleteOwner(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	_, tok := env.authedToken("alice@example.test")
	createRec := authedReq(t, env, http.MethodPost, "/agents", tok,
		map[string]string{"name": "tmp", "config_yaml": "c: 1"})
	var c map[string]any
	_ = json.Unmarshal(createRec.Body.Bytes(), &c)
	aid := c["agent_id"].(string)

	rec := authedReq(t, env, http.MethodDelete, "/agents/"+aid, tok, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Subsequent GET → 404.
	rec2 := authedReq(t, env, http.MethodGet, "/agents/"+aid, tok, nil)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", rec2.Code)
	}
}

func TestAgents_DeleteCrossUser_403(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	_, tok := env.authedToken("alice@example.test")
	other, _ := env.agentsRepo.Create(context.Background(), "other-user-3", "n", "x: 1")
	rec := authedReq(t, env, http.MethodDelete, "/agents/"+other.AgentID, tok, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestAgents_TokenForDifferentSecret_401(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	bad, err := gateway.IssueUserToken("u", "x@y.test", "not-the-real-secret", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rec := authedReq(t, env, http.MethodGet, "/agents", bad, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}
