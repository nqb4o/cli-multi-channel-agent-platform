package orchestrator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"
)

type stubSource struct {
	users []string
	calls []sourceCall
}

type sourceCall struct {
	since time.Time
	limit int
}

func (s *stubSource) TopActiveUsers(_ context.Context, since time.Time, limit int) ([]string, error) {
	s.calls = append(s.calls, sourceCall{since: since, limit: limit})
	end := limit
	if end > len(s.users) {
		end = len(s.users)
	}
	out := make([]string, end)
	copy(out, s.users[:end])
	return out, nil
}

func TestWarmPoolDefaultsMatchBrief(t *testing.T) {
	if DefaultTopN != 100 {
		t.Errorf("DefaultTopN = %d", DefaultTopN)
	}
	if DefaultWarmWindow != 7*24*time.Hour {
		t.Errorf("DefaultWarmWindow = %v", DefaultWarmWindow)
	}
	if DefaultRefreshInterval != time.Hour {
		t.Errorf("DefaultRefreshInterval = %v", DefaultRefreshInterval)
	}
}

func TestWarmPoolRefreshPrewarmsTopN(t *testing.T) {
	pool, _, fake, _ := newTestPool(8)
	src := &stubSource{users: []string{"a", "b", "c"}}
	wp := NewWarmPoolManager(pool, src, WithWarmPoolTopN(3))

	snap, err := wp.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.LastRefreshSize != 3 {
		t.Errorf("last_refresh_size = %d", snap.LastRefreshSize)
	}
	users := append([]string{}, snap.PinnedUserIDs...)
	sort.Strings(users)
	if len(users) != 3 || users[0] != "a" || users[1] != "b" || users[2] != "c" {
		t.Errorf("pinned_user_ids = %v", users)
	}
	if fake.CreateCalls != 3 {
		t.Errorf("create_calls = %d", fake.CreateCalls)
	}
	for _, sid := range wp.PinnedUsers() {
		if !pool.IsPinned(sid) {
			t.Errorf("expected sandbox %s pinned", sid)
		}
	}
}

func TestWarmPoolRefreshEvictsDropped(t *testing.T) {
	pool, _, fake, _ := newTestPool(8)
	src := &stubSource{users: []string{"a", "b", "c"}}
	wp := NewWarmPoolManager(pool, src, WithWarmPoolTopN(3))
	if _, err := wp.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// b drops out of the top-N.
	src.users = []string{"a", "c", "d"}
	if _, err := wp.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	pinned := wp.PinnedUsers()
	if _, ok := pinned["b"]; ok {
		t.Errorf("expected b unpinned, got %v", pinned)
	}
	if _, ok := pinned["d"]; !ok {
		t.Errorf("expected d pinned, got %v", pinned)
	}
	hibernated := false
	for _, n := range fake.StopCalls {
		if n >= 1 {
			hibernated = true
			break
		}
	}
	if !hibernated {
		t.Error("expected b to be hibernated on eviction")
	}
	if !pool.IsPinned(pinned["a"]) || !pool.IsPinned(pinned["c"]) {
		t.Errorf("expected a/c still pinned: %v", pinned)
	}
}

func TestWarmPoolRefreshUsesWindow(t *testing.T) {
	pool, _, _, _ := newTestPool(4)
	src := &stubSource{}
	window := 7 * 24 * time.Hour
	wp := NewWarmPoolManager(pool, src, WithWarmPoolTopN(1), WithWarmPoolWindow(window))

	before := time.Now().UTC()
	if _, err := wp.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()

	if len(src.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(src.calls))
	}
	call := src.calls[0]
	if call.limit != 1 {
		t.Errorf("limit = %d", call.limit)
	}
	want_min := before.Add(-window).Add(-time.Second)
	want_max := after.Add(-window).Add(time.Second)
	if call.since.Before(want_min) || call.since.After(want_max) {
		t.Errorf("since out of bounds: got %v, want [%v, %v]", call.since, want_min, want_max)
	}
}

func TestWarmPoolStatusEmptyBeforeRefresh(t *testing.T) {
	pool, _, _, _ := newTestPool(4)
	src := &stubSource{users: []string{"a"}}
	wp := NewWarmPoolManager(pool, src, WithWarmPoolTopN(1))
	snap := wp.Status()
	if len(snap.PinnedUserIDs) != 0 {
		t.Errorf("expected empty pinned, got %v", snap.PinnedUserIDs)
	}
	if snap.TopN != 1 {
		t.Errorf("top_n = %d", snap.TopN)
	}
	if snap.LastRefreshAt != nil {
		t.Errorf("expected nil last_refresh_at")
	}
}

// HTTP admin tests
func TestAdminRouterRefreshAndStatus(t *testing.T) {
	pool, _, _, _ := newTestPool(4)
	src := &stubSource{users: []string{"a"}}
	wp := NewWarmPoolManager(pool, src, WithWarmPoolTopN(1))

	deps := Deps{WarmPool: wp}
	router := NewRouter(deps)
	srv := httptest.NewServer(router)
	defer srv.Close()

	// Status before refresh — empty pinned.
	resp, err := http.Get(srv.URL + "/admin/warm-pool/status")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Refresh.
	resp, err = http.Post(srv.URL+"/admin/warm-pool/refresh", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("refresh status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Status after refresh — pinned should include "a".
	resp, err = http.Get(srv.URL + "/admin/warm-pool/status")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status after refresh = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdminRouter503WhenUnconfigured(t *testing.T) {
	router := NewRouter(Deps{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/admin/warm-pool/status")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 503 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err = http.Post(srv.URL+"/admin/warm-pool/refresh", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 503 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestWarmPoolStartStopIdempotent(t *testing.T) {
	pool, _, _, _ := newTestPool(4)
	src := &stubSource{}
	wp := NewWarmPoolManager(pool, src, WithWarmPoolTopN(1), WithWarmPoolRefreshInterval(time.Hour))
	wp.Start()
	wp.Start()
	wp.Stop()
	wp.Stop()
}
