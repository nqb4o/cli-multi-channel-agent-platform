package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestRouter(t *testing.T) (*httptest.Server, *Orchestrator, *FakeDaytonaClient, *SandboxPool) {
	t.Helper()
	pool, orch, fake, _ := newTestPool(4)
	presence := NewPresenceTrigger(pool)
	deps := Deps{
		Orchestrator: orch,
		Pool:         pool,
		Presence:     presence,
	}
	srv := httptest.NewServer(NewRouter(deps))
	t.Cleanup(srv.Close)
	return srv, orch, fake, pool
}

func TestAPIHealthzOK(t *testing.T) {
	srv, _, _, _ := newTestRouter(t)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("body = %v", body)
	}
}

func TestAPIHealthzWhenUnconfigured(t *testing.T) {
	srv := httptest.NewServer(NewRouter(Deps{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestAPIHealthzDegradedOnProviderFailure(t *testing.T) {
	pool, _, fake, _ := newTestPool(4)
	fake.SetHealthOK(false)
	deps := Deps{Orchestrator: pool.Orchestrator(), Pool: pool}
	srv := httptest.NewServer(NewRouter(deps))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestAPIPostSandboxCreates(t *testing.T) {
	srv, _, fake, _ := newTestRouter(t)
	body := bytes.NewBufferString(`{"user_id":"alice"}`)
	resp, err := http.Post(srv.URL+"/sandboxes", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	var view map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&view)
	if view["user_id"] != "alice" {
		t.Errorf("user_id = %v", view["user_id"])
	}
	if view["state"] != "running" {
		t.Errorf("state = %v", view["state"])
	}
	if fake.CreateCalls != 1 {
		t.Errorf("create_calls = %d", fake.CreateCalls)
	}
}

func TestAPIPostSandboxRejectsEmpty(t *testing.T) {
	srv, _, _, _ := newTestRouter(t)
	resp, err := http.Post(srv.URL+"/sandboxes", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestAPIPostSandboxBadJSON(t *testing.T) {
	srv, _, _, _ := newTestRouter(t)
	resp, err := http.Post(srv.URL+"/sandboxes", "application/json", bytes.NewBufferString("not json"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestAPIGetSandbox(t *testing.T) {
	srv, orch, _, _ := newTestRouter(t)
	sandbox, _ := orch.Create(context.Background(), "alice")
	resp, err := http.Get(srv.URL + "/sandboxes/" + sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestAPIGetSandboxNotFound(t *testing.T) {
	srv, _, _, _ := newTestRouter(t)
	resp, err := http.Get(srv.URL + "/sandboxes/missing")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestAPIPostHibernateAndDelete(t *testing.T) {
	srv, orch, _, _ := newTestRouter(t)
	sandbox, _ := orch.Create(context.Background(), "alice")

	resp, err := http.Post(srv.URL+"/sandboxes/"+sandbox.ID+"/hibernate", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("hibernate status = %d", resp.StatusCode)
	}

	req, _ := http.NewRequest("DELETE", srv.URL+"/sandboxes/"+sandbox.ID, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("delete status = %d", resp.StatusCode)
	}
}

func TestAPIPostResumeRunningPassesThrough(t *testing.T) {
	srv, orch, _, _ := newTestRouter(t)
	sandbox, _ := orch.Create(context.Background(), "alice")
	resp, err := http.Post(srv.URL+"/sandboxes/"+sandbox.ID+"/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestAPIPostResumeHibernatedReboots(t *testing.T) {
	srv, orch, _, _ := newTestRouter(t)
	sandbox, _ := orch.Create(context.Background(), "alice")
	_ = orch.Hibernate(context.Background(), sandbox.ID)
	resp, err := http.Post(srv.URL+"/sandboxes/"+sandbox.ID+"/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestAPIPostExec(t *testing.T) {
	srv, orch, _, _ := newTestRouter(t)
	sandbox, _ := orch.Create(context.Background(), "alice")
	body := bytes.NewBufferString(`{"cmd":["true"],"timeout_s":10}`)
	resp, err := http.Post(srv.URL+"/sandboxes/"+sandbox.ID+"/exec", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), `"timed_out":false`) {
		t.Errorf("body = %s", out)
	}
}

func TestAPIPostExecRejectsEmptyCmd(t *testing.T) {
	srv, orch, _, _ := newTestRouter(t)
	sandbox, _ := orch.Create(context.Background(), "alice")
	body := bytes.NewBufferString(`{"cmd":[]}`)
	resp, err := http.Post(srv.URL+"/sandboxes/"+sandbox.ID+"/exec", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestAPIPostExecBadStdinB64(t *testing.T) {
	srv, orch, _, _ := newTestRouter(t)
	sandbox, _ := orch.Create(context.Background(), "alice")
	body := bytes.NewBufferString(`{"cmd":["true"],"stdin_b64":"!!!not base64"}`)
	resp, err := http.Post(srv.URL+"/sandboxes/"+sandbox.ID+"/exec", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}
