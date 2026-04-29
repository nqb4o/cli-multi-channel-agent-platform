package orchestrator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPrewarmCreatesSandboxFirstTime(t *testing.T) {
	pool, _, fake, clock := newTestPool(4)
	trig := NewPresenceTrigger(pool, WithPresenceClock(clock.Now))
	res, err := trig.Prewarm(context.Background(), "u-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ready" && res.Status != "prewarming" {
		t.Errorf("status = %q", res.Status)
	}
	if res.UserID != "u-1" {
		t.Errorf("user_id = %q", res.UserID)
	}
	if res.SandboxID == "" {
		t.Error("sandbox_id empty")
	}
	if fake.CreateCalls != 1 {
		t.Errorf("create_calls = %d", fake.CreateCalls)
	}
}

func TestPrewarmResumesHibernated(t *testing.T) {
	// Acceptance criterion #6.
	pool, _, fake, clock := newTestPool(4)
	trig := NewPresenceTrigger(pool, WithPresenceClock(clock.Now))
	ctx := context.Background()

	first, _ := trig.Prewarm(ctx, "u-1")
	sandboxID := first.SandboxID

	if _, err := fake.StopSandbox(ctx, sandboxID); err != nil {
		t.Fatal(err)
	}
	pool.Remove("u-1")

	// Allow debounce to pass.
	clock.Tick(10 * time.Second)

	second, err := trig.Prewarm(ctx, "u-1")
	if err != nil {
		t.Fatal(err)
	}
	if second.SandboxID != sandboxID {
		t.Errorf("expected reuse, got %s vs %s", second.SandboxID, sandboxID)
	}
	if second.SandboxState != string(StateRunning) {
		t.Errorf("state = %s", second.SandboxState)
	}
	if fake.StartCalls[sandboxID] != 1 {
		t.Errorf("start_calls = %v", fake.StartCalls)
	}
}

func TestPrewarmDebounces(t *testing.T) {
	pool, _, _, clock := newTestPool(4)
	trig := NewPresenceTrigger(pool, WithDebounce(2*time.Second), WithPresenceClock(clock.Now))
	ctx := context.Background()

	res, _ := trig.Prewarm(ctx, "u-1")
	if res.Status == "debounced" {
		t.Error("first prewarm should not be debounced")
	}

	res, _ = trig.Prewarm(ctx, "u-1")
	if res.Status != "debounced" {
		t.Errorf("expected debounced, got %q", res.Status)
	}

	clock.Tick(3 * time.Second)
	res, _ = trig.Prewarm(ctx, "u-1")
	if res.Status == "debounced" {
		t.Error("after debounce passes, should not be debounced")
	}
}

func TestPrewarmRouteReturns202(t *testing.T) {
	pool, _, _, clock := newTestPool(4)
	trig := NewPresenceTrigger(pool, WithPresenceClock(clock.Now))
	deps := Deps{Pool: pool, Presence: trig, Orchestrator: pool.Orchestrator()}
	router := NewRouter(deps)
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/sandboxes/u-1/prewarm", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestPrewarmRoute503WhenUnconfigured(t *testing.T) {
	router := NewRouter(Deps{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/sandboxes/u-1/prewarm", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestPrewarmRejectsEmptyUserID(t *testing.T) {
	pool, _, _, clock := newTestPool(4)
	trig := NewPresenceTrigger(pool, WithPresenceClock(clock.Now))
	if _, err := trig.Prewarm(context.Background(), ""); err == nil {
		t.Error("expected error for empty user_id")
	}
}

func TestPresenceLastPrewarmAt(t *testing.T) {
	pool, _, _, clock := newTestPool(4)
	trig := NewPresenceTrigger(pool, WithPresenceClock(clock.Now))
	if !trig.LastPrewarmAt("u-1").IsZero() {
		t.Errorf("expected zero before prewarm")
	}
	_, _ = trig.Prewarm(context.Background(), "u-1")
	if trig.LastPrewarmAt("u-1").IsZero() {
		t.Errorf("expected non-zero after prewarm")
	}
}
