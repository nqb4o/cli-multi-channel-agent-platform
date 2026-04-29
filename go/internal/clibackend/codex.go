// F02 — OpenAI Codex CLI backend (`codex exec`).
//
// Spawns the Codex CLI as a subprocess in JSONL mode (`codex exec --json`),
// parses its event stream, extracts the final assistant text + session_id
// for resume, and classifies failures so the agent loop can drop bad
// sessions or surface auth-renewal prompts to the user.
//
// Ported from services/runtime/src/runtime/cli_backends/codex.py. The
// argv assembly mirrors the Python `_build_argv`; the JSONL parsing
// mirrors `parse_codex_jsonl` / `parse_codex_resume_text` /
// `_extract_session_id_*` / `_extract_usage` / `_collect_text`. Error
// classification mirrors `classify_codex_error`.
//
// Several helpers (maybeLoadRecord, collectText, isInlineError,
// resolveExecutable, terminate) are shared with claude.go in this package
// and reused as-is. Anything Codex-specific (the alternate
// session-id ordering with thread_id fallback, the nested
// input_tokens_details preservation, the inline-TOML config overrides) is
// scoped here.
//
// Attribution: the original Python code adapts openclaw/src/agents/
// cli-runner/ + cli-output.ts (MIT, openclaw/LICENSE,
// @cb4ec1265f8b2e3bb78a20fb2ee83285b9076e7e). This Go file is a
// translation of the Python rewrite — no openclaw types are imported.

package clibackend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// CodexBackendID is the stable provider id used in agent configs.
const CodexBackendID = "codex-cli"

// Mirror of the OpenClaw bundled `codex-cli` defaults (the bundled OpenAI
// plugin registers these). Kept in sync with Python's
// `_DEFAULT_FRESH_ARGS`.
var defaultCodexFreshArgs = []string{
	"exec",
	"--json",
	"--color",
	"never",
	"--sandbox",
	"workspace-write",
	"--skip-git-repo-check",
}

// Resume args replace the fresh args wholesale. `sandbox_mode` is baked
// in via `-c` because `codex exec resume` does not accept `--sandbox`.
// `{sessionId}` is substituted at argv-build time.
var defaultCodexResumeArgs = []string{
	"exec",
	"resume",
	"{sessionId}",
	"-c",
	`sandbox_mode="workspace-write"`,
	"--skip-git-repo-check",
}

const (
	codexModelFlag             = "--model"
	codexImageFlag             = "--image"
	codexConfigFlag            = "-c"
	codexSystemPromptConfigKey = "model_instructions_file"
	codexMCPServersConfigKey   = "mcp_servers"

	// codexMaxPromptArgChars is the threshold above which the user
	// prompt is sent on stdin instead of as argv.
	codexMaxPromptArgChars = 8000
)

// CodexBackend is the F02 implementation of [CliBackend] for the Codex CLI.
type CodexBackend struct {
	command        []string
	freshArgs      []string
	resumeArgs     []string
	tempfileRoot   string
	maxPromptChars int
}

// CodexOption configures a [CodexBackend].
type CodexOption func(*CodexBackend)

// WithCodexCommand overrides argv[0]. Single string or full argv slice
// — both are accepted.
func WithCodexCommand(cmd ...string) CodexOption {
	return func(b *CodexBackend) {
		b.command = append([]string(nil), cmd...)
	}
}

// WithCodexFreshArgs overrides the default fresh-session args.
func WithCodexFreshArgs(args []string) CodexOption {
	return func(b *CodexBackend) {
		b.freshArgs = append([]string(nil), args...)
	}
}

// WithCodexResumeArgs overrides the default resume args. `{sessionId}`
// inside any arg is replaced with the resumed session id.
func WithCodexResumeArgs(args []string) CodexOption {
	return func(b *CodexBackend) {
		b.resumeArgs = append([]string(nil), args...)
	}
}

// WithCodexSystemPromptRoot sets the directory the per-turn system-prompt
// tempfile is written under. Defaults to os.TempDir().
func WithCodexSystemPromptRoot(dir string) CodexOption {
	return func(b *CodexBackend) {
		b.tempfileRoot = dir
	}
}

// WithCodexMaxPromptArgChars overrides the argv-vs-stdin threshold.
func WithCodexMaxPromptArgChars(n int) CodexOption {
	return func(b *CodexBackend) {
		b.maxPromptChars = n
	}
}

// NewCodexBackend builds a new [CodexBackend] with the supplied options.
func NewCodexBackend(opts ...CodexOption) *CodexBackend {
	b := &CodexBackend{
		command:        []string{"codex"},
		freshArgs:      append([]string(nil), defaultCodexFreshArgs...),
		resumeArgs:     append([]string(nil), defaultCodexResumeArgs...),
		tempfileRoot:   os.TempDir(),
		maxPromptChars: codexMaxPromptArgChars,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// ID implements [CliBackend].
func (*CodexBackend) ID() string { return CodexBackendID }

// DefaultCommand implements [CliBackend].
func (b *CodexBackend) DefaultCommand() []string {
	return append([]string(nil), b.command...)
}

// SupportsResumeInStream implements [CliBackend]. Codex's `exec resume`
// switches to plain text — no JSONL.
func (*CodexBackend) SupportsResumeInStream() bool { return false }

// Turn implements [CliBackend].
func (b *CodexBackend) Turn(ctx context.Context, in CliTurnInput) (*CliTurnOutput, *CliTurnError) {
	runID := ""
	if in.RunID != nil && *in.RunID != "" {
		runID = *in.RunID
	} else {
		runID = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	useResume := in.SessionID != nil && *in.SessionID != ""

	var tempfilesToClean []string
	defer func() {
		for _, p := range tempfilesToClean {
			_ = os.Remove(p)
		}
	}()

	argv, stdinPayload, sysPromptFile, err := b.buildArgv(in, runID, useResume)
	if err != nil {
		return nil, &CliTurnError{
			Class:    Unknown,
			Message:  fmt.Sprintf("codex argv build failed: %v", err),
			ExitCode: -1,
		}
	}
	if sysPromptFile != "" {
		tempfilesToClean = append(tempfilesToClean, sysPromptFile)
	}

	return b.spawnAndParse(ctx, argv, stdinPayload, in, useResume)
}

// ---------------------------------------------------------------------------
// Argv assembly
// ---------------------------------------------------------------------------

func (b *CodexBackend) buildArgv(in CliTurnInput, runID string, useResume bool) (argv []string, stdinPayload []byte, sysPromptFile string, err error) {
	argv = append(argv, b.command...)

	if useResume {
		sid := *in.SessionID
		for _, entry := range b.resumeArgs {
			argv = append(argv, strings.ReplaceAll(entry, "{sessionId}", sid))
		}
	} else {
		argv = append(argv, b.freshArgs...)
	}

	if in.Model != nil && *in.Model != "" {
		argv = append(argv, codexModelFlag, *in.Model)
	}

	// System prompt — only on a fresh session. Codex has no
	// --append-system-prompt; we drop it to a tempfile and point the CLI
	// at it via the model_instructions_file config override.
	if !useResume && strings.TrimSpace(in.SystemPrompt) != "" {
		path, werr := b.writeSystemPrompt(in.SystemPrompt, runID)
		if werr != nil {
			return nil, nil, "", fmt.Errorf("write system prompt: %w", werr)
		}
		sysPromptFile = path
		override, oerr := FormatTOMLConfigOverride(codexSystemPromptConfigKey, path)
		if oerr != nil {
			return nil, nil, sysPromptFile, oerr
		}
		argv = append(argv, codexConfigFlag, override)
	}

	// MCP loopback overlay (F11). Codex uses inline-TOML overrides;
	// serialise the map to inline TOML and emit a single
	// `-c mcp_servers=…` argv entry.
	if in.MCPConfig != nil {
		override, oerr := FormatTOMLConfigOverride(codexMCPServersConfigKey, mapAsAny(in.MCPConfig))
		if oerr != nil {
			return nil, nil, sysPromptFile, oerr
		}
		argv = append(argv, codexConfigFlag, override)
	}

	// Image attachments — one --image <path> per file.
	for _, image := range in.Images {
		argv = append(argv, codexImageFlag, image)
	}

	// User prompt placement — argv if short enough, else stdin.
	if in.UserPrompt != "" {
		if len(in.UserPrompt) > b.maxPromptChars {
			stdinPayload = []byte(in.UserPrompt)
		} else {
			argv = append(argv, in.UserPrompt)
		}
	}

	return argv, stdinPayload, sysPromptFile, nil
}

// mapAsAny coerces a typed CliTurnInput.MCPConfig to the bare `any` that
// SerializeTOMLInlineValue's type-switch on `map[string]any` matches.
func mapAsAny(m map[string]any) any { return m }

func (b *CodexBackend) writeSystemPrompt(systemPrompt, runID string) (string, error) {
	if err := os.MkdirAll(b.tempfileRoot, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(b.tempfileRoot, fmt.Sprintf("codex_sysprompt_%s.md", runID))
	if err := os.WriteFile(path, []byte(systemPrompt), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// ---------------------------------------------------------------------------
// Subprocess
// ---------------------------------------------------------------------------

func (b *CodexBackend) spawnAndParse(ctx context.Context, argv []string, stdinPayload []byte, in CliTurnInput, useResume bool) (*CliTurnOutput, *CliTurnError) {
	if len(argv) == 0 {
		return nil, &CliTurnError{Class: Unknown, Message: "codex argv is empty", ExitCode: -1}
	}

	// Pre-flight — codex not on PATH yields an obvious error class.
	if !resolveExecutable(argv[0]) {
		return nil, &CliTurnError{
			Class:    Unknown,
			Message:  fmt.Sprintf("codex CLI not found on PATH (looked for %q)", argv[0]),
			ExitCode: -1,
		}
	}

	// We manage termination ourselves (SIGTERM-then-SIGKILL grace) so
	// we deliberately do NOT use exec.CommandContext, which would send
	// SIGKILL immediately on cancellation.
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = composeEnv(in.ExtraEnv)
	// Put the subprocess in its own process group so signals reach any
	// child shells (the fake_codex.sh fixture forks `cat`, `sleep`).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if stdinPayload != nil {
		cmd.Stdin = bytes.NewReader(stdinPayload)
	}
	// else: leave Stdin nil → /dev/null on Unix.

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, &CliTurnError{
			Class:    Unknown,
			Message:  fmt.Sprintf("codex CLI spawn failed: %v", err),
			ExitCode: -1,
		}
	}

	// Optional wall-clock timeout via TimeoutSec.
	var timeoutCh <-chan time.Time
	if in.TimeoutSec != nil {
		t := time.NewTimer(time.Duration(float64(time.Second) * *in.TimeoutSec))
		defer t.Stop()
		timeoutCh = t.C
	}

	// Wait in a goroutine so we can race cancel + timeout.
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var (
		waitErr  error
		timedOut bool
		ctxDone  bool
	)
	select {
	case waitErr = <-waitCh:
	case <-ctx.Done():
		ctxDone = true
		terminateProcess(cmd)
		waitErr = <-waitCh
	case <-timeoutCh:
		timedOut = true
		terminateProcess(cmd)
		waitErr = <-waitCh
	}

	stdout := stdoutBuf.String()
	stderr := stderrBuf.String()
	stderrTail := stderr
	if len(stderrTail) > stderrTailBytes {
		stderrTail = stderrTail[len(stderrTail)-stderrTailBytes:]
	}
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	if ctxDone {
		return nil, &CliTurnError{
			Class:      Unknown,
			Message:    fmt.Sprintf("codex CLI cancelled: %v", ctx.Err()),
			ExitCode:   exitCode,
			StderrTail: stderrTail,
		}
	}
	if timedOut {
		return nil, &CliTurnError{
			Class:      Transient,
			Message:    fmt.Sprintf("codex CLI exceeded timeout (%.1fs) and was terminated", *in.TimeoutSec),
			ExitCode:   exitCode,
			StderrTail: stderrTail,
		}
	}

	if waitErr != nil && exitCode == -1 {
		return nil, &CliTurnError{
			Class:      Unknown,
			Message:    fmt.Sprintf("codex CLI wait failed: %v", waitErr),
			ExitCode:   -1,
			StderrTail: stderrTail,
		}
	}

	if exitCode != 0 {
		errText := stderr
		if errText == "" {
			errText = stdout
		}
		var parsed parsedCodexJSONL
		if useResume {
			parsed = parseCodexResumeText(stdout)
		} else {
			parsed = parseCodexJSONL(stdout)
		}
		msg := strings.TrimSpace(errText)
		if msg == "" {
			msg = "codex CLI exited with non-zero status"
		}
		return nil, &CliTurnError{
			Class:      ClassifyCodexError(errText),
			Message:    msg,
			ExitCode:   exitCode,
			StderrTail: stderrTail,
			RawEvents:  parsed.events,
		}
	}

	// Even on exit 0, Codex may signal failure via inline error event.
	var parsed parsedCodexJSONL
	if useResume {
		parsed = parseCodexResumeText(stdout)
	} else {
		parsed = parseCodexJSONL(stdout)
	}
	if parsed.inlineError != "" {
		return nil, &CliTurnError{
			Class:      ClassifyCodexError(parsed.inlineError),
			Message:    parsed.inlineError,
			ExitCode:   exitCode,
			StderrTail: stderrTail,
			RawEvents:  parsed.events,
		}
	}

	// Resume mode preserves the prior session id even though the CLI
	// does not echo one back in plain-text mode.
	var newSID *string
	if parsed.sessionID != "" {
		s := parsed.sessionID
		newSID = &s
	} else if useResume && in.SessionID != nil {
		s := *in.SessionID
		newSID = &s
	}

	usage := parsed.usage
	if usage == nil {
		usage = map[string]any{}
	}
	events := parsed.events
	if events == nil {
		events = []map[string]any{}
	}
	return &CliTurnOutput{
		Text:         parsed.text,
		NewSessionID: newSID,
		Usage:        usage,
		RawEvents:    events,
	}, nil
}

// ---------------------------------------------------------------------------
// JSONL parsing
// ---------------------------------------------------------------------------

// parsedCodexJSONL holds the result of parseCodexJSONL /
// parseCodexResumeText. The lowercase form is internal; cross-language
// tests use the exported [ParseCodexJSONL].
type parsedCodexJSONL struct {
	text        string
	sessionID   string
	usage       map[string]any
	events      []map[string]any
	inlineError string
}

var (
	codexSessionIDPrimaryFields  = []string{"session_id", "sessionId", "conversation_id", "conversationId"}
	codexSessionIDFallbackFields = []string{"thread_id", "threadId"}
)

func extractCodexSessionIDPrimary(event map[string]any) string {
	for _, key := range codexSessionIDPrimaryFields {
		if v, ok := event[key]; ok {
			if s, ok := v.(string); ok {
				if t := strings.TrimSpace(s); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

func extractCodexSessionIDFallback(event map[string]any) string {
	for _, key := range codexSessionIDFallbackFields {
		if v, ok := event[key]; ok {
			if s, ok := v.(string); ok {
				if t := strings.TrimSpace(s); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

// extractCodexUsage pulls token usage out of any event that carries it.
// Mirrors openclaw `toCliUsage` plus the F02 acceptance criterion that
// nested `input_tokens_details.cached_tokens` survives parsing.
//
// Distinct from the Claude `extractUsage` because Codex preserves the
// nested input_tokens_details / prompt_tokens_details map and accepts
// `stats` as a fallback carrier (Gemini-shape).
func extractCodexUsage(event map[string]any) map[string]any {
	rawAny, ok := event["usage"]
	var raw map[string]any
	if ok {
		if m, ok := rawAny.(map[string]any); ok {
			raw = m
		}
	}
	if raw == nil {
		if v, ok := event["stats"]; ok {
			if m, ok := v.(map[string]any); ok {
				raw = m
			}
		}
	}
	if raw == nil {
		return nil
	}

	out := map[string]any{}
	for _, key := range []string{
		"input_tokens",
		"output_tokens",
		"total_tokens",
		"cache_read_input_tokens",
		"cache_creation_input_tokens",
		"cached_input_tokens",
	} {
		if v, ok := raw[key]; ok {
			if n, ok := numericToInt64(v); ok {
				out[key] = n
			}
		}
	}
	for _, nestKey := range []string{"input_tokens_details", "prompt_tokens_details"} {
		nestedAny, ok := raw[nestKey]
		if !ok {
			continue
		}
		nested, ok := nestedAny.(map[string]any)
		if !ok {
			continue
		}
		preserved := map[string]any{}
		for _, sub := range []string{"cached_tokens", "cached_input_tokens"} {
			if v, ok := nested[sub]; ok {
				if n, ok := numericToInt64(v); ok {
					preserved[sub] = n
				}
			}
		}
		if len(preserved) > 0 {
			out[nestKey] = preserved
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// numericToInt64 converts a JSON-decoded number into an int64. Bools are
// not treated as numeric (Python `isinstance(value, (int, float))` skips
// bool because we already filter for known token-count keys).
func numericToInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case float32:
		return int64(n), true
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint:
		return int64(n), true
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return i, true
		}
		f, err := n.Float64()
		if err == nil {
			return int64(f), true
		}
	}
	return 0, false
}

// ParseCodexJSONL parses the full JSONL stream from `codex exec --json`.
// Exported for cross-language fixture validation.
func ParseCodexJSONL(raw string) (text, sessionID string, usage map[string]any, events []map[string]any, inlineError string) {
	p := parseCodexJSONL(raw)
	return p.text, p.sessionID, p.usage, p.events, p.inlineError
}

func parseCodexJSONL(raw string) parsedCodexJSONL {
	var (
		sessionID    string
		usage        = map[string]any{}
		events       []map[string]any
		itemTexts    []string
		fallbackText string
		inlineError  string
	)

	for _, line := range strings.Split(raw, "\n") {
		record := maybeLoadRecord(line)
		if record == nil {
			continue
		}
		events = append(events, record)

		if sessionID == "" {
			sessionID = extractCodexSessionIDPrimary(record)
		}
		if sessionID == "" {
			sessionID = extractCodexSessionIDFallback(record)
		}

		if newUsage := extractCodexUsage(record); len(newUsage) > 0 {
			for k, v := range newUsage {
				usage[k] = v
			}
		}

		if inlineError == "" {
			if e := isInlineError(record); e != "" {
				inlineError = e
			}
		}

		// Collect item.text from item-typed events.
		if itemAny, ok := record["item"]; ok {
			if item, ok := itemAny.(map[string]any); ok {
				if textField, ok := item["text"].(string); ok && textField != "" {
					typeField, _ := item["type"].(string)
					if typeField == "" || strings.Contains(strings.ToLower(typeField), "message") {
						itemTexts = append(itemTexts, textField)
						continue
					}
				}
			}
		}

		if len(itemTexts) == 0 {
			piece := strings.TrimSpace(collectText(record))
			if piece != "" {
				fallbackText = piece
			}
		}
	}

	var finalText string
	if len(itemTexts) > 0 {
		finalText = strings.TrimSpace(strings.Join(itemTexts, "\n"))
	} else {
		finalText = fallbackText
	}

	if events == nil {
		events = []map[string]any{}
	}
	return parsedCodexJSONL{
		text:        finalText,
		sessionID:   sessionID,
		usage:       usage,
		events:      events,
		inlineError: inlineError,
	}
}

func parseCodexResumeText(raw string) parsedCodexJSONL {
	return parsedCodexJSONL{
		text:   strings.TrimSpace(raw),
		usage:  map[string]any{},
		events: []map[string]any{},
	}
}

// ---------------------------------------------------------------------------
// Error classification
// ---------------------------------------------------------------------------

var (
	codexAuthExpiredPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(?:not\s+logged\s+in|please\s+(?:log\s+in|run\s+` + "`?" + `codex\s+login)|run\s+` + "`?" + `codex\s+login` + "`?" + `)\b`),
		regexp.MustCompile(`(?i)\b(?:auth(?:entication)?\s+(?:expired|required|failed))\b`),
		regexp.MustCompile(`(?i)\b(?:invalid|expired|missing)\s+(?:credentials|token|api\s*key|oauth|session)\b`),
		regexp.MustCompile(`(?i)\b401\b.*(?:unauthor)`),
		regexp.MustCompile(`(?i)"code"\s*:\s*"unauthorized"`),
		regexp.MustCompile(`(?i)\bChatGPT\s+(?:login|account)\s+(?:required|expired)\b`),
		regexp.MustCompile(`(?i)\boauth\s+token\s+(?:expired|invalid|missing)\b`),
	}

	codexRateLimitPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\brate[\s_-]?limit`),
		regexp.MustCompile(`(?i)\b429\b`),
		regexp.MustCompile(`(?i)\busage\s+(?:limit|quota)\b`),
		regexp.MustCompile(`(?i)\b(?:quota|tokens?)\s+(?:exhausted|exceeded)\b`),
		regexp.MustCompile(`(?i)\btoo\s+many\s+requests\b`),
		regexp.MustCompile(`(?i)\b(?:weekly|daily|hourly)\s+(?:limit|quota|usage)\s+(?:reached|exceeded)\b`),
		regexp.MustCompile(`(?i)\bplan\s+limit\s+(?:reached|exceeded)\b`),
	}

	codexTransientPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:ECONN(?:RESET|REFUSED|ABORTED)|ETIMEDOUT|EAI_AGAIN)\b`),
		regexp.MustCompile(`(?i)\b(?:connection\s+(?:reset|refused|aborted|timed?\s*out))\b`),
		regexp.MustCompile(`(?i)\b5\d\d\b.*(?:server|gateway|service)`),
		regexp.MustCompile(`(?i)\b(?:network|dns)\s+error\b`),
		regexp.MustCompile(`(?i)\btemporar(?:y|ily)\s+unavailable\b`),
		regexp.MustCompile(`(?i)\boverloaded_error\b`),
		regexp.MustCompile(`(?i)\bservice\s+unavailable\b`),
	}
)

// ClassifyCodexError categorises a Codex CLI failure based on its
// stderr / stdout output. Same four-way split as the Python
// `classify_codex_error`.
func ClassifyCodexError(text string) ErrorClass {
	if text == "" {
		return Unknown
	}
	for _, p := range codexAuthExpiredPatterns {
		if p.MatchString(text) {
			return AuthExpired
		}
	}
	for _, p := range codexRateLimitPatterns {
		if p.MatchString(text) {
			return RateLimit
		}
	}
	for _, p := range codexTransientPatterns {
		if p.MatchString(text) {
			return Transient
		}
	}
	return Unknown
}

// Compile-time check.
var _ CliBackend = (*CodexBackend)(nil)

// errors import is used via context.Err / errors.Is in spawnAndParse.
var _ = errors.Is
