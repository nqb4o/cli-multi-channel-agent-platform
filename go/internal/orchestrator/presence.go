// Presence-signal trigger.
//
// When a channel adapter sees a typing indicator or "user opened chat"
// event, it POSTs to /sandboxes/{user_id}/prewarm and the orchestrator
// resumes the user's sandbox. Repeated prewarms within DefaultDebounceS
// collapse into one in-flight resume (also benefiting from the pool's
// SingleFlight).
package orchestrator

import (
	"context"
	"errors"
	"sync"
	"time"
)

// DefaultDebounceS is the minimum gap between prewarms for the same user.
// Typing indicators fire every few hundred ms; we don't thrash the pool
// for every signal.
const DefaultDebounceS = 2 * time.Second

// PresenceTriggerOption configures a PresenceTrigger.
type PresenceTriggerOption func(*PresenceTrigger)

// WithDebounce overrides the per-user debounce interval.
func WithDebounce(d time.Duration) PresenceTriggerOption {
	return func(p *PresenceTrigger) { p.debounce = d }
}

// WithPresenceClock injects a deterministic clock for tests.
func WithPresenceClock(clock func() time.Time) PresenceTriggerOption {
	return func(p *PresenceTrigger) { p.clock = clock }
}

// PrewarmResult is the structured response from PresenceTrigger.Prewarm.
type PrewarmResult struct {
	Status       string `json:"status"` // "ready" | "prewarming" | "debounced"
	UserID       string `json:"user_id"`
	SandboxID    string `json:"sandbox_id,omitempty"`
	SandboxState string `json:"sandbox_state,omitempty"`
	DebounceS    int    `json:"debounce_s,omitempty"`
}

// PresenceTrigger coalesces presence signals into prewarm calls.
type PresenceTrigger struct {
	pool     *SandboxPool
	debounce time.Duration
	clock    func() time.Time

	mu         sync.Mutex
	lastPrewarm map[string]time.Time
}

// NewPresenceTrigger constructs a trigger wrapping the given pool.
func NewPresenceTrigger(pool *SandboxPool, opts ...PresenceTriggerOption) *PresenceTrigger {
	t := &PresenceTrigger{
		pool:        pool,
		debounce:    DefaultDebounceS,
		clock:       time.Now,
		lastPrewarm: map[string]time.Time{},
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Prewarm triggers a prewarm for userID. Returns "debounced" if a recent
// call already kicked off the resume.
func (t *PresenceTrigger) Prewarm(ctx context.Context, userID string) (*PrewarmResult, error) {
	if userID == "" {
		return nil, errors.New("user_id is required")
	}
	now := t.clock()
	t.mu.Lock()
	last := t.lastPrewarm[userID]
	if !last.IsZero() && now.Sub(last) < t.debounce {
		t.mu.Unlock()
		return &PrewarmResult{
			Status:    "debounced",
			UserID:    userID,
			DebounceS: int(t.debounce.Seconds()),
		}, nil
	}
	t.lastPrewarm[userID] = now
	t.mu.Unlock()

	sandbox, err := t.pool.GetOrResume(ctx, userID)
	if err != nil {
		return nil, err
	}
	statusLabel := "prewarming"
	if sandbox.State == StateRunning {
		statusLabel = "ready"
	}
	return &PrewarmResult{
		Status:       statusLabel,
		UserID:       userID,
		SandboxID:    sandbox.ID,
		SandboxState: string(sandbox.State),
	}, nil
}

// LastPrewarmAt returns the last prewarm time for userID, or zero.
func (t *PresenceTrigger) LastPrewarmAt(userID string) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastPrewarm[userID]
}
