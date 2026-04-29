package orchestrator

import (
	"context"
	"testing"
	"time"
)

type staticTierResolver struct {
	tiers map[string]string
}

func (s staticTierResolver) TierFor(_ context.Context, userID string) (string, error) {
	return s.tiers[userID], nil
}

func TestDefaultPolicyThresholds(t *testing.T) {
	policy := DefaultHibernatePolicy()
	if policy.ThresholdFor("free") != DefaultFreeIdleS {
		t.Errorf("free = %v", policy.ThresholdFor("free"))
	}
	if DefaultFreeIdleS != 120*time.Second {
		t.Errorf("free idle = %v", DefaultFreeIdleS)
	}
	if policy.ThresholdFor("default") != DefaultDefaultIdleS {
		t.Errorf("default = %v", policy.ThresholdFor("default"))
	}
	if DefaultDefaultIdleS != 300*time.Second {
		t.Errorf("default idle = %v", DefaultDefaultIdleS)
	}
	if policy.ThresholdFor("pro") != DefaultProIdleS {
		t.Errorf("pro = %v", policy.ThresholdFor("pro"))
	}
	if DefaultProIdleS != 900*time.Second {
		t.Errorf("pro idle = %v", DefaultProIdleS)
	}
	if policy.ThresholdFor("") != DefaultDefaultIdleS {
		t.Errorf("empty tier = %v", policy.ThresholdFor(""))
	}
	if policy.ThresholdFor("enterprise") != DefaultDefaultIdleS {
		t.Errorf("unknown tier = %v", policy.ThresholdFor("enterprise"))
	}
	if policy.ScanIntervalS != 30*time.Second {
		t.Errorf("scan_interval = %v", policy.ScanIntervalS)
	}
}

func TestIdleSandboxHibernatedAfterThreshold(t *testing.T) {
	pool, _, fake, clock := newTestPool(4)
	ctx := context.Background()
	sb, _ := pool.GetOrResume(ctx, "u-1")
	sched := NewHibernateScheduler(pool,
		WithTierResolver(staticTierResolver{tiers: map[string]string{"u-1": "default"}}),
		WithSchedulerClock(clock.Now),
	)

	// Just before threshold — no hibernation.
	clock.Tick(299 * time.Second)
	hibernated := sched.ScanOnce(ctx)
	if len(hibernated) != 0 {
		t.Errorf("unexpected hibernated %v", hibernated)
	}
	if fake.StopCalls[sb.ID] != 0 {
		t.Errorf("expected no stops, got %v", fake.StopCalls)
	}

	// Cross the threshold.
	clock.Tick(2 * time.Second)
	hibernated = sched.ScanOnce(ctx)
	if len(hibernated) != 1 || hibernated[0] != sb.ID {
		t.Errorf("expected [%s], got %v", sb.ID, hibernated)
	}
	if fake.StopCalls[sb.ID] == 0 {
		t.Errorf("expected stop, got %v", fake.StopCalls)
	}
	if pool.ActiveCount() != 0 {
		t.Errorf("active_count = %d", pool.ActiveCount())
	}
}

func TestPerTierThresholds(t *testing.T) {
	pool, _, _, clock := newTestPool(4)
	ctx := context.Background()
	sbFree, _ := pool.GetOrResume(ctx, "u-free")
	sbPro, _ := pool.GetOrResume(ctx, "u-pro")
	sched := NewHibernateScheduler(pool,
		WithTierResolver(staticTierResolver{tiers: map[string]string{"u-free": "free", "u-pro": "pro"}}),
		WithSchedulerClock(clock.Now),
	)

	clock.Tick(130 * time.Second)
	hibernated := sched.ScanOnce(ctx)
	if !contains(hibernated, sbFree.ID) {
		t.Errorf("expected free hibernated, got %v", hibernated)
	}
	if contains(hibernated, sbPro.ID) {
		t.Errorf("did not expect pro hibernated yet, got %v", hibernated)
	}

	clock.Tick(900 * time.Second)
	hibernated = sched.ScanOnce(ctx)
	if !contains(hibernated, sbPro.ID) {
		t.Errorf("expected pro hibernated, got %v", hibernated)
	}
}

func TestPinnedSandboxSkipped(t *testing.T) {
	pool, _, fake, clock := newTestPool(4)
	ctx := context.Background()
	sb, _ := pool.GetOrResume(ctx, "u-pinned")
	pool.Pin(sb.ID)
	sched := NewHibernateScheduler(pool,
		WithTierResolver(staticTierResolver{tiers: map[string]string{"u-pinned": "free"}}),
		WithSchedulerClock(clock.Now),
	)

	clock.Tick(10000 * time.Second)
	hibernated := sched.ScanOnce(ctx)
	if len(hibernated) != 0 {
		t.Errorf("expected no hibernated, got %v", hibernated)
	}
	if fake.StopCalls[sb.ID] != 0 {
		t.Errorf("expected no stops, got %v", fake.StopCalls)
	}
}

func TestSchedulerStartStopIdempotent(t *testing.T) {
	pool, _, _, _ := newTestPool(4)
	sched := NewHibernateScheduler(pool)
	sched.Start()
	sched.Start()
	sched.Stop()
	sched.Stop()
}

func TestDefaultTierResolverReturnsDefault(t *testing.T) {
	r := DefaultTierResolver{}
	tier, err := r.TierFor(context.Background(), "anyone")
	if err != nil {
		t.Fatal(err)
	}
	if tier != "default" {
		t.Errorf("got %q", tier)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
