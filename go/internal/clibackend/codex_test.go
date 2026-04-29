// F02 — tests for [CodexBackend].
//
// Tests drive CodexBackend against a fake codex shell script
// (testdata/codex/fake_codex.sh) so we exercise the real subprocess /
// stdin / argv code paths without needing a live codex binary.
//
// Coverage map (mirrors F02 acceptance criteria):
//
//   - JSONL parsing — happy path, nested cached_tokens preserved.
//   - Inline error events — auth_expired / rate_limit classification.
//   - Non-zero exit + stderr — auth_expired classification.
//   - Resume turn — switches to `codex exec resume <sid>`, drops
//     --json, parses plain-text stdout, preserves prior session id.
//   - Long-prompt routing — > 8000 char prompts go on stdin.
//   - MCP config — emitted as a single inline-TOML -c argv entry.
//   - Cancellation — context.Cancel kills the subprocess in well under
//     one second.
//   - TOML inline serialiser — booleans, escapes, nested maps.
//   - Cross-language — Python fixture JSONL produces identical text +
//     session_id + usage when parsed in Go.

package clibackend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCodexInvocation captures the env / argv / stdin a fake_codex.sh
// run will see.
type fakeCodexInvocation struct {
	env         map[string]string
	argvLogPath string
	stdinLogPath string
}

func (f *fakeCodexInvocation) ReadArgv(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.argvLogPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("read argv log: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func (f *fakeCodexInvocation) ReadStdin(t *testing.T) string {
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

// codexFixturesDir returns the absolute path to the codex testdata dir,
// using the runtime caller's filename as the anchor (so the path is
// independent of the working directory the test was invoked from).
func codexFixturesDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "testdata", "codex")
}

// fakeCodexPath returns the absolute path to the fake codex shell
// script, ensuring the executable bit is set (git can lose it).
func fakeCodexPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(codexFixturesDir(t), "fake_codex.sh")
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat fake_codex.sh: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatalf("chmod fake_codex.sh: %v", err)
		}
	}
	return p
}

// fakeCodexEnvOpts configures fakeCodexEnv.
type fakeCodexEnvOpts struct {
	fixture      string
	rawStdout    string
	stderrText   string
	exitCode     int
	sleepSeconds float64
}

// fakeCodexEnv builds an invocation under tmp_path with the given
// behaviour knobs. Tests pass invocation.env as CliTurnInput.ExtraEnv.
func fakeCodexEnv(t *testing.T, opts fakeCodexEnvOpts) *fakeCodexInvocation {
	t.Helper()
	tmpDir := t.TempDir()
	argvLog := filepath.Join(tmpDir, "argv.log")
	stdinLog := filepath.Join(tmpDir, "stdin.log")

	env := map[string]string{
		"ARGV_LOG":     argvLog,
		"STDIN_LOG":    stdinLog,
		"STDERR_TEXT":  opts.stderrText,
		"EXIT_CODE":    fmt.Sprintf("%d", opts.exitCode),
		"FIXTURES_DIR": codexFixturesDir(t),
	}
	if opts.fixture != "" {
		if opts.rawStdout != "" {
			t.Fatal("pass either fixture or rawStdout, not both")
		}
		env["FIXTURE"] = opts.fixture
	}
	if opts.rawStdout != "" {
		env["RAW_STDOUT"] = opts.rawStdout
	}
	if opts.sleepSeconds > 0 {
		env["SLEEP_SECONDS"] = fmt.Sprintf("%g", opts.sleepSeconds)
	}
	return &fakeCodexInvocation{
		env:          env,
		argvLogPath:  argvLog,
		stdinLogPath: stdinLog,
	}
}

// readFixture loads a JSONL fixture out of the testdata directory.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(codexFixturesDir(t), name)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

// strPtr is a tiny convenience for the *string fields on CliTurnInput.
func strPtr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// JSONL parser unit tests
// ---------------------------------------------------------------------------

func TestParseCodexJSONLHappyPath(t *testing.T) {
	raw := readFixture(t, "happy_path.jsonl")
	parsed := parseCodexJSONL(raw)

	if parsed.text != "hello world" {
		t.Errorf("text: got %q want %q", parsed.text, "hello world")
	}
	if parsed.sessionID != "sess-abc-123" {
		t.Errorf("sessionID: got %q want %q", parsed.sessionID, "sess-abc-123")
	}
	if got, _ := parsed.usage["input_tokens"].(int64); got != 12 {
		t.Errorf("input_tokens: got %v", parsed.usage["input_tokens"])
	}
	if got, _ := parsed.usage["output_tokens"].(int64); got != 4 {
		t.Errorf("output_tokens: got %v", parsed.usage["output_tokens"])
	}
	nested, ok := parsed.usage["input_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("input_tokens_details not preserved: %v", parsed.usage)
	}
	if got, _ := nested["cached_tokens"].(int64); got != 4 {
		t.Errorf("cached_tokens: got %v", nested["cached_tokens"])
	}
	if parsed.inlineError != "" {
		t.Errorf("inlineError: %q", parsed.inlineError)
	}
	if len(parsed.events) != 4 {
		t.Errorf("events: got %d want 4", len(parsed.events))
	}
}

func TestParseCodexJSONLInlineErrorAuth(t *testing.T) {
	raw := readFixture(t, "auth_expired.jsonl")
	parsed := parseCodexJSONL(raw)

	if parsed.inlineError == "" {
		t.Fatal("inlineError should be set")
	}
	if !strings.Contains(strings.ToLower(parsed.inlineError), "codex login") {
		t.Errorf("inlineError missing 'codex login': %q", parsed.inlineError)
	}
	if got := ClassifyCodexError(parsed.inlineError); got != AuthExpired {
		t.Errorf("classification: got %v want %v", got, AuthExpired)
	}
}

func TestParseCodexJSONLInlineErrorRateLimit(t *testing.T) {
	raw := readFixture(t, "rate_limit.jsonl")
	parsed := parseCodexJSONL(raw)

	if parsed.inlineError == "" {
		t.Fatal("inlineError should be set")
	}
	if got := ClassifyCodexError(parsed.inlineError); got != RateLimit {
		t.Errorf("classification: got %v want %v", got, RateLimit)
	}
}

func TestParseCodexJSONLSkipsGarbageLines(t *testing.T) {
	raw := strings.Join([]string{
		"",
		"not json at all",
		`{"type": "session.created", "session_id": "sid-1"}`,
		`partial { "type":`,
		`{"type": "item.completed", "item": {"type": "assistant_message", "text": "ok"}}`,
	}, "\n")
	parsed := parseCodexJSONL(raw)
	if parsed.text != "ok" {
		t.Errorf("text: got %q", parsed.text)
	}
	if parsed.sessionID != "sid-1" {
		t.Errorf("sessionID: got %q", parsed.sessionID)
	}
}

func TestClassifyCodexErrorBuckets(t *testing.T) {
	cases := []struct {
		input string
		want  ErrorClass
	}{
		{"Please run `codex login` to continue.", AuthExpired},
		{"HTTP 429 — rate limit", RateLimit},
		{"ECONNRESET on outbound socket", Transient},
		{"the cat sat on the mat", Unknown},
		{"", Unknown},
	}
	for _, tc := range cases {
		if got := ClassifyCodexError(tc.input); got != tc.want {
			t.Errorf("ClassifyCodexError(%q): got %v want %v", tc.input, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// TOML inline serialiser
// ---------------------------------------------------------------------------

func TestSerializeTOMLInlinePrimitiveValues(t *testing.T) {
	cases := []struct {
		input any
		want  string
	}{
		{"hello", `"hello"`},
		{42, "42"},
		{true, "true"},
		{false, "false"},
	}
	for _, tc := range cases {
		got, err := SerializeTOMLInlineValue(tc.input)
		if err != nil {
			t.Errorf("SerializeTOMLInlineValue(%v): unexpected error %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("SerializeTOMLInlineValue(%v): got %q want %q", tc.input, got, tc.want)
		}
	}
}

func TestSerializeTOMLInlineEscapesStrings(t *testing.T) {
	got, err := SerializeTOMLInlineValue(`he said "hi" \ then left`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `"he said \"hi\" \\ then left"`
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestSerializeTOMLInlineNestedMapAndSlice(t *testing.T) {
	value := map[string]any{
		"servers": []any{
			map[string]any{
				"name": "loopback",
				"port": 8080,
			},
		},
	}
	got, err := SerializeTOMLInlineValue(value)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Sorted keys: "name" < "port"; "servers" is the only top-level key.
	want := `{ servers = [{ name = "loopback", port = 8080 }] }`
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestFormatTOMLConfigOverrideShape(t *testing.T) {
	got, err := FormatTOMLConfigOverride("model_instructions_file", "/tmp/x.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `model_instructions_file="/tmp/x.md"`
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Backend integration tests against the fake codex script
// ---------------------------------------------------------------------------

func TestSupportsResumeInStreamIsFalse(t *testing.T) {
	b := NewCodexBackend(WithCodexCommand(fakeCodexPath(t)))
	if b.SupportsResumeInStream() {
		t.Fatal("Codex should NOT support resume in stream")
	}
}

func TestCodexBackendID(t *testing.T) {
	b := NewCodexBackend()
	if b.ID() != "codex-cli" {
		t.Fatalf("ID: got %q want %q", b.ID(), "codex-cli")
	}
}

func TestTurnHappyPath(t *testing.T) {
	tmp := t.TempDir()
	inv := fakeCodexEnv(t, fakeCodexEnvOpts{fixture: "happy_path"})
	b := NewCodexBackend(
		WithCodexCommand(fakeCodexPath(t)),
		WithCodexSystemPromptRoot(tmp),
	)

	out, err := b.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "be concise",
		UserPrompt:   "hi",
		Model:        strPtr("gpt-5"),
		ExtraEnv:     inv.env,
		RunID:        strPtr("run-happy"),
	})
	if err != nil {
		t.Fatalf("turn error: %+v", err)
	}
	if out.Text != "hello world" {
		t.Errorf("Text: got %q want %q", out.Text, "hello world")
	}
	if out.NewSessionID == nil || *out.NewSessionID != "sess-abc-123" {
		t.Errorf("NewSessionID: got %v", out.NewSessionID)
	}
	if got, _ := out.Usage["input_tokens"].(int64); got != 12 {
		t.Errorf("input_tokens: got %v", out.Usage["input_tokens"])
	}
	nested, ok := out.Usage["input_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("input_tokens_details missing: %v", out.Usage)
	}
	if got, _ := nested["cached_tokens"].(int64); got != 4 {
		t.Errorf("cached_tokens: got %v", nested["cached_tokens"])
	}
	if len(out.RawEvents) != 4 {
		t.Errorf("RawEvents: got %d want 4", len(out.RawEvents))
	}

	argv := inv.ReadArgv(t)
	// Default fresh args present.
	if len(argv) < 2 || argv[0] != "exec" || argv[1] != "--json" {
		t.Fatalf("argv prefix: got %v", argv[:min(len(argv), 4)])
	}
	// Find -c followed by model_instructions_file=...
	var sysPromptArg string
	for _, a := range argv {
		if strings.HasPrefix(a, "model_instructions_file=") {
			sysPromptArg = a
		}
	}
	if sysPromptArg == "" {
		t.Fatalf("model_instructions_file -c override missing in argv: %v", argv)
	}
	// The tempfile should have been cleaned up after the run.
	pathPart := strings.TrimPrefix(sysPromptArg, "model_instructions_file=")
	pathPart = strings.Trim(pathPart, `"`)
	if filepath.Dir(pathPart) != tmp {
		t.Errorf("system prompt dir: got %s want %s", filepath.Dir(pathPart), tmp)
	}
	if _, err := os.Stat(pathPart); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("system prompt tempfile should be cleaned up: %v", err)
	}

	// Model flag.
	modelIdx := codexIndexOf(argv, "--model")
	if modelIdx < 0 || argv[modelIdx+1] != "gpt-5" {
		t.Errorf("--model gpt-5 missing: %v", argv)
	}
	// Short prompt rides argv as last entry.
	if argv[len(argv)-1] != "hi" {
		t.Errorf("expected 'hi' as last argv entry: %v", argv)
	}
	if got := inv.ReadStdin(t); got != "" {
		t.Errorf("stdin should be empty for short prompt: got %q", got)
	}
}

func TestTurnLongPromptUsesStdin(t *testing.T) {
	inv := fakeCodexEnv(t, fakeCodexEnvOpts{fixture: "happy_path"})
	b := NewCodexBackend(
		WithCodexCommand(fakeCodexPath(t)),
		WithCodexMaxPromptArgChars(16),
	)

	longPrompt := strings.Repeat("x", 200)
	out, cerr := b.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "",
		UserPrompt:   longPrompt,
		ExtraEnv:     inv.env,
	})
	if cerr != nil {
		t.Fatalf("turn error: %+v", cerr)
	}
	_ = out

	argv := inv.ReadArgv(t)
	for _, a := range argv {
		if a == longPrompt {
			t.Errorf("long prompt should not appear in argv")
		}
	}
	if got := inv.ReadStdin(t); got != longPrompt {
		t.Errorf("stdin: got %q (len %d) want long prompt (len %d)", truncate(got, 40), len(got), len(longPrompt))
	}
}

func TestTurnImageArgs(t *testing.T) {
	inv := fakeCodexEnv(t, fakeCodexEnvOpts{fixture: "happy_path"})
	b := NewCodexBackend(WithCodexCommand(fakeCodexPath(t)))

	_, cerr := b.Turn(context.Background(), CliTurnInput{
		UserPrompt: "describe these",
		Images:     []string{"/sandbox/a.png", "/sandbox/b.jpg"},
		ExtraEnv:   inv.env,
	})
	if cerr != nil {
		t.Fatalf("turn error: %+v", cerr)
	}

	argv := inv.ReadArgv(t)
	type pair struct{ flag, value string }
	var pairs []pair
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == "--image" {
			pairs = append(pairs, pair{argv[i], argv[i+1]})
		}
	}
	want := []pair{
		{"--image", "/sandbox/a.png"},
		{"--image", "/sandbox/b.jpg"},
	}
	if len(pairs) != len(want) {
		t.Fatalf("image pairs: got %v want %v", pairs, want)
	}
	for i, p := range pairs {
		if p != want[i] {
			t.Errorf("image pair %d: got %v want %v", i, p, want[i])
		}
	}
}

func TestTurnMCPConfigInlinedAsTOML(t *testing.T) {
	inv := fakeCodexEnv(t, fakeCodexEnvOpts{fixture: "happy_path"})
	b := NewCodexBackend(WithCodexCommand(fakeCodexPath(t)))

	mcpConfig := map[string]any{
		"loopback": map[string]any{
			"command": "openclaw-mcp",
			"args":    []any{"--token", "abc"},
		},
	}
	_, cerr := b.Turn(context.Background(), CliTurnInput{
		UserPrompt: "hi",
		MCPConfig:  mcpConfig,
		ExtraEnv:   inv.env,
	})
	if cerr != nil {
		t.Fatalf("turn error: %+v", cerr)
	}

	argv := inv.ReadArgv(t)
	var mcpArg string
	for _, a := range argv {
		if strings.HasPrefix(a, "mcp_servers=") {
			mcpArg = a
			break
		}
	}
	if mcpArg == "" {
		t.Fatalf("mcp_servers -c override missing: %v", argv)
	}
	if !strings.HasPrefix(mcpArg, "mcp_servers={") {
		t.Errorf("mcp_servers should start with inline-table: got %q", mcpArg)
	}
	if !strings.Contains(mcpArg, "loopback") {
		t.Errorf("mcp_servers should mention 'loopback': got %q", mcpArg)
	}
	if !strings.Contains(mcpArg, `"openclaw-mcp"`) {
		t.Errorf("mcp_servers should contain quoted 'openclaw-mcp': got %q", mcpArg)
	}
}

func TestTurnResumeDropsJSONAndUsesTextOutput(t *testing.T) {
	// Resume turn emits plain text → feed RAW_STDOUT.
	inv := fakeCodexEnv(t, fakeCodexEnvOpts{rawStdout: "hello, again.\n"})
	b := NewCodexBackend(WithCodexCommand(fakeCodexPath(t)))

	out, cerr := b.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "ignored on resume",
		UserPrompt:   "continue",
		SessionID:    strPtr("sess-prev-1"),
		ExtraEnv:     inv.env,
	})
	if cerr != nil {
		t.Fatalf("turn error: %+v", cerr)
	}

	if out.Text != "hello, again." {
		t.Errorf("Text: got %q want %q", out.Text, "hello, again.")
	}
	if out.NewSessionID == nil || *out.NewSessionID != "sess-prev-1" {
		t.Errorf("NewSessionID should preserve prior session: got %v", out.NewSessionID)
	}
	if len(out.Usage) != 0 {
		t.Errorf("Usage should be empty on plain-text resume: %v", out.Usage)
	}
	if len(out.RawEvents) != 0 {
		t.Errorf("RawEvents should be empty on plain-text resume: %v", out.RawEvents)
	}

	argv := inv.ReadArgv(t)
	if len(argv) < 3 || argv[0] != "exec" || argv[1] != "resume" || argv[2] != "sess-prev-1" {
		t.Fatalf("argv prefix: got %v", argv[:min(len(argv), 4)])
	}
	for _, a := range argv {
		if a == "--json" {
			t.Errorf("--json should NOT appear on resume: %v", argv)
		}
		if strings.HasPrefix(a, "model_instructions_file=") {
			t.Errorf("model_instructions_file should NOT be emitted on resume: %v", argv)
		}
	}
}

func TestTurnInlineErrorAuthExpired(t *testing.T) {
	inv := fakeCodexEnv(t, fakeCodexEnvOpts{fixture: "auth_expired"})
	b := NewCodexBackend(WithCodexCommand(fakeCodexPath(t)))

	out, cerr := b.Turn(context.Background(), CliTurnInput{
		UserPrompt: "hi",
		ExtraEnv:   inv.env,
	})
	if out != nil {
		t.Fatalf("expected error, got output: %+v", out)
	}
	if cerr.Class != AuthExpired {
		t.Errorf("Class: got %v want %v", cerr.Class, AuthExpired)
	}
	if !strings.Contains(strings.ToLower(cerr.Message), "codex login") {
		t.Errorf("message should mention 'codex login': %q", cerr.Message)
	}
	hasErrorEvent := false
	for _, ev := range cerr.RawEvents {
		if t, _ := ev["type"].(string); t == "error" {
			hasErrorEvent = true
			break
		}
	}
	if !hasErrorEvent {
		t.Errorf("RawEvents should contain the error event: %v", cerr.RawEvents)
	}
}

func TestTurnInlineErrorRateLimit(t *testing.T) {
	inv := fakeCodexEnv(t, fakeCodexEnvOpts{fixture: "rate_limit"})
	b := NewCodexBackend(WithCodexCommand(fakeCodexPath(t)))

	out, cerr := b.Turn(context.Background(), CliTurnInput{
		UserPrompt: "hi",
		ExtraEnv:   inv.env,
	})
	if out != nil {
		t.Fatalf("expected error, got output: %+v", out)
	}
	if cerr.Class != RateLimit {
		t.Errorf("Class: got %v want %v", cerr.Class, RateLimit)
	}
}

func TestTurnNonzeroExitClassifiesStderr(t *testing.T) {
	inv := fakeCodexEnv(t, fakeCodexEnvOpts{
		rawStdout:  "",
		stderrText: "auth expired — please run codex login\n",
		exitCode:   2,
	})
	b := NewCodexBackend(WithCodexCommand(fakeCodexPath(t)))

	out, cerr := b.Turn(context.Background(), CliTurnInput{
		UserPrompt: "hi",
		ExtraEnv:   inv.env,
	})
	if out != nil {
		t.Fatalf("expected error, got output: %+v", out)
	}
	if cerr.Class != AuthExpired {
		t.Errorf("Class: got %v want %v", cerr.Class, AuthExpired)
	}
	if cerr.ExitCode != 2 {
		t.Errorf("ExitCode: got %d want 2", cerr.ExitCode)
	}
	if !strings.Contains(strings.ToLower(cerr.StderrTail), "codex login") {
		t.Errorf("StderrTail should contain 'codex login': %q", cerr.StderrTail)
	}
}

func TestTurnMissingExecutableReturnsUnknownError(t *testing.T) {
	tmp := t.TempDir()
	noSuch := filepath.Join(tmp, "no-such-codex")
	b := NewCodexBackend(WithCodexCommand(noSuch))

	out, cerr := b.Turn(context.Background(), CliTurnInput{UserPrompt: "hi"})
	if out != nil {
		t.Fatalf("expected error, got output: %+v", out)
	}
	if cerr.Class != Unknown {
		t.Errorf("Class: got %v want %v", cerr.Class, Unknown)
	}
	if !strings.Contains(strings.ToLower(cerr.Message), "not found") {
		t.Errorf("Message should mention 'not found': %q", cerr.Message)
	}
}

func TestTurnCancellationKillsSubprocessUnderOneSecond(t *testing.T) {
	inv := fakeCodexEnv(t, fakeCodexEnvOpts{
		fixture:      "happy_path",
		sleepSeconds: 10.0,
	})
	b := NewCodexBackend(WithCodexCommand(fakeCodexPath(t)))

	ctx, cancel := context.WithCancel(context.Background())

	var (
		wg      sync.WaitGroup
		runErr  *CliTurnError
		runOut  *CliTurnOutput
		started = make(chan struct{})
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		runOut, runErr = b.Turn(ctx, CliTurnInput{
			UserPrompt: "hi",
			ExtraEnv:   inv.env,
		})
	}()

	<-started
	// Give the subprocess a moment to spawn and start sleeping.
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	cancel()
	wg.Wait()
	elapsed := time.Since(start)

	if runOut != nil {
		t.Fatalf("expected error, got output: %+v", runOut)
	}
	if runErr == nil {
		t.Fatal("expected CliTurnError")
	}
	// Acceptance criterion: cancellation kills subprocess in < 1 s.
	if elapsed > 2*time.Second {
		t.Errorf("cancellation took %v (> 2s)", elapsed)
	}
}

// ---------------------------------------------------------------------------
// Cross-language compatibility — the Python fixture parses identically
// in Go.
// ---------------------------------------------------------------------------

func TestCrossLangHappyPathFixtureMatchesPython(t *testing.T) {
	// Same JSONL the Python tests load via `(jsonl_dir / "happy_path.jsonl").read_text()`.
	raw := readFixture(t, "happy_path.jsonl")
	text, sessionID, usage, events, inlineError := ParseCodexJSONL(raw)

	if text != "hello world" {
		t.Errorf("text: got %q want %q", text, "hello world")
	}
	if sessionID != "sess-abc-123" {
		t.Errorf("sessionID: got %q want %q", sessionID, "sess-abc-123")
	}
	if got, _ := usage["input_tokens"].(int64); got != 12 {
		t.Errorf("input_tokens: got %v", usage["input_tokens"])
	}
	if got, _ := usage["output_tokens"].(int64); got != 4 {
		t.Errorf("output_tokens: got %v", usage["output_tokens"])
	}
	nested, ok := usage["input_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("input_tokens_details missing: %v", usage)
	}
	if got, _ := nested["cached_tokens"].(int64); got != 4 {
		t.Errorf("cached_tokens: got %v", nested["cached_tokens"])
	}
	if inlineError != "" {
		t.Errorf("inlineError: %q", inlineError)
	}
	if len(events) != 4 {
		t.Errorf("events: got %d want 4", len(events))
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func codexIndexOf(slice []string, target string) int {
	for i, s := range slice {
		if s == target {
			return i
		}
	}
	return -1
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
