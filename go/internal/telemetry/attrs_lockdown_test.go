// Lock-down test for canonical names — must match Python's test_attrs.py.
//
// The constants in attrs.go are FROZEN: changes need a coordinated dashboard
// rev. The test asserts both the literal values and the count, so a careless
// rename or deletion is caught at PR time.
package telemetry

import (
	"reflect"
	"regexp"
	"testing"
)

func TestSpanTreeHas13CanonicalSpans(t *testing.T) {
	if got := len(CanonicalSpanTree); got != 13 {
		t.Fatalf("expected 13 canonical spans, got %d", got)
	}
	seen := map[string]bool{}
	for _, name := range CanonicalSpanTree {
		if seen[name] {
			t.Fatalf("duplicate span name in CanonicalSpanTree: %q", name)
		}
		seen[name] = true
	}
}

func TestSpanNamesMatchBrief(t *testing.T) {
	expected := []string{
		"gateway.handle_webhook",
		"gateway.enqueue",
		"orchestrator.run",
		"orchestrator.resume_sandbox",
		"orchestrator.exec_runtime",
		"runtime.run_loop",
		"runtime.skill_resolve",
		"runtime.mcp_bridge_start",
		"runtime.cli_turn",
		"runtime.cli_spawn",
		"runtime.cli_parse",
		"runtime.tool_call",
		"runtime.respond",
	}
	if !reflect.DeepEqual(CanonicalSpanTree, expected) {
		t.Fatalf("CanonicalSpanTree drift\n got: %v\nwant: %v", CanonicalSpanTree, expected)
	}
}

func TestMetricNamesMatchBrief(t *testing.T) {
	expected := []string{
		"agent_runs_total",
		"agent_run_latency_seconds",
		"cli_turn_latency_seconds",
		"sandbox_cold_start_seconds",
		"mcp_tool_call_total",
	}
	if !reflect.DeepEqual(CanonicalMetrics, expected) {
		t.Fatalf("CanonicalMetrics drift\n got: %v\nwant: %v", CanonicalMetrics, expected)
	}
}

// TestPlatformAttrsUseDottedNamespace mirrors Python's regex check —
// every platform.* constant must live under the platform.<group>.<leaf>
// namespace so dashboards have a single prefix to filter on.
func TestPlatformAttrsUseDottedNamespace(t *testing.T) {
	pat := regexp.MustCompile(`^platform\.[a-z][a-z0-9_]*(\.[a-z0-9_]+)*$`)
	for name, value := range platformAttrConstants() {
		if !pat.MatchString(value) {
			t.Errorf("%s = %q does not match platform.* namespace", name, value)
		}
		dots := 0
		for _, c := range value {
			if c == '.' {
				dots++
			}
		}
		if dots < 1 {
			t.Errorf("%s = %q missing namespace separator", name, value)
		}
	}
}

func TestErrorClassAttributeConstant(t *testing.T) {
	// Hard-coded by the sampler — must stay "platform.error.class".
	if PlatformErrorClass != "platform.error.class" {
		t.Fatalf("PlatformErrorClass drift: got %q", PlatformErrorClass)
	}
}

func TestConstantsAreStrings(t *testing.T) {
	// Every Span* / Platform* / Metric* constant must be a string. Trivially
	// true in Go (constants are typed) — the test exists for parity with
	// Python's lockdown which had to assert isinstance(value, str) at
	// runtime.
	allConstants := map[string]string{}
	for k, v := range spanConstants() {
		allConstants[k] = v
	}
	for k, v := range platformAttrConstants() {
		allConstants[k] = v
	}
	for k, v := range metricConstants() {
		allConstants[k] = v
	}
	if len(allConstants) == 0 {
		t.Fatal("expected at least one constant, got 0")
	}
}

// platformAttrConstants returns the canonical platform.* attribute names.
// Listed explicitly (rather than via reflection on a package var) so a new
// constant has to be enrolled here on the same PR — preserving the
// "lockdown" intent of Python's test that walks dir(attrs).
func platformAttrConstants() map[string]string {
	return map[string]string{
		"PlatformUserID":           PlatformUserID,
		"PlatformAgentID":          PlatformAgentID,
		"PlatformChannelID":        PlatformChannelID,
		"PlatformThreadID":         PlatformThreadID,
		"PlatformSessionID":        PlatformSessionID,
		"PlatformRunID":            PlatformRunID,
		"PlatformProvider":         PlatformProvider,
		"PlatformProviderFallback": PlatformProviderFallback,
		"PlatformModel":            PlatformModel,
		"PlatformSandboxID":        PlatformSandboxID,
		"PlatformSandboxState":     PlatformSandboxState,
		"PlatformSandboxColdStart": PlatformSandboxColdStart,
		"PlatformSkillID":          PlatformSkillID,
		"PlatformSkillVersion":     PlatformSkillVersion,
		"PlatformSkillSource":      PlatformSkillSource,
		"PlatformToolName":         PlatformToolName,
		"PlatformToolResult":       PlatformToolResult,
		"PlatformCliArgvHash":      PlatformCliArgvHash,
		"PlatformCliExitCode":      PlatformCliExitCode,
		"PlatformCliBytesOut":      PlatformCliBytesOut,
		"PlatformErrorClass":       PlatformErrorClass,
		"PlatformErrorMessage":     PlatformErrorMessage,
	}
}

func spanConstants() map[string]string {
	return map[string]string{
		"SpanGatewayHandleWebhook":      SpanGatewayHandleWebhook,
		"SpanGatewayEnqueue":            SpanGatewayEnqueue,
		"SpanOrchestratorRun":           SpanOrchestratorRun,
		"SpanOrchestratorResumeSandbox": SpanOrchestratorResumeSandbox,
		"SpanOrchestratorExecRuntime":   SpanOrchestratorExecRuntime,
		"SpanRuntimeRunLoop":            SpanRuntimeRunLoop,
		"SpanRuntimeSkillResolve":       SpanRuntimeSkillResolve,
		"SpanRuntimeMCPBridgeStart":     SpanRuntimeMCPBridgeStart,
		"SpanRuntimeCliTurn":            SpanRuntimeCliTurn,
		"SpanRuntimeCliSpawn":           SpanRuntimeCliSpawn,
		"SpanRuntimeCliParse":           SpanRuntimeCliParse,
		"SpanRuntimeToolCall":           SpanRuntimeToolCall,
		"SpanRuntimeRespond":            SpanRuntimeRespond,
	}
}

func metricConstants() map[string]string {
	return map[string]string{
		"MetricAgentRunsTotal":          MetricAgentRunsTotal,
		"MetricAgentRunLatencySeconds":  MetricAgentRunLatencySeconds,
		"MetricCliTurnLatencySeconds":   MetricCliTurnLatencySeconds,
		"MetricSandboxColdStartSeconds": MetricSandboxColdStartSeconds,
		"MetricMCPToolCallTotal":        MetricMCPToolCallTotal,
	}
}
