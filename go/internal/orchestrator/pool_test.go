package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestPoolGetOrResumeCreatesThenReuses(t *testing.T) {
	pool, _, fake, _ := newTestPool(4)
	ctx := context.Background()
	a, _ := pool.GetOrResume(ctx, "u-1")
	b, _ := pool.GetOrResume(ctx, "u-1")
	if a.ID != b.ID {
		t.Errorf("expected reuse, got %s vs %s", a.ID, b.ID)
	}
	if fake.CreateCalls != 1 {
		t.Errorf("create_calls = %d", fake.CreateCalls)
	}
	if pool.ActiveCount() != 1 {
		t.Errorf("active_count = %d", pool.ActiveCount())
	}
}

func TestPoolSingleFlightResume(t *testing.T) {
	// Acceptance criterion #5: 10 concurrent get_or_resume calls for a
	// hibernated user issue exactly 1 start_sandbox.
	orch, fake := newTestOrchestrator()
	clock := NewFakeClock()
	pool := NewSandboxPool(orch, WithPoolCapacity(4), WithPoolClock(clock.Now))
	ctx := context.Background()

	sb, _ := pool.GetOrResume(ctx, "u-1")
	if _, err := fake.StopSandbox(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}

	// Provider latency on resume so callers actually overlap.
	fake.SetResumeDelay(50 * time.Millisecond)

	pool.Remove("u-1")

	var wg sync.WaitGroup
	results := make([]*Sandbox, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := pool.GetOrResume(ctx, "u-1")
			if err != nil {
				t.Errorf("call %d: %v", i, err)
				return
			}
			results[i] = s
		}(i)
	}
	wg.Wait()

	ids := map[string]struct{}{}
	for _, r := range results {
		if r != nil {
			ids[r.ID] = struct{}{}
		}
	}
	if len(ids) != 1 {
		t.Errorf("expected 1 sandbox id across all callers, got %v", ids)
	}
	if fake.StartCalls[sb.ID] != 1 {
		t.Errorf("expected 1 start_sandbox call, got %v", fake.StartCalls)
	}
}

func TestPoolLRUEvictionAtCapacity(t *testing.T) {
	pool, _, fake, clock := newTestPool(4)
	ctx := context.Background()

	_, _ = pool.GetOrResume(ctx, "u-1")
	clock.Tick(time.Second)
	sb2, _ := pool.GetOrResume(ctx, "u-2")
	clock.Tick(time.Second)
	_, _ = pool.GetOrResume(ctx, "u-3")
	clock.Tick(time.Second)
	_, _ = pool.GetOrResume(ctx, "u-4")
	if pool.ActiveCount() != 4 {
		t.Errorf("active_count = %d, want 4", pool.ActiveCount())
	}
	if len(fake.StopCalls) != 0 {
		t.Errorf("unexpected stop_calls: %v", fake.StopCalls)
	}

	// Touch u-1 so u-2 becomes the LRU.
	clock.Tick(time.Second)
	_, _ = pool.GetOrResume(ctx, "u-1")

	clock.Tick(time.Second)
	sb5, _ := pool.GetOrResume(ctx, "u-5")
	if pool.ActiveCount() != 4 {
		t.Errorf("active_count after eviction = %d, want 4", pool.ActiveCount())
	}
	if fake.StopCalls[sb2.ID] == 0 {
		t.Errorf("expected sb2 hibernated, stop_calls=%v", fake.StopCalls)
	}
	found := false
	for _, item := range pool.ActiveItems() {
		if item.SandboxID == sb5.ID {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected sb5 to be in active set")
	}
}

func TestPoolPinnedSkippedDuringEviction(t *testing.T) {
	pool, _, fake, clock := newTestPool(4)
	ctx := context.Background()

	sb1, _ := pool.GetOrResume(ctx, "u-1")
	pool.Pin(sb1.ID)
	clock.Tick(time.Second)
	sb2, _ := pool.GetOrResume(ctx, "u-2")
	clock.Tick(time.Second)
	_, _ = pool.GetOrResume(ctx, "u-3")
	clock.Tick(time.Second)
	_, _ = pool.GetOrResume(ctx, "u-4")
	clock.Tick(time.Second)
	_, _ = pool.GetOrResume(ctx, "u-5")
	if fake.StopCalls[sb1.ID] != 0 {
		t.Errorf("u-1 was pinned but got hibernated: %v", fake.StopCalls)
	}
	if fake.StopCalls[sb2.ID] == 0 {
		t.Errorf("expected u-2 hibernated, stop_calls=%v", fake.StopCalls)
	}
}

func TestPoolTouchUpdatesLRUOrder(t *testing.T) {
	pool, _, _, clock := newTestPool(4)
	ctx := context.Background()
	sb1, _ := pool.GetOrResume(ctx, "u-1")
	clock.Tick(5 * time.Second)
	sb2, _ := pool.GetOrResume(ctx, "u-2")
	clock.Tick(5 * time.Second)
	pool.Touch(sb1.ID)
	items := pool.ActiveItems()
	if items[0].SandboxID != sb2.ID {
		t.Errorf("expected sb2 LRU, got %s", items[0].SandboxID)
	}
	if items[len(items)-1].SandboxID != sb1.ID {
		t.Errorf("expected sb1 MRU, got %s", items[len(items)-1].SandboxID)
	}
}

func TestPoolRemoveDropsUser(t *testing.T) {
	pool, _, _, _ := newTestPool(4)
	ctx := context.Background()
	sb1, _ := pool.GetOrResume(ctx, "u-1")
	if pool.ActiveCount() != 1 {
		t.Fatalf("active_count = %d", pool.ActiveCount())
	}
	pool.Remove("u-1")
	if pool.ActiveCount() != 0 {
		t.Errorf("active_count = %d", pool.ActiveCount())
	}
	if !pool.LastActive(sb1.ID).IsZero() {
		t.Errorf("expected last_active cleared")
	}
}

func TestPoolHibernateRemovesFromActive(t *testing.T) {
	pool, _, fake, _ := newTestPool(4)
	ctx := context.Background()
	sb, _ := pool.GetOrResume(ctx, "u-1")
	if err := pool.Hibernate(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	if pool.ActiveCount() != 0 {
		t.Errorf("active_count = %d", pool.ActiveCount())
	}
	if fake.StopCalls[sb.ID] != 1 {
		t.Errorf("stop_calls = %v", fake.StopCalls)
	}
}

func TestLruCandidatesSortsByLastActive(t *testing.T) {
	now := time.Now()
	items := []ActiveItem{
		{UserID: "a", SandboxID: "sb-a", LastActive: now.Add(2 * time.Second)},
		{UserID: "b", SandboxID: "sb-b", LastActive: now},
		{UserID: "c", SandboxID: "sb-c", LastActive: now.Add(time.Second)},
	}
	sorted := LruCandidates(items)
	if sorted[0].UserID != "b" || sorted[1].UserID != "c" || sorted[2].UserID != "a" {
		t.Errorf("unexpected sort order: %+v", sorted)
	}
}
