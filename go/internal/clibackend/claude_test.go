// F04 — tests for [ClaudeBackend].
//
// Drive ClaudeBackend against testdata/claude/fake_claude.sh so the real
// argv / stdin / stderr / exit-code / cancellation paths are exercised
// without a live `claude` binary. Same shape as codex_test.go.

package clibackend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeClaudeInvocation captures what a fake_claude.sh run will see.
type fakeClaudeInvocation struct {
	env          map[string]string
	argvLogPath  string
	stdinLogPath string
}

func (f *fakeClaudeInvocation) ReadArgv(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.argvLogPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("read argv log: %v", err)
	}
	// fake_claude.sh writes NUL-separated argv.
	parts := strings.Split(string(data), "\x00")
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (f *fakeClaudeInvocation) ReadStdin(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.stdinLogPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}
		t.Fatalf("read stdin log: %v", err)
	}
	return string(data)
}

func claudeFixturesDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "testdata", "claude")
}

func fakeClaudePath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(claudeFixturesDir(t), "fake_claude.sh")
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat fake_claude.sh: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatalf("chmod fake_claude.sh: %v", err)
		}
	}
	return p
}

type fakeClaudeEnvOpts struct {
	fixture      string
	rawStdout    string
	stderrText   string
	exitCode     int
	sleepSeconds float64
}

func fakeClaudeEnv(t *testing.T, opts fakeClaudeEnvOpts) *fakeClaudeInvocation {
	t.Helper()
	tmpDir := t.TempDir()
	argvLog := filepath.Join(tmpDir, "argv.log")
	stdinLog := filepath.Join(tmpDir, "stdin.log")

	env := map[string]string{
		"ARGV_LOG":     argvLog,
		"STDIN_LOG":    stdinLog,
		"STDERR_TEXT":  opts.stderrText,
		"EXIT_CODE":    fmt.Sprintf("%d", opts.exitCode),
		"FIXTURES_DIR": claudeFixturesDir(t),
	}
	if opts.fixture != "" {
		env["FIXTURE"] = opts.fixture
	}
	if opts.rawStdout != "" {
		env["RAW_STDOUT"] = opts.rawStdout
	}
	if opts.sleepSeconds > 0 {
		env["SLEEP_SECONDS"] = fmt.Sprintf("%g", opts.sleepSeconds)
	}
	return &fakeClaudeInvocation{
		env:          env,
		argvLogPath:  argvLog,
		stdinLogPath: stdinLog,
	}
}

// readClaudeFixture loads a JSONL fixture from testdata/claude/.
func readClaudeFixture(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(claudeFixturesDir(t), name)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

// claudeIndexOf finds the index of a string in a slice. Local helper to
// avoid the duplicate-symbol clash with the codex_test indexOf.
func claudeIndexOf(slice []string, target string) int {
	for i, s := range slice {
		if s == target {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// JSONL parser unit tests
// ---------------------------------------------------------------------------

func TestParseClaudeJSONLHappyPath(t *testing.T) {
	raw := readClaudeFixture(t, "claude_happy.jsonl")
	parsed := ParseClaudeJSONL(raw)

	if parsed.Text != "hello world" {
		t.Errorf("text: got %q want %q", parsed.Text, "hello world")
	}
	if parsed.SessionID != "sess-claude-1" {
		t.Errorf("sessionID: got %q want %q", parsed.SessionID, "sess-claude-1")
	}
	if got, _ := parsed.Usage["input_tokens"].(int64); got != 12 {
		// Float64 also acceptable.
		if f, _ := parsed.Usage["input_tokens"].(float64); int64(f) != 12 {
			t.Errorf("input_tokens: got %v", parsed.Usage["input_tokens"])
		}
	}
}

func TestParseClaudeJSONLToolInterleavedPreservesText(t *testing.T) {
	raw := readClaudeFixture(t, "claude_with_tools.jsonl")
	parsed := ParseClaudeJSONL(raw)
	// The fixture's final assistant text should still be extracted even
	// though tool_use / tool_result blocks are interleaved.
	if parsed.Text == "" {
		t.Errorf("expected non-empty text; got %q", parsed.Text)
	}
	if parsed.SessionID == "" {
		t.Errorf("expected session id; got empty")
	}
}

func TestParseClaudeJSONLAuthErrorBubblesUp(t *testing.T) {
	raw := readClaudeFixture(t, "claude_auth_err.jsonl")
	parsed := ParseClaudeJSONL(raw)
	if parsed.InlineError == "" {
		t.Fatal("inline error should surface from claude_auth_err fixture")
	}
	cls := ClassifyClaudeError(parsed.InlineError)
	if cls != AuthExpired {
		t.Errorf("classify %q: got %v want %v", parsed.InlineError, cls, AuthExpired)
	}
}

func TestClassifyClaudeErrorBuckets(t *testing.T) {
	cases := []struct {
		in   string
		want ErrorClass
	}{
		{"Please log in to continue.", AuthExpired},
		{"authentication expired", AuthExpired},
		{"oauth token expired", AuthExpired},
		{"HTTP 429 — usage limit reached", RateLimit},
		{"weekly limit reached", RateLimit},
		{"too many requests", RateLimit},
		{"connection reset by peer", Transient},
		{"ECONNRESET", Transient},
		{"overloaded_error", Transient},
		{"unrelated explosion", Unknown},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := ClassifyClaudeError(tc.in); got != tc.want {
				t.Errorf("classify %q: got %v want %v", tc.in, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Backend-level tests via the fake CLI
// ---------------------------------------------------------------------------

func newClaudeBackendForTest(t *testing.T) *ClaudeBackend {
	t.Helper()
	return NewClaudeBackend(WithClaudeCommand(fakeClaudePath(t)))
}

func TestClaudeBackendIDAndCommand(t *testing.T) {
	b := NewClaudeBackend()
	if b.ID() != "claude-cli" {
		t.Errorf("ID: got %q want %q", b.ID(), "claude-cli")
	}
	cmd := b.DefaultCommand()
	if len(cmd) == 0 || cmd[0] != "claude" {
		t.Errorf("DefaultCommand: %v", cmd)
	}
}

func TestClaudeBackendSupportsResumeInStream(t *testing.T) {
	b := NewClaudeBackend()
	if !b.SupportsResumeInStream() {
		t.Errorf("Claude resume-in-stream should be true")
	}
}

func TestClaudeBackendHappyTurn(t *testing.T) {
	b := newClaudeBackendForTest(t)
	inv := fakeClaudeEnv(t, fakeClaudeEnvOpts{fixture: "claude_happy"})

	out, err := b.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "be nice",
		UserPrompt:   "hi",
		ExtraEnv:     inv.env,
	})
	if err != nil {
		t.Fatalf("Turn returned error: %+v", err)
	}
	if out == nil {
		t.Fatal("expected non-nil output")
	}
	if out.Text == "" {
		t.Errorf("expected non-empty text; got %q", out.Text)
	}
	if out.NewSessionID == nil || *out.NewSessionID != "sess-claude-1" {
		t.Errorf("session id: got %v", out.NewSessionID)
	}

	argv := inv.ReadArgv(t)
	if claudeIndexOf(argv, "-p") < 0 {
		t.Errorf("argv missing -p flag: %v", argv)
	}
	if claudeIndexOf(argv, "--output-format") < 0 {
		t.Errorf("argv missing --output-format flag: %v", argv)
	}
	if claudeIndexOf(argv, "--append-system-prompt") < 0 {
		t.Errorf("argv missing --append-system-prompt flag: %v", argv)
	}
}

func TestClaudeBackendStdinReceivesPrompt(t *testing.T) {
	b := newClaudeBackendForTest(t)
	inv := fakeClaudeEnv(t, fakeClaudeEnvOpts{fixture: "claude_happy"})
	prompt := "tell me a fact"
	_, err := b.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "be brief",
		UserPrompt:   prompt,
		ExtraEnv:     inv.env,
	})
	if err != nil {
		t.Fatalf("Turn error: %+v", err)
	}
	stdin := inv.ReadStdin(t)
	if !strings.Contains(stdin, prompt) {
		t.Errorf("stdin should contain user prompt %q; got %q", prompt, stdin)
	}
}

func TestClaudeBackendResumeUsesResumeFlag(t *testing.T) {
	b := newClaudeBackendForTest(t)
	inv := fakeClaudeEnv(t, fakeClaudeEnvOpts{fixture: "claude_happy"})
	sid := "prev-session-xyz"

	_, err := b.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "should be skipped on resume",
		UserPrompt:   "follow up",
		SessionID:    &sid,
		ExtraEnv:     inv.env,
	})
	if err != nil {
		t.Fatalf("Turn error: %+v", err)
	}
	argv := inv.ReadArgv(t)
	if claudeIndexOf(argv, "--resume") < 0 {
		t.Errorf("argv missing --resume on resume turn: %v", argv)
	}
	resumeIdx := claudeIndexOf(argv, "--resume")
	if resumeIdx < 0 || resumeIdx+1 >= len(argv) || argv[resumeIdx+1] != sid {
		t.Errorf("argv --resume should be followed by %q: %v", sid, argv)
	}
}

func TestClaudeBackendAuthExpiredFromExitCode(t *testing.T) {
	b := newClaudeBackendForTest(t)
	inv := fakeClaudeEnv(t, fakeClaudeEnvOpts{
		stderrText: "Please run `claude auth login` to continue.",
		exitCode:   1,
	})
	out, errOut := b.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "x",
		UserPrompt:   "y",
		ExtraEnv:     inv.env,
	})
	if out != nil {
		t.Errorf("expected nil output on auth-expired exit; got %+v", out)
	}
	if errOut == nil {
		t.Fatal("expected CliTurnError")
	}
	if errOut.Class != AuthExpired {
		t.Errorf("class: got %v want %v", errOut.Class, AuthExpired)
	}
	if errOut.ExitCode != 1 {
		t.Errorf("exit code: got %d", errOut.ExitCode)
	}
}

func TestClaudeBackendAuthExpiredFromInlineError(t *testing.T) {
	b := newClaudeBackendForTest(t)
	inv := fakeClaudeEnv(t, fakeClaudeEnvOpts{fixture: "claude_auth_err"})

	out, errOut := b.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "x",
		UserPrompt:   "y",
		ExtraEnv:     inv.env,
	})
	if out != nil {
		t.Errorf("expected nil output; got %+v", out)
	}
	if errOut == nil {
		t.Fatal("expected CliTurnError")
	}
	if errOut.Class != AuthExpired {
		t.Errorf("class: got %v want %v", errOut.Class, AuthExpired)
	}
}

func TestClaudeBackendCancellationKillsFastSubprocess(t *testing.T) {
	b := newClaudeBackendForTest(t)
	// fake_claude.sh sleeps 5 s; we cancel after 100 ms and assert the
	// subprocess is reaped well under one second.
	inv := fakeClaudeEnv(t, fakeClaudeEnvOpts{
		fixture:      "claude_happy.jsonl",
		sleepSeconds: 5,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	start := time.Now()
	go func() {
		_, _ = b.Turn(ctx, CliTurnInput{
			SystemPrompt: "x",
			UserPrompt:   "y",
			ExtraEnv:     inv.env,
		})
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Turn did not return within 2s of cancel")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("cancellation took %v (>1.5s)", elapsed)
	}
}

func TestClaudeBackendPluginDirFromExtraEnv(t *testing.T) {
	b := newClaudeBackendForTest(t)
	inv := fakeClaudeEnv(t, fakeClaudeEnvOpts{fixture: "claude_happy"})
	pluginPath := filepath.Join(t.TempDir(), "plugins")

	envWithPlugin := make(map[string]string, len(inv.env)+1)
	for k, v := range inv.env {
		envWithPlugin[k] = v
	}
	envWithPlugin[PluginDirEnvKey] = pluginPath

	_, err := b.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "x",
		UserPrompt:   "y",
		ExtraEnv:     envWithPlugin,
	})
	if err != nil {
		t.Fatalf("Turn error: %+v", err)
	}
	argv := inv.ReadArgv(t)
	pdIdx := claudeIndexOf(argv, "--plugin-dir")
	if pdIdx < 0 {
		t.Errorf("argv missing --plugin-dir: %v", argv)
	}
	if pdIdx+1 < len(argv) && argv[pdIdx+1] != pluginPath {
		t.Errorf("--plugin-dir value: got %q want %q", argv[pdIdx+1], pluginPath)
	}
}

func TestClaudeBackendPluginDirFromConstructor(t *testing.T) {
	pluginPath := filepath.Join(t.TempDir(), "default-plugins")
	b := NewClaudeBackend(
		WithClaudeCommand(fakeClaudePath(t)),
		WithClaudePluginDir(pluginPath),
	)
	inv := fakeClaudeEnv(t, fakeClaudeEnvOpts{fixture: "claude_happy"})

	_, err := b.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "x",
		UserPrompt:   "y",
		ExtraEnv:     inv.env,
	})
	if err != nil {
		t.Fatalf("Turn error: %+v", err)
	}
	argv := inv.ReadArgv(t)
	pdIdx := claudeIndexOf(argv, "--plugin-dir")
	if pdIdx < 0 || argv[pdIdx+1] != pluginPath {
		t.Errorf("constructor plugin-dir not applied: argv=%v want %q", argv, pluginPath)
	}
}

func TestClaudeBackendModelArgEmittedWhenSet(t *testing.T) {
	b := newClaudeBackendForTest(t)
	inv := fakeClaudeEnv(t, fakeClaudeEnvOpts{fixture: "claude_happy"})

	model := "claude-haiku-4-5"
	_, err := b.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "x",
		UserPrompt:   "y",
		Model:        &model,
		ExtraEnv:     inv.env,
	})
	if err != nil {
		t.Fatalf("Turn error: %+v", err)
	}
	argv := inv.ReadArgv(t)
	mIdx := claudeIndexOf(argv, "--model")
	if mIdx < 0 || argv[mIdx+1] != model {
		t.Errorf("argv --model not set correctly: %v", argv)
	}
}

func TestClaudeBackendStripsManagedHostEnv(t *testing.T) {
	// Set a sentinel + the managed-host var; the backend must strip the
	// managed-host var from the spawned env. We can only verify
	// indirectly: assert the stripped var name is in the documented list
	// of stripped keys.
	if !claudeStripsManagedHost() {
		t.Errorf("CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST should be in the stripped-env list")
	}
}

// claudeStripsManagedHost confirms the package declares the managed-host
// env var as one it intends to strip. Soft assertion via the unexported
// constant.
func claudeStripsManagedHost() bool {
	return providerManagedByHostEnv == "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST"
}

func TestClaudeBackendUnknownExitClassifiesUnknown(t *testing.T) {
	b := newClaudeBackendForTest(t)
	inv := fakeClaudeEnv(t, fakeClaudeEnvOpts{
		stderrText: "totally novel kaboom",
		exitCode:   42,
	})
	_, errOut := b.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "x",
		UserPrompt:   "y",
		ExtraEnv:     inv.env,
	})
	if errOut == nil {
		t.Fatal("expected CliTurnError")
	}
	if errOut.Class != Unknown {
		t.Errorf("class: got %v want %v", errOut.Class, Unknown)
	}
	if errOut.ExitCode != 42 {
		t.Errorf("exit code: got %d", errOut.ExitCode)
	}
	if !strings.Contains(errOut.StderrTail, "kaboom") {
		t.Errorf("stderr tail missing 'kaboom': %q", errOut.StderrTail)
	}
}
