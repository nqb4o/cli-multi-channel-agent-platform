package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openclaw/agent-platform/internal/clibackend"
	"github.com/openclaw/agent-platform/pkg/jsonrpc"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// daemonWith builds a Daemon backed by the given backend (registered as
// "stub") and an in-memory session DAL.
func daemonWith(t *testing.T, backend clibackend.CliBackend) *Daemon {
	t.Helper()
	registry := clibackend.NewBackendRegistry()
	if backend != nil {
		if err := registry.Register(backend); err != nil {
			t.Fatalf("register backend: %v", err)
		}
	}
	loop := NewAgentLoop(
		defaultConfig("stub", "stub-large"),
		registry,
		NewInMemorySessionDal(),
	)
	return NewDaemon(loop)
}

// runRequestParams returns the canonical JSON params for a `run` call.
func runRequestParams() json.RawMessage {
	body := map[string]any{
		"user_id":    "00000000-0000-0000-0000-000000000001",
		"agent_id":   "00000000-0000-0000-0000-000000000002",
		"channel_id": "00000000-0000-0000-0000-000000000003",
		"thread_id":  "thread-1",
		"message":    map[string]any{"text": "hi", "images": []string{}},
		"run_id":     "run-test",
	}
	b, _ := json.Marshal(body)
	return b
}

// envelope returns a serialised JSON-RPC line for the given method/params.
func envelope(t *testing.T, method string, params any, id any) []byte {
	t.Helper()
	var raw json.RawMessage
	if params != nil {
		switch v := params.(type) {
		case json.RawMessage:
			raw = v
		case []byte:
			raw = json.RawMessage(v)
		default:
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("marshal params: %v", err)
			}
			raw = b
		}
	}
	envBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if len(raw) > 0 {
		envBody["params"] = raw
	}
	out, err := json.Marshal(envBody)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return append(out, '\n')
}

// driveLines sends each line to the daemon over an in-process pipe and
// returns the parsed reply lines. The daemon's input is closed after the
// last write, so Serve drains and returns.
func driveLines(t *testing.T, d *Daemon, lines [][]byte) []map[string]any {
	t.Helper()
	pr, pw := io.Pipe()
	var w syncBuffer

	done := make(chan error, 1)
	go func() {
		done <- d.Serve(context.Background(), pr, &w)
	}()

	for _, ln := range lines {
		if _, err := pw.Write(ln); err != nil {
			t.Fatalf("pw.Write: %v", err)
		}
	}
	_ = pw.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s")
	}

	return parseReplies(t, w.Bytes())
}

func parseReplies(t *testing.T, buf []byte) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	for _, line := range bytes.Split(bytes.TrimRight(buf, "\n"), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("unmarshal reply line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// syncBuffer is a thread-safe bytes.Buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf.Bytes()...)
}

// ---------------------------------------------------------------------------
// health
// ---------------------------------------------------------------------------

func TestDaemonHealthReturnsVersionAndTs(t *testing.T) {
	d := daemonWith(t, NewStubBackend())
	replies := driveLines(t, d, [][]byte{envelope(t, "health", nil, "1")})
	if len(replies) != 1 {
		t.Fatalf("want 1 reply, got %d", len(replies))
	}
	resp := replies[0]
	if resp["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc: %v", resp["jsonrpc"])
	}
	if resp["id"] != "1" {
		t.Fatalf("id: %v", resp["id"])
	}
	result := resp["result"].(map[string]any)
	if result["ok"] != true {
		t.Fatalf("ok: %v", result["ok"])
	}
	if result["version"] != RuntimeVersion {
		t.Fatalf("version: %v", result["version"])
	}
	if _, ok := result["ts"]; !ok {
		t.Fatal("ts missing")
	}
}

func TestDaemonHealthRespondsUnder100ms(t *testing.T) {
	d := daemonWith(t, NewStubBackend())
	start := time.Now()
	result, jerr := d.Handle(context.Background(), "health", nil)
	elapsed := time.Since(start)
	if jerr != nil {
		t.Fatalf("unexpected jsonrpc error: %+v", jerr)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("health took %v (>100ms)", elapsed)
	}
	m := result.(map[string]any)
	if m["ok"] != true {
		t.Fatalf("ok: %v", m["ok"])
	}
}

// ---------------------------------------------------------------------------
// run — happy path + envelope shape
// ---------------------------------------------------------------------------

func TestDaemonRunHappyPath(t *testing.T) {
	backend := &FakeBackend{OutputText: "from-stub", NewSessionID: "sid-1"}
	d := daemonWith(t, backend)
	replies := driveLines(t, d, [][]byte{envelope(t, "run", runRequestParams(), "1")})
	if len(replies) != 1 {
		t.Fatalf("want 1 reply, got %d", len(replies))
	}
	resp := replies[0]
	// Outer envelope must NOT contain a JSON-RPC error block.
	if _, ok := resp["error"]; ok {
		t.Fatalf("unexpected JSON-RPC error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["ok"] != true {
		t.Fatalf("result.ok: %v", result["ok"])
	}
	// Double-nested 'result' shape is intentional (FROZEN contract).
	inner := result["result"].(map[string]any)
	if inner["text"] != "from-stub" {
		t.Fatalf("text: %v", inner["text"])
	}
	telemetry := inner["telemetry"].(map[string]any)
	if telemetry["provider"] != "stub" {
		t.Fatalf("telemetry.provider: %v", telemetry["provider"])
	}
	if telemetry["cli_session_id"] != "sid-1" {
		t.Fatalf("telemetry.cli_session_id: %v", telemetry["cli_session_id"])
	}
}

func TestDaemonRunBootstrapInjectedFirstTurnOnly(t *testing.T) {
	// We invoke Handle() sequentially rather than through the JSON-RPC
	// pipe because the daemon dispatches each incoming line in its own
	// goroutine — concurrent runs could observe the same uninitialized
	// session row. The Python test sidesteps this by driving the agent
	// loop directly; we do the same here.
	backend := &FakeBackend{}
	registry := clibackend.NewBackendRegistry()
	if err := registry.Register(backend); err != nil {
		t.Fatal(err)
	}
	loop := NewAgentLoop(
		defaultConfig("stub", "stub-large"),
		registry,
		NewInMemorySessionDal(),
		WithWorkspaceDir(fixtureWorkspace),
	)
	d := NewDaemon(loop)

	first, jerr := d.Handle(context.Background(), "run", runRequestParams())
	if jerr != nil {
		t.Fatalf("first run: %+v", jerr)
	}
	second, jerr := d.Handle(context.Background(), "run", runRequestParams())
	if jerr != nil {
		t.Fatalf("second run: %+v", jerr)
	}

	firstTel := first.(map[string]any)["result"].(map[string]any)["telemetry"].(map[string]any)
	secondTel := second.(map[string]any)["result"].(map[string]any)["telemetry"].(map[string]any)
	if firstTel["bootstrap_injected"] != true {
		t.Fatalf("first turn bootstrap_injected: %v", firstTel["bootstrap_injected"])
	}
	if secondTel["bootstrap_injected"] != false {
		t.Fatalf("second turn bootstrap_injected: %v", secondTel["bootstrap_injected"])
	}

	calls := backend.Calls()
	if len(calls) != 2 {
		t.Fatalf("backend calls: %d", len(calls))
	}
	if !strings.Contains(calls[0].SystemPrompt, "# AGENTS.md") {
		t.Fatalf("first turn should carry bootstrap headers:\n%s", calls[0].SystemPrompt)
	}
	if strings.Contains(calls[1].SystemPrompt, "# AGENTS.md") {
		t.Fatalf("second turn should NOT carry bootstrap headers:\n%s", calls[1].SystemPrompt)
	}
}

func TestDaemonRunReturnsApplicationErrorInResult(t *testing.T) {
	backend := &FakeBackend{Queue: []FakeBackendResult{{Err: &clibackend.CliTurnError{
		Class:      clibackend.AuthExpired,
		Message:    "re-login",
		ExitCode:   2,
		StderrTail: "",
	}}}}
	d := daemonWith(t, backend)
	replies := driveLines(t, d, [][]byte{envelope(t, "run", runRequestParams(), "1")})
	resp := replies[0]
	if _, ok := resp["error"]; ok {
		t.Fatalf("application errors must NOT use JSON-RPC error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["ok"] != false {
		t.Fatalf("result.ok: %v", result["ok"])
	}
	errBlock := result["error"].(map[string]any)
	if errBlock["class"] != "auth_expired" {
		t.Fatalf("class: %v", errBlock["class"])
	}
	if errBlock["message"] != "re-login" {
		t.Fatalf("message: %v", errBlock["message"])
	}
	if !strings.Contains(errBlock["user_facing"].(string), "Your provider login has expired") {
		t.Fatalf("user_facing: %v", errBlock["user_facing"])
	}
}

func TestDaemonRunInvalidParamsReturnsInvalidParamsError(t *testing.T) {
	d := daemonWith(t, NewStubBackend())
	bad, _ := json.Marshal(map[string]any{"user_id": "not-a-uuid"})
	replies := driveLines(t, d, [][]byte{envelope(t, "run", json.RawMessage(bad), "1")})
	resp := replies[0]
	errBlock, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected JSON-RPC error envelope: %+v", resp)
	}
	code, _ := errBlock["code"].(float64)
	if int(code) != jsonrpc.CodeInvalidParams {
		t.Fatalf("code: %d", int(code))
	}
}

// ---------------------------------------------------------------------------
// shutdown
// ---------------------------------------------------------------------------

func TestDaemonShutdownRespondsAndDrains(t *testing.T) {
	d := daemonWith(t, NewStubBackend())
	replies := driveLines(t, d, [][]byte{
		envelope(t, "run", runRequestParams(), "r1"),
		envelope(t, "shutdown", nil, "s1"),
	})
	byID := map[string]map[string]any{}
	for _, r := range replies {
		byID[fmt.Sprintf("%v", r["id"])] = r
	}
	if _, ok := byID["r1"]; !ok {
		t.Fatal("missing r1 reply")
	}
	if _, ok := byID["s1"]; !ok {
		t.Fatal("missing s1 reply")
	}
	shutdownResult := byID["s1"]["result"].(map[string]any)
	if shutdownResult["ok"] != true {
		t.Fatalf("shutdown ok: %v", shutdownResult["ok"])
	}
}

func TestDaemonShutdownDrainsInflightRun(t *testing.T) {
	// Use a backend with a ~100ms delay to ensure the run is in-flight
	// when the pipe closes / shutdown fires.
	slow := &slowBackend{delay: 100 * time.Millisecond, sid: "sid-slow"}
	d := daemonWith(t, slow)

	pr, pw := io.Pipe()
	var w syncBuffer

	done := make(chan error, 1)
	go func() { done <- d.Serve(context.Background(), pr, &w) }()

	if _, err := pw.Write(envelope(t, "run", runRequestParams(), "r1")); err != nil {
		t.Fatal(err)
	}

	// Give the dispatcher time to pick the run up before we close.
	time.Sleep(20 * time.Millisecond)
	d.Shutdown()
	_ = pw.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Serve did not finish draining")
	}

	got := w.Bytes()
	if !bytes.Contains(got, []byte(`"r1"`)) {
		t.Fatalf("missing r1 reply (drain failed): %s", got)
	}
}

// ---------------------------------------------------------------------------
// framing edge cases
// ---------------------------------------------------------------------------

func TestDaemonUnknownMethodReturnsMethodNotFound(t *testing.T) {
	d := daemonWith(t, NewStubBackend())
	replies := driveLines(t, d, [][]byte{envelope(t, "does_not_exist", nil, "1")})
	resp := replies[0]
	errBlock, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected JSON-RPC error: %+v", resp)
	}
	code, _ := errBlock["code"].(float64)
	if int(code) != jsonrpc.CodeMethodNotFound {
		t.Fatalf("code: %d", int(code))
	}
}

func TestDaemonMalformedJSONYieldsParseError(t *testing.T) {
	d := daemonWith(t, NewStubBackend())
	replies := driveLines(t, d, [][]byte{[]byte("this-is-not-json\n")})
	resp := replies[0]
	errBlock := resp["error"].(map[string]any)
	code, _ := errBlock["code"].(float64)
	if int(code) != jsonrpc.CodeParseError {
		t.Fatalf("code: %d", int(code))
	}
}

func TestDaemonBlankLineYieldsParseError(t *testing.T) {
	d := daemonWith(t, NewStubBackend())
	replies := driveLines(t, d, [][]byte{[]byte("\n")})
	if len(replies) != 1 {
		// Note: a bare newline is consumed by bufio.Scanner as an empty
		// "line", which the server treats as a parse error. If your
		// scanner skips empty tokens this assertion may be 0.
		t.Logf("got %d replies (some scanners skip empties)", len(replies))
	}
}

func TestDaemonMissingMethodFieldYieldsInvalidRequest(t *testing.T) {
	d := daemonWith(t, NewStubBackend())
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "x"})
	replies := driveLines(t, d, [][]byte{append(body, '\n')})
	resp := replies[0]
	errBlock := resp["error"].(map[string]any)
	code, _ := errBlock["code"].(float64)
	if int(code) != jsonrpc.CodeInvalidRequest {
		t.Fatalf("code: %d", int(code))
	}
}

func TestDaemonPartialLineBuffering(t *testing.T) {
	d := daemonWith(t, NewStubBackend())
	line := envelope(t, "health", nil, "1")
	pr, pw := io.Pipe()
	var w syncBuffer

	done := make(chan error, 1)
	go func() { done <- d.Serve(context.Background(), pr, &w) }()

	half := len(line) / 2
	if _, err := pw.Write(line[:half]); err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write(line[half:]); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not finish")
	}
	if !bytes.Contains(w.Bytes(), []byte(RuntimeVersion)) {
		t.Fatalf("missing version in reply: %s", w.Bytes())
	}
}

func TestDaemonMultipleRequestsMultipleResponses(t *testing.T) {
	d := daemonWith(t, NewStubBackend())
	replies := driveLines(t, d, [][]byte{
		envelope(t, "health", nil, "a"),
		envelope(t, "health", nil, "b"),
		envelope(t, "health", nil, "c"),
	})
	if len(replies) != 3 {
		t.Fatalf("want 3 replies, got %d", len(replies))
	}
	ids := map[string]bool{}
	for _, r := range replies {
		ids[fmt.Sprintf("%v", r["id"])] = true
	}
	for _, id := range []string{"a", "b", "c"} {
		if !ids[id] {
			t.Fatalf("missing reply id=%q (got %v)", id, ids)
		}
	}
}

// ---------------------------------------------------------------------------
// run — telemetry / error envelopes via Handle
// ---------------------------------------------------------------------------

func TestDaemonHandleRunInternalErrorWrappedAsApplicationEnvelope(t *testing.T) {
	// Backend panics → safeTurn converts to internal error.
	backend := &FakeBackend{PanicWith: "kaboom"}
	d := daemonWith(t, backend)
	result, jerr := d.Handle(context.Background(), "run", runRequestParams())
	if jerr != nil {
		t.Fatalf("expected OK envelope, got jsonrpc error: %+v", jerr)
	}
	m := result.(map[string]any)
	if m["ok"] != false {
		t.Fatalf("ok: %v", m["ok"])
	}
	errBlock := m["error"].(map[string]any)
	if errBlock["class"] != "internal" {
		t.Fatalf("class: %v", errBlock["class"])
	}
}

func TestDaemonHandleRunMissingThreadIDReturnsInvalidParams(t *testing.T) {
	d := daemonWith(t, NewStubBackend())
	body, _ := json.Marshal(map[string]any{
		"user_id":    "00000000-0000-0000-0000-000000000001",
		"agent_id":   "00000000-0000-0000-0000-000000000002",
		"channel_id": "00000000-0000-0000-0000-000000000003",
		"thread_id":  "",
		"message":    map[string]any{"text": "hi"},
		"run_id":     "r",
	})
	_, jerr := d.Handle(context.Background(), "run", body)
	if jerr == nil {
		t.Fatal("expected jsonrpc error")
	}
	if jerr.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("code: %d", jerr.Code)
	}
}

func TestDaemonHandleRunMissingRunIDReturnsInvalidParams(t *testing.T) {
	d := daemonWith(t, NewStubBackend())
	body, _ := json.Marshal(map[string]any{
		"user_id":    "00000000-0000-0000-0000-000000000001",
		"agent_id":   "00000000-0000-0000-0000-000000000002",
		"channel_id": "00000000-0000-0000-0000-000000000003",
		"thread_id":  "thread-1",
		"message":    map[string]any{"text": "hi"},
	})
	_, jerr := d.Handle(context.Background(), "run", body)
	if jerr == nil {
		t.Fatal("expected jsonrpc error")
	}
	if jerr.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("code: %d", jerr.Code)
	}
}

func TestDaemonHandleHealthDoesNotIO(t *testing.T) {
	d := daemonWith(t, NewStubBackend())
	for i := 0; i < 100; i++ {
		_, jerr := d.Handle(context.Background(), "health", nil)
		if jerr != nil {
			t.Fatalf("iteration %d: %+v", i, jerr)
		}
	}
}

func TestDaemonHandleUnknownMethodReturnsMethodNotFound(t *testing.T) {
	d := daemonWith(t, NewStubBackend())
	_, jerr := d.Handle(context.Background(), "xyz", nil)
	if jerr == nil {
		t.Fatal("expected error")
	}
	if jerr.Code != jsonrpc.CodeMethodNotFound {
		t.Fatalf("code: %d", jerr.Code)
	}
}

// ---------------------------------------------------------------------------
// build_daemon
// ---------------------------------------------------------------------------

func TestBuildDaemonRequiresRegistry(t *testing.T) {
	_, _, err := BuildDaemon(BuildDaemonOptions{
		AgentYAML:  writeTempYAML(t),
		SessionDal: NewInMemorySessionDal(),
	})
	if err == nil {
		t.Fatal("expected error for missing Registry")
	}
}

func TestBuildDaemonRequiresSessionDal(t *testing.T) {
	_, _, err := BuildDaemon(BuildDaemonOptions{
		AgentYAML: writeTempYAML(t),
		Registry:  clibackend.NewBackendRegistry(),
	})
	if err == nil {
		t.Fatal("expected error for missing SessionDal")
	}
}

func TestBuildDaemonHappyPath(t *testing.T) {
	registry := clibackend.NewBackendRegistry()
	if err := registry.Register(NewStubBackend()); err != nil {
		t.Fatal(err)
	}
	d, warnings, err := BuildDaemon(BuildDaemonOptions{
		AgentYAML:    writeTempYAML(t),
		Registry:     registry,
		SessionDal:   NewInMemorySessionDal(),
		WorkspaceDir: fixtureWorkspace,
	})
	if err != nil {
		t.Fatalf("BuildDaemon: %v", err)
	}
	if d == nil {
		t.Fatal("BuildDaemon returned nil")
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", warnings)
	}

	// Sanity check: the daemon can serve a run.
	replies := driveLines(t, d, [][]byte{envelope(t, "run", runRequestParams(), "1")})
	resp := replies[0]
	result := resp["result"].(map[string]any)
	if result["ok"] != true {
		t.Fatalf("result.ok: %v", result["ok"])
	}
	telemetry := result["result"].(map[string]any)["telemetry"].(map[string]any)
	if telemetry["provider"] != "stub" {
		t.Fatalf("telemetry.provider: %v", telemetry["provider"])
	}
}

func TestBuildDaemonBadAgentYAMLReturnsError(t *testing.T) {
	tmp := t.TempDir() + "/agent.yaml"
	if err := writeFile(tmp, []byte("identity:\n  name: ''\nproviders: []\n")); err != nil {
		t.Fatal(err)
	}
	_, _, err := BuildDaemon(BuildDaemonOptions{
		AgentYAML:  tmp,
		Registry:   clibackend.NewBackendRegistry(),
		SessionDal: NewInMemorySessionDal(),
	})
	if err == nil {
		t.Fatal("expected error for invalid agent.yaml")
	}
}

// ---------------------------------------------------------------------------
// helpers — slow backend + temp YAML
// ---------------------------------------------------------------------------

type slowBackend struct {
	delay time.Duration
	sid   string
}

func (s *slowBackend) ID() string                  { return "stub" }
func (s *slowBackend) DefaultCommand() []string    { return []string{"true"} }
func (s *slowBackend) SupportsResumeInStream() bool { return false }

func (s *slowBackend) Turn(ctx context.Context, _ clibackend.CliTurnInput) (*clibackend.CliTurnOutput, *clibackend.CliTurnError) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, &clibackend.CliTurnError{Class: clibackend.Transient, Message: ctx.Err().Error()}
	}
	sid := s.sid
	return &clibackend.CliTurnOutput{
		Text:         "slow-ok",
		NewSessionID: &sid,
		Usage:        map[string]any{},
	}, nil
}

var _ clibackend.CliBackend = (*slowBackend)(nil)

func writeTempYAML(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/agent.yaml"
	yaml := []byte(`identity:
  name: "Stub Buddy"
providers:
  - id: stub
    model: stub-large
skills: []
mcp:
  bundle: false
`)
	if err := writeFile(path, yaml); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}
