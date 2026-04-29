// F16 — provider fallback chain + circuit breaker.
//
// When the primary provider fails with a failover-worthy error the agent loop
// walks the configured chain, retrying on the next provider until one succeeds
// or the chain is exhausted.
//
// Design points:
//   - Failover scope: only AuthExpired / RateLimit / Transient errors advance
//     the chain. Unknown is terminal — surfacing it immediately surfaces real
//     bugs instead of masking them by silently retrying.
//   - Per-provider session isolation: each fallback attempt resolves its own
//     cli_session_id via the session DAL keyed by provider. Falling back from
//     Claude to Codex never reuses Claude's session id.
//   - Original-failure logging: even when the chain succeeds via a fallback,
//     every preceding failure is stamped parent_error=true.
//   - Circuit breaker: per-process, in-memory. State is per provider_id.
//     Cross-node sharing is a Phase 3 concern (per F16's out-of-band note).
//   - Telemetry integration via duck-typed HealthTrackerLike interface; the
//     telemetry package is optional.
//
// Mirrors services/runtime/src/runtime/fallback.py (Go port).
package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/openclaw/agent-platform/internal/clibackend"
)

// ---------------------------------------------------------------------------
// Failover-worthy classification
// ---------------------------------------------------------------------------

// failoverWorthy is the set of error classes that advance the chain.
// Unknown is intentionally absent — it terminates the chain immediately.
var failoverWorthy = map[clibackend.ErrorClass]bool{
	clibackend.AuthExpired: true,
	clibackend.RateLimit:   true,
	clibackend.Transient:   true,
}

// IsFailoverWorthy returns true when the error class should advance the chain.
func IsFailoverWorthy(class clibackend.ErrorClass) bool {
	return failoverWorthy[class]
}

// ---------------------------------------------------------------------------
// Provider entry
// ---------------------------------------------------------------------------

// ProviderEntry is one ordered slot in a fallback chain.
// Mirrors ProviderConfig but decoupled so the chain can be built directly
// from tests / synthetic configs.
type ProviderEntry struct {
	ID           string
	Model        string // empty means CLI picks default
	FallbackOnly bool
}

// ---------------------------------------------------------------------------
// Circuit breaker
// ---------------------------------------------------------------------------

// breakerState is the internal per-provider state.
type breakerState int

const (
	breakerClosed   breakerState = iota // normal operation
	breakerOpen                         // tripped — preemptively skipping
	breakerHalfOpen                     // cooldown elapsed; one trial attempt
)

type breakerEntry struct {
	failures         []float64
	state            breakerState
	openedAt         float64
	halfOpenInflight bool
}

// CircuitBreakerConfig groups the configurable thresholds.
type CircuitBreakerConfig struct {
	// FailureThreshold is the number of failures within WindowSeconds that
	// trips the breaker. Default 3.
	FailureThreshold int
	// WindowSeconds is the rolling window length in seconds. Default 60.
	WindowSeconds float64
	// CooldownSeconds is the time the breaker stays open before half-opening.
	// Default 30.
	CooldownSeconds float64
	// Clock is the time source — nil defaults to time.Now(). Injected by
	// tests for determinism.
	Clock func() float64
}

func (c *CircuitBreakerConfig) applyDefaults() {
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = 3
	}
	if c.WindowSeconds <= 0 {
		c.WindowSeconds = 60.0
	}
	if c.CooldownSeconds <= 0 {
		c.CooldownSeconds = 30.0
	}
	if c.Clock == nil {
		c.Clock = func() float64 {
			t := time.Now()
			return float64(t.Unix()) + float64(t.Nanosecond())/1e9
		}
	}
}

// CircuitBreaker is an in-memory per-process circuit breaker keyed by
// provider_id. Thread-safe within a single goroutine (the agent loop is
// single-goroutine per request). The breaker does NOT require a mutex because
// the agent loop is a goroutine-per-request model; shared state across
// goroutines requires the caller to provide external synchronisation.
//
// For multi-goroutine use, wrap with a mutex or use the agent_loop's
// single-goroutine call site (the default).
type CircuitBreaker struct {
	cfg     CircuitBreakerConfig
	entries map[string]*breakerEntry
}

// NewCircuitBreaker creates a CircuitBreaker with custom config.
//
// FailureThreshold, WindowSeconds, and CooldownSeconds default to 3, 60, and
// 30 respectively when zero (unset). Negative values are rejected.
func NewCircuitBreaker(cfg CircuitBreakerConfig) (*CircuitBreaker, error) {
	if cfg.FailureThreshold < 0 {
		return nil, fmt.Errorf("FailureThreshold must be >= 0 (0 = use default 3)")
	}
	if cfg.WindowSeconds < 0 {
		return nil, fmt.Errorf("WindowSeconds must be >= 0 (0 = use default 60)")
	}
	if cfg.CooldownSeconds < 0 {
		return nil, fmt.Errorf("CooldownSeconds must be >= 0 (0 = use default 30)")
	}
	cfg.applyDefaults()
	return &CircuitBreaker{
		cfg:     cfg,
		entries: make(map[string]*breakerEntry),
	}, nil
}

// NewDefaultCircuitBreaker creates a CircuitBreaker with default thresholds.
func NewDefaultCircuitBreaker() *CircuitBreaker {
	cb, _ := NewCircuitBreaker(CircuitBreakerConfig{})
	return cb
}

// State returns the current string state of the provider's breaker.
func (cb *CircuitBreaker) State(providerID string) string {
	e := cb.entries[providerID]
	if e == nil {
		return "closed"
	}
	cb.maybeHalfOpen(providerID, e)
	switch e.state {
	case breakerOpen:
		return "open"
	case breakerHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

// IsOpen returns true iff the provider should be skipped preemptively.
// HALF_OPEN with an in-flight trial also counts as open.
func (cb *CircuitBreaker) IsOpen(providerID string) bool {
	e := cb.entries[providerID]
	if e == nil {
		return false
	}
	cb.maybeHalfOpen(providerID, e)
	if e.state == breakerOpen {
		return true
	}
	if e.state == breakerHalfOpen && e.halfOpenInflight {
		return true
	}
	return false
}

// AttemptStarted marks that an attempt has started — relevant only in half-open
// so we block concurrent attempts from all slipping through together.
func (cb *CircuitBreaker) AttemptStarted(providerID string) {
	e := cb.entries[providerID]
	if e == nil {
		return
	}
	if e.state == breakerHalfOpen {
		e.halfOpenInflight = true
	}
}

// RecordSuccess closes the breaker and clears failures. One success is enough.
func (cb *CircuitBreaker) RecordSuccess(providerID string) {
	e := cb.entries[providerID]
	if e == nil {
		return
	}
	e.failures = e.failures[:0]
	e.state = breakerClosed
	e.openedAt = 0
	e.halfOpenInflight = false
}

// RecordFailure appends a failure. Only failover-worthy classes count toward
// the threshold — unknown errors terminate the chain immediately without
// tripping the breaker for failover-unworthy reasons.
func (cb *CircuitBreaker) RecordFailure(providerID string, class clibackend.ErrorClass) {
	if !IsFailoverWorthy(class) {
		// Still handle half-open trial result for non-worthy classes.
		e := cb.entries[providerID]
		if e != nil && e.state == breakerHalfOpen {
			e.state = breakerOpen
			e.openedAt = cb.cfg.Clock()
			e.halfOpenInflight = false
		}
		return
	}
	now := cb.cfg.Clock()
	e := cb.entries[providerID]
	if e == nil {
		e = &breakerEntry{}
		cb.entries[providerID] = e
	}
	cutoff := now - cb.cfg.WindowSeconds
	filtered := e.failures[:0]
	for _, t := range e.failures {
		if t >= cutoff {
			filtered = append(filtered, t)
		}
	}
	e.failures = append(filtered, now)
	e.halfOpenInflight = false

	if e.state == breakerHalfOpen || len(e.failures) >= cb.cfg.FailureThreshold {
		e.state = breakerOpen
		e.openedAt = now
	}
}

func (cb *CircuitBreaker) maybeHalfOpen(providerID string, e *breakerEntry) {
	if e.state != breakerOpen || e.openedAt == 0 {
		return
	}
	if cb.cfg.Clock()-e.openedAt >= cb.cfg.CooldownSeconds {
		e.state = breakerHalfOpen
		e.halfOpenInflight = false
		e.failures = e.failures[:0]
	}
}

// ---------------------------------------------------------------------------
// Health tracker shim (optional telemetry integration)
// ---------------------------------------------------------------------------

// HealthTrackerLike is the duck-typed subset of F14's ProviderHealthTracker
// that the fallback chain needs. The runtime can run without the telemetry
// package installed; callers may pass nil for any provider.
type HealthTrackerLike interface {
	RecordSuccess()
	RecordFailure()
}

// ---------------------------------------------------------------------------
// Attempt + result records
// ---------------------------------------------------------------------------

// FallbackAttempt records one leg of a chain walk.
type FallbackAttempt struct {
	ProviderID     string
	Model          string // empty if no model override
	StartedAt      float64
	LatencyMS      int
	Succeeded      bool
	ErrorClass     string // empty if succeeded
	ErrorMessage   string // empty if succeeded
	SkippedBreaker bool   // true if the circuit breaker pre-empted this attempt
	ParentError    bool   // true if this failed attempt preceded an eventual success
}

// ProviderChainResult is the outcome of ProviderChain.Turn.
// Either Output or Error is non-nil.
type ProviderChainResult struct {
	Output             *clibackend.CliTurnOutput
	Error              *clibackend.CliTurnError
	Attempts           []FallbackAttempt
	SelectedProviderID string // empty if chain exhausted without success
}

// Succeeded returns true when a successful output is present.
func (r *ProviderChainResult) Succeeded() bool {
	return r.Output != nil
}

// ---------------------------------------------------------------------------
// TurnDispatcher
// ---------------------------------------------------------------------------

// TurnDispatcher is the function the agent loop provides; the chain calls it
// with (entry, input) and gets back a result or classified error.
// Dispatcher does not return a Go error — classifiable failures come back as
// *clibackend.CliTurnError (nil output). Go-level errors propagate as panics
// or via the recover wrapper.
type TurnDispatcher func(ctx context.Context, entry ProviderEntry, inp clibackend.CliTurnInput) (*clibackend.CliTurnOutput, *clibackend.CliTurnError)

// ---------------------------------------------------------------------------
// ProviderChain
// ---------------------------------------------------------------------------

// ProviderChain walks an ordered list of providers, advancing on
// failover-worthy errors.
type ProviderChain struct {
	entries         []ProviderEntry
	breaker         *CircuitBreaker
	healthTrackers  map[string]HealthTrackerLike
	clock           func() float64
	onAttempt       func(FallbackAttempt)
	logger          *slog.Logger
}

// ProviderChainOption configures a ProviderChain.
type ProviderChainOption func(*ProviderChain)

// WithHealthTracker sets a health tracker for a specific provider.
func WithHealthTracker(providerID string, tracker HealthTrackerLike) ProviderChainOption {
	return func(pc *ProviderChain) {
		if pc.healthTrackers == nil {
			pc.healthTrackers = make(map[string]HealthTrackerLike)
		}
		pc.healthTrackers[providerID] = tracker
	}
}

// WithChainClock overrides the clock function (for tests).
func WithChainClock(clock func() float64) ProviderChainOption {
	return func(pc *ProviderChain) { pc.clock = clock }
}

// WithAttemptCallback sets a callback called after each attempt.
func WithAttemptCallback(fn func(FallbackAttempt)) ProviderChainOption {
	return func(pc *ProviderChain) { pc.onAttempt = fn }
}

// WithChainLogger overrides the slog.Logger.
func WithChainLogger(logger *slog.Logger) ProviderChainOption {
	return func(pc *ProviderChain) { pc.logger = logger }
}

// NewProviderChain builds a ProviderChain.
//
// entries must be non-empty and at least one entry must have FallbackOnly=false.
func NewProviderChain(entries []ProviderEntry, breaker *CircuitBreaker, opts ...ProviderChainOption) (*ProviderChain, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("ProviderChain requires at least one entry")
	}
	hasNonFallback := false
	for _, e := range entries {
		if !e.FallbackOnly {
			hasNonFallback = true
			break
		}
	}
	if !hasNonFallback {
		return nil, fmt.Errorf("ProviderChain requires at least one non-fallback_only entry")
	}
	if breaker == nil {
		return nil, fmt.Errorf("ProviderChain requires a non-nil CircuitBreaker")
	}
	defaultClock := func() float64 {
		t := time.Now()
		return float64(t.Unix()) + float64(t.Nanosecond())/1e9
	}
	pc := &ProviderChain{
		entries:        copyEntries(entries),
		breaker:        breaker,
		healthTrackers: make(map[string]HealthTrackerLike),
		clock:          defaultClock,
		logger:         slog.Default(),
	}
	for _, opt := range opts {
		opt(pc)
	}
	return pc, nil
}

// Entries returns a copy of the provider entries.
func (pc *ProviderChain) Entries() []ProviderEntry {
	return copyEntries(pc.entries)
}

// Breaker returns the circuit breaker.
func (pc *ProviderChain) Breaker() *CircuitBreaker { return pc.breaker }

// OrderedForDispatch returns entries in attempt order: primaries first, then
// fallback-only providers. Sorts defensively so config order cannot promote a
// fallback above a primary.
func (pc *ProviderChain) OrderedForDispatch() []ProviderEntry {
	var primaries, fallbacks []ProviderEntry
	for _, e := range pc.entries {
		if e.FallbackOnly {
			fallbacks = append(fallbacks, e)
		} else {
			primaries = append(primaries, e)
		}
	}
	return append(primaries, fallbacks...)
}

// Turn walks the chain and returns the first success or the final error.
//
// On overall success the preceding failed attempts are stamped
// ParentError=true so callers can distinguish "primary worked" from "primary
// failed but fallback rescued the run".
func (pc *ProviderChain) Turn(ctx context.Context, inp clibackend.CliTurnInput, dispatcher TurnDispatcher) *ProviderChainResult {
	var attempts []FallbackAttempt
	var lastErr *clibackend.CliTurnError

	for _, entry := range pc.OrderedForDispatch() {
		// 1. Breaker check — preemptively skip if open.
		if pc.breaker.IsOpen(entry.ID) {
			att := FallbackAttempt{
				ProviderID:     entry.ID,
				Model:          entry.Model,
				StartedAt:      pc.clock(),
				LatencyMS:      0,
				Succeeded:      false,
				ErrorMessage:   fmt.Sprintf("circuit breaker open for %q; skipping preemptively", entry.ID),
				SkippedBreaker: true,
			}
			attempts = append(attempts, att)
			pc.emitAttempt(att)
			continue
		}

		// 2. Dispatch the turn.
		pc.breaker.AttemptStarted(entry.ID)
		started := pc.clock()
		out, cliErr := dispatcher(ctx, entry, inp)
		latencyMS := int((pc.clock() - started) * 1000)

		// 3. Successful turn — record + return.
		if out != nil && cliErr == nil {
			att := FallbackAttempt{
				ProviderID: entry.ID,
				Model:      entry.Model,
				StartedAt:  started,
				LatencyMS:  latencyMS,
				Succeeded:  true,
			}
			attempts = append(attempts, att)
			pc.emitAttempt(att)
			pc.breaker.RecordSuccess(entry.ID)
			if tracker, ok := pc.healthTrackers[entry.ID]; ok && tracker != nil {
				tracker.RecordSuccess()
			}
			return &ProviderChainResult{
				Output:             out,
				Error:              nil,
				Attempts:           stampParentErrors(attempts),
				SelectedProviderID: entry.ID,
			}
		}

		// 4. CliTurnError — branch on whether it advances the chain.
		if cliErr == nil {
			// nil/nil contract violation — treat as transient.
			cliErr = &clibackend.CliTurnError{
				Class:   clibackend.Transient,
				Message: "backend returned nil output and nil error",
			}
		}

		att := FallbackAttempt{
			ProviderID:   entry.ID,
			Model:        entry.Model,
			StartedAt:    started,
			LatencyMS:    latencyMS,
			Succeeded:    false,
			ErrorClass:   string(cliErr.Class),
			ErrorMessage: cliErr.Message,
		}
		attempts = append(attempts, att)
		pc.emitAttempt(att)
		pc.breaker.RecordFailure(entry.ID, cliErr.Class)
		if tracker, ok := pc.healthTrackers[entry.ID]; ok && tracker != nil {
			tracker.RecordFailure()
		}
		lastErr = cliErr

		if !IsFailoverWorthy(cliErr.Class) {
			// Unknown (or any class not in failover set) — surface immediately.
			// The brief is explicit: "Never fall back on unknown — surface
			// immediately (might indicate a bug)."
			return &ProviderChainResult{
				Output:   nil,
				Error:    cliErr,
				Attempts: attempts,
			}
		}
		// fall through and try the next entry
	}

	// Exhausted the chain.
	if lastErr == nil {
		lastErr = &clibackend.CliTurnError{
			Class:   clibackend.Transient,
			Message: "every provider in the fallback chain was skipped by an open circuit breaker",
		}
	}
	return &ProviderChainResult{
		Output:   nil,
		Error:    lastErr,
		Attempts: attempts,
	}
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

func (pc *ProviderChain) emitAttempt(att FallbackAttempt) {
	if att.Succeeded {
		pc.logger.Info("fallback chain attempt",
			"provider", att.ProviderID,
			"model", att.Model,
			"succeeded", att.Succeeded,
			"latency_ms", att.LatencyMS,
		)
	} else {
		pc.logger.Warn("fallback chain attempt failed",
			"provider", att.ProviderID,
			"model", att.Model,
			"error_class", att.ErrorClass,
			"latency_ms", att.LatencyMS,
			"skipped_breaker", att.SkippedBreaker,
		)
	}
	if pc.onAttempt != nil {
		pc.onAttempt(att)
	}
}

// stampParentErrors returns a copy of attempts with prior failures flagged as
// ParentError=true. Called only on overall chain success.
func stampParentErrors(attempts []FallbackAttempt) []FallbackAttempt {
	out := make([]FallbackAttempt, len(attempts))
	copy(out, attempts)
	succeededSeen := false
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Succeeded {
			succeededSeen = true
			break
		}
	}
	if !succeededSeen {
		return out
	}
	for i, att := range out {
		if !att.Succeeded {
			out[i].ParentError = true
		} else {
			break
		}
	}
	return out
}

func copyEntries(entries []ProviderEntry) []ProviderEntry {
	out := make([]ProviderEntry, len(entries))
	copy(out, entries)
	return out
}
