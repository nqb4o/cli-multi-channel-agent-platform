package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/openclaw/agent-platform/internal/clibackend"
)

// FakeBackend is the test-only scriptable CliBackend used across the
// daemon + agent_loop tests. Mirrors the Python conftest StubBackend.
type FakeBackend struct {
	BackendID    string
	OutputText   string
	NewSessionID string
	Usage        map[string]any

	// Optional: a queue of scripted results consumed in order. When the
	// queue is empty the default output is returned (or a panic if both
	// are unset).
	Queue []FakeBackendResult

	// Optional: a single result returned on every call (after the queue
	// is drained). If nil, the default output is constructed from
	// OutputText/NewSessionID/Usage.
	Default *FakeBackendResult

	// Optional: when set, Turn panics with this string. Used by the
	// "backend raises → internal error" tests.
	PanicWith string

	mu    sync.Mutex
	calls []clibackend.CliTurnInput
}

// FakeBackendResult is one queued result. Exactly one of Output / Err
// is non-nil.
type FakeBackendResult struct {
	Output *clibackend.CliTurnOutput
	Err    *clibackend.CliTurnError
}

func (f *FakeBackend) ID() string {
	if f.BackendID == "" {
		return "stub"
	}
	return f.BackendID
}

func (f *FakeBackend) DefaultCommand() []string       { return []string{"true"} }
func (f *FakeBackend) SupportsResumeInStream() bool { return false }

func (f *FakeBackend) Turn(_ context.Context, in clibackend.CliTurnInput) (*clibackend.CliTurnOutput, *clibackend.CliTurnError) {
	if f.PanicWith != "" {
		panic(f.PanicWith)
	}
	f.mu.Lock()
	f.calls = append(f.calls, in)
	f.mu.Unlock()

	if len(f.Queue) > 0 {
		r := f.Queue[0]
		f.Queue = f.Queue[1:]
		return r.Output, r.Err
	}
	if f.Default != nil {
		return f.Default.Output, f.Default.Err
	}
	text := f.OutputText
	if text == "" {
		text = "stub reply"
	}
	sid := f.NewSessionID
	if sid == "" {
		sid = "stub-sid-1"
	}
	sidCopy := sid
	usage := f.Usage
	if usage == nil {
		usage = map[string]any{}
	}
	return &clibackend.CliTurnOutput{
		Text:         text,
		NewSessionID: &sidCopy,
		Usage:        usage,
		RawEvents:    []map[string]any{},
	}, nil
}

func (f *FakeBackend) Calls() []clibackend.CliTurnInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]clibackend.CliTurnInput, len(f.calls))
	copy(out, f.calls)
	return out
}

// ---------------------------------------------------------------------------
// StubBackend tests
// ---------------------------------------------------------------------------

func TestStubBackendID(t *testing.T) {
	s := NewStubBackend()
	if s.ID() != "stub" {
		t.Fatalf("ID: %q", s.ID())
	}
}

func TestStubBackendDefaultCommand(t *testing.T) {
	s := NewStubBackend()
	cmd := s.DefaultCommand()
	if len(cmd) != 1 || cmd[0] != "true" {
		t.Fatalf("DefaultCommand: %v", cmd)
	}
}

func TestStubBackendSupportsResumeInStream(t *testing.T) {
	s := NewStubBackend()
	if s.SupportsResumeInStream() {
		t.Fatal("StubBackend should not support resume-in-stream")
	}
}

func TestStubBackendEchoesPrompt(t *testing.T) {
	s := NewStubBackend()
	out, err := s.Turn(context.Background(), clibackend.CliTurnInput{
		UserPrompt: "hello world",
	})
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if out == nil {
		t.Fatal("expected non-nil output")
	}
	if !strings.Contains(out.Text, "hello world") {
		t.Fatalf("Text=%q does not contain 'hello world'", out.Text)
	}
	if !strings.HasPrefix(out.Text, "[stub echo]") {
		t.Fatalf("Text=%q does not start with [stub echo]", out.Text)
	}
}

func TestStubBackendCannedSessionID(t *testing.T) {
	s := NewStubBackend()
	out, _ := s.Turn(context.Background(), clibackend.CliTurnInput{UserPrompt: "x"})
	if out.NewSessionID == nil || *out.NewSessionID != "stub-sid-1" {
		t.Fatalf("NewSessionID: %v", out.NewSessionID)
	}
}

func TestStubBackendCustomPrefix(t *testing.T) {
	s := &StubBackend{Prefix: "[custom] "}
	out, _ := s.Turn(context.Background(), clibackend.CliTurnInput{UserPrompt: "x"})
	if out.Text != "[custom] x" {
		t.Fatalf("Text: %q", out.Text)
	}
}

func TestStubBackendCustomSessionID(t *testing.T) {
	s := &StubBackend{SessionID: "custom-sid"}
	out, _ := s.Turn(context.Background(), clibackend.CliTurnInput{UserPrompt: "x"})
	if out.NewSessionID == nil || *out.NewSessionID != "custom-sid" {
		t.Fatalf("NewSessionID: %v", out.NewSessionID)
	}
}

func TestStubBackendUsageEmpty(t *testing.T) {
	s := NewStubBackend()
	out, _ := s.Turn(context.Background(), clibackend.CliTurnInput{UserPrompt: "x"})
	// Default usage has zero counts (matches Python tree).
	if out.Usage["input_tokens"] != 0 {
		t.Fatalf("input_tokens: %v", out.Usage["input_tokens"])
	}
}

// Compile-time interface assertion for the test fake.
var _ clibackend.CliBackend = (*FakeBackend)(nil)
