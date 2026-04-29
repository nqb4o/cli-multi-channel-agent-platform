package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func collectMetrics(t *testing.T) map[string]metricdata.Metrics {
	t.Helper()
	reader := GetInMemoryMetricReader()
	if reader == nil {
		t.Fatal("metric reader not initialised")
	}
	rm := metricdata.ResourceMetrics{}
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func TestCounterHelpersReturnCounters(t *testing.T) {
	initIsolated(t, "metrics-tests")
	if AgentRunsTotal() == nil {
		t.Fatal("AgentRunsTotal should not be nil")
	}
	if MCPToolCallTotal() == nil {
		t.Fatal("MCPToolCallTotal should not be nil")
	}
}

func TestHistogramHelpersReturnHistograms(t *testing.T) {
	initIsolated(t, "metrics-tests")
	if AgentRunLatencySeconds() == nil {
		t.Fatal("AgentRunLatencySeconds should not be nil")
	}
	if CliTurnLatencySeconds() == nil {
		t.Fatal("CliTurnLatencySeconds should not be nil")
	}
	if SandboxColdStartSeconds() == nil {
		t.Fatal("SandboxColdStartSeconds should not be nil")
	}
}

func TestHelpersAreMemoised(t *testing.T) {
	initIsolated(t, "metrics-tests")
	if AgentRunsTotal() != AgentRunsTotal() {
		t.Fatal("AgentRunsTotal should be memoised across calls")
	}
	if AgentRunLatencySeconds() != AgentRunLatencySeconds() {
		t.Fatal("AgentRunLatencySeconds should be memoised across calls")
	}
}

func TestRecordingEmitsMetricDataWithCanonicalNames(t *testing.T) {
	initIsolated(t, "metrics-tests")
	ctx := context.Background()

	AgentRunsTotal().Add(ctx, 1, metric.WithAttributes(
		attribute.String("provider", "claude"),
		attribute.String("outcome", "ok"),
	))
	AgentRunLatencySeconds().Record(ctx, 1.25, metric.WithAttributes(
		attribute.String("provider", "claude"),
	))
	CliTurnLatencySeconds().Record(ctx, 0.42, metric.WithAttributes(
		attribute.String("provider", "claude"),
		attribute.String("cli", "claude"),
	))
	SandboxColdStartSeconds().Record(ctx, 2.5, metric.WithAttributes(
		attribute.String("cold", "true"),
	))
	MCPToolCallTotal().Add(ctx, 1, metric.WithAttributes(
		attribute.String("tool", "search"),
		attribute.String("skill", "web"),
		attribute.String("outcome", "ok"),
	))

	seen := collectMetrics(t)
	for _, name := range CanonicalMetrics {
		if _, ok := seen[name]; !ok {
			t.Errorf("metric %q not emitted; got %v", name, keys(seen))
		}
	}
}

func TestAllInstrumentsReturnsFive(t *testing.T) {
	initIsolated(t, "metrics-tests")
	insts := AllInstruments()
	if len(insts) != 5 {
		t.Fatalf("expected 5 instruments, got %d", len(insts))
	}
	for _, name := range CanonicalMetrics {
		if _, ok := insts[name]; !ok {
			t.Errorf("missing %q in AllInstruments", name)
		}
	}
}

func TestHistogramUnitsAreSeconds(t *testing.T) {
	initIsolated(t, "metrics-tests")
	ctx := context.Background()
	AgentRunLatencySeconds().Record(ctx, 0.1)
	CliTurnLatencySeconds().Record(ctx, 0.1)
	SandboxColdStartSeconds().Record(ctx, 0.1)
	seen := collectMetrics(t)
	for _, name := range []string{
		MetricAgentRunLatencySeconds,
		MetricCliTurnLatencySeconds,
		MetricSandboxColdStartSeconds,
	} {
		m, ok := seen[name]
		if !ok {
			t.Fatalf("missing metric %q", name)
		}
		if m.Unit != "s" {
			t.Errorf("unit for %q: got %q, want %q", name, m.Unit, "s")
		}
	}
}

func keys(m map[string]metricdata.Metrics) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
