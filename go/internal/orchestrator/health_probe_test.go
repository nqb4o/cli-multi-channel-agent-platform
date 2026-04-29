package orchestrator

import (
	"context"
	"testing"
	"time"
)

// fakeHealthProber adapts FakeDaytonaClient.PingHealth into HealthProber.
type fakeHealthProber struct {
	fake *FakeDaytonaClient
}

func (p fakeHealthProber) PingHealth(ctx context.Context, sandboxID string, timeout time.Duration) bool {
	return p.fake.PingHealth(ctx, sandboxID, timeout)
}

func TestHealthyNoRecreate(t *testing.T) {
	pool, _, fake, _ := newTestPool(4)
	ctx := context.Background()
	sb, _ := pool.GetOrResume(ctx, "u-1")
	loop := NewHealthProbeLoop(pool, fakeHealthProber{fake: fake}, WithFailureGrace(3))

	recreated := loop.ScanOnce(ctx)
	if len(recreated) != 0 {
		t.Errorf("recreated = %v", recreated)
	}
	stats := loop.Stats()
	if stats.Probes != 1 {
		t.Errorf("probes = %d", stats.Probes)
	}
	if stats.Failures != 0 {
		t.Errorf("failures = %d", stats.Failures)
	}
	if fake.DeleteCalls[sb.ID] != 0 {
		t.Errorf("expected no delete, got %v", fake.DeleteCalls)
	}
}

func TestGracePeriodThenForceRecreate(t *testing.T) {
	// Acceptance criterion #7.
	pool, _, fake, _ := newTestPool(4)
	ctx := context.Background()
	sb, _ := pool.GetOrResume(ctx, "u-1")
	fake.MarkDead(sb.ID)
	loop := NewHealthProbeLoop(pool, fakeHealthProber{fake: fake}, WithFailureGrace(3))

	r1 := loop.ScanOnce(ctx)
	r2 := loop.ScanOnce(ctx)
	if len(r1) != 0 || len(r2) != 0 {
		t.Errorf("expected no recreate yet, got %v %v", r1, r2)
	}
	if fake.DeleteCalls[sb.ID] != 0 {
		t.Errorf("unexpected delete: %v", fake.DeleteCalls)
	}
	stats := loop.Stats()
	if stats.Failures != 2 {
		t.Errorf("failures = %d", stats.Failures)
	}
	if stats.Recreates != 0 {
		t.Errorf("recreates = %d", stats.Recreates)
	}

	// Third failure crosses the grace threshold.
	r3 := loop.ScanOnce(ctx)
	if len(r3) != 1 || r3[0] != sb.ID {
		t.Errorf("expected recreate of %s, got %v", sb.ID, r3)
	}
	if fake.DeleteCalls[sb.ID] != 1 {
		t.Errorf("expected delete, got %v", fake.DeleteCalls)
	}
	stats = loop.Stats()
	if stats.Recreates != 1 {
		t.Errorf("recreates = %d", stats.Recreates)
	}

	// User has a fresh sandbox now.
	items := pool.ActiveItems()
	if len(items) != 1 {
		t.Fatalf("active_items = %d, want 1", len(items))
	}
	if items[0].SandboxID == sb.ID {
		t.Error("expected new sandbox after recreate")
	}
}

func TestRecoveryResetsCounter(t *testing.T) {
	pool, _, fake, _ := newTestPool(4)
	ctx := context.Background()
	sb, _ := pool.GetOrResume(ctx, "u-1")
	fake.MarkDead(sb.ID)
	loop := NewHealthProbeLoop(pool, fakeHealthProber{fake: fake}, WithFailureGrace(3))

	loop.ScanOnce(ctx)
	loop.ScanOnce(ctx)
	if loop.Stats().ConsecutiveFailures[sb.ID] != 2 {
		t.Errorf("counter = %d", loop.Stats().ConsecutiveFailures[sb.ID])
	}

	fake.MarkAlive(sb.ID)
	loop.ScanOnce(ctx)
	if _, ok := loop.Stats().ConsecutiveFailures[sb.ID]; ok {
		t.Errorf("expected counter cleared")
	}

	fake.MarkDead(sb.ID)
	loop.ScanOnce(ctx)
	if loop.Stats().ConsecutiveFailures[sb.ID] != 1 {
		t.Errorf("counter = %d, want 1", loop.Stats().ConsecutiveFailures[sb.ID])
	}
	if fake.DeleteCalls[sb.ID] != 0 {
		t.Errorf("unexpected delete, got %v", fake.DeleteCalls)
	}
}

func TestHealthLoopStartStopIdempotent(t *testing.T) {
	pool, _, fake, _ := newTestPool(4)
	loop := NewHealthProbeLoop(pool, fakeHealthProber{fake: fake})
	loop.Start()
	loop.Start()
	loop.Stop()
	loop.Stop()
}

func TestHealthProbeDefaults(t *testing.T) {
	pool, _, fake, _ := newTestPool(4)
	loop := NewHealthProbeLoop(pool, fakeHealthProber{fake: fake})
	if loop.ProbeInterval() != 30*time.Second {
		t.Errorf("interval = %v", loop.ProbeInterval())
	}
	if loop.FailureGrace() != 3 {
		t.Errorf("grace = %d", loop.FailureGrace())
	}
}
