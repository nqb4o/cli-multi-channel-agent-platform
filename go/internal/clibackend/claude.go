// Package clibackend — F04 Claude Code CLI backend (`claude -p`).
//
// Spawns the Claude Code CLI as a subprocess in stream-json mode, parses
// the emitted JSONL events, extracts the final assistant text +
// session_id for resume, and classifies failures so the agent loop can
// drop bad sessions / surface auth-renewal prompts to the user.
//
// Go port of services/runtime/src/runtime/cli_backends/claude.py. The
// behaviour mirrors the Python implementation byte-for-byte for the
// happy paths exercised by the shared fixtures.
//
// Spawn shape:
//
//	claude -p \
//	    --output-format stream-json \
//	    --include-partial-messages \
//	    --verbose \
//	    --setting-sources user \
//	    --permission-mode bypassPermissions \
//	    [--append-system-prompt <text>] \
//	    [--mcp-config <file>] \
//	    [--plugin-dir <dir>] \
//	    [--model <model>] \
//	    # user prompt is sent on stdin
//	    # session resume: append [--resume <sessionId>]
//
// Reuses several free helpers from shared.go in this package:
//   - composeEnv (env-merge with extra overrides)
//   - resolveExecutable (PATH / abs-path resolution → bool)
//   - terminateProcess (SIGTERM-then-SIGKILL grace)
//   - maybeLoadRecord, collectText, isInlineError, extractUsage (parser
//     shared utilities)
//
// Pattern sets and session-id extraction are Claude-specific and live
// in this file.

package clibackend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// ClaudeBackendID is the stable provider id used in agent configs.
const ClaudeBackendID = "claude-cli"

// PluginDirEnvKey carries the F09-generated, run-scoped Claude plugin
// dir. See ClaudeBackend doc for the rationale (frozen base class
// workaround).
const PluginDirEnvKey = "CLAUDE_SKILL_PLUGIN_DIR"

// providerManagedByHostEnv routes the CLI into a separate Anthropic
// billing tier; we always strip it from the spawned env.
const providerManagedByHostEnv = "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST"

// claudeResumeFlag is the documented Claude resume flag.
const claudeResumeFlag = "--resume"

// defaultClaudeFreshArgs mirrors the OpenClaw bundled `claude-cli`
// defaults. `-p` (print mode) + JSONL stream output + bypassPermissions
// (we run inside the user sandbox, not on the host).
var defaultClaudeFreshArgs = []string{
	"-p",
	"--output-format",
	"stream-json",
	"--include-partial-messages",
	"--verbose",
	"--setting-sources",
	"user",
	"--permission-mode",
	"bypassPermissions",
}

// defaultClaudeResumeArgs reuses the same set so the streaming JSONL
// parser stays attached on resume turns.
var defaultClaudeResumeArgs = defaultClaudeFreshArgs

// ---------------------------------------------------------------------
// Error classification — pattern-match stderr / stdout for the
// well-known Claude CLI auth / rate / network failure modes.
//
// Same four-way split as ClassifyCodexError so the agent loop's
// branching logic is provider-agnostic.
// ---------------------------------------------------------------------

var claudeAuthExpiredPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:not\s+logged\s+in|please\s+log\s+in|run\s+` + "`" + `?(?:claude|/login)` + "`" + `?)\b`),
	regexp.MustCompile(`(?i)\b(?:auth(?:entication)?\s+(?:expired|required|failed))\b`),
	regexp.MustCompile(`(?i)\b(?:invalid|expired|missing)\s+(?:credentials|token|api\s*key|oauth)\b`),
	regexp.MustCompile(`(?i)\b401\b.*(?:unauthor)`),
	regexp.MustCompile(`(?i)"code"\s*:\s*"unauthorized"`),
	regexp.MustCompile(`(?i)\boauth\s+token\s+(?:expired|invalid|missing)\b`),
}

var claudeRateLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\brate[\s_-]?limit`),
	regexp.MustCompile(`(?i)\b429\b`),
	regexp.MustCompile(`(?i)\busage\s+(?:limit|quota)\b`),
	regexp.MustCompile(`(?i)\b(?:quota|tokens?)\s+(?:exhausted|exceeded)\b`),
	regexp.MustCompile(`(?i)\btoo\s+many\s+requests\b`),
	regexp.MustCompile(`(?i)\bweekly\s+(?:limit|quota|usage)\s+(?:reached|exceeded)\b`),
}

var claudeTransientPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b(?:ECONN(?:RESET|REFUSED|ABORTED)|ETIMEDOUT|EAI_AGAIN)\b`),
	regexp.MustCompile(`(?i)\b(?:connection\s+(?:reset|refused|aborted|timed?\s*out))\b`),
	regexp.MustCompile(`(?i)\b5\d\d\b.*(?:server|gateway|service)`),
	regexp.MustCompile(`(?i)\b(?:network|dns)\s+error\b`),
	regexp.MustCompile(`(?i)\btemporar(?:y|ily)\s+unavailable\b`),
	regexp.MustCompile(`(?i)\boverloaded_error\b`),
}

// ClassifyClaudeError categorises a Claude CLI failure based on its
// stderr / stdout output. Same four-way split as ClassifyCodexError so
// the agent loop's branching logic is provider-agnostic.
func ClassifyClaudeError(text string) ErrorClass {
	if text == "" {
		return Unknown
	}
	for _, p := range claudeAuthExpiredPatterns {
		if p.MatchString(text) {
			return AuthExpired
		}
	}
	for _, p := range claudeRateLimitPatterns {
		if p.MatchString(text) {
			return RateLimit
		}
	}
	for _, p := range claudeTransientPatterns {
		if p.MatchString(text) {
			return Transient
		}
	}
	return Unknown
}

// ---------------------------------------------------------------------
// stream-json parsing — extract final assistant text + session id +
// usage from the Claude CLI's JSONL stream.
// ---------------------------------------------------------------------

// ParsedClaudeJSONL is the structured result of parsing a Claude
// stream-json stdout dump.
type ParsedClaudeJSONL struct {
	Text        string
	SessionID   string
	Usage       map[string]any
	Events      []map[string]any
	InlineError string
}

// claudeSessionIDFields lists the keys the Claude CLI may use for the
// session/thread identifier.
var claudeSessionIDFields = []string{
	"session_id",
	"sessionId",
	"thread_id",
	"threadId",
}

func extractClaudeSessionID(event map[string]any) string {
	for _, key := range claudeSessionIDFields {
		if v, ok := event[key].(string); ok {
			s := strings.TrimSpace(v)
			if s != "" {
				return s
			}
		}
	}
	return ""
}

// ParseClaudeJSONL parses the full JSONL stream from
// `claude -p --output-format stream-json`.
//
// Priority for final text:
//
//  1. The last `result` event's `result` string (authoritative).
//  2. The concatenation of all `assistant` event message contents
//     (older CLI versions / partial runs).
//  3. Stream-delta accumulator (text_delta concatenation).
func ParseClaudeJSONL(raw string) ParsedClaudeJSONL {
	var (
		finalText     string
		assistantText string
		deltaText     strings.Builder
		sessionID     string
		usage         = map[string]any{}
		events        []map[string]any
		inlineError   string
	)

	for _, line := range strings.Split(raw, "\n") {
		record := maybeLoadRecord(line)
		if record == nil {
			continue
		}
		events = append(events, record)

		if sid := extractClaudeSessionID(record); sid != "" {
			sessionID = sid
		}
		if newUsage := extractUsage(record); len(newUsage) > 0 {
			for k, v := range newUsage {
				usage[k] = v
			}
		}

		if inlineError == "" {
			if err := isInlineError(record); err != "" {
				inlineError = err
			}
		}
		// Claude can also signal failure via `is_error: true` on a
		// `result` event. The shared isInlineError doesn't catch that
		// shape; check it here.
		if inlineError == "" {
			if msg := isErrorOnResult(record); msg != "" {
				inlineError = msg
			}
		}

		typeField, ok := record["type"].(string)
		if !ok {
			continue
		}
		typeLower := strings.ToLower(typeField)

		switch typeLower {
		case "result":
			if rv, ok := record["result"].(string); ok {
				finalText = strings.TrimSpace(rv)
			}
		case "assistant":
			piece := strings.TrimSpace(claudeCollectMessageText(record["message"]))
			if piece != "" {
				if assistantText == "" {
					assistantText = piece
				} else {
					assistantText = assistantText + "\n" + piece
				}
			}
		case "stream_event":
			if eventObj, ok := record["event"].(map[string]any); ok {
				if t, ok := eventObj["type"].(string); ok && t == "content_block_delta" {
					if delta, ok := eventObj["delta"].(map[string]any); ok {
						if dt, ok := delta["type"].(string); ok && dt == "text_delta" {
							if chunk, ok := delta["text"].(string); ok {
								deltaText.WriteString(chunk)
							}
						}
					}
				}
			}
		}
	}

	if finalText == "" {
		if assistantText != "" {
			finalText = assistantText
		} else {
			finalText = strings.TrimSpace(deltaText.String())
		}
	}

	if events == nil {
		events = []map[string]any{}
	}
	if len(usage) == 0 {
		usage = map[string]any{}
	}
	return ParsedClaudeJSONL{
		Text:        finalText,
		SessionID:   sessionID,
		Usage:       usage,
		Events:      events,
		InlineError: inlineError,
	}
}

// claudeCollectMessageText extracts the text out of a Claude assistant
// message payload, joining plain text blocks in order and ignoring
// tool_use / tool_result blocks (which are present on tool-call turns
// but must not leak into the user-facing text).
func claudeCollectMessageText(msg any) string {
	m, ok := msg.(map[string]any)
	if !ok {
		return ""
	}
	contentAny, ok := m["content"]
	if !ok {
		return collectText(m)
	}
	switch c := contentAny.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, blk := range c {
			block, ok := blk.(map[string]any)
			if !ok {
				continue
			}
			t, _ := block["type"].(string)
			if t == "text" {
				if s, ok := block["text"].(string); ok {
					b.WriteString(s)
				}
			}
			// tool_use / tool_result / image / etc → skip.
		}
		return b.String()
	}
	return ""
}

// isErrorOnResult catches the
// `{"type":"result","is_error":true,"result":"..."}` shape used by
// Claude when a turn fails but still emits a result block.
func isErrorOnResult(event map[string]any) string {
	t, _ := event["type"].(string)
	if !strings.EqualFold(t, "result") {
		return ""
	}
	flag, ok := event["is_error"].(bool)
	if !ok || !flag {
		return ""
	}
	for _, key := range []string{"result", "message", "error"} {
		v, ok := event[key]
		if !ok {
			continue
		}
		if s, ok := v.(string); ok {
			if t := strings.TrimSpace(s); t != "" {
				return t
			}
		}
		if m, ok := v.(map[string]any); ok {
			if msg, ok := m["message"].(string); ok && strings.TrimSpace(msg) != "" {
				return strings.TrimSpace(msg)
			}
		}
	}
	if b, err := json.Marshal(event); err == nil {
		return string(b)
	}
	return ""
}

// ---------------------------------------------------------------------
// ClaudeBackend
// ---------------------------------------------------------------------

// ClaudeBackend is the concrete CliBackend for the Anthropic Claude
// Code CLI.
//
// Construction parameters:
//
//   - Command — argv[0]. Tests pass an absolute path to a fake binary
//     (`testdata/claude/fake_claude.sh`).
//   - FreshArgs / ResumeArgs — overridable for fixtures.
//   - PluginDir — default plugin dir to pass via `--plugin-dir` when
//     none is supplied through CliTurnInput.ExtraEnv. Useful for test
//     fixtures and pinned multi-tenant deployments.
//   - MCPConfigRoot — where to drop the per-turn MCP config file that
//     gets passed to `--mcp-config`. Defaults to os.TempDir().
type ClaudeBackend struct {
	Command       []string
	FreshArgs     []string
	ResumeArgs    []string
	PluginDir     string
	MCPConfigRoot string
}

// ClaudeOption is a functional option for NewClaudeBackend.
type ClaudeOption func(*ClaudeBackend)

// WithClaudeCommand overrides argv[0] (and any default flags).
func WithClaudeCommand(cmd ...string) ClaudeOption {
	return func(b *ClaudeBackend) {
		b.Command = append([]string(nil), cmd...)
	}
}

// WithClaudeFreshArgs overrides the fresh-mode argv suffix.
func WithClaudeFreshArgs(args []string) ClaudeOption {
	return func(b *ClaudeBackend) {
		b.FreshArgs = append([]string(nil), args...)
	}
}

// WithClaudeResumeArgs overrides the resume-mode argv suffix.
func WithClaudeResumeArgs(args []string) ClaudeOption {
	return func(b *ClaudeBackend) {
		b.ResumeArgs = append([]string(nil), args...)
	}
}

// WithClaudePluginDir sets the default `--plugin-dir` value (used when
// the per-turn ExtraEnv does not supply one).
func WithClaudePluginDir(dir string) ClaudeOption {
	return func(b *ClaudeBackend) { b.PluginDir = dir }
}

// WithClaudeMCPConfigRoot sets the directory for the per-turn MCP
// config tempfile.
func WithClaudeMCPConfigRoot(dir string) ClaudeOption {
	return func(b *ClaudeBackend) { b.MCPConfigRoot = dir }
}

// NewClaudeBackend constructs a ClaudeBackend with sensible defaults.
func NewClaudeBackend(opts ...ClaudeOption) *ClaudeBackend {
	b := &ClaudeBackend{
		Command:       []string{"claude"},
		FreshArgs:     append([]string(nil), defaultClaudeFreshArgs...),
		ResumeArgs:    append([]string(nil), defaultClaudeResumeArgs...),
		MCPConfigRoot: os.TempDir(),
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// ID returns the stable provider id.
func (*ClaudeBackend) ID() string { return ClaudeBackendID }

// DefaultCommand returns the configured argv[0] (and any default
// flags). Tests / configs may override.
func (b *ClaudeBackend) DefaultCommand() []string {
	if len(b.Command) > 0 {
		out := make([]string, len(b.Command))
		copy(out, b.Command)
		return out
	}
	return []string{"claude"}
}

// SupportsResumeInStream reports that Claude's resume mode reuses the
// same JSONL stream as a fresh session.
func (*ClaudeBackend) SupportsResumeInStream() bool { return true }

// resolvePluginDir determines the run-scoped Claude plugin dir.
//
// Resolution order (frozen-base-class workaround):
//
//  1. CliTurnInput.ExtraEnv[PluginDirEnvKey]
//  2. ClaudeBackend.PluginDir constructor default
//  3. (future) input.SkillPluginDir once the base struct adds the
//     field. Not present today.
func (b *ClaudeBackend) resolvePluginDir(in CliTurnInput) string {
	if v, ok := in.ExtraEnv[PluginDirEnvKey]; ok && strings.TrimSpace(v) != "" {
		return v
	}
	return b.PluginDir
}

// freshArgs returns a fresh copy of the configured fresh-mode args.
func (b *ClaudeBackend) freshArgs() []string {
	if b.FreshArgs == nil {
		return append([]string(nil), defaultClaudeFreshArgs...)
	}
	return append([]string(nil), b.FreshArgs...)
}

// resumeArgs returns a fresh copy of the configured resume-mode args.
func (b *ClaudeBackend) resumeArgs() []string {
	if b.ResumeArgs == nil {
		return append([]string(nil), defaultClaudeResumeArgs...)
	}
	return append([]string(nil), b.ResumeArgs...)
}

// buildArgv builds the full argv for the Claude subprocess.
//
// Returns (argv, mcpFile) where mcpFile is a tempfile path the caller
// must remove after the run, or "" if no MCP config was written.
func (b *ClaudeBackend) buildArgv(in CliTurnInput, runID string, useResume bool) ([]string, string, error) {
	argv := append([]string(nil), b.Command...)
	var mcpFile string

	if useResume {
		argv = append(argv, b.resumeArgs()...)
		argv = append(argv, claudeResumeFlag, *in.SessionID)
	} else {
		argv = append(argv, b.freshArgs()...)
	}

	if in.Model != nil && *in.Model != "" {
		argv = append(argv, "--model", *in.Model)
	}

	// System prompt — only on a fresh session; resume reuses the prior
	// session's appended prompt.
	if !useResume && strings.TrimSpace(in.SystemPrompt) != "" {
		argv = append(argv, "--append-system-prompt", in.SystemPrompt)
	}

	if in.MCPConfig != nil {
		path, err := b.writeMCPConfig(in.MCPConfig, runID)
		if err != nil {
			return nil, "", err
		}
		mcpFile = path
		argv = append(argv, "--mcp-config", path)
	}

	if pluginDir := b.resolvePluginDir(in); pluginDir != "" {
		argv = append(argv, "--plugin-dir", pluginDir)
	}

	return argv, mcpFile, nil
}

// writeMCPConfig writes the MCP config to a tempfile (Claude takes a
// file path).
func (b *ClaudeBackend) writeMCPConfig(cfg map[string]any, runID string) (string, error) {
	root := b.MCPConfigRoot
	if root == "" {
		root = os.TempDir()
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("claude: mcp config root: %w", err)
	}
	path := filepath.Join(root, fmt.Sprintf("claude_mcp_%s.json", runID))
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("claude: mcp config marshal: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("claude: mcp config write: %w", err)
	}
	return path, nil
}

// ComposeEnv builds the env slice for the spawned subprocess.
//
// Mirrors the Python `_compose_env`: starts from os.Environ, drops the
// host-managed-by-host flag (Anthropic billing-tier guard), drops the
// plugin-dir helper key (transport for the backend, not for the CLI),
// then layers the cleaned extras on top.
//
// Exported (capitalised) so tests can probe the env-strip behaviour.
func (b *ClaudeBackend) ComposeEnv(extra map[string]string) []string {
	clean := make(map[string]string, len(extra))
	for k, v := range extra {
		if k == PluginDirEnvKey {
			continue
		}
		clean[k] = v
	}
	merged := composeEnv(clean)
	out := merged[:0]
	prefix := providerManagedByHostEnv + "="
	for _, kv := range merged {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Turn runs one turn of the Claude CLI.
func (b *ClaudeBackend) Turn(ctx context.Context, in CliTurnInput) (*CliTurnOutput, *CliTurnError) {
	runID := ""
	if in.RunID != nil && *in.RunID != "" {
		runID = *in.RunID
	} else {
		runID = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	useResume := in.SessionID != nil && *in.SessionID != ""

	argv, mcpFile, err := b.buildArgv(in, runID, useResume)
	if err != nil {
		return nil, &CliTurnError{
			Class:    Unknown,
			Message:  err.Error(),
			ExitCode: -1,
		}
	}
	if mcpFile != "" {
		defer func() { _ = os.Remove(mcpFile) }()
	}

	// Pre-flight executable check for a clear "not found" error class.
	if !resolveExecutable(argv[0]) {
		return nil, &CliTurnError{
			Class:    Unknown,
			Message:  fmt.Sprintf("claude CLI not found on PATH (looked for %q)", argv[0]),
			ExitCode: -1,
		}
	}

	return b.spawnAndParse(ctx, argv, []byte(in.UserPrompt), in, useResume)
}

// spawnAndParse runs the subprocess, waits for it (honouring ctx
// cancellation + optional TimeoutSec), and parses stdout.
func (b *ClaudeBackend) spawnAndParse(
	ctx context.Context,
	argv []string,
	stdinPayload []byte,
	in CliTurnInput,
	useResume bool,
) (*CliTurnOutput, *CliTurnError) {
	// We manage termination ourselves (SIGTERM-then-SIGKILL grace) so
	// we deliberately do NOT use exec.CommandContext, which would send
	// SIGKILL immediately on cancellation.
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = b.ComposeEnv(in.ExtraEnv)
	// Put the subprocess in its own process group so we can signal it
	// (and any forks) cleanly on cancellation.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, &CliTurnError{
			Class:    Unknown,
			Message:  fmt.Sprintf("claude CLI stdin pipe failed: %s", err),
			ExitCode: -1,
		}
	}
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, &CliTurnError{
			Class:    Unknown,
			Message:  fmt.Sprintf("claude CLI spawn failed: %s", err),
			ExitCode: -1,
		}
	}

	// Feed stdin in a goroutine so a slow child doesn't deadlock.
	stdinErrCh := make(chan error, 1)
	go func() {
		_, werr := stdin.Write(stdinPayload)
		cerr := stdin.Close()
		if werr != nil && !errors.Is(werr, io.ErrClosedPipe) {
			stdinErrCh <- werr
			return
		}
		stdinErrCh <- cerr
	}()

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
	// Drain stdin goroutine.
	<-stdinErrCh

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
			Class:      Transient,
			Message:    "claude CLI cancelled by context",
			ExitCode:   exitCode,
			StderrTail: stderrTail,
		}
	}
	if timedOut {
		return nil, &CliTurnError{
			Class: Transient,
			Message: fmt.Sprintf(
				"claude CLI exceeded timeout (%.1fs) and was terminated",
				*in.TimeoutSec,
			),
			ExitCode:   exitCode,
			StderrTail: "",
		}
	}

	if waitErr != nil && exitCode == -1 {
		return nil, &CliTurnError{
			Class:      Unknown,
			Message:    fmt.Sprintf("claude CLI wait failed: %s", waitErr),
			ExitCode:   -1,
			StderrTail: stderrTail,
		}
	}

	if exitCode != 0 {
		errText := stderr
		if errText == "" {
			errText = stdout
		}
		msg := strings.TrimSpace(errText)
		if msg == "" {
			msg = "claude CLI exited with non-zero status"
		}
		return nil, &CliTurnError{
			Class:      ClassifyClaudeError(errText),
			Message:    msg,
			ExitCode:   exitCode,
			StderrTail: stderrTail,
		}
	}

	parsed := ParseClaudeJSONL(stdout)
	if parsed.InlineError != "" {
		return nil, &CliTurnError{
			Class:      ClassifyClaudeError(parsed.InlineError),
			Message:    parsed.InlineError,
			ExitCode:   exitCode,
			StderrTail: stderrTail,
			RawEvents:  parsed.Events,
		}
	}

	var newSessionID *string
	switch {
	case parsed.SessionID != "":
		s := parsed.SessionID
		newSessionID = &s
	case useResume && in.SessionID != nil:
		s := *in.SessionID
		newSessionID = &s
	}

	usage := parsed.Usage
	if usage == nil {
		usage = map[string]any{}
	}
	return &CliTurnOutput{
		Text:         parsed.Text,
		NewSessionID: newSessionID,
		Usage:        usage,
		RawEvents:    parsed.Events,
	}, nil
}

// Compile-time check that *ClaudeBackend satisfies CliBackend.
var _ CliBackend = (*ClaudeBackend)(nil)
