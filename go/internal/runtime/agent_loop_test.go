package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/openclaw/agent-platform/internal/clibackend"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func runRequest(text string) RunRequest {
	return RunRequest{
		UserID:    uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		AgentID:   uuid.MustParse("00000000-0000-0000-0000-000000000002"),
		ChannelID: uuid.MustParse("00000000-0000-0000-0000-000000000003"),
		ThreadID:  "thread-1",
		Message:   RunMessage{Text: text},
		RunID:     "run-test",
	}
}

func defaultConfig(provider string, model string) *AgentConfig {
	return &AgentConfig{
		Identity:  IdentityConfig{Name: "Stub Buddy"},
		Providers: []ProviderConfig{{ID: provider, Model: model}},
	}
}

func buildLoop(t *testing.T, backend clibackend.CliBackend, opts ...AgentLoopOption) (*AgentLoop, *DBSessionDal) {
	t.Helper()
	registry := clibackend.NewBackendRegistry()
	if backend != nil {
		if err := registry.Register(backend); err != nil {
			t.Fatalf("register backend: %v", err)
		}
	}
	dal := NewInMemorySessionDal()
	loop := NewAgentLoop(defaultConfig("stub", "stub-large"), registry, dal, opts...)
	return loop, dal
}

// ---------------------------------------------------------------------------
// happy path
// ---------------------------------------------------------------------------

func TestAgentLoopHappyPath(t *testing.T) {
	backend := &FakeBackend{OutputText: "stub reply", NewSessionID: "stub-sid-1"}
	loop, _ := buildLoop(t, backend)

	result, errResult := loop.Run(context.Background(), runRequest("hi"))
	if errResult != nil {
		t.Fatalf("unexpected error: %+v", errResult)
	}
	if result.Text != "stub reply" {
		t.Fatalf("Text=%q", result.Text)
	}
	if result.Telemetry["provider"] != "stub" {
		t.Fatalf("provider=%v", result.Telemetry["provider"])
	}
	if result.Telemetry["model"] != "stub-large" {
		t.Fatalf("model=%v", result.Telemetry["model"])
	}
	if result.Telemetry["cli_session_id"] != "stub-sid-1" {
		t.Fatalf("cli_session_id=%v", result.Telemetry["cli_session_id"])
	}
	if result.Telemetry["adk_harness"] != false {
		t.Fatalf("adk_harness=%v", result.Telemetry["adk_harness"])
	}
	if result.Telemetry["bootstrap_injected"] != true {
		t.Fatalf("bootstrap_injected=%v on first turn", result.Telemetry["bootstrap_injected"])
	}
}

func TestAgentLoopSessionPersistedAfterSuccess(t *testing.T) {
	backend := &FakeBackend{NewSessionID: "stub-sid-1"}
	loop, dal := buildLoop(t, backend)
	if _, err := loop.Run(context.Background(), runRequest("hi")); err != nil {
		t.Fatalf("err: %+v", err)
	}
	sid, ok, gerr := dal.LookupSessionID(context.Background(),
		uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		uuid.MustParse("00000000-0000-0000-0000-000000000003"),
		"thread-1", "stub")
	if gerr != nil {
		t.Fatalf("LookupSessionID: %v", gerr)
	}
	if !ok || sid != "stub-sid-1" {
		t.Fatalf("session not persisted: ok=%v sid=%q", ok, sid)
	}
}

// ---------------------------------------------------------------------------
// bootstrap injection
// ---------------------------------------------------------------------------

func TestAgentLoopBootstrapInjectedOnFirstTurnOnly(t *testing.T) {
	backend := &FakeBackend{}
	loop, _ := buildLoop(t, backend, WithWorkspaceDir(fixtureWorkspace))

	first, err := loop.Run(context.Background(), runRequest("hi"))
	if err != nil {
		t.Fatalf("err: %+v", err)
	}
	if first.Telemetry["bootstrap_injected"] != true {
		t.Fatal("first turn should report bootstrap_injected=true")
	}
	calls := backend.Calls()
	if len(calls) < 1 {
		t.Fatal("backend should have been called once")
	}
	firstSys := calls[0].SystemPrompt
	if !strings.Contains(firstSys, "# AGENTS.md") {
		t.Fatalf("first turn system prompt missing AGENTS header:\n%s", firstSys)
	}
	if !strings.Contains(firstSys, "# SOUL.md") {
		t.Fatalf("first turn system prompt missing SOUL header:\n%s", firstSys)
	}

	second, err := loop.Run(context.Background(), runRequest("again"))
	if err != nil {
		t.Fatalf("err: %+v", err)
	}
	if second.Telemetry["bootstrap_injected"] != false {
		t.Fatal("second turn should report bootstrap_injected=false")
	}
	calls = backend.Calls()
	if len(calls) < 2 {
		t.Fatalf("expected ≥2 backend calls, got %d", len(calls))
	}
	secondSys := calls[1].SystemPrompt
	if strings.Contains(secondSys, "# AGENTS.md") {
		t.Fatal("second turn should not include bootstrap headers")
	}
	if !strings.Contains(secondSys, "You are Stub Buddy.") {
		t.Fatal("second turn should still carry identity preamble")
	}
}

func TestAgentLoopFirstTurnSessionIDIsNil(t *testing.T) {
	backend := &FakeBackend{NewSessionID: "sid-X"}
	loop, _ := buildLoop(t, backend)
	if _, err := loop.Run(context.Background(), runRequest("hi")); err != nil {
		t.Fatalf("err: %+v", err)
	}
	calls := backend.Calls()
	if calls[0].SessionID != nil {
		t.Fatalf("first turn SessionID should be nil, got %v", *calls[0].SessionID)
	}
}

func TestAgentLoopSecondTurnResumesSessionID(t *testing.T) {
	sidA := "sid-A"
	sidB := "sid-B"
	backend := &FakeBackend{
		Queue: []FakeBackendResult{
			{Output: &clibackend.CliTurnOutput{Text: "t1", NewSessionID: &sidA, Usage: map[string]any{}}},
			{Output: &clibackend.CliTurnOutput{Text: "t2", NewSessionID: &sidB, Usage: map[string]any{}}},
		},
	}
	loop, _ := buildLoop(t, backend)
	if _, err := loop.Run(context.Background(), runRequest("turn-1")); err != nil {
		t.Fatalf("err: %+v", err)
	}
	if _, err := loop.Run(context.Background(), runRequest("turn-2")); err != nil {
		t.Fatalf("err: %+v", err)
	}
	calls := backend.Calls()
	if calls[0].SessionID != nil {
		t.Fatalf("turn-1 SessionID should be nil, got %v", *calls[0].SessionID)
	}
	if calls[1].SessionID == nil || *calls[1].SessionID != "sid-A" {
		t.Fatalf("turn-2 SessionID should be sid-A, got %v", calls[1].SessionID)
	}
}

// ---------------------------------------------------------------------------
// error paths
// ---------------------------------------------------------------------------

func TestAgentLoopAuthExpiredReturnsRunErrorResult(t *testing.T) {
	backend := &FakeBackend{
		Queue: []FakeBackendResult{{Err: &clibackend.CliTurnError{
			Class:      clibackend.AuthExpired,
			Message:    "re-login please",
			ExitCode:   2,
			StderrTail: "oauth token expired",
		}}},
	}
	loop, _ := buildLoop(t, backend)
	_, errResult := loop.Run(context.Background(), runRequest("hi"))
	if errResult == nil {
		t.Fatal("expected RunErrorResult")
	}
	if errResult.ErrorClass != "auth_expired" {
		t.Fatalf("class: %q", errResult.ErrorClass)
	}
	if errResult.Message != "re-login please" {
		t.Fatalf("message: %q", errResult.Message)
	}
	if !strings.Contains(errResult.UserFacing, "Your provider login has expired") {
		t.Fatalf("user_facing: %q", errResult.UserFacing)
	}
	if errResult.Details["provider"] != "stub" {
		t.Fatalf("details.provider: %v", errResult.Details["provider"])
	}
	if errResult.Details["exit_code"] != 2 {
		t.Fatalf("details.exit_code: %v", errResult.Details["exit_code"])
	}
	if errResult.Details["stderr_tail"] != "oauth token expired" {
		t.Fatalf("details.stderr_tail: %v", errResult.Details["stderr_tail"])
	}
}

func TestAgentLoopRateLimitClassified(t *testing.T) {
	backend := &FakeBackend{
		Queue: []FakeBackendResult{{Err: &clibackend.CliTurnError{
			Class:    clibackend.RateLimit,
			Message:  "429",
			ExitCode: 1,
		}}},
	}
	loop, _ := buildLoop(t, backend)
	_, errResult := loop.Run(context.Background(), runRequest("hi"))
	if errResult == nil {
		t.Fatal("expected RunErrorResult")
	}
	if errResult.ErrorClass != "rate_limit" {
		t.Fatalf("class: %q", errResult.ErrorClass)
	}
	if !strings.Contains(strings.ToLower(errResult.UserFacing), "rate limit") {
		t.Fatalf("user_facing should mention rate limit, got %q", errResult.UserFacing)
	}
}

func TestAgentLoopBackendPanicYieldsInternalError(t *testing.T) {
	backend := &FakeBackend{PanicWith: "boom from backend"}
	loop, _ := buildLoop(t, backend)
	_, errResult := loop.Run(context.Background(), runRequest("hi"))
	if errResult == nil {
		t.Fatal("expected RunErrorResult")
	}
	if errResult.ErrorClass != "internal" {
		t.Fatalf("class: %q", errResult.ErrorClass)
	}
	if !strings.Contains(errResult.Message, "boom from backend") {
		t.Fatalf("message: %q", errResult.Message)
	}
}

func TestAgentLoopUnknownProvider(t *testing.T) {
	registry := clibackend.NewBackendRegistry() // empty
	dal := NewInMemorySessionDal()
	loop := NewAgentLoop(defaultConfig("not-registered", ""), registry, dal)
	_, errResult := loop.Run(context.Background(), runRequest("hi"))
	if errResult == nil {
		t.Fatal("expected RunErrorResult")
	}
	if errResult.ErrorClass != "unknown_provider" {
		t.Fatalf("class: %q", errResult.ErrorClass)
	}
	if errResult.Details["provider"] != "not-registered" {
		t.Fatalf("details.provider: %v", errResult.Details["provider"])
	}
}

func TestAgentLoopNoProvidersConfigured(t *testing.T) {
	cfg := &AgentConfig{Identity: IdentityConfig{Name: "X"}}
	registry := clibackend.NewBackendRegistry()
	loop := NewAgentLoop(cfg, registry, NewInMemorySessionDal())
	_, errResult := loop.Run(context.Background(), runRequest("hi"))
	if errResult == nil {
		t.Fatal("expected RunErrorResult")
	}
	if errResult.ErrorClass != "config_error" {
		t.Fatalf("class: %q", errResult.ErrorClass)
	}
}

// ---------------------------------------------------------------------------
// telemetry shape
// ---------------------------------------------------------------------------

func TestAgentLoopTelemetryCarriesRunIDAndUsage(t *testing.T) {
	backend := &FakeBackend{Usage: map[string]any{"input_tokens": 7, "output_tokens": 3}}
	loop, _ := buildLoop(t, backend)
	result, err := loop.Run(context.Background(), runRequest("hi"))
	if err != nil {
		t.Fatalf("err: %+v", err)
	}
	if result.Telemetry["run_id"] != "run-test" {
		t.Fatalf("run_id: %v", result.Telemetry["run_id"])
	}
	usage := result.Telemetry["usage"].(map[string]any)
	if usage["input_tokens"] != 7 {
		t.Fatalf("usage.input_tokens: %v", usage["input_tokens"])
	}
	if _, ok := result.Telemetry["latency_ms"]; !ok {
		t.Fatal("latency_ms missing from telemetry")
	}
}

func TestAgentLoopTelemetryNoFallbackKey(t *testing.T) {
	backend := &FakeBackend{}
	loop, _ := buildLoop(t, backend)
	result, err := loop.Run(context.Background(), runRequest("hi"))
	if err != nil {
		t.Fatalf("err: %+v", err)
	}
	if _, present := result.Telemetry["fallback"]; present {
		t.Fatal("single-provider telemetry must not carry a 'fallback' key")
	}
}

func TestAgentLoopAdkHarnessAlwaysFalse(t *testing.T) {
	// Go runtime drops the ADK harness — telemetry should always
	// report adk_harness=false regardless of any future env.
	backend := &FakeBackend{}
	loop, _ := buildLoop(t, backend)
	result, _ := loop.Run(context.Background(), runRequest("hi"))
	if result.Telemetry["adk_harness"] != false {
		t.Fatalf("adk_harness=%v", result.Telemetry["adk_harness"])
	}
}

// ---------------------------------------------------------------------------
// nil-output contract violation
// ---------------------------------------------------------------------------

func TestAgentLoopNilOutputAndNilErrorIsInternal(t *testing.T) {
	backend := &FakeBackend{
		Queue: []FakeBackendResult{{Output: nil, Err: nil}},
	}
	loop, _ := buildLoop(t, backend)
	_, errResult := loop.Run(context.Background(), runRequest("hi"))
	if errResult == nil {
		t.Fatal("expected RunErrorResult")
	}
	if errResult.ErrorClass != "internal" {
		t.Fatalf("class: %q", errResult.ErrorClass)
	}
}
