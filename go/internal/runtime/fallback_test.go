package runtime

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/openclaw/agent-platform/internal/clibackend"
)

// ---------------------------------------------------------------------------
// Test helpers / scripted backends
// ---------------------------------------------------------------------------

// scriptedResult is one queued dispatcher result.
type scriptedResult struct {
	out *clibackend.CliTurnOutput
	err *clibackend.CliTurnError
}

// ScriptedDispatcher is a TurnDispatcher that returns pre-scripted results per
// provider in call order.
type ScriptedDispatcher struct {
	queues map[string][]scriptedResult
	calls  map[string]int
}

func newScriptedDispatcher() *ScriptedDispatcher {
	return &ScriptedDispatcher{
		queues: make(map[string][]scriptedResult),
		calls:  make(map[string]int),
	}
}

func (d *ScriptedDispatcher) addResult(providerID string, out *clibackend.CliTurnOutput, err *clibackend.CliTurnError) {
	d.queues[providerID] = append(d.queues[providerID], scriptedResult{out, err})
}

func (d *ScriptedDispatcher) addSuccess(providerID, text string) {
	sid := providerID + "-sid"
	d.addResult(providerID, &clibackend.CliTurnOutput{
		Text:         text,
		NewSessionID: &sid,
		Usage:        map[string]any{},
	}, nil)
}

func (d *ScriptedDispatcher) addError(providerID string, class clibackend.ErrorClass, msg string) {
	d.addResult(providerID, nil, &clibackend.CliTurnError{
		Class:   class,
		Message: msg,
	})
}

func (d *ScriptedDispatcher) dispatch(_ context.Context, entry ProviderEntry, _ clibackend.CliTurnInput) (*clibackend.CliTurnOutput, *clibackend.CliTurnError) {
	d.calls[entry.ID]++
	q := d.queues[entry.ID]
	if len(q) == 0 {
		return nil, &clibackend.CliTurnError{
			Class:   clibackend.Unknown,
			Message: fmt.Sprintf("scripted dispatcher: no more results for %q", entry.ID),
		}
	}
	r := q[0]
	d.queues[entry.ID] = q[1:]
	return r.out, r.err
}

// alwaysSucceed returns a dispatcher that always succeeds for any provider.
func alwaysSucceed(text string) TurnDispatcher {
	return func(_ context.Context, entry ProviderEntry, _ clibackend.CliTurnInput) (*clibackend.CliTurnOutput, *clibackend.CliTurnError) {
		sid := entry.ID + "-sid"
		return &clibackend.CliTurnOutput{
			Text:         text,
			NewSessionID: &sid,
			Usage:        map[string]any{},
		}, nil
	}
}

// alwaysFail returns a dispatcher that always fails with the given class.
func alwaysFail(class clibackend.ErrorClass, msg string) TurnDispatcher {
	return func(_ context.Context, _ ProviderEntry, _ clibackend.CliTurnInput) (*clibackend.CliTurnOutput, *clibackend.CliTurnError) {
		return nil, &clibackend.CliTurnError{Class: class, Message: msg}
	}
}

// callCounter is an atomic turn counter for a dispatcher wrapper.
type callCounter struct {
	n       atomic.Int32
	wrapped TurnDispatcher
}

func (c *callCounter) dispatch(ctx context.Context, entry ProviderEntry, inp clibackend.CliTurnInput) (*clibackend.CliTurnOutput, *clibackend.CliTurnError) {
	c.n.Add(1)
	return c.wrapped(ctx, entry, inp)
}

// makeClock returns a controllable fake clock.
func makeClock(start float64) (clock func() float64, advance func(float64)) {
	t := start
	return func() float64 { return t }, func(d float64) { t += d }
}

// ---------------------------------------------------------------------------
// IsFailoverWorthy
// ---------------------------------------------------------------------------

func TestIsFailoverWorthyAuthExpired(t *testing.T) {
	if !IsFailoverWorthy(clibackend.AuthExpired) {
		t.Fatal("AuthExpired should be failover-worthy")
	}
}

func TestIsFailoverWorthyRateLimit(t *testing.T) {
	if !IsFailoverWorthy(clibackend.RateLimit) {
		t.Fatal("RateLimit should be failover-worthy")
	}
}

func TestIsFailoverWorthyTransient(t *testing.T) {
	if !IsFailoverWorthy(clibackend.Transient) {
		t.Fatal("Transient should be failover-worthy")
	}
}

func TestIsFailoverWorthyUnknownIsFalse(t *testing.T) {
	if IsFailoverWorthy(clibackend.Unknown) {
		t.Fatal("Unknown should NOT be failover-worthy")
	}
}

// ---------------------------------------------------------------------------
// NewProviderChain validation
// ---------------------------------------------------------------------------

func TestNewProviderChainEmptyEntriesError(t *testing.T) {
	_, err := NewProviderChain(nil, NewDefaultCircuitBreaker())
	if err == nil {
		t.Fatal("expected error for empty entries")
	}
}

func TestNewProviderChainAllFallbackOnlyError(t *testing.T) {
	entries := []ProviderEntry{
		{ID: "a", FallbackOnly: true},
		{ID: "b", FallbackOnly: true},
	}
	_, err := NewProviderChain(entries, NewDefaultCircuitBreaker())
	if err == nil {
		t.Fatal("expected error when all entries are fallback_only")
	}
}

func TestNewProviderChainNilBreakerError(t *testing.T) {
	entries := []ProviderEntry{{ID: "a"}}
	_, err := NewProviderChain(entries, nil)
	if err == nil {
		t.Fatal("expected error for nil breaker")
	}
}

func TestNewProviderChainValid(t *testing.T) {
	entries := []ProviderEntry{
		{ID: "primary"},
		{ID: "fallback", FallbackOnly: true},
	}
	chain, err := NewProviderChain(entries, NewDefaultCircuitBreaker())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chain == nil {
		t.Fatal("chain should not be nil")
	}
}

// ---------------------------------------------------------------------------
// OrderedForDispatch
// ---------------------------------------------------------------------------

func TestOrderedForDispatchPrimariesFirst(t *testing.T) {
	entries := []ProviderEntry{
		{ID: "fallback-1", FallbackOnly: true},
		{ID: "primary-1"},
		{ID: "fallback-2", FallbackOnly: true},
		{ID: "primary-2"},
	}
	chain, _ := NewProviderChain(entries, NewDefaultCircuitBreaker())
	ordered := chain.OrderedForDispatch()
	if len(ordered) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(ordered))
	}
	if ordered[0].FallbackOnly || ordered[1].FallbackOnly {
		t.Fatal("primaries should come first")
	}
	if !ordered[2].FallbackOnly || !ordered[3].FallbackOnly {
		t.Fatal("fallbacks should come last")
	}
}

// ---------------------------------------------------------------------------
// ProviderChain.Turn — happy path (single provider)
// ---------------------------------------------------------------------------

func TestChainTurnSingleProviderSuccess(t *testing.T) {
	entries := []ProviderEntry{{ID: "claude"}}
	chain, _ := NewProviderChain(entries, NewDefaultCircuitBreaker())
	d := newScriptedDispatcher()
	d.addSuccess("claude", "hello from claude")
	result := chain.Turn(context.Background(), clibackend.CliTurnInput{}, d.dispatch)
	if !result.Succeeded() {
		t.Fatalf("expected success, got error: %v", result.Error)
	}
	if result.Output.Text != "hello from claude" {
		t.Fatalf("Text=%q", result.Output.Text)
	}
	if result.SelectedProviderID != "claude" {
		t.Fatalf("SelectedProviderID=%q", result.SelectedProviderID)
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(result.Attempts))
	}
	if !result.Attempts[0].Succeeded {
		t.Fatal("attempt should be succeeded")
	}
}

// ---------------------------------------------------------------------------
// Fallback on failover-worthy errors
// ---------------------------------------------------------------------------

func TestChainTurnRateLimitFallsBackToSecondary(t *testing.T) {
	entries := []ProviderEntry{
		{ID: "claude"},
		{ID: "codex", FallbackOnly: true},
	}
	chain, _ := NewProviderChain(entries, NewDefaultCircuitBreaker())
	d := newScriptedDispatcher()
	d.addError("claude", clibackend.RateLimit, "429 rate limit")
	d.addSuccess("codex", "hello from codex")
	result := chain.Turn(context.Background(), clibackend.CliTurnInput{}, d.dispatch)
	if !result.Succeeded() {
		t.Fatalf("expected success via fallback, got error: %v", result.Error)
	}
	if result.SelectedProviderID != "codex" {
		t.Fatalf("SelectedProviderID=%q", result.SelectedProviderID)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(result.Attempts))
	}
}

func TestChainTurnAuthExpiredFallsBack(t *testing.T) {
	entries := []ProviderEntry{
		{ID: "claude"},
		{ID: "gemini", FallbackOnly: true},
	}
	chain, _ := NewProviderChain(entries, NewDefaultCircuitBreaker())
	d := newScriptedDispatcher()
	d.addError("claude", clibackend.AuthExpired, "token expired")
	d.addSuccess("gemini", "hello from gemini")
	result := chain.Turn(context.Background(), clibackend.CliTurnInput{}, d.dispatch)
	if !result.Succeeded() {
		t.Fatalf("expected success, got: %v", result.Error)
	}
	if result.SelectedProviderID != "gemini" {
		t.Fatalf("SelectedProviderID=%q", result.SelectedProviderID)
	}
}

func TestChainTurnTransientFallsBack(t *testing.T) {
	entries := []ProviderEntry{
		{ID: "a"},
		{ID: "b", FallbackOnly: true},
	}
	chain, _ := NewProviderChain(entries, NewDefaultCircuitBreaker())
	d := newScriptedDispatcher()
	d.addError("a", clibackend.Transient, "network error")
	d.addSuccess("b", "ok")
	result := chain.Turn(context.Background(), clibackend.CliTurnInput{}, d.dispatch)
	if !result.Succeeded() {
		t.Fatalf("expected success: %v", result.Error)
	}
}

// ---------------------------------------------------------------------------
// Unknown errors do NOT trigger fallback
// ---------------------------------------------------------------------------

func TestChainTurnUnknownDoesNotFallBack(t *testing.T) {
	entries := []ProviderEntry{
		{ID: "a"},
		{ID: "b", FallbackOnly: true},
	}
	cc := &callCounter{wrapped: alwaysFail(clibackend.Unknown, "bug")}
	chain, _ := NewProviderChain(entries, NewDefaultCircuitBreaker())
	result := chain.Turn(context.Background(), clibackend.CliTurnInput{}, cc.dispatch)
	if result.Succeeded() {
		t.Fatal("expected failure on unknown error")
	}
	if result.Error.Class != clibackend.Unknown {
		t.Fatalf("error class should be Unknown, got %q", result.Error.Class)
	}
	// Only one attempt should have been made.
	if cc.n.Load() != 1 {
		t.Fatalf("expected 1 dispatcher call, got %d", cc.n.Load())
	}
}

// ---------------------------------------------------------------------------
// Parent error stamping
// ---------------------------------------------------------------------------

func TestChainTurnParentErrorsStamped(t *testing.T) {
	entries := []ProviderEntry{
		{ID: "primary"},
		{ID: "fallback", FallbackOnly: true},
	}
	chain, _ := NewProviderChain(entries, NewDefaultCircuitBreaker())
	d := newScriptedDispatcher()
	d.addError("primary", clibackend.RateLimit, "429")
	d.addSuccess("fallback", "ok")
	result := chain.Turn(context.Background(), clibackend.CliTurnInput{}, d.dispatch)
	if !result.Succeeded() {
		t.Fatalf("expected success: %v", result.Error)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(result.Attempts))
	}
	// First attempt (primary) should be marked parent_error=true.
	if !result.Attempts[0].ParentError {
		t.Fatal("primary failure should be marked parent_error=true")
	}
	// Second attempt (fallback success) should NOT be parent_error.
	if result.Attempts[1].ParentError {
		t.Fatal("successful fallback should NOT be parent_error")
	}
}

func TestChainTurnNoParentErrorOnDirectSuccess(t *testing.T) {
	entries := []ProviderEntry{{ID: "a"}}
	chain, _ := NewProviderChain(entries, NewDefaultCircuitBreaker())
	result := chain.Turn(context.Background(), clibackend.CliTurnInput{}, alwaysSucceed("ok"))
	if result.Attempts[0].ParentError {
		t.Fatal("direct success should not be parent_error")
	}
}

// ---------------------------------------------------------------------------
// Exhausted chain
// ---------------------------------------------------------------------------

func TestChainTurnAllProvidersFail(t *testing.T) {
	entries := []ProviderEntry{
		{ID: "a"},
		{ID: "b", FallbackOnly: true},
		{ID: "c", FallbackOnly: true},
	}
	chain, _ := NewProviderChain(entries, NewDefaultCircuitBreaker())
	result := chain.Turn(context.Background(), clibackend.CliTurnInput{}, alwaysFail(clibackend.RateLimit, "429"))
	if result.Succeeded() {
		t.Fatal("should fail when all providers fail")
	}
	if result.Error == nil {
		t.Fatal("error should not be nil")
	}
	if len(result.Attempts) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(result.Attempts))
	}
}

// ---------------------------------------------------------------------------
// Circuit breaker integration
// ---------------------------------------------------------------------------

func TestCircuitBreakerTripAfterThreshold(t *testing.T) {
	clock, advance := makeClock(1000.0)
	cb, _ := NewCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 2,
		WindowSeconds:    60,
		CooldownSeconds:  30,
		Clock:            clock,
	})
	cb.RecordFailure("p1", clibackend.RateLimit)
	cb.RecordFailure("p1", clibackend.RateLimit)

	if !cb.IsOpen("p1") {
		t.Fatal("breaker should be open after 2 failures")
	}
	if cb.State("p1") != "open" {
		t.Fatalf("state should be open, got %q", cb.State("p1"))
	}

	// Advance past cooldown.
	advance(35)
	if cb.IsOpen("p1") {
		t.Fatal("breaker should be half-open after cooldown")
	}
	if cb.State("p1") != "half_open" {
		t.Fatalf("state should be half_open, got %q", cb.State("p1"))
	}
}

func TestCircuitBreakerClosesOnSuccess(t *testing.T) {
	clock, _ := makeClock(1000.0)
	cb, _ := NewCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 2,
		WindowSeconds:    60,
		CooldownSeconds:  30,
		Clock:            clock,
	})
	cb.RecordFailure("p1", clibackend.RateLimit)
	cb.RecordFailure("p1", clibackend.RateLimit)
	if !cb.IsOpen("p1") {
		t.Fatal("breaker should be open")
	}
	cb.RecordSuccess("p1")
	if cb.IsOpen("p1") {
		t.Fatal("breaker should close on success")
	}
	if cb.State("p1") != "closed" {
		t.Fatalf("state should be closed, got %q", cb.State("p1"))
	}
}

func TestCircuitBreakerUnknownDoesNotCountTowardThreshold(t *testing.T) {
	clock, _ := makeClock(1000.0)
	cb, _ := NewCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 2,
		WindowSeconds:    60,
		CooldownSeconds:  30,
		Clock:            clock,
	})
	// Record only unknown failures — should NOT trip.
	cb.RecordFailure("p1", clibackend.Unknown)
	cb.RecordFailure("p1", clibackend.Unknown)
	if cb.IsOpen("p1") {
		t.Fatal("unknown failures should NOT trip the breaker")
	}
}

func TestCircuitBreakerWindowEviction(t *testing.T) {
	clock, advance := makeClock(1000.0)
	cb, _ := NewCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 2,
		WindowSeconds:    10,
		CooldownSeconds:  30,
		Clock:            clock,
	})
	cb.RecordFailure("p1", clibackend.Transient) // at t=1000
	advance(15)
	cb.RecordFailure("p1", clibackend.Transient) // at t=1015 (first one expired)
	// Only 1 failure in the window — should not trip.
	if cb.IsOpen("p1") {
		t.Fatal("breaker should not trip; first failure evicted from window")
	}
}

func TestCircuitBreakerSkipsOpenProviderInChain(t *testing.T) {
	clock, _ := makeClock(1000.0)
	cb, _ := NewCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 1,
		WindowSeconds:    60,
		CooldownSeconds:  3600,
		Clock:            clock,
	})
	cb.RecordFailure("primary", clibackend.RateLimit) // trip immediately

	entries := []ProviderEntry{
		{ID: "primary"},
		{ID: "fallback", FallbackOnly: true},
	}
	chain, _ := NewProviderChain(entries, cb, WithChainClock(clock))
	d := newScriptedDispatcher()
	d.addSuccess("fallback", "ok from fallback")
	result := chain.Turn(context.Background(), clibackend.CliTurnInput{}, d.dispatch)
	if !result.Succeeded() {
		t.Fatalf("expected fallback success, got: %v", result.Error)
	}
	// First attempt should be breaker-skipped.
	if !result.Attempts[0].SkippedBreaker {
		t.Fatal("first attempt should be skipped_breaker=true")
	}
	if result.Attempts[0].ProviderID != "primary" {
		t.Fatalf("first skipped attempt should be primary, got %q", result.Attempts[0].ProviderID)
	}
}

func TestAllBreakerOpenYieldsTransientError(t *testing.T) {
	clock, _ := makeClock(1000.0)
	cb, _ := NewCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 1,
		WindowSeconds:    60,
		CooldownSeconds:  3600,
		Clock:            clock,
	})
	cb.RecordFailure("a", clibackend.Transient)
	cb.RecordFailure("b", clibackend.Transient)

	entries := []ProviderEntry{{ID: "a"}, {ID: "b", FallbackOnly: true}}
	chain, _ := NewProviderChain(entries, cb, WithChainClock(clock))
	result := chain.Turn(context.Background(), clibackend.CliTurnInput{},
		func(_ context.Context, _ ProviderEntry, _ clibackend.CliTurnInput) (*clibackend.CliTurnOutput, *clibackend.CliTurnError) {
			t.Fatal("dispatcher should not be called when all breakers are open")
			return nil, nil
		},
	)
	if result.Succeeded() {
		t.Fatal("should fail")
	}
}

// ---------------------------------------------------------------------------
// Health tracker integration
// ---------------------------------------------------------------------------

type stubHealthTracker struct {
	successes int
	failures  int
}

func (s *stubHealthTracker) RecordSuccess() { s.successes++ }
func (s *stubHealthTracker) RecordFailure() { s.failures++ }

var _ HealthTrackerLike = (*stubHealthTracker)(nil)

func TestChainTurnCallsHealthTrackerOnSuccess(t *testing.T) {
	tracker := &stubHealthTracker{}
	entries := []ProviderEntry{{ID: "p"}}
	chain, _ := NewProviderChain(entries, NewDefaultCircuitBreaker(),
		WithHealthTracker("p", tracker))
	chain.Turn(context.Background(), clibackend.CliTurnInput{}, alwaysSucceed("ok"))
	if tracker.successes != 1 {
		t.Fatalf("expected 1 success, got %d", tracker.successes)
	}
}

func TestChainTurnCallsHealthTrackerOnFailure(t *testing.T) {
	tracker := &stubHealthTracker{}
	entries := []ProviderEntry{{ID: "p"}}
	chain, _ := NewProviderChain(entries, NewDefaultCircuitBreaker(),
		WithHealthTracker("p", tracker))
	chain.Turn(context.Background(), clibackend.CliTurnInput{}, alwaysFail(clibackend.RateLimit, "429"))
	if tracker.failures != 1 {
		t.Fatalf("expected 1 failure, got %d", tracker.failures)
	}
}

// ---------------------------------------------------------------------------
// Attempt callback
// ---------------------------------------------------------------------------

func TestChainTurnAttemptCallbackCalled(t *testing.T) {
	var recorded []FallbackAttempt
	entries := []ProviderEntry{{ID: "x"}}
	chain, _ := NewProviderChain(entries, NewDefaultCircuitBreaker(),
		WithAttemptCallback(func(a FallbackAttempt) { recorded = append(recorded, a) }))
	chain.Turn(context.Background(), clibackend.CliTurnInput{}, alwaysSucceed("ok"))
	if len(recorded) != 1 {
		t.Fatalf("expected 1 callback, got %d", len(recorded))
	}
}

// ---------------------------------------------------------------------------
// Circuit breaker half-open trial
// ---------------------------------------------------------------------------

func TestCircuitBreakerHalfOpenAllowsOneTrial(t *testing.T) {
	clock, advance := makeClock(1000.0)
	cb, _ := NewCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 1,
		WindowSeconds:    60,
		CooldownSeconds:  30,
		Clock:            clock,
	})
	cb.RecordFailure("p", clibackend.Transient) // open
	advance(35)                                  // past cooldown → half_open
	if cb.State("p") != "half_open" {
		t.Fatalf("expected half_open, got %q", cb.State("p"))
	}
	cb.AttemptStarted("p")
	if !cb.IsOpen("p") {
		t.Fatal("half_open with inflight should be treated as open")
	}
}

// ---------------------------------------------------------------------------
// NewCircuitBreaker validation
// ---------------------------------------------------------------------------

func TestNewCircuitBreakerNegativeThresholdError(t *testing.T) {
	_, err := NewCircuitBreaker(CircuitBreakerConfig{FailureThreshold: -1})
	if err == nil {
		t.Fatal("expected error for negative failure threshold")
	}
}

// ---------------------------------------------------------------------------
// stampParentErrors (internal helper, tested through public API)
// ---------------------------------------------------------------------------

func TestStampParentErrorsNoneIfNoSuccess(t *testing.T) {
	entries := []ProviderEntry{{ID: "a"}, {ID: "b", FallbackOnly: true}}
	chain, _ := NewProviderChain(entries, NewDefaultCircuitBreaker())
	result := chain.Turn(context.Background(), clibackend.CliTurnInput{}, alwaysFail(clibackend.Transient, "error"))
	for _, att := range result.Attempts {
		if att.ParentError {
			t.Fatal("no attempt should be parent_error when chain fails entirely")
		}
	}
}

// ---------------------------------------------------------------------------
// Three-provider chain
// ---------------------------------------------------------------------------

func TestChainTurnThreeProviders(t *testing.T) {
	entries := []ProviderEntry{
		{ID: "a"},
		{ID: "b", FallbackOnly: true},
		{ID: "c", FallbackOnly: true},
	}
	chain, _ := NewProviderChain(entries, NewDefaultCircuitBreaker())
	d := newScriptedDispatcher()
	d.addError("a", clibackend.RateLimit, "429 from a")
	d.addError("b", clibackend.Transient, "error from b")
	d.addSuccess("c", "ok from c")
	result := chain.Turn(context.Background(), clibackend.CliTurnInput{}, d.dispatch)
	if !result.Succeeded() {
		t.Fatalf("expected success: %v", result.Error)
	}
	if result.SelectedProviderID != "c" {
		t.Fatalf("SelectedProviderID=%q", result.SelectedProviderID)
	}
	if len(result.Attempts) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(result.Attempts))
	}
	// Both a and b should be parent errors.
	if !result.Attempts[0].ParentError {
		t.Fatal("a should be parent_error")
	}
	if !result.Attempts[1].ParentError {
		t.Fatal("b should be parent_error")
	}
	if result.Attempts[2].ParentError {
		t.Fatal("c (success) should not be parent_error")
	}
}
