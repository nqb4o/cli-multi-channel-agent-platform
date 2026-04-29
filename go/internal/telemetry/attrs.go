// Package telemetry — Go port of platform_telemetry (F14).
//
// Canonical span / attribute / metric names. The contents of this file are
// FROZEN: they are wire-compatible with the Python reference implementation
// in packages/telemetry/src/platform_telemetry/attrs.py. Both Python and Go
// services emit into the same Tempo backend, so renames must be coordinated
// across the dashboard layer + a deprecation cycle.
package telemetry

// ---------------------------------------------------------------------------
// Canonical span tree (FROZEN)
//
//	gateway.handle_webhook
//	└── gateway.enqueue
//	    └── orchestrator.run
//	        ├── orchestrator.resume_sandbox
//	        └── orchestrator.exec_runtime
//	            └── runtime.run_loop
//	                ├── runtime.skill_resolve
//	                ├── runtime.mcp_bridge_start
//	                ├── runtime.cli_turn
//	                │   ├── runtime.cli_spawn
//	                │   └── runtime.cli_parse
//	                ├── runtime.tool_call
//	                └── runtime.respond
// ---------------------------------------------------------------------------

const (
	SpanGatewayHandleWebhook = "gateway.handle_webhook"
	SpanGatewayEnqueue       = "gateway.enqueue"

	SpanOrchestratorRun            = "orchestrator.run"
	SpanOrchestratorResumeSandbox  = "orchestrator.resume_sandbox"
	SpanOrchestratorExecRuntime    = "orchestrator.exec_runtime"

	SpanRuntimeRunLoop         = "runtime.run_loop"
	SpanRuntimeSkillResolve    = "runtime.skill_resolve"
	SpanRuntimeMCPBridgeStart  = "runtime.mcp_bridge_start"
	SpanRuntimeCliTurn         = "runtime.cli_turn"
	SpanRuntimeCliSpawn        = "runtime.cli_spawn"
	SpanRuntimeCliParse        = "runtime.cli_parse"
	SpanRuntimeToolCall        = "runtime.tool_call"
	SpanRuntimeRespond         = "runtime.respond"
)

// CanonicalSpanTree is the ordered list of all 13 canonical span names. Used
// by the lockdown test and replay tests.
var CanonicalSpanTree = []string{
	SpanGatewayHandleWebhook,
	SpanGatewayEnqueue,
	SpanOrchestratorRun,
	SpanOrchestratorResumeSandbox,
	SpanOrchestratorExecRuntime,
	SpanRuntimeRunLoop,
	SpanRuntimeSkillResolve,
	SpanRuntimeMCPBridgeStart,
	SpanRuntimeCliTurn,
	SpanRuntimeCliSpawn,
	SpanRuntimeCliParse,
	SpanRuntimeToolCall,
	SpanRuntimeRespond,
}

// ---------------------------------------------------------------------------
// Canonical span attributes (platform.*)
// ---------------------------------------------------------------------------

const (
	PlatformUserID    = "platform.user.id"
	PlatformAgentID   = "platform.agent.id"
	PlatformChannelID = "platform.channel.id"
	PlatformThreadID  = "platform.thread.id"
	PlatformSessionID = "platform.session.id"
	PlatformRunID     = "platform.run.id"

	PlatformProvider         = "platform.provider"
	PlatformProviderFallback = "platform.provider.fallback"
	PlatformModel            = "platform.model"

	PlatformSandboxID        = "platform.sandbox.id"
	PlatformSandboxState     = "platform.sandbox.state"
	PlatformSandboxColdStart = "platform.sandbox.cold_start"

	PlatformSkillID      = "platform.skill.id"
	PlatformSkillVersion = "platform.skill.version"
	PlatformSkillSource  = "platform.skill.source"

	PlatformToolName   = "platform.tool.name"
	PlatformToolResult = "platform.tool.result"

	PlatformCliArgvHash = "platform.cli.argv_hash"
	PlatformCliExitCode = "platform.cli.exit_code"
	PlatformCliBytesOut = "platform.cli.bytes_out"

	PlatformErrorClass   = "platform.error.class"
	PlatformErrorMessage = "platform.error.message"
)

// ---------------------------------------------------------------------------
// Canonical metric names (FROZEN)
// ---------------------------------------------------------------------------

const (
	MetricAgentRunsTotal           = "agent_runs_total"
	MetricAgentRunLatencySeconds   = "agent_run_latency_seconds"
	MetricCliTurnLatencySeconds    = "cli_turn_latency_seconds"
	MetricSandboxColdStartSeconds  = "sandbox_cold_start_seconds"
	MetricMCPToolCallTotal         = "mcp_tool_call_total"
)

// CanonicalMetrics is the ordered list of the five metric names. Order matches
// the Python tuple so the cross-language diff stays trivial.
var CanonicalMetrics = []string{
	MetricAgentRunsTotal,
	MetricAgentRunLatencySeconds,
	MetricCliTurnLatencySeconds,
	MetricSandboxColdStartSeconds,
	MetricMCPToolCallTotal,
}
