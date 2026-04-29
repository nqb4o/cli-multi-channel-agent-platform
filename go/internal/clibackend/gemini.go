// F03 — Google Gemini CLI backend (gemini --output-format json).
//
// Spawns the Gemini CLI as a subprocess, parses its single-object JSON
// response, extracts the final assistant text + session_id for resume,
// classifies failures, and overlays an MCP loopback config via
// ~/.gemini/settings.json swap when CliTurnInput.MCPConfig is set.
//
// Adapted from openclaw/src/agents/cli-runner/ and
// openclaw/src/agents/cli-output.ts (MIT, openclaw/LICENSE). The JSON-shape
// parsing mirrors OpenClaw's parseCliJson / readCliUsage (cli-output.ts
// L113-278). The settings overlay mirrors
// cli-runner/bundle-mcp.ts::writeGeminiSystemSettings (L220-256). The
// implementation is a fresh Go rewrite — we do not import OpenClaw types.
//
// Spawn shape:
//
//	gemini --output-format json --prompt <user_prompt>
//	    [--model <model>]
//	    [@<image-path> ...]
//	    # session resume: prepend [--resume <sessionId>]
//
// Per OpenClaw docs (docs/gateway/cli-backends.md L231-253) the bundled
// google-gemini-cli backend uses single-object JSON output (NOT JSONL like
// Claude). Resume mode reuses the same JSON output, so
// SupportsResumeInStream() → true.
//
// Settings overlay (F11 bundleMcp):
// The Gemini CLI reads MCP config out of ~/.gemini/settings.json (or
// $GEMINI_CLI_SYSTEM_SETTINGS_PATH if set). When MCPConfig is supplied on a
// turn, this backend writes a temp settings file merging the MCP servers
// in, and points GEMINI_CLI_SYSTEM_SETTINGS_PATH at it for the spawned
// subprocess only. The user's settings are not modified — env-var override
// is atomic and reversible.
//
// Image arg syntax:
// Gemini uses @<path> (different from Codex's --image <path>). Per F03
// brief: image paths must be inside the workspaceRoot and are rejected
// before spawn if they escape the sandbox.
//
// Version probe:
// gemini --version runs once at construction (skippable for tests via
// CheckVersionOnInit: false). A failed probe logs a warning but does not
// raise — the actual Turn call surfaces the missing-CLI error with a
// proper *CliTurnError.
package clibackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// GeminiID is the FROZEN provider id surfaced via [GeminiBackend.ID].
const GeminiID = "google-gemini-cli"

// Default fresh-session argv tail. Single-object JSON (NOT jsonl) +
// --prompt flag carrying the user prompt. We omit the {prompt} placeholder
// here because it is appended at argv assembly time with the actual user
// prompt.
var defaultGeminiFreshArgs = []string{"--output-format", "json"}

// Resume mode: --resume <sessionId> is prepended; the rest of the argv
// (--output-format json --prompt <prompt>) is identical so the same parser
// stays attached on resume.
var defaultGeminiResumeArgs = []string{"--output-format", "json"}

// terminateGrace is how long to wait after SIGTERM before SIGKILL.
const geminiTerminateGrace = 1 * time.Second

// stderrTailBytes is the maximum stderr tail returned in errors.
const geminiStderrTailBytes = 4096

// ---------------------------------------------------------------------------
// Error classification — pattern-match stderr / stdout for well-known
// Gemini CLI auth / rate / network failure modes.
//
// Same four-way split (AuthExpired / RateLimit / Transient / Unknown) as
// Codex/Claude so the agent loop's branching is provider-agnostic.
// ---------------------------------------------------------------------------

var geminiAuthExpiredPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:not\s+(?:logged|signed)\s+in|please\s+(?:log|sign)\s+in|run\s+` + "`?" + `gemini\s+auth` + "`?" + `)\b`),
	regexp.MustCompile(`(?i)\b(?:auth(?:entication)?\s+(?:expired|required|failed))\b`),
	regexp.MustCompile(`(?i)\b(?:invalid|expired|missing)\s+(?:credentials|token|api\s*key|oauth)\b`),
	regexp.MustCompile(`(?i)\b401\b.*(?:unauthor)`),
	regexp.MustCompile(`(?i)\b403\b.*(?:forbidden|permission)`),
	regexp.MustCompile(`(?i)"code"\s*:\s*"unauthorized"`),
	regexp.MustCompile(`(?i)\boauth\s+token\s+(?:expired|invalid|missing)\b`),
	regexp.MustCompile(`(?i)\bgemini\s+api\s+key\s+(?:invalid|expired|missing)\b`),
}

var geminiRateLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\brate[\s_-]?limit`),
	regexp.MustCompile(`(?i)\b429\b`),
	regexp.MustCompile(`(?i)\busage\s+(?:limit|quota)\b`),
	regexp.MustCompile(`(?i)\b(?:quota|tokens?)\s+(?:exhausted|exceeded)\b`),
	regexp.MustCompile(`(?i)\btoo\s+many\s+requests\b`),
	regexp.MustCompile(`(?i)\bresource_exhausted\b`),
}

var geminiTransientPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b(?:ECONN(?:RESET|REFUSED|ABORTED)|ETIMEDOUT|EAI_AGAIN)\b`),
	regexp.MustCompile(`(?i)\b(?:connection\s+(?:reset|refused|aborted|timed?\s*out))\b`),
	regexp.MustCompile(`(?i)\b5\d\d\b.*(?:server|gateway|service)`),
	regexp.MustCompile(`(?i)\b(?:network|dns)\s+error\b`),
	regexp.MustCompile(`(?i)\btemporar(?:y|ily)\s+unavailable\b`),
	regexp.MustCompile(`(?i)\bunavailable\b.*(?:retry|temporar)`),
}

// ClassifyGeminiError categorises a Gemini CLI failure based on its stderr
// / stdout output. Same four-way split as ClassifyClaudeError so the agent
// loop's branching is provider-agnostic.
func ClassifyGeminiError(text string) ErrorClass {
	if text == "" {
		return Unknown
	}
	for _, p := range geminiAuthExpiredPatterns {
		if p.MatchString(text) {
			return AuthExpired
		}
	}
	for _, p := range geminiRateLimitPatterns {
		if p.MatchString(text) {
			return RateLimit
		}
	}
	for _, p := range geminiTransientPatterns {
		if p.MatchString(text) {
			return Transient
		}
	}
	return Unknown
}

// ---------------------------------------------------------------------------
// JSON parsing — single-object response from `gemini --output-format json`.
// ---------------------------------------------------------------------------

// GeminiParsed is the result of parsing one Gemini --output-format json
// response. Exposed so tests can inspect intermediate fields.
type GeminiParsed struct {
	Text        string
	SessionID   string
	Usage       map[string]any
	Events      []map[string]any
	InlineError string
}

// isNum reports whether v is a numeric (int / float64) value, ignoring
// booleans (which Go's json package decodes to bool, but JSON has booleans
// distinct from numbers anyway — this is the Python-parity guard).
func isGeminiNumber(v any) bool {
	switch v.(type) {
	case float64, int, int64:
		return true
	default:
		return false
	}
}

// numToInt extracts a positive integer from a numeric any. Returns
// (value, true) when v is numeric and > 0.
func geminiNumToInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		if x > 0 {
			return int(x), true
		}
	case int:
		if x > 0 {
			return x, true
		}
	case int64:
		if x > 0 {
			return int(x), true
		}
	}
	return 0, false
}

// pickInt returns the first numeric value present at any of keys (>0).
func pickGeminiInt(raw map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		v, ok := raw[k]
		if !ok {
			continue
		}
		if n, ok := geminiNumToInt(v); ok {
			return n, true
		}
	}
	return 0, false
}

// normalizeGeminiUsage translates a Gemini CLI usage/stats dict into the
// shared shape used by every backend.
//
// Mirrors OpenClaw's toCliUsage (cli-output.ts L113-147), with
// Gemini-specific behaviour:
//   - stats.cached → cache_read_input_tokens.
//   - If "input" is absent and "cached" is present, derive input from
//     input_tokens - cached (matches cli-output.ts L137-139).
func normalizeGeminiUsage(raw map[string]any) map[string]any {
	if raw == nil {
		return map[string]any{}
	}

	cached, hasCached := pickGeminiInt(raw,
		"cache_read_input_tokens", "cached_input_tokens", "cacheRead", "cached")

	var nestedCached int
	var hasNestedCached bool
	for _, key := range []string{"input_tokens_details", "prompt_tokens_details"} {
		nested, ok := raw[key].(map[string]any)
		if !ok {
			continue
		}
		v, ok := nested["cached_tokens"]
		if !ok || !isGeminiNumber(v) {
			continue
		}
		if n, ok := geminiNumToInt(v); ok {
			nestedCached, hasNestedCached = n, true
			break
		}
	}
	if !hasCached && hasNestedCached {
		cached, hasCached = nestedCached, true
	}

	totalInput, hasTotalInput := pickGeminiInt(raw, "input_tokens", "inputTokens")
	output, hasOutput := pickGeminiInt(raw, "output_tokens", "outputTokens", "output")
	cacheWrite, hasCacheWrite := pickGeminiInt(raw,
		"cache_creation_input_tokens", "cache_write_input_tokens", "cacheWrite")
	total, hasTotal := pickGeminiInt(raw, "total_tokens", "total")

	explicitInput, hasExplicit := pickGeminiInt(raw, "input")

	var inputTokens int
	var hasInput bool
	switch {
	case hasExplicit:
		inputTokens, hasInput = explicitInput, true
	case (geminiHasKey(raw, "cached") || hasNestedCached) && hasTotalInput:
		// Derive input = total_input - cached (clamped at 0).
		c := 0
		if hasCached {
			c = cached
		}
		v := totalInput - c
		if v < 0 {
			v = 0
		}
		inputTokens, hasInput = v, true
	case hasTotalInput:
		inputTokens, hasInput = totalInput, true
	}

	out := map[string]any{}
	if hasInput {
		out["input_tokens"] = inputTokens
	}
	if hasOutput {
		out["output_tokens"] = output
	}
	if hasCached {
		out["cache_read_input_tokens"] = cached
	}
	if hasCacheWrite {
		out["cache_creation_input_tokens"] = cacheWrite
	}
	if hasTotal {
		out["total_tokens"] = total
	}
	return out
}

// geminiHasKey is a small helper since map[string]any has no `in` operator.
func geminiHasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}

// readGeminiUsage picks usage from parsed.usage or falls back to parsed.stats.
func readGeminiUsage(parsed map[string]any) map[string]any {
	if usage, ok := parsed["usage"].(map[string]any); ok {
		normalised := normalizeGeminiUsage(usage)
		if len(normalised) > 0 {
			return normalised
		}
	}
	if stats, ok := parsed["stats"].(map[string]any); ok {
		return normalizeGeminiUsage(stats)
	}
	return map[string]any{}
}

// collectGeminiText recursively digs text out of a Gemini message payload.
// Mirrors OpenClaw's collectCliText (cli-output.ts L162-194), Gemini
// variant: prefers response (top-level field) then falls through text /
// result / content / nested message.
func collectGeminiText(value any) string {
	if value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, entry := range v {
			b.WriteString(collectGeminiText(entry))
		}
		return b.String()
	case map[string]any:
		for _, key := range []string{"response", "text", "result"} {
			if s, ok := v[key].(string); ok {
				return s
			}
		}
		if content, ok := v["content"]; ok {
			switch c := content.(type) {
			case string:
				return c
			case []any:
				var b strings.Builder
				for _, entry := range c {
					b.WriteString(collectGeminiText(entry))
				}
				return b.String()
			}
		}
		if msg, ok := v["message"].(map[string]any); ok {
			return collectGeminiText(msg)
		}
	}
	return ""
}

// extractGeminiSessionID returns the first non-empty session id present
// under any of the four supported keys (snake / camel + alias).
func extractGeminiSessionID(parsed map[string]any) string {
	for _, key := range []string{"session_id", "sessionId", "conversation_id", "conversationId"} {
		if v, ok := parsed[key].(string); ok {
			trimmed := strings.TrimSpace(v)
			if trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// inlineGeminiError detects an inline error block in the JSON response.
//
// Two shapes:
//   - {"error": {"code": "...", "message": "..."}}
//   - {"is_error": true, "result"|"message"|"error": "..."}
func inlineGeminiError(parsed map[string]any) string {
	if errBlock, ok := parsed["error"].(map[string]any); ok {
		if msg, ok := errBlock["message"].(string); ok {
			trimmed := strings.TrimSpace(msg)
			if trimmed != "" {
				return trimmed
			}
		}
		// Fall back to a stable JSON dump so the classifier still sees
		// something to match against.
		dump, _ := json.Marshal(stableGeminiDump(errBlock))
		return string(dump)
	}
	if errStr, ok := parsed["error"].(string); ok {
		trimmed := strings.TrimSpace(errStr)
		if trimmed != "" {
			return trimmed
		}
	}
	if isErr, ok := parsed["is_error"].(bool); ok && isErr {
		for _, key := range []string{"result", "message", "error"} {
			if v, ok := parsed[key].(string); ok {
				trimmed := strings.TrimSpace(v)
				if trimmed != "" {
					return trimmed
				}
			}
		}
		dump, _ := json.Marshal(stableGeminiDump(parsed))
		return string(dump)
	}
	return ""
}

// stableDump turns a generic decoded JSON value back into something
// json.Marshal will emit with sorted map keys (Go already does this for
// map[string]any, so this is essentially a passthrough — kept for
// readability parity with the Python json.dumps(sort_keys=True) call).
func stableGeminiDump(v any) any { return v }

// ParseGeminiJSON parses the single-object JSON response from
// `gemini --output-format json`.
//
// Tolerates a couple of edge cases:
//   - Some Gemini builds emit the JSON object wrapped in a leading
//     progress/log line. We strip everything before the first `{` so a
//     stray "Loading session…" banner doesn't break parsing.
//   - If the object is missing entirely (empty body), we return an empty
//     parsed result; the caller will surface that as a *CliTurnError
//     (since exit code != 0 is the dominant failure path the parser would
//     never see).
func ParseGeminiJSON(raw string) GeminiParsed {
	text := strings.TrimSpace(raw)
	if text == "" {
		return GeminiParsed{Usage: map[string]any{}, Events: []map[string]any{}}
	}

	// Trim a leading non-JSON banner if present.
	if brace := strings.IndexByte(text, '{'); brace > 0 {
		text = text[brace:]
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		// Truncate raw to ~200 chars to mirror the Python repr(raw[:200]).
		preview := raw
		if len(preview) > 200 {
			preview = preview[:200]
		}
		return GeminiParsed{
			Usage:       map[string]any{},
			Events:      []map[string]any{},
			InlineError: fmt.Sprintf("could not parse gemini JSON output: %q", preview),
		}
	}
	if parsed == nil {
		return GeminiParsed{
			Usage:       map[string]any{},
			Events:      []map[string]any{},
			InlineError: "unexpected gemini JSON shape: null",
		}
	}

	events := []map[string]any{parsed}
	inlineErr := inlineGeminiError(parsed)
	sessionID := extractGeminiSessionID(parsed)
	usage := readGeminiUsage(parsed)

	finalText := ""
	if respValue, ok := parsed["response"].(string); ok {
		finalText = strings.TrimSpace(respValue)
	}
	if finalText == "" {
		finalText = strings.TrimSpace(collectGeminiText(parsed))
	}

	return GeminiParsed{
		Text:        finalText,
		SessionID:   sessionID,
		Usage:       usage,
		Events:      events,
		InlineError: inlineErr,
	}
}

// ---------------------------------------------------------------------------
// Image path scoping — `@<path>` syntax must stay inside the workspace.
// ---------------------------------------------------------------------------

// geminiImagePathError is returned by normalizeGeminiImageArg when the path
// escapes workspaceRoot (or is otherwise invalid).
type geminiImagePathError struct{ msg string }

func (e *geminiImagePathError) Error() string { return e.msg }

// normalizeGeminiImageArg validates one image path and returns the
// `@<path>` argv form.
//
//   - Strips an optional leading `@` for caller convenience.
//   - Resolves to absolute, then enforces that it sits under workspaceRoot
//     (if provided). Symlink escapes are blocked by EvalSymlinks where
//     possible (best-effort: a non-existent path falls back to lexical
//     cleaning, which still defends against `..` escapes).
//   - Returns the `@<absolute-path>` form expected by the Gemini CLI.
func normalizeGeminiImageArg(raw string, workspaceRoot string) (string, error) {
	raw = strings.TrimLeft(raw, "@")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", &geminiImagePathError{msg: "empty image path"}
	}

	expanded := expandGeminiUserHome(raw)
	abs := expanded
	if !filepath.IsAbs(abs) && workspaceRoot != "" {
		abs = filepath.Join(workspaceRoot, abs)
	}
	abs = resolveGeminiPath(abs)

	if workspaceRoot != "" {
		wsResolved := resolveGeminiPath(workspaceRoot)
		rel, err := filepath.Rel(wsResolved, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", &geminiImagePathError{
				msg: fmt.Sprintf(
					"image path %q resolves to %s outside workspace %s",
					raw, abs, wsResolved,
				),
			}
		}
	}

	return "@" + abs, nil
}

// expandUserHome replaces a leading "~" with $HOME (best-effort).
func expandGeminiUserHome(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// resolvePath emulates pathlib.Path.resolve(strict=False): make absolute,
// follow symlinks where possible, and fall back to lexical cleaning when
// the path doesn't exist.
func resolveGeminiPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return filepath.Clean(abs)
}

// ---------------------------------------------------------------------------
// GeminiBackend
// ---------------------------------------------------------------------------

// GeminiConfig holds GeminiBackend construction parameters.
type GeminiConfig struct {
	// Command is argv[0]. Tests pass an absolute path to a fake binary.
	// If empty, defaults to []string{"gemini"}. May be a multi-element
	// slice to express e.g. ["bash", "-c", ...].
	Command []string

	// FreshArgs / ResumeArgs override the argv block emitted between the
	// CLI binary and the prompt. Defaults are
	// {"--output-format", "json"}.
	FreshArgs  []string
	ResumeArgs []string

	// WorkspaceRoot is required (in production) for `@<path>` image-path
	// scoping. Empty disables the check (tests / probes).
	WorkspaceRoot string

	// SettingsRoot is the directory for the per-turn settings overlay
	// tempfile. Defaults to os.TempDir().
	SettingsRoot string

	// BaseSettingsPath is the base ~/.gemini/settings.json to merge MCP
	// config onto. Defaults to $HOME/.gemini/settings.json.
	BaseSettingsPath string

	// CheckVersionOnInit controls whether `gemini --version` runs once
	// at construction. Defaults to true; tests pass false.
	CheckVersionOnInit bool
}

// GeminiBackend is the concrete CliBackend for the Google Gemini CLI.
//
// Construction parameters live on GeminiConfig; see [NewGeminiBackend].
type GeminiBackend struct {
	command          []string
	freshArgs        []string
	resumeArgs       []string
	workspaceRoot    string
	settingsRoot     string
	baseSettingsPath string
}

// Compile-time check that GeminiBackend satisfies the CliBackend
// interface.
var _ CliBackend = (*GeminiBackend)(nil)

// NewGeminiBackend constructs a GeminiBackend. Errors are returned only
// for programmer-config issues (none today); a missing CLI binary is
// surfaced via Turn() as a *CliTurnError so the agent loop can keep
// running with the other providers.
func NewGeminiBackend(cfg GeminiConfig) *GeminiBackend {
	command := cfg.Command
	if len(command) == 0 {
		command = []string{"gemini"}
	}
	freshArgs := cfg.FreshArgs
	if freshArgs == nil {
		freshArgs = append([]string(nil), defaultGeminiFreshArgs...)
	}
	resumeArgs := cfg.ResumeArgs
	if resumeArgs == nil {
		resumeArgs = append([]string(nil), defaultGeminiResumeArgs...)
	}

	settingsRoot := cfg.SettingsRoot
	if settingsRoot == "" {
		settingsRoot = os.TempDir()
	}

	baseSettingsPath := cfg.BaseSettingsPath
	if baseSettingsPath == "" {
		home := os.Getenv("HOME")
		if home == "" {
			if h, err := os.UserHomeDir(); err == nil {
				home = h
			}
		}
		if home != "" {
			baseSettingsPath = filepath.Join(home, ".gemini", "settings.json")
		}
	}

	b := &GeminiBackend{
		command:          append([]string(nil), command...),
		freshArgs:        append([]string(nil), freshArgs...),
		resumeArgs:       append([]string(nil), resumeArgs...),
		workspaceRoot:    cfg.WorkspaceRoot,
		settingsRoot:     settingsRoot,
		baseSettingsPath: baseSettingsPath,
	}

	if cfg.CheckVersionOnInit {
		b.probeVersion()
	}

	return b
}

// ID returns the FROZEN provider id "google-gemini-cli".
func (b *GeminiBackend) ID() string { return GeminiID }

// DefaultCommand returns the configured command (argv[0] + any leading
// flags). Always non-nil; defaults to []string{"gemini"}.
func (b *GeminiBackend) DefaultCommand() []string {
	return append([]string(nil), b.command...)
}

// SupportsResumeInStream is true for Gemini: resume mode reuses the same
// single-object JSON output, so the parser stays attached on resume turns.
func (b *GeminiBackend) SupportsResumeInStream() bool { return true }

// Turn runs one turn of the Gemini CLI.
func (b *GeminiBackend) Turn(ctx context.Context, in CliTurnInput) (*CliTurnOutput, *CliTurnError) {
	runID := ""
	if in.RunID != nil {
		runID = *in.RunID
	}
	if runID == "" {
		runID = uuid.NewString()
	}

	useResume := in.SessionID != nil && *in.SessionID != ""

	argv, err := b.buildArgv(in, useResume)
	if err != nil {
		var ipe *geminiImagePathError
		if errors.As(err, &ipe) {
			return nil, &CliTurnError{
				Class:    Unknown,
				Message:  fmt.Sprintf("gemini image path rejected: %s", ipe.Error()),
				ExitCode: -1,
			}
		}
		return nil, &CliTurnError{
			Class:    Unknown,
			Message:  fmt.Sprintf("gemini argv build failed: %s", err.Error()),
			ExitCode: -1,
		}
	}

	extraEnv := map[string]string{}
	for k, v := range in.ExtraEnv {
		extraEnv[k] = v
	}

	var overlay *geminiSettingsOverlay
	if len(in.MCPConfig) > 0 {
		ov, err := buildGeminiSettingsOverlay(
			in.MCPConfig, b.baseSettingsPath, b.settingsRoot, runID,
		)
		if err != nil {
			return nil, &CliTurnError{
				Class:    Unknown,
				Message:  fmt.Sprintf("gemini settings overlay failed: %s", err.Error()),
				ExitCode: -1,
			}
		}
		overlay = ov
		extraEnv[geminiSettingsEnv] = overlay.Path()
	}
	defer overlay.cleanup()

	return b.spawnAndParse(ctx, argv, in, useResume, extraEnv)
}

// buildArgv builds the full argv for the Gemini subprocess.
//
// Shape (per cli-backends.md L231-240):
//
//	gemini [--resume <sid>] --output-format json --prompt <text>
//	       [--model <m>] [@<image-path> ...]
//
// System prompt: Gemini doesn't expose a dedicated --append-system-prompt
// flag the way Claude does. Per F03 brief we prepend it to the user prompt
// with a clear [SYSTEM] separator on a fresh session; on resume the
// prompt-only form is sent (the prior session retains the original system
// prompt).
func (b *GeminiBackend) buildArgv(in CliTurnInput, useResume bool) ([]string, error) {
	argv := append([]string(nil), b.command...)

	if useResume {
		argv = append(argv, "--resume", *in.SessionID)
		argv = append(argv, b.resumeArgs...)
	} else {
		argv = append(argv, b.freshArgs...)
	}

	// Model selection — Gemini takes --model <id>.
	if in.Model != nil && *in.Model != "" {
		argv = append(argv, "--model", *in.Model)
	}

	// Image pass-through. @<path> syntax with workspace scoping.
	for _, imgPath := range in.Images {
		normed, err := normalizeGeminiImageArg(imgPath, b.workspaceRoot)
		if err != nil {
			return nil, err
		}
		argv = append(argv, normed)
	}

	// Compose the prompt: prepend system prompt on fresh sessions.
	prompt := in.UserPrompt
	if !useResume && strings.TrimSpace(in.SystemPrompt) != "" {
		prompt = fmt.Sprintf("[SYSTEM]\n%s\n\n[USER]\n%s", in.SystemPrompt, in.UserPrompt)
	}
	argv = append(argv, "--prompt", prompt)
	return argv, nil
}

// spawnAndParse runs the Gemini subprocess to completion, parses output,
// and produces either *CliTurnOutput or *CliTurnError.
func (b *GeminiBackend) spawnAndParse(
	ctx context.Context,
	argv []string,
	in CliTurnInput,
	useResume bool,
	extraEnv map[string]string,
) (*CliTurnOutput, *CliTurnError) {
	if len(argv) == 0 {
		return nil, &CliTurnError{
			Class:    Unknown,
			Message:  "gemini argv is empty",
			ExitCode: -1,
		}
	}

	if !resolveGeminiExecutable(argv[0]) {
		return nil, &CliTurnError{
			Class:    Unknown,
			Message:  fmt.Sprintf("gemini CLI not found on PATH (looked for %q)", argv[0]),
			ExitCode: -1,
		}
	}

	// Apply optional overall timeout via a derived context.
	cmdCtx := ctx
	var cancel context.CancelFunc
	if in.TimeoutSec != nil {
		cmdCtx, cancel = context.WithTimeout(ctx, time.Duration(*in.TimeoutSec*float64(time.Second)))
		defer cancel()
	}

	cmd := exec.CommandContext(cmdCtx, argv[0], argv[1:]...)
	cmd.Env = composeGeminiEnv(extraEnv)
	// Cancel by sending SIGTERM first; CommandContext defaults to Kill.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = geminiTerminateGrace

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, &CliTurnError{
			Class:    Unknown,
			Message:  fmt.Sprintf("gemini CLI stdout pipe failed: %s", err.Error()),
			ExitCode: -1,
		}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, &CliTurnError{
			Class:    Unknown,
			Message:  fmt.Sprintf("gemini CLI stderr pipe failed: %s", err.Error()),
			ExitCode: -1,
		}
	}

	slog.Debug(
		"gemini spawn",
		"argv0", argv[0],
		"use_resume", useResume,
		"prompt_chars", len(in.UserPrompt),
	)

	if err := cmd.Start(); err != nil {
		return nil, &CliTurnError{
			Class:    Unknown,
			Message:  fmt.Sprintf("gemini CLI spawn failed: %s", err.Error()),
			ExitCode: -1,
		}
	}

	stdoutBytes, _ := io.ReadAll(stdout)
	stderrBytes, _ := io.ReadAll(stderr)

	waitErr := cmd.Wait()

	// If the parent context was cancelled (not via the timeout we set),
	// surface that as a Go error so the caller can react. Mirrors the
	// Python `raise CancelledError`.
	if ctx.Err() != nil {
		return nil, &CliTurnError{
			Class:    Transient,
			Message:  fmt.Sprintf("gemini CLI cancelled: %s", ctx.Err().Error()),
			ExitCode: cmd.ProcessState.ExitCode(),
		}
	}

	// Timeout handling — cmdCtx may have hit deadline.
	if cmdCtx.Err() == context.DeadlineExceeded {
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		t := 0.0
		if in.TimeoutSec != nil {
			t = *in.TimeoutSec
		}
		return nil, &CliTurnError{
			Class:    Transient,
			Message:  fmt.Sprintf("gemini CLI exceeded timeout (%.1fs) and was terminated", t),
			ExitCode: exitCode,
		}
	}

	stdoutStr := string(stdoutBytes)
	stderrStr := string(stderrBytes)
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	stderrTail := stderrStr
	if len(stderrTail) > geminiStderrTailBytes {
		stderrTail = stderrTail[len(stderrTail)-geminiStderrTailBytes:]
	}

	parsed := ParseGeminiJSON(stdoutStr)

	// exit != 0 — even if Wait() returned an *exec.ExitError, we treat
	// the structured-error parse path as primary.
	if exitCode != 0 {
		errText := parsed.InlineError
		if errText == "" {
			errText = stderrStr
		}
		if errText == "" {
			errText = stdoutStr
		}
		_ = waitErr // already reflected in exit code
		message := strings.TrimSpace(errText)
		if message == "" {
			message = "gemini CLI exited with non-zero status"
		}
		return nil, &CliTurnError{
			Class:      ClassifyGeminiError(errText),
			Message:    message,
			ExitCode:   exitCode,
			StderrTail: stderrTail,
			RawEvents:  parsed.Events,
		}
	}

	if parsed.InlineError != "" {
		return nil, &CliTurnError{
			Class:      ClassifyGeminiError(parsed.InlineError),
			Message:    parsed.InlineError,
			ExitCode:   exitCode,
			StderrTail: stderrTail,
			RawEvents:  parsed.Events,
		}
	}

	// Resume mode preserves the prior session id even if the CLI does
	// not echo one back — match Claude/Codex parity.
	var newSessionID *string
	if parsed.SessionID != "" {
		s := parsed.SessionID
		newSessionID = &s
	} else if useResume && in.SessionID != nil {
		s := *in.SessionID
		newSessionID = &s
	}

	return &CliTurnOutput{
		Text:         parsed.Text,
		NewSessionID: newSessionID,
		Usage:        parsed.Usage,
		RawEvents:    parsed.Events,
	}, nil
}

// composeGeminiEnv merges os.Environ() with extra (extra wins on collision).
func composeGeminiEnv(extra map[string]string) []string {
	if len(extra) == 0 {
		return os.Environ()
	}
	out := make([]string, 0, len(os.Environ())+len(extra))
	seen := make(map[string]bool, len(extra))
	for _, kv := range os.Environ() {
		idx := strings.IndexByte(kv, '=')
		if idx < 0 {
			out = append(out, kv)
			continue
		}
		key := kv[:idx]
		if v, ok := extra[key]; ok {
			out = append(out, key+"="+v)
			seen[key] = true
		} else {
			out = append(out, kv)
		}
	}
	for k, v := range extra {
		if !seen[k] {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// probeVersion runs `<command> --version` once at construction.
//
// Treated as a soft prereq: a missing/broken binary logs a warning and
// Turn() later returns a structured *CliTurnError. We do NOT raise here —
// the agent loop should be able to instantiate every backend even when one
// CLI is missing on the host.
func (b *GeminiBackend) probeVersion() {
	if len(b.command) == 0 {
		return
	}
	cmdName := b.command[0]
	if !resolveGeminiExecutable(cmdName) {
		slog.Warn("gemini CLI not found on PATH; --version probe skipped", "cmd", cmdName)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := append(append([]string(nil), b.command[1:]...), "--version")
	cmd := exec.CommandContext(ctx, cmdName, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("gemini --version probe failed",
			"err", err, "output", strings.TrimSpace(string(out)))
		return
	}
	if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() != 0 {
		preview := strings.TrimSpace(string(out))
		if len(preview) > 200 {
			preview = preview[:200]
		}
		slog.Warn("gemini --version probe exited non-zero",
			"exit", cmd.ProcessState.ExitCode(), "output", preview)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// resolveGeminiExecutable is like exec.LookPath but tolerates absolute
// paths and explicit relative paths (./foo, ../foo) — those are checked
// for the execute bit directly.
func resolveGeminiExecutable(cmd string) bool {
	if filepath.IsAbs(cmd) || strings.HasPrefix(cmd, "./") || strings.HasPrefix(cmd, "../") {
		info, err := os.Stat(cmd)
		if err != nil {
			return false
		}
		if info.IsDir() {
			return false
		}
		return info.Mode().Perm()&0o111 != 0
	}
	_, err := exec.LookPath(cmd)
	return err == nil
}
