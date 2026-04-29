package telemetry

import (
	"math"
	"testing"
)

func TestOutcomesEnumValues(t *testing.T) {
	if HealthOutcomeHealthy != "healthy" {
		t.Fatalf("HEALTHY enum drift: %q", HealthOutcomeHealthy)
	}
	if HealthOutcomeDegraded != "degraded" {
		t.Fatalf("DEGRADED enum drift: %q", HealthOutcomeDegraded)
	}
	if HealthOutcomeUnhealthy != "unhealthy" {
		t.Fatalf("UNHEALTHY enum drift: %q", HealthOutcomeUnhealthy)
	}
}

func TestEmptyTrackerReportsHealthy(t *testing.T) {
	tr, err := NewProviderHealthTracker("claude")
	if err != nil {
		t.Fatal(err)
	}
	snap := tr.Snapshot()
	if snap.Outcome != HealthOutcomeHealthy {
		t.Fatalf("empty tracker outcome: %q", snap.Outcome)
	}
	if snap.FailureRate != 0.0 {
		t.Fatalf("empty failure rate: %v", snap.FailureRate)
	}
	if snap.SampleSize != 0 {
		t.Fatalf("empty sample size: %d", snap.SampleSize)
	}
	if snap.HasFailure {
		t.Fatal("HasFailure should be false on empty tracker")
	}
}

func TestAllSuccessesIsHealthy(t *testing.T) {
	clock := 1000.0
	tr, err := NewProviderHealthTrackerWithConfig("claude", ProviderHealthTrackerConfig{
		WindowSeconds: 60.0,
		Clock:         func() float64 { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		tr.RecordSuccess()
		clock += 1.0
	}
	snap := tr.Snapshot()
	if snap.Outcome != HealthOutcomeHealthy {
		t.Fatalf("outcome: %q", snap.Outcome)
	}
	if snap.FailureRate != 0.0 {
		t.Fatalf("failure rate: %v", snap.FailureRate)
	}
	if snap.SampleSize != 10 {
		t.Fatalf("sample size: %d", snap.SampleSize)
	}
	if snap.LastSuccessAt != 1009.0 {
		t.Fatalf("last_success_at: %v want 1009.0", snap.LastSuccessAt)
	}
}

func TestHighFailureRateIsUnhealthy(t *testing.T) {
	clock := 1000.0
	tr, err := NewProviderHealthTrackerWithConfig("claude", ProviderHealthTrackerConfig{
		WindowSeconds:      60.0,
		DegradedThreshold:  0.1,
		UnhealthyThreshold: 0.5,
		Clock:              func() float64 { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		tr.RecordFailure()
		clock += 1.0
	}
	for i := 0; i < 2; i++ {
		tr.RecordSuccess()
		clock += 1.0
	}
	snap := tr.Snapshot()
	if snap.Outcome != HealthOutcomeUnhealthy {
		t.Fatalf("outcome: %q", snap.Outcome)
	}
	if math.Abs(snap.FailureRate-0.8) > 0.001 {
		t.Fatalf("failure rate: %v", snap.FailureRate)
	}
	if snap.SampleSize != 10 {
		t.Fatalf("sample size: %d", snap.SampleSize)
	}
}

func TestLowFailureRateIsDegraded(t *testing.T) {
	clock := 1000.0
	tr, err := NewProviderHealthTrackerWithConfig("claude", ProviderHealthTrackerConfig{
		WindowSeconds:      60.0,
		DegradedThreshold:  0.1,
		UnhealthyThreshold: 0.5,
		Clock:              func() float64 { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		tr.RecordFailure()
		clock += 1.0
	}
	for i := 0; i < 8; i++ {
		tr.RecordSuccess()
		clock += 1.0
	}
	snap := tr.Snapshot()
	if snap.Outcome != HealthOutcomeDegraded {
		t.Fatalf("outcome: %q", snap.Outcome)
	}
	if math.Abs(snap.FailureRate-0.2) > 0.001 {
		t.Fatalf("failure rate: %v", snap.FailureRate)
	}
}

func TestOldSamplesAreEvictedFromWindow(t *testing.T) {
	clock := 1000.0
	tr, err := NewProviderHealthTrackerWithConfig("claude", ProviderHealthTrackerConfig{
		WindowSeconds: 60.0,
		Clock:         func() float64 { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		tr.RecordFailure()
		clock += 1.0
	}
	clock += 120.0 // well past the window
	for i := 0; i < 5; i++ {
		tr.RecordSuccess()
		clock += 1.0
	}
	snap := tr.Snapshot()
	if snap.SampleSize != 5 {
		t.Fatalf("sample size after eviction: %d", snap.SampleSize)
	}
	if snap.FailureRate != 0.0 {
		t.Fatalf("failure rate after eviction: %v", snap.FailureRate)
	}
	if snap.Outcome != HealthOutcomeHealthy {
		t.Fatalf("outcome after eviction: %q", snap.Outcome)
	}
}

func TestUnhealthyRequiresMinSamples(t *testing.T) {
	clock := 1000.0
	tr, err := NewProviderHealthTrackerWithConfig("claude", ProviderHealthTrackerConfig{
		MinSamplesForUnhealthy: 5,
		Clock:                  func() float64 { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	tr.RecordFailure()
	clock += 1.0
	tr.RecordFailure()
	snap := tr.Snapshot()
	if snap.SampleSize != 2 {
		t.Fatalf("sample size: %d", snap.SampleSize)
	}
	// 100% failure but only 2 samples → still DEGRADED, not UNHEALTHY.
	if snap.Outcome != HealthOutcomeDegraded {
		t.Fatalf("outcome should be DEGRADED with too few samples; got %q", snap.Outcome)
	}
}

func TestThresholdValidation(t *testing.T) {
	_, err := NewProviderHealthTrackerWithConfig("x", ProviderHealthTrackerConfig{
		DegradedThreshold:  0.7,
		UnhealthyThreshold: 0.5,
	})
	if err == nil {
		t.Fatal("inverted thresholds should error")
	}
	_, err = NewProviderHealthTrackerWithConfig("x", ProviderHealthTrackerConfig{
		WindowSeconds:      -1.0,
		DegradedThreshold:  0.1,
		UnhealthyThreshold: 0.5,
	})
	if err == nil {
		t.Fatal("non-positive WindowSeconds should error")
	}
}

func TestProviderHealthTrackerSatisfiesProbe(t *testing.T) {
	tr, err := NewProviderHealthTracker("claude")
	if err != nil {
		t.Fatal(err)
	}
	var probe HealthProbe = tr // compile-time interface check
	if probe.ProviderID() != "claude" {
		t.Fatalf("provider id: %q", probe.ProviderID())
	}
	if probe.Snapshot().ProviderID != "claude" {
		t.Fatal("snapshot.ProviderID mismatch")
	}
}

func TestRegistryRegisterAndLookup(t *testing.T) {
	reg := NewHealthRegistry()
	tr, _ := NewProviderHealthTracker("claude")
	if err := reg.Register(tr); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !reg.Contains("claude") {
		t.Fatal("registry should contain claude")
	}
	if reg.Get("claude") != tr {
		t.Fatal("registry Get should return same tracker")
	}
	if reg.Len() != 1 {
		t.Fatalf("registry len: %d", reg.Len())
	}
}

func TestRegistryRejectsDuplicateID(t *testing.T) {
	reg := NewHealthRegistry()
	t1, _ := NewProviderHealthTracker("claude")
	t2, _ := NewProviderHealthTracker("claude")
	if err := reg.Register(t1); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(t2); err == nil {
		t.Fatal("duplicate provider id should error")
	}
	// Re-registering the same object is a no-op.
	if err := reg.Register(t1); err != nil {
		t.Fatalf("re-register same: %v", err)
	}
}

func TestRegistryUnregister(t *testing.T) {
	reg := NewHealthRegistry()
	tr, _ := NewProviderHealthTracker("claude")
	_ = reg.Register(tr)
	reg.Unregister("claude")
	if reg.Contains("claude") {
		t.Fatal("unregister should remove probe")
	}
	if reg.Get("claude") != nil {
		t.Fatal("Get should return nil after unregister")
	}
	// Unregistering a missing id is a no-op.
	reg.Unregister("missing")
}

func TestRegistrySnapshotAll(t *testing.T) {
	reg := NewHealthRegistry()
	clock := 1000.0
	a, _ := NewProviderHealthTrackerWithConfig("claude", ProviderHealthTrackerConfig{Clock: func() float64 { return clock }})
	b, _ := NewProviderHealthTrackerWithConfig("codex", ProviderHealthTrackerConfig{Clock: func() float64 { return clock }})
	_ = reg.Register(a)
	_ = reg.Register(b)
	a.RecordFailure()
	b.RecordSuccess()
	snaps := reg.SnapshotAll()
	if len(snaps) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snaps))
	}
	if _, ok := snaps["claude"]; !ok {
		t.Fatal("missing claude snapshot")
	}
	if _, ok := snaps["codex"]; !ok {
		t.Fatal("missing codex snapshot")
	}
}

func TestCustomProbeImplementsInterface(t *testing.T) {
	probe := &fixedProbe{id: "fixed"}
	reg := NewHealthRegistry()
	if err := reg.Register(probe); err != nil {
		t.Fatal(err)
	}
	got := reg.Get("fixed")
	if got == nil {
		t.Fatal("registered probe should be retrievable")
	}
	if got.Snapshot().Outcome != HealthOutcomeHealthy {
		t.Fatal("custom probe snapshot mismatch")
	}
}

type fixedProbe struct{ id string }

func (f *fixedProbe) ProviderID() string { return f.id }
func (f *fixedProbe) Snapshot() HealthSnapshot {
	return HealthSnapshot{
		ProviderID:    f.id,
		Outcome:       HealthOutcomeHealthy,
		WindowSeconds: 60.0,
	}
}

func TestTrackerResetClearsState(t *testing.T) {
	tr, _ := NewProviderHealthTracker("claude")
	tr.RecordFailure()
	tr.RecordFailure()
	tr.Reset()
	snap := tr.Snapshot()
	if snap.SampleSize != 0 {
		t.Fatalf("sample size after reset: %d", snap.SampleSize)
	}
	if snap.HasFailure {
		t.Fatal("HasFailure should be false after reset")
	}
	if snap.Outcome != HealthOutcomeHealthy {
		t.Fatalf("outcome after reset: %q", snap.Outcome)
	}
}

func TestRegistryRejectsEmptyProviderID(t *testing.T) {
	reg := NewHealthRegistry()
	probe := &fixedProbe{id: ""}
	if err := reg.Register(probe); err == nil {
		t.Fatal("empty provider id should error")
	}
}

func TestDefaultRegistryLazySingleton(t *testing.T) {
	ResetDefaultRegistry()
	a := DefaultRegistry()
	b := DefaultRegistry()
	if a != b {
		t.Fatal("DefaultRegistry should return the same instance across calls")
	}
	ResetDefaultRegistry()
	c := DefaultRegistry()
	if c == a {
		t.Fatal("after ResetDefaultRegistry a fresh instance is expected")
	}
}
