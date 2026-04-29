package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthz_AlwaysReturns200(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Fatalf("body=%v", body)
	}
}

func TestReadyz_Returns200WhenRedisAndDBOK(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "ready" || body["redis"] != true || body["db"] != true {
		t.Fatalf("body=%v", body)
	}
}

func TestReadyz_Returns503WhenDBUnhealthy(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	env.dbHealth.Set(false)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "not_ready" || body["redis"] != true || body["db"] != false {
		t.Fatalf("body=%v", body)
	}
}

func TestReadyz_Returns503WhenRedisUnhealthy(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	env.mux.Close() // kill the fake redis
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["redis"] != false {
		t.Fatalf("redis should be false, body=%v", body)
	}
}
