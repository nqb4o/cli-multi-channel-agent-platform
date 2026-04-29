// Five canonical metric helpers (Go port of metrics.py).
//
// Each helper memoises the instrument so callers can `telemetry.AgentRunsTotal()
// .Add(ctx, 1, metric.WithAttributes(...))` without paying the meter lookup
// cost on every call. Names + units are FROZEN — Grafana dashboards depend on
// them.
package telemetry

import (
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

const meterName = "platform_telemetry"

var (
	instMu sync.Mutex

	cAgentRunsTotal           metric.Int64Counter
	hAgentRunLatencySeconds   metric.Float64Histogram
	hCliTurnLatencySeconds    metric.Float64Histogram
	hSandboxColdStartSeconds  metric.Float64Histogram
	cMCPToolCallTotal         metric.Int64Counter
)

func meter() metric.Meter {
	return otel.Meter(meterName)
}

// resetInstruments drops all memoised instruments — called by ForceReset.
func resetInstruments() {
	instMu.Lock()
	defer instMu.Unlock()
	cAgentRunsTotal = nil
	hAgentRunLatencySeconds = nil
	hCliTurnLatencySeconds = nil
	hSandboxColdStartSeconds = nil
	cMCPToolCallTotal = nil
}

// ResetInstruments is the exported alias used by tests.
func ResetInstruments() { resetInstruments() }

// AgentRunsTotal — total agent runs. Labels: provider, agent_id, outcome.
func AgentRunsTotal() metric.Int64Counter {
	instMu.Lock()
	defer instMu.Unlock()
	if cAgentRunsTotal != nil {
		return cAgentRunsTotal
	}
	c, err := meter().Int64Counter(
		MetricAgentRunsTotal,
		metric.WithDescription("Total number of agent runs"),
		metric.WithUnit("1"),
	)
	if err != nil {
		panic(err) // unreachable with the SDK meter; preserved for parity.
	}
	cAgentRunsTotal = c
	return c
}

// AgentRunLatencySeconds — end-to-end agent-run latency in seconds.
func AgentRunLatencySeconds() metric.Float64Histogram {
	instMu.Lock()
	defer instMu.Unlock()
	if hAgentRunLatencySeconds != nil {
		return hAgentRunLatencySeconds
	}
	h, err := meter().Float64Histogram(
		MetricAgentRunLatencySeconds,
		metric.WithDescription("End-to-end agent-run latency in seconds"),
		metric.WithUnit("s"),
	)
	if err != nil {
		panic(err)
	}
	hAgentRunLatencySeconds = h
	return h
}

// CliTurnLatencySeconds — CLI subprocess turn latency in seconds.
func CliTurnLatencySeconds() metric.Float64Histogram {
	instMu.Lock()
	defer instMu.Unlock()
	if hCliTurnLatencySeconds != nil {
		return hCliTurnLatencySeconds
	}
	h, err := meter().Float64Histogram(
		MetricCliTurnLatencySeconds,
		metric.WithDescription("CLI subprocess turn latency in seconds"),
		metric.WithUnit("s"),
	)
	if err != nil {
		panic(err)
	}
	hCliTurnLatencySeconds = h
	return h
}

// SandboxColdStartSeconds — sandbox resume / cold-start latency in seconds.
func SandboxColdStartSeconds() metric.Float64Histogram {
	instMu.Lock()
	defer instMu.Unlock()
	if hSandboxColdStartSeconds != nil {
		return hSandboxColdStartSeconds
	}
	h, err := meter().Float64Histogram(
		MetricSandboxColdStartSeconds,
		metric.WithDescription("Sandbox resume / cold-start latency in seconds"),
		metric.WithUnit("s"),
	)
	if err != nil {
		panic(err)
	}
	hSandboxColdStartSeconds = h
	return h
}

// MCPToolCallTotal — total MCP tool calls.
func MCPToolCallTotal() metric.Int64Counter {
	instMu.Lock()
	defer instMu.Unlock()
	if cMCPToolCallTotal != nil {
		return cMCPToolCallTotal
	}
	c, err := meter().Int64Counter(
		MetricMCPToolCallTotal,
		metric.WithDescription("Total number of MCP tool calls"),
		metric.WithUnit("1"),
	)
	if err != nil {
		panic(err)
	}
	cMCPToolCallTotal = c
	return c
}

// AllInstruments materialises and returns all five instruments. Useful for
// warmup at process boot.
func AllInstruments() map[string]any {
	return map[string]any{
		MetricAgentRunsTotal:          AgentRunsTotal(),
		MetricAgentRunLatencySeconds:  AgentRunLatencySeconds(),
		MetricCliTurnLatencySeconds:   CliTurnLatencySeconds(),
		MetricSandboxColdStartSeconds: SandboxColdStartSeconds(),
		MetricMCPToolCallTotal:        MCPToolCallTotal(),
	}
}
