package telemetry

import (
	"context"
	"testing"
)

// initWithRatio mirrors Python's `custom_sampler` fixture — fresh state,
// explicit sample ratio.
func initWithRatio(t *testing.T, ratio float64) {
	t.Helper()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "")
	ForceReset()
	ResetDefaultRegistry()
	r := ratio
	if err := Init("sampler-tests", InitOptions{ForceReset: true, SampleRatio: &r}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() {
		ForceReset()
		ResetDefaultRegistry()
	})
}

func TestZeroPercentDropsAllTraces(t *testing.T) {
	initWithRatio(t, 0.0)
	exp := GetInMemorySpanExporter()
	exp.Reset()

	ctx := context.Background()
	for i := 0; i < 20; i++ {
		_, end := StartSpan(ctx, "noisy.span")
		end(nil)
	}
	if got := len(exp.GetSpans()); got != 0 {
		t.Fatalf("expected 0 sampled spans at ratio 0.0, got %d", got)
	}
}

func TestOneHundredPercentKeepsAllTraces(t *testing.T) {
	initWithRatio(t, 1.0)
	exp := GetInMemorySpanExporter()
	exp.Reset()

	ctx := context.Background()
	for i := 0; i < 20; i++ {
		_, end := StartSpan(ctx, "loud.span")
		end(nil)
	}
	if got := len(exp.GetSpans()); got != 20 {
		t.Fatalf("expected 20 sampled spans at ratio 1.0, got %d", got)
	}
}

func TestPartialRatioKeepsRoughlyThatShare(t *testing.T) {
	initWithRatio(t, 0.5)
	exp := GetInMemorySpanExporter()
	exp.Reset()

	ctx := context.Background()
	const n = 400
	for i := 0; i < n; i++ {
		_, end := StartSpan(ctx, "medium.span")
		end(nil)
	}
	kept := len(exp.GetSpans())
	// TraceIDRatioBased is deterministic on trace_id so the share floats —
	// tolerate the same generous band as Python's test (0.25..0.75).
	if kept < int(0.25*n) || kept > int(0.75*n) {
		t.Fatalf("ratio drift: kept=%d/%d outside [0.25n, 0.75n]", kept, n)
	}
}

func TestErrorClassAttributeForcesSamplingAtZeroRatio(t *testing.T) {
	initWithRatio(t, 0.0)
	exp := GetInMemorySpanExporter()
	exp.Reset()

	ctx := context.Background()
	_, end := StartSpan(ctx, "doomed", Attrs{PlatformErrorClass: "ValueError"})
	end(nil)
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("error_class span should always be sampled — got %d spans", len(spans))
	}
	if attrString(spans[0].Attributes, PlatformErrorClass) != "ValueError" {
		t.Fatal("platform.error.class attribute should survive sampling")
	}
}

func TestNoErrorClassAtZeroRatioIsDropped(t *testing.T) {
	initWithRatio(t, 0.0)
	exp := GetInMemorySpanExporter()
	exp.Reset()

	ctx := context.Background()
	_, end := StartSpan(ctx, "clean.span")
	end(nil)
	if got := len(exp.GetSpans()); got != 0 {
		t.Fatalf("expected 0 spans for clean.span at ratio 0.0, got %d", got)
	}
}

func TestEmptyErrorClassDoesNotForceSampling(t *testing.T) {
	initWithRatio(t, 0.0)
	exp := GetInMemorySpanExporter()
	exp.Reset()

	ctx := context.Background()
	_, end := StartSpan(ctx, "fake.error", Attrs{PlatformErrorClass: ""})
	end(nil)
	if got := len(exp.GetSpans()); got != 0 {
		t.Fatalf("empty error_class should not force sampling; got %d spans", got)
	}
}
