// Package clibackend defines the FROZEN contract every provider CLI
// backend implements: the [CliBackend] interface plus the three shared
// data structures [CliTurnInput], [CliTurnOutput], and [CliTurnError].
//
// This is a Go port of services/runtime/src/runtime/cli_backends/base.py.
// The shape MUST stay in sync with the Python ABC — F02 (Codex), F03
// (Gemini), and F04 (Claude) all implement this interface without
// modification.
//
// If a future backend genuinely cannot fit, raise it as an interface
// change request before touching anything here. Per the parallelization
// rules in docs/06-parallelization.md:
//
//	"F02 defines `CliBackend` base class first. F03/F04 inherit it
//	without modification — if they need changes, stop and propose a
//	base class update."
//
// Attribution: the contract shape is informed by openclaw/src/agents/
// cli-runner/ (MIT, openclaw/LICENSE) but is a fresh Go rewrite; we do
// not import OpenClaw types.
package clibackend

import "context"

// ErrorClass is a coarse classification of CLI backend failures.
//
// The orchestrator + agent loop branch on these values:
//   - AuthExpired — user must re-login to the provider CLI. The cached
//     cli_session_id for that provider is dropped.
//   - RateLimit — provider rate-limited or usage quota exhausted.
//     Surface to the user; do not retry blindly.
//   - Transient — network blip / 5xx / timeout. Caller may retry.
//   - Unknown — could not classify. Treated as terminal.
type ErrorClass string

// Frozen ErrorClass values. The string forms are persisted in the
// telemetry pipeline and DAL — do not rename.
const (
	AuthExpired ErrorClass = "auth_expired"
	RateLimit   ErrorClass = "rate_limit"
	Transient   ErrorClass = "transient"
	Unknown     ErrorClass = "unknown"
)

// CliTurnInput is the single-turn input to a CLI backend.
//
// Provider-agnostic at the call site — anything provider-specific
// (Codex's model_instructions_file config trick, Claude's --plugin-dir,
// Gemini's imageArg: "@") is hidden inside the implementation.
//
// Pointer fields (SessionID, Model, RunID, TimeoutSec) are nil when the
// caller has nothing to supply. Maps and slices are nil-tolerant — a nil
// ExtraEnv is equivalent to an empty map.
type CliTurnInput struct {
	// SystemPrompt is the system / instructions prompt for this turn.
	SystemPrompt string

	// UserPrompt is the user's message text.
	UserPrompt string

	// Images holds image paths inside the sandbox FS. Backends pass
	// these through to the CLI in whatever flag/syntax the provider
	// expects.
	Images []string

	// SessionID is the cached provider session id from a prior turn.
	// nil for the first turn.
	SessionID *string

	// Model is the model selector (e.g. "gpt-5", "opus"). nil lets the
	// CLI pick its default.
	Model *string

	// ExtraEnv carries per-turn environment overrides
	// (e.g. OPENCLAW_MCP_TOKEN).
	ExtraEnv map[string]string

	// MCPConfig is opaque MCP-loopback config; set by F11 when
	// bundleMcp is enabled. The backend translates this into
	// provider-specific syntax (Codex inline -c mcp_servers=...,
	// Claude config file, Gemini settings file).
	MCPConfig map[string]any

	// RunID is a caller-supplied identifier for trace correlation +
	// tempfile naming. nil → backend allocates a uuid.
	RunID *string

	// TimeoutSec is a hard wall-clock deadline. nil means no overall
	// timeout (the no-output watchdog is per-backend).
	TimeoutSec *float64
}

// CliTurnOutput is a successful turn result.
type CliTurnOutput struct {
	// Text is the final assistant message text.
	Text string

	// NewSessionID is the session id the CLI emitted for this turn.
	// Caller persists it in the sessions table keyed by
	// (user, channel, thread, provider).
	NewSessionID *string

	// Usage holds token counts, e.g. {"input_tokens": 12,
	// "output_tokens": 4, "cache_read_input_tokens": 4}. Empty map if
	// the CLI did not report.
	Usage map[string]any

	// RawEvents is every parsed JSONL event (or raw-text echo for
	// resume mode). Useful for telemetry, debugging, audit log.
	RawEvents []map[string]any
}

// CliTurnError is a classified turn failure.
//
// Backends RETURN this (as the error pointer) instead of raising; only
// programmer errors and OS-level failures should propagate as Go errors.
type CliTurnError struct {
	// Class is the coarse classification — see [ErrorClass].
	Class ErrorClass

	// Message is a developer-facing description.
	Message string

	// ExitCode is the subprocess exit code (-1 if the process never
	// produced one — e.g. killed before exec).
	ExitCode int

	// StderrTail is the last ~4 KiB of stderr, for forwarding to the
	// user / logs.
	StderrTail string

	// RawEvents are any JSONL events parsed before the failure.
	RawEvents []map[string]any
}

// CliBackend is the FROZEN abstract base for every provider CLI backend.
//
// Implementations spawn a CLI subprocess, parse its output, and return
// either a *CliTurnOutput (with nil error) or a *CliTurnError (with nil
// output). They MUST handle cancellation: when the supplied context is
// cancelled, the underlying subprocess must be terminated (SIGTERM,
// then SIGKILL after ~1s).
type CliBackend interface {
	// ID returns the stable provider id used in agent configs
	// ("codex-cli", "google-gemini-cli", "claude-cli").
	ID() string

	// DefaultCommand is the default argv[0] (and any default flags)
	// for the CLI. Tests / configs override this with an absolute path
	// or a fake binary.
	DefaultCommand() []string

	// Turn runs one turn of the provider CLI.
	//
	// Implementations MUST:
	//   - NOT return a Go error for classifiable provider errors;
	//     return *CliTurnError instead. (OS errors / programmer errors
	//     may still propagate via the second return — but the Python
	//     contract uses the error struct exclusively, and Go callers
	//     expect the same.)
	//   - honour context cancellation by killing the subprocess
	//     promptly.
	//   - clean up any tempfiles they wrote (system prompt, image
	//     payloads, MCP config).
	//
	// Exactly one of the two return values is non-nil.
	Turn(ctx context.Context, in CliTurnInput) (*CliTurnOutput, *CliTurnError)

	// SupportsResumeInStream reports whether the CLI's resume mode
	// emits the same JSONL stream as a fresh session.
	//
	//   - false — Codex: `codex exec resume` switches to plain text.
	//   - true  — Gemini, Claude: same JSON/JSONL format.
	//
	// F05's loop uses this to decide whether to attach the streaming
	// delta parser on resume turns.
	SupportsResumeInStream() bool
}
