package clibackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fixturesDir returns the absolute path to the Gemini testdata dir, used by
// fake_gemini.sh + the JSON fixtures referenced from the FIXTURE env var.
func geminiFixturesDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("could not resolve gemini fixtures dir")
	}
	return filepath.Join(filepath.Dir(thisFile), "testdata", "gemini")
}

func fakeGeminiPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(geminiFixturesDir(t), "fake_gemini.sh")
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("fake_gemini.sh stat: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatalf("chmod fake_gemini.sh: %v", err)
		}
	}
	return p
}

// fakeGeminiInvocation mirrors FakeGeminiInvocation in the Python tests:
// owns the env dict for one fake_gemini.sh run + the argv log path.
type fakeGeminiInvocation struct {
	env        map[string]string
	argvLogPath string
}

func (f *fakeGeminiInvocation) ReadArgv(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.argvLogPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("read argv log: %v", err)
	}
	if len(data) == 0 {
		return nil
	}
	parts := strings.Split(string(data), "\x00")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

type fakeGeminiOpts struct {
	Fixture       string
	RawStdout     string
	StderrText    string
	ExitCode      int
	SleepSeconds  string
	VersionOutput string
}

func newFakeGeminiInvocation(t *testing.T, opts fakeGeminiOpts) *fakeGeminiInvocation {
	t.Helper()
	tmp := t.TempDir()
	argvLog := filepath.Join(tmp, "argv.log")
	env := map[string]string{
		"ARGV_LOG":     argvLog,
		"STDERR_TEXT":  opts.StderrText,
		"EXIT_CODE":    fmt.Sprintf("%d", opts.ExitCode),
		"FIXTURES_DIR": geminiFixturesDir(t),
	}
	if opts.Fixture != "" && opts.RawStdout != "" {
		t.Fatalf("pass either Fixture or RawStdout, not both")
	}
	if opts.Fixture != "" {
		env["FIXTURE"] = opts.Fixture
	}
	if opts.RawStdout != "" {
		env["RAW_STDOUT"] = opts.RawStdout
	}
	if opts.SleepSeconds != "" {
		env["SLEEP_SECONDS"] = opts.SleepSeconds
	}
	if opts.VersionOutput != "" {
		env["VERSION_OUTPUT"] = opts.VersionOutput
	}
	return &fakeGeminiInvocation{env: env, argvLogPath: argvLog}
}

func ptrStr(s string) *string  { return &s }
func ptrFloat(f float64) *float64 { return &f }

func makeGeminiBackend(t *testing.T, fakePath, workspace, settingsRoot, baseSettings string) *GeminiBackend {
	t.Helper()
	return NewGeminiBackend(GeminiConfig{
		Command:            []string{fakePath},
		WorkspaceRoot:      workspace,
		SettingsRoot:       settingsRoot,
		BaseSettingsPath:   baseSettings,
		CheckVersionOnInit: false,
	})
}

// ---------------------------------------------------------------------------
// Pure parser tests (no subprocess).
// ---------------------------------------------------------------------------

func TestGeminiParseHappyFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(geminiFixturesDir(t), "gemini_happy.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	parsed := ParseGeminiJSON(string(raw))
	if parsed.Text != "hello world" {
		t.Errorf("Text: %q", parsed.Text)
	}
	if parsed.SessionID != "sess-gem-happy-001" {
		t.Errorf("SessionID: %q", parsed.SessionID)
	}
	if parsed.InlineError != "" {
		t.Errorf("InlineError unexpectedly non-empty: %q", parsed.InlineError)
	}
	// stats.cached → cache_read_input_tokens.
	if got := parsed.Usage["cache_read_input_tokens"]; got != 12 {
		t.Errorf("cache_read_input_tokens: %v (%T)", got, got)
	}
	// stats.input_tokens=50, cached=12 → input=38.
	if got := parsed.Usage["input_tokens"]; got != 38 {
		t.Errorf("input_tokens: %v (%T)", got, got)
	}
	if got := parsed.Usage["output_tokens"]; got != 7 {
		t.Errorf("output_tokens: %v (%T)", got, got)
	}
}

func TestGeminiParseResumeFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(geminiFixturesDir(t), "gemini_resume.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	parsed := ParseGeminiJSON(string(raw))
	if parsed.Text != "resumed reply text" {
		t.Errorf("Text: %q", parsed.Text)
	}
	if parsed.SessionID != "prev-sess-42" {
		t.Errorf("SessionID: %q", parsed.SessionID)
	}
	if parsed.Usage["input_tokens"] != 30 {
		t.Errorf("input_tokens: %v", parsed.Usage["input_tokens"])
	}
	if parsed.Usage["output_tokens"] != 4 {
		t.Errorf("output_tokens: %v", parsed.Usage["output_tokens"])
	}
	if parsed.Usage["cache_read_input_tokens"] != 8 {
		t.Errorf("cache_read_input_tokens: %v", parsed.Usage["cache_read_input_tokens"])
	}
}

func TestGeminiParseAuthErrFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(geminiFixturesDir(t), "gemini_auth_err.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	parsed := ParseGeminiJSON(string(raw))
	if parsed.InlineError == "" {
		t.Fatalf("expected InlineError to be set")
	}
	lower := strings.ToLower(parsed.InlineError)
	if !strings.Contains(lower, "401") && !strings.Contains(lower, "invalid") {
		t.Errorf("InlineError missing 401/invalid: %q", parsed.InlineError)
	}
}

func TestGeminiParseStripsLeadingBanner(t *testing.T) {
	raw := "Loading session...\n" + `{"response":"ok","session_id":"s1"}`
	parsed := ParseGeminiJSON(raw)
	if parsed.Text != "ok" {
		t.Errorf("Text: %q", parsed.Text)
	}
	if parsed.SessionID != "s1" {
		t.Errorf("SessionID: %q", parsed.SessionID)
	}
}

func TestGeminiParseHandlesEmptyInput(t *testing.T) {
	parsed := ParseGeminiJSON("")
	if parsed.Text != "" || parsed.SessionID != "" || parsed.InlineError != "" {
		t.Errorf("expected zero values, got %+v", parsed)
	}
}

func TestGeminiParseHandlesMalformedJSON(t *testing.T) {
	parsed := ParseGeminiJSON("not-json-at-all{{{")
	if parsed.Text != "" {
		t.Errorf("Text should be empty, got %q", parsed.Text)
	}
	if !strings.Contains(parsed.InlineError, "could not parse") {
		t.Errorf("InlineError: %q", parsed.InlineError)
	}
}

func TestGeminiParsePicksCamelCaseSessionID(t *testing.T) {
	raw := `{"response":"x","sessionId":"s-camel"}`
	parsed := ParseGeminiJSON(raw)
	if parsed.SessionID != "s-camel" {
		t.Errorf("SessionID: %q", parsed.SessionID)
	}
}

func TestGeminiParseFallsBackToTextField(t *testing.T) {
	raw := `{"text":"from text field","session_id":"s2"}`
	parsed := ParseGeminiJSON(raw)
	if parsed.Text != "from text field" {
		t.Errorf("Text: %q", parsed.Text)
	}
}

func TestGeminiParseUsagePrefersUsageOverStats(t *testing.T) {
	raw := `{
        "response": "x",
        "usage": {"input_tokens": 1, "output_tokens": 2},
        "stats": {"input": 999, "output": 999, "cached": 999}
    }`
	parsed := ParseGeminiJSON(raw)
	if parsed.Usage["input_tokens"] != 1 {
		t.Errorf("input_tokens: %v", parsed.Usage["input_tokens"])
	}
	if parsed.Usage["output_tokens"] != 2 {
		t.Errorf("output_tokens: %v", parsed.Usage["output_tokens"])
	}
	if _, ok := parsed.Usage["cache_read_input_tokens"]; ok {
		t.Errorf("cache_read_input_tokens should NOT be present (stats was ignored)")
	}
}

func TestGeminiParseStatsCachedNormalisesToCacheRead(t *testing.T) {
	raw := `{"response":"x","stats":{"input":10,"output":5,"cached":3}}`
	parsed := ParseGeminiJSON(raw)
	if parsed.Usage["input_tokens"] != 10 {
		t.Errorf("input_tokens: %v", parsed.Usage["input_tokens"])
	}
	if parsed.Usage["output_tokens"] != 5 {
		t.Errorf("output_tokens: %v", parsed.Usage["output_tokens"])
	}
	if parsed.Usage["cache_read_input_tokens"] != 3 {
		t.Errorf("cache_read_input_tokens: %v", parsed.Usage["cache_read_input_tokens"])
	}
}

// ---------------------------------------------------------------------------
// Error classifier.
// ---------------------------------------------------------------------------

func TestClassifyGeminiError(t *testing.T) {
	cases := []struct {
		text string
		want ErrorClass
	}{
		{"401 Unauthorized: oauth token expired", AuthExpired},
		{"Please sign in to gemini before using this command", AuthExpired},
		{"gemini API key invalid", AuthExpired},
		{"HTTP 429 too many requests", RateLimit},
		{"RESOURCE_EXHAUSTED: quota exceeded", RateLimit},
		{"connection reset by peer", Transient},
		{"503 service unavailable, retry later", Transient},
		{"totally inscrutable failure", Unknown},
		{"", Unknown},
	}
	for _, tc := range cases {
		got := ClassifyGeminiError(tc.text)
		if got != tc.want {
			t.Errorf("classify(%q): got %s want %s", tc.text, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Backend integration with the fake CLI.
// ---------------------------------------------------------------------------

func TestGeminiIDAndResumeFlag(t *testing.T) {
	backend := makeGeminiBackend(t, fakeGeminiPath(t), "", "", "")
	if backend.ID() != "google-gemini-cli" {
		t.Errorf("ID: %q", backend.ID())
	}
	if !backend.SupportsResumeInStream() {
		t.Error("SupportsResumeInStream should be true")
	}
}

func TestGeminiHappyTurn(t *testing.T) {
	backend := makeGeminiBackend(t, fakeGeminiPath(t), "", "", "")
	inv := newFakeGeminiInvocation(t, fakeGeminiOpts{Fixture: "gemini_happy"})

	out, errOut := backend.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "be helpful",
		UserPrompt:   "say hi",
		ExtraEnv:     inv.env,
	})
	if errOut != nil {
		t.Fatalf("expected output, got error: %+v", errOut)
	}
	if out.Text != "hello world" {
		t.Errorf("Text: %q", out.Text)
	}
	if out.NewSessionID == nil || *out.NewSessionID != "sess-gem-happy-001" {
		t.Errorf("NewSessionID: %v", out.NewSessionID)
	}
	if out.Usage["cache_read_input_tokens"] != 12 {
		t.Errorf("cache_read_input_tokens: %v", out.Usage["cache_read_input_tokens"])
	}
	if out.Usage["input_tokens"] != 38 {
		t.Errorf("input_tokens: %v", out.Usage["input_tokens"])
	}
	if out.Usage["output_tokens"] != 7 {
		t.Errorf("output_tokens: %v", out.Usage["output_tokens"])
	}

	argv := inv.ReadArgv(t)
	if !contains(argv, "--output-format") {
		t.Errorf("argv missing --output-format: %v", argv)
	}
	if !contains(argv, "json") {
		t.Errorf("argv missing 'json': %v", argv)
	}
	pIdx := indexOf(argv, "--prompt")
	if pIdx < 0 || pIdx+1 >= len(argv) {
		t.Fatalf("argv missing --prompt or its value: %v", argv)
	}
	prompt := argv[pIdx+1]
	if !strings.Contains(prompt, "[SYSTEM]") {
		t.Errorf("prompt missing [SYSTEM] marker: %q", prompt)
	}
	if !strings.Contains(prompt, "be helpful") {
		t.Errorf("prompt missing system prompt body: %q", prompt)
	}
	if !strings.Contains(prompt, "say hi") {
		t.Errorf("prompt missing user prompt body: %q", prompt)
	}
}

func TestGeminiResumeArgvShape(t *testing.T) {
	backend := makeGeminiBackend(t, fakeGeminiPath(t), "", "", "")
	inv := newFakeGeminiInvocation(t, fakeGeminiOpts{Fixture: "gemini_resume"})

	out, errOut := backend.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "ignored on resume",
		UserPrompt:   "continue please",
		SessionID:    ptrStr("prev-sess-42"),
		ExtraEnv:     inv.env,
	})
	if errOut != nil {
		t.Fatalf("expected output, got error: %+v", errOut)
	}
	if out.NewSessionID == nil || *out.NewSessionID != "prev-sess-42" {
		t.Errorf("NewSessionID: %v", out.NewSessionID)
	}
	if out.Text != "resumed reply text" {
		t.Errorf("Text: %q", out.Text)
	}

	argv := inv.ReadArgv(t)
	resumeIdx := indexOf(argv, "--resume")
	if resumeIdx < 0 {
		t.Fatalf("argv missing --resume: %v", argv)
	}
	if argv[resumeIdx+1] != "prev-sess-42" {
		t.Errorf("--resume value: %q", argv[resumeIdx+1])
	}
	if !contains(argv, "--output-format") || !contains(argv, "--prompt") {
		t.Errorf("argv missing required flags: %v", argv)
	}
	prompt := argv[indexOf(argv, "--prompt")+1]
	if strings.Contains(prompt, "[SYSTEM]") {
		t.Errorf("[SYSTEM] should NOT be prepended on resume: %q", prompt)
	}
	if prompt != "continue please" {
		t.Errorf("prompt: %q", prompt)
	}
}

func TestGeminiAuthErrorClassified(t *testing.T) {
	backend := makeGeminiBackend(t, fakeGeminiPath(t), "", "", "")
	inv := newFakeGeminiInvocation(t, fakeGeminiOpts{
		Fixture:  "gemini_auth_err",
		ExitCode: 2,
	})
	out, errOut := backend.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "x",
		UserPrompt:   "x",
		ExtraEnv:     inv.env,
	})
	if out != nil {
		t.Fatalf("expected error, got output: %+v", out)
	}
	if errOut == nil {
		t.Fatal("expected error, got nil")
	}
	if errOut.Class != AuthExpired {
		t.Errorf("Class: %s", errOut.Class)
	}
	if errOut.ExitCode != 2 {
		t.Errorf("ExitCode: %d", errOut.ExitCode)
	}
	lower := strings.ToLower(errOut.Message)
	if !strings.Contains(lower, "401") && !strings.Contains(lower, "invalid") {
		t.Errorf("Message: %q", errOut.Message)
	}
}

func TestGeminiAuthErrorInlineOnZeroExit(t *testing.T) {
	backend := makeGeminiBackend(t, fakeGeminiPath(t), "", "", "")
	inv := newFakeGeminiInvocation(t, fakeGeminiOpts{
		Fixture:  "gemini_auth_err",
		ExitCode: 0,
	})
	out, errOut := backend.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "x",
		UserPrompt:   "x",
		ExtraEnv:     inv.env,
	})
	if out != nil {
		t.Fatalf("expected error, got output: %+v", out)
	}
	if errOut == nil {
		t.Fatal("expected error, got nil")
	}
	if errOut.Class != AuthExpired {
		t.Errorf("Class: %s", errOut.Class)
	}
}

func TestGeminiModelArgv(t *testing.T) {
	backend := makeGeminiBackend(t, fakeGeminiPath(t), "", "", "")
	inv := newFakeGeminiInvocation(t, fakeGeminiOpts{Fixture: "gemini_happy"})
	_, errOut := backend.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "",
		UserPrompt:   "hi",
		Model:        ptrStr("gemini-2.5-pro"),
		ExtraEnv:     inv.env,
	})
	if errOut != nil {
		t.Fatalf("turn errored: %+v", errOut)
	}
	argv := inv.ReadArgv(t)
	mIdx := indexOf(argv, "--model")
	if mIdx < 0 {
		t.Fatalf("argv missing --model: %v", argv)
	}
	if argv[mIdx+1] != "gemini-2.5-pro" {
		t.Errorf("--model value: %q", argv[mIdx+1])
	}
}

func TestGeminiImageArgInsideWorkspace(t *testing.T) {
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	imgPath := filepath.Join(ws, "shot.png")
	if err := os.WriteFile(imgPath, []byte("\x89PNG\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	backend := makeGeminiBackend(t, fakeGeminiPath(t), ws, "", "")
	inv := newFakeGeminiInvocation(t, fakeGeminiOpts{Fixture: "gemini_happy"})
	out, errOut := backend.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "",
		UserPrompt:   "describe",
		Images:       []string{imgPath},
		ExtraEnv:     inv.env,
	})
	if errOut != nil {
		t.Fatalf("expected output, got error: %+v", errOut)
	}
	if out == nil {
		t.Fatal("expected non-nil output")
	}

	argv := inv.ReadArgv(t)
	var imageArgs []string
	for _, a := range argv {
		if strings.HasPrefix(a, "@") {
			imageArgs = append(imageArgs, a)
		}
	}
	if len(imageArgs) != 1 {
		t.Fatalf("expected 1 image arg, got %v", imageArgs)
	}
	resolved := resolveGeminiPath(imgPath)
	want := "@" + resolved
	if imageArgs[0] != want {
		t.Errorf("image arg: %q, want %q", imageArgs[0], want)
	}
}

func TestGeminiImageArgOutsideWorkspaceRejected(t *testing.T) {
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(tmp, "outside.png")
	if err := os.WriteFile(outside, []byte("\x89PNG\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	backend := makeGeminiBackend(t, fakeGeminiPath(t), ws, "", "")
	inv := newFakeGeminiInvocation(t, fakeGeminiOpts{Fixture: "gemini_happy"})
	out, errOut := backend.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "",
		UserPrompt:   "describe",
		Images:       []string{outside},
		ExtraEnv:     inv.env,
	})
	if out != nil {
		t.Fatalf("expected error, got output: %+v", out)
	}
	if errOut == nil {
		t.Fatal("expected error, got nil")
	}
	if errOut.Class != Unknown {
		t.Errorf("Class: %s", errOut.Class)
	}
	if !strings.Contains(errOut.Message, "outside workspace") {
		t.Errorf("Message: %q", errOut.Message)
	}
	// fake binary must not have been invoked at all.
	if argv := inv.ReadArgv(t); len(argv) != 0 {
		t.Errorf("fake binary should not have been invoked, argv=%v", argv)
	}
}

func TestGeminiImageArgWithAtPrefixAccepted(t *testing.T) {
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	imgPath := filepath.Join(ws, "shot.png")
	if err := os.WriteFile(imgPath, []byte("\x89PNG\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	backend := makeGeminiBackend(t, fakeGeminiPath(t), ws, "", "")
	inv := newFakeGeminiInvocation(t, fakeGeminiOpts{Fixture: "gemini_happy"})
	_, errOut := backend.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "",
		UserPrompt:   "describe",
		Images:       []string{"@" + imgPath},
		ExtraEnv:     inv.env,
	})
	if errOut != nil {
		t.Fatalf("turn errored: %+v", errOut)
	}
	argv := inv.ReadArgv(t)
	want := "@" + resolveGeminiPath(imgPath)
	if !contains(argv, want) {
		t.Errorf("argv missing %q: %v", want, argv)
	}
}

func TestGeminiMCPSettingsOverlay(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, "user-gemini", "settings.json")
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		t.Fatal(err)
	}
	baseContent := `{"theme":"dark","mcpServers":{"existing":{"url":"u"}}}`
	if err := os.WriteFile(base, []byte(baseContent), 0o644); err != nil {
		t.Fatal(err)
	}

	settingsRoot := filepath.Join(tmp, "settings-tmp")
	backend := makeGeminiBackend(t, fakeGeminiPath(t), "", settingsRoot, base)
	inv := newFakeGeminiInvocation(t, fakeGeminiOpts{Fixture: "gemini_happy"})

	mcp := map[string]any{
		"mcpServers": map[string]any{
			"loopback": map[string]any{
				"url":   "http://127.0.0.1:8765/mcp",
				"trust": true,
			},
		},
	}

	_, errOut := backend.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "",
		UserPrompt:   "hi",
		MCPConfig:    mcp,
		ExtraEnv:     inv.env,
	})
	if errOut != nil {
		t.Fatalf("expected output, got error: %+v", errOut)
	}

	// Overlay tempdir should be cleaned up after the turn.
	leftovers := []string{}
	if _, err := os.Stat(settingsRoot); err == nil {
		_ = filepath.Walk(settingsRoot, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if !info.IsDir() && strings.HasSuffix(path, "settings.json") {
				leftovers = append(leftovers, path)
			}
			return nil
		})
	}
	if len(leftovers) != 0 {
		t.Errorf("expected overlay tempdir cleaned, leftovers=%v", leftovers)
	}
}

func TestGeminiMCPSettingsOverlayMergesUserConfig(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, "user-gemini", "settings.json")
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		t.Fatal(err)
	}
	baseContent := `{"theme":"dark","mcpServers":{"existing":{"url":"u-existing"}}}`
	if err := os.WriteFile(base, []byte(baseContent), 0o644); err != nil {
		t.Fatal(err)
	}

	settingsRoot := filepath.Join(tmp, "settings-tmp")
	mcp := map[string]any{
		"mcpServers": map[string]any{
			"loopback": map[string]any{"url": "u-loop", "trust": true},
		},
	}
	overlay, err := buildGeminiSettingsOverlay(mcp, base, settingsRoot, "run-abc")
	if err != nil {
		t.Fatalf("buildGeminiSettingsOverlay: %v", err)
	}
	defer overlay.cleanup()

	data, err := os.ReadFile(overlay.Path())
	if err != nil {
		t.Fatalf("read overlay: %v", err)
	}
	var merged map[string]any
	if err := json.Unmarshal(data, &merged); err != nil {
		t.Fatalf("parse overlay: %v", err)
	}
	if merged["theme"] != "dark" {
		t.Errorf("theme not preserved: %v", merged["theme"])
	}
	servers, _ := merged["mcpServers"].(map[string]any)
	if _, ok := servers["existing"]; !ok {
		t.Errorf("existing server lost: %+v", servers)
	}
	if _, ok := servers["loopback"]; !ok {
		t.Errorf("loopback server missing: %+v", servers)
	}
	loopback, _ := servers["loopback"].(map[string]any)
	if loopback["url"] != "u-loop" {
		t.Errorf("loopback url: %v", loopback["url"])
	}
	mcpBlock, _ := merged["mcp"].(map[string]any)
	allowed, _ := mcpBlock["allowed"].([]any)
	hasLoopback := false
	for _, a := range allowed {
		if a == "loopback" {
			hasLoopback = true
		}
	}
	if !hasLoopback {
		t.Errorf("mcp.allowed missing loopback: %+v", allowed)
	}
}

func TestGeminiExtraEnvPassedThrough(t *testing.T) {
	backend := makeGeminiBackend(t, fakeGeminiPath(t), "", "", "")
	inv := newFakeGeminiInvocation(t, fakeGeminiOpts{Fixture: "gemini_happy"})
	inv.env["OPENCLAW_MCP_TOKEN"] = "secret-token-abc"
	_, errOut := backend.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "",
		UserPrompt:   "hi",
		ExtraEnv:     inv.env,
	})
	if errOut != nil {
		t.Fatalf("turn errored: %+v", errOut)
	}
}

func TestGeminiTimeoutReturnsTransientError(t *testing.T) {
	backend := makeGeminiBackend(t, fakeGeminiPath(t), "", "", "")
	inv := newFakeGeminiInvocation(t, fakeGeminiOpts{
		Fixture:      "gemini_happy",
		SleepSeconds: "2.0",
	})
	out, errOut := backend.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "",
		UserPrompt:   "hi",
		ExtraEnv:     inv.env,
		TimeoutSec:   ptrFloat(0.3),
	})
	if out != nil {
		t.Fatalf("expected error, got output: %+v", out)
	}
	if errOut == nil {
		t.Fatal("expected error")
	}
	if errOut.Class != Transient {
		t.Errorf("Class: %s", errOut.Class)
	}
	if !strings.Contains(strings.ToLower(errOut.Message), "timeout") {
		t.Errorf("Message: %q", errOut.Message)
	}
}

func TestGeminiCancellationKillsSubprocess(t *testing.T) {
	backend := makeGeminiBackend(t, fakeGeminiPath(t), "", "", "")
	inv := newFakeGeminiInvocation(t, fakeGeminiOpts{
		Fixture:      "gemini_happy",
		SleepSeconds: "5.0",
	})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		_, _ = backend.Turn(ctx, CliTurnInput{
			SystemPrompt: "",
			UserPrompt:   "hi",
			ExtraEnv:     inv.env,
		})
		close(done)
	}()

	// Give the subprocess a moment to actually spawn.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// success
	case <-time.After(5 * time.Second):
		t.Fatal("Turn did not return after cancel")
	}
}

func TestGeminiMissingExecutable(t *testing.T) {
	tmp := t.TempDir()
	backend := NewGeminiBackend(GeminiConfig{
		Command:            []string{filepath.Join(tmp, "nope-gemini-binary")},
		CheckVersionOnInit: false,
	})
	out, errOut := backend.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "",
		UserPrompt:   "x",
	})
	if out != nil {
		t.Fatalf("expected error, got output: %+v", out)
	}
	if errOut == nil {
		t.Fatal("expected error")
	}
	if errOut.Class != Unknown {
		t.Errorf("Class: %s", errOut.Class)
	}
	lower := strings.ToLower(errOut.Message)
	if !strings.Contains(lower, "not found") && !strings.Contains(lower, "spawn failed") {
		t.Errorf("Message: %q", errOut.Message)
	}
}

func TestGeminiRunIDPropagatesToOverlayDir(t *testing.T) {
	tmp := t.TempDir()
	settingsRoot := filepath.Join(tmp, "sroot")
	overlay, err := buildGeminiSettingsOverlay(
		map[string]any{
			"mcpServers": map[string]any{
				"x": map[string]any{"url": "u"},
			},
		},
		"",
		settingsRoot,
		"my-run-42",
	)
	if err != nil {
		t.Fatalf("buildGeminiSettingsOverlay: %v", err)
	}
	defer overlay.cleanup()
	parent := filepath.Base(filepath.Dir(overlay.Path()))
	if !strings.Contains(parent, "my-run-42") {
		t.Errorf("overlay parent %q missing run id", parent)
	}
}

func TestGeminiResumePreservesSessionIDWhenCLIOmitsIt(t *testing.T) {
	backend := makeGeminiBackend(t, fakeGeminiPath(t), "", "", "")
	rawStdout := `{"response":"still here","usage":{"input_tokens":1}}`
	inv := newFakeGeminiInvocation(t, fakeGeminiOpts{RawStdout: rawStdout})
	out, errOut := backend.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "",
		UserPrompt:   "continue",
		SessionID:    ptrStr("sticky-sess"),
		ExtraEnv:     inv.env,
	})
	if errOut != nil {
		t.Fatalf("turn errored: %+v", errOut)
	}
	if out.NewSessionID == nil || *out.NewSessionID != "sticky-sess" {
		t.Errorf("NewSessionID: %v", out.NewSessionID)
	}
}

// ---------------------------------------------------------------------------
// Cross-language fixture parity check.
//
// Reads the same JSON files the Python tests use and asserts identical
// text/session_id/usage. This is the F03 cross-language requirement: a fixture
// produced for the Python backend must round-trip through the Go parser
// without divergence.
// ---------------------------------------------------------------------------

func TestGeminiCrossLanguageFixtureParity(t *testing.T) {
	// Path to the Python test fixtures. The fixtures here under
	// testdata/gemini/ are byte-for-byte copies of the Python originals,
	// so we check both directories for byte equality + the parsed
	// equality in one go.
	pyFixtures := filepath.Join(
		"..", "..", "..", "services", "runtime", "tests", "cli_backends",
		"fixtures", "json",
	)

	cases := []struct {
		name       string
		fixture    string
		wantText   string
		wantSID    string
		wantInput  any
		wantOutput any
		wantCache  any
	}{
		{
			"happy",
			"gemini_happy.json",
			"hello world",
			"sess-gem-happy-001",
			38, 7, 12,
		},
		{
			"resume",
			"gemini_resume.json",
			"resumed reply text",
			"prev-sess-42",
			30, 4, 8,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pyRaw, errPy := os.ReadFile(filepath.Join(pyFixtures, tc.fixture))
			goRaw, errGo := os.ReadFile(filepath.Join(geminiFixturesDir(t), tc.fixture))
			if errPy != nil {
				t.Skipf("skip cross-language check, Python fixture not readable: %v", errPy)
			}
			if errGo != nil {
				t.Fatalf("Go fixture missing: %v", errGo)
			}
			// Byte parity (whitespace allowed to differ — re-encode both).
			var pyDecoded, goDecoded any
			if err := json.Unmarshal(pyRaw, &pyDecoded); err != nil {
				t.Fatalf("parse py fixture: %v", err)
			}
			if err := json.Unmarshal(goRaw, &goDecoded); err != nil {
				t.Fatalf("parse go fixture: %v", err)
			}
			pyJSON, _ := json.Marshal(pyDecoded)
			goJSON, _ := json.Marshal(goDecoded)
			if string(pyJSON) != string(goJSON) {
				t.Fatalf("fixture content drift\npy=%s\ngo=%s", pyJSON, goJSON)
			}

			// Parse via the Go backend; assertions are on the decoded
			// shape, not the raw bytes — Python tests use the same
			// invariants.
			parsed := ParseGeminiJSON(string(pyRaw))
			if parsed.Text != tc.wantText {
				t.Errorf("text: got %q want %q", parsed.Text, tc.wantText)
			}
			if parsed.SessionID != tc.wantSID {
				t.Errorf("session: got %q want %q", parsed.SessionID, tc.wantSID)
			}
			if parsed.Usage["input_tokens"] != tc.wantInput {
				t.Errorf("input_tokens: got %v want %v", parsed.Usage["input_tokens"], tc.wantInput)
			}
			if parsed.Usage["output_tokens"] != tc.wantOutput {
				t.Errorf("output_tokens: got %v want %v", parsed.Usage["output_tokens"], tc.wantOutput)
			}
			if parsed.Usage["cache_read_input_tokens"] != tc.wantCache {
				t.Errorf("cache: got %v want %v", parsed.Usage["cache_read_input_tokens"], tc.wantCache)
			}
		})
	}
}

func TestGeminiAuthErrFixtureCrossLanguage(t *testing.T) {
	pyPath := filepath.Join(
		"..", "..", "..", "services", "runtime", "tests", "cli_backends",
		"fixtures", "json", "gemini_auth_err.json",
	)
	raw, err := os.ReadFile(pyPath)
	if err != nil {
		t.Skipf("Python fixture not available: %v", err)
	}
	parsed := ParseGeminiJSON(string(raw))
	if parsed.InlineError == "" {
		t.Fatal("expected InlineError populated")
	}
	if ClassifyGeminiError(parsed.InlineError) != AuthExpired {
		t.Errorf("classify: %s (msg=%q)", ClassifyGeminiError(parsed.InlineError), parsed.InlineError)
	}
}

// ---------------------------------------------------------------------------
// Tiny generic helpers. (Slice utilities exist in Go 1.21+, but a local
// version keeps us insensitive to module-wide go.mod tweaks.)
// ---------------------------------------------------------------------------

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func indexOf(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return -1
}
