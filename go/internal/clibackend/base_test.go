package clibackend

import (
	"context"
	"testing"
)

// mockBackend exists to prove the CliBackend interface is implementable
// from outside this file's tests and to exercise both return modes
// (success / classified error) of Turn().
type mockBackend struct {
	id           string
	cmd          []string
	resumeStream bool

	wantOutput *CliTurnOutput
	wantError  *CliTurnError

	lastInput CliTurnInput
}

func (m *mockBackend) ID() string                { return m.id }
func (m *mockBackend) DefaultCommand() []string  { return m.cmd }
func (m *mockBackend) SupportsResumeInStream() bool {
	return m.resumeStream
}

func (m *mockBackend) Turn(_ context.Context, in CliTurnInput) (*CliTurnOutput, *CliTurnError) {
	m.lastInput = in
	return m.wantOutput, m.wantError
}

// Compile-time check that mockBackend satisfies the interface.
var _ CliBackend = (*mockBackend)(nil)

func TestErrorClassValues(t *testing.T) {
	// FROZEN string forms — these are persisted in telemetry.
	tests := map[ErrorClass]string{
		AuthExpired: "auth_expired",
		RateLimit:   "rate_limit",
		Transient:   "transient",
		Unknown:     "unknown",
	}
	for c, want := range tests {
		if string(c) != want {
			t.Fatalf("ErrorClass %v: got %q want %q", c, string(c), want)
		}
	}
}

func TestMockBackendImplementsInterface(t *testing.T) {
	// Compile-time assertion is the main guarantee; this is a runtime
	// smoke that all four methods are reachable.
	sid := "sess-1"
	b := &mockBackend{
		id:           "mock",
		cmd:          []string{"mock-cli"},
		resumeStream: true,
		wantOutput: &CliTurnOutput{
			Text:         "ok",
			NewSessionID: &sid,
			Usage:        map[string]any{"input_tokens": 1.0},
			RawEvents:    []map[string]any{{"type": "tool"}},
		},
	}
	var iface CliBackend = b
	if iface.ID() != "mock" {
		t.Fatalf("ID: %q", iface.ID())
	}
	if got := iface.DefaultCommand(); len(got) != 1 || got[0] != "mock-cli" {
		t.Fatalf("DefaultCommand: %v", got)
	}
	if !iface.SupportsResumeInStream() {
		t.Fatal("expected resume-in-stream")
	}

	out, err := iface.Turn(context.Background(), CliTurnInput{
		SystemPrompt: "sys",
		UserPrompt:   "hi",
	})
	if err != nil {
		t.Fatalf("expected output, got error: %+v", err)
	}
	if out.Text != "ok" {
		t.Fatalf("Text: %q", out.Text)
	}
	if out.NewSessionID == nil || *out.NewSessionID != "sess-1" {
		t.Fatalf("NewSessionID: %v", out.NewSessionID)
	}
}

func TestMockBackendCanReturnError(t *testing.T) {
	b := &mockBackend{
		id: "mock",
		wantError: &CliTurnError{
			Class:      AuthExpired,
			Message:    "session expired",
			ExitCode:   1,
			StderrTail: "Please re-login.",
		},
	}
	out, err := b.Turn(context.Background(), CliTurnInput{})
	if out != nil {
		t.Fatalf("expected nil output, got %+v", out)
	}
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Class != AuthExpired {
		t.Fatalf("class: %v", err.Class)
	}
}

func TestCliTurnInputOptionalFields(t *testing.T) {
	// All optional fields zero — backend must be able to read them
	// without panicking. (Smoke-tests the Go translation of Python's
	// dataclass(frozen=True) defaults.)
	in := CliTurnInput{
		SystemPrompt: "s",
		UserPrompt:   "u",
	}
	if in.SessionID != nil {
		t.Fatal("SessionID should be nil by default")
	}
	if in.Model != nil {
		t.Fatal("Model should be nil by default")
	}
	if in.RunID != nil {
		t.Fatal("RunID should be nil by default")
	}
	if in.TimeoutSec != nil {
		t.Fatal("TimeoutSec should be nil by default")
	}
	if in.Images != nil {
		t.Fatal("Images should be nil by default")
	}
	if in.ExtraEnv != nil {
		t.Fatal("ExtraEnv should be nil by default")
	}
	if in.MCPConfig != nil {
		t.Fatal("MCPConfig should be nil by default")
	}
}
