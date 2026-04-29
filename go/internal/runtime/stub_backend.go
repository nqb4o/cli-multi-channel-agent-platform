package runtime

import (
	"context"

	"github.com/openclaw/agent-platform/internal/clibackend"
)

// StubBackend is a no-op CliBackend that echoes the user prompt with a
// canned prefix. It is wired by [Daemon] when --register-stub is passed
// on the command line — only used for manual smoke tests.
//
// Mirrors the Python tree's _StubBackend in services/runtime/src/runtime/
// daemon.py: same id "stub", same fixed session id "stub-sid-1", same
// "[stub echo] <prompt>" reply shape.
//
// Tests that need a scripted/recording backend should use the
// FakeBackend in stub_backend_test.go (and elsewhere) — StubBackend is
// intentionally minimal.
type StubBackend struct {
	// Prefix is the string prepended to the user prompt in the echo reply.
	// Defaults to "[stub echo] " when unset.
	Prefix string

	// SessionID is the canned NewSessionID emitted on every turn.
	// Defaults to "stub-sid-1" when unset.
	SessionID string
}

// Compile-time interface assertion.
var _ clibackend.CliBackend = (*StubBackend)(nil)

// NewStubBackend builds a StubBackend with the canonical defaults
// (matches the Python --register-stub flag's behaviour byte-for-byte).
func NewStubBackend() *StubBackend {
	return &StubBackend{}
}

// ID implements [clibackend.CliBackend].
func (s *StubBackend) ID() string { return "stub" }

// DefaultCommand implements [clibackend.CliBackend]. The stub never
// shells out, but tests that call DefaultCommand() expect a non-nil
// slice — `["true"]` matches the Python implementation.
func (s *StubBackend) DefaultCommand() []string { return []string{"true"} }

// SupportsResumeInStream implements [clibackend.CliBackend]. The stub
// has no real CLI, so resume-in-stream is meaningless; report false to
// match the Python tree.
func (s *StubBackend) SupportsResumeInStream() bool { return false }

// Turn implements [clibackend.CliBackend]. Returns a CliTurnOutput with
// the echoed text + canned session id.
func (s *StubBackend) Turn(_ context.Context, in clibackend.CliTurnInput) (*clibackend.CliTurnOutput, *clibackend.CliTurnError) {
	prefix := s.Prefix
	if prefix == "" {
		prefix = "[stub echo] "
	}
	sid := s.SessionID
	if sid == "" {
		sid = "stub-sid-1"
	}
	sidCopy := sid
	return &clibackend.CliTurnOutput{
		Text:         prefix + in.UserPrompt,
		NewSessionID: &sidCopy,
		Usage:        map[string]any{"input_tokens": 0, "output_tokens": 0},
		RawEvents:    []map[string]any{},
	}, nil
}
