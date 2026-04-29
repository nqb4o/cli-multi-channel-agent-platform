package telemetry

import (
	"strings"
	"testing"
)

// initIsolated wraps Init with the same fixture semantics the Python tests
// get from conftest.py — full reset, no env-var bleed, ratio=1.0 unless
// overridden.
func initIsolated(t *testing.T, name string) {
	t.Helper()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "")
	ForceReset()
	ResetDefaultRegistry()
	if err := Init(name, InitOptions{ForceReset: true}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() {
		ForceReset()
		ResetDefaultRegistry()
	})
}

func TestInitInstallsProviders(t *testing.T) {
	initIsolated(t, "telemetry-tests")
	if got := GetServiceName(); got != "telemetry-tests" {
		t.Fatalf("service name: got %q, want telemetry-tests", got)
	}
	if GetTracerProvider() == nil {
		t.Fatal("tracer provider not set")
	}
	if GetMeterProvider() == nil {
		t.Fatal("meter provider not set")
	}
	if GetInMemorySpanExporter() == nil {
		t.Fatal("in-memory span exporter not set")
	}
	if GetInMemoryMetricReader() == nil {
		t.Fatal("in-memory metric reader not set")
	}
}

func TestInitFirstCallWins(t *testing.T) {
	initIsolated(t, "first-name")
	// Re-init with the same name is a no-op.
	if err := Init("first-name"); err != nil {
		t.Fatalf("repeat Init(same name): unexpected err %v", err)
	}
	// Re-init with a different name is rejected.
	err := Init("different-name")
	if err == nil {
		t.Fatal("expected error when re-initialising with different service name")
	}
	if !strings.Contains(err.Error(), "first-name") {
		t.Fatalf("error message %q should mention the existing name", err.Error())
	}
}

func TestInitForceResetReinstalls(t *testing.T) {
	initIsolated(t, "before-reset")
	if err := Init("after-reset", InitOptions{ForceReset: true}); err != nil {
		t.Fatalf("force-reset Init: %v", err)
	}
	if got := GetServiceName(); got != "after-reset" {
		t.Fatalf("service name after force-reset: got %q", got)
	}
}

func TestInitRejectsEmptyName(t *testing.T) {
	ForceReset()
	defer ForceReset()
	if err := Init(""); err == nil {
		t.Fatal("expected error for empty service name")
	}
}

func TestErrorAwareSamplerRejectsBadRatio(t *testing.T) {
	if _, err := NewErrorAwareRatioSampler(-0.1); err == nil {
		t.Fatal("ratio < 0 should error")
	}
	if _, err := NewErrorAwareRatioSampler(1.5); err == nil {
		t.Fatal("ratio > 1 should error")
	}
}

func TestForceResetClearsState(t *testing.T) {
	initIsolated(t, "to-clear")
	ForceReset()
	if GetServiceName() != "" {
		t.Fatal("service name should be empty after ForceReset")
	}
	if GetTracerProvider() != nil {
		t.Fatal("tracer provider should be nil after ForceReset")
	}
}
