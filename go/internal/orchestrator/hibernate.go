// Idle-hibernate scheduler.
//
// Walks the SandboxPool's active set every scan_interval (default 30s) and
// hibernates any sandbox whose last_active_at is older than the per-tier
// idle threshold:
//
//   - free:    120s   (2 min)
//   - default: 300s   (5 min)
//   - pro:     900s   (15 min)
//
// Sandboxes pinned by the warm pool are skipped.
package orchestrator

import (
	"context"
	"log"
	"sync"
	"time"
)

// F15 brief defaults.
const (
	DefaultScanIntervalS  = 30 * time.Second
	DefaultFreeIdleS      = 120 * time.Second
	DefaultDefaultIdleS   = 300 * time.Second
	DefaultProIdleS       = 900 * time.Second
)

// HibernatePolicy holds per-tier idle thresholds + the scan cadence.
//
// Thresholds map tier_label -> idle duration. The "default" key is
// mandatory and applied to any user without an explicit tier.
type HibernatePolicy struct {
	Thresholds     map[string]time.Duration
	ScanIntervalS  time.Duration
}

// DefaultHibernatePolicy returns the F15 default policy.
func DefaultHibernatePolicy() HibernatePolicy {
	return HibernatePolicy{
		Thresholds: map[string]time.Duration{
			"free":    DefaultFreeIdleS,
			"default": DefaultDefaultIdleS,
			"pro":     DefaultProIdleS,
		},
		ScanIntervalS: DefaultScanIntervalS,
	}
}

// ThresholdFor resolves the idle threshold for a tier label. Empty/unknown
// labels fall back to the "default" threshold.
func (p HibernatePolicy) ThresholdFor(tier string) time.Duration {
	if tier != "" {
		if v, ok := p.Thresholds[tier]; ok {
			return v
		}
	}
	if v, ok := p.Thresholds["default"]; ok {
		return v
	}
	return DefaultDefaultIdleS
}

// TierResolver returns the tier label for a user. Deployments without
// tiered SLAs use DefaultTierResolver.
type TierResolver interface {
	TierFor(ctx context.Context, userID string) (string, error)
}

// DefaultTierResolver is a TierResolver that always returns "default".
type DefaultTierResolver struct{}

// TierFor implements TierResolver.
func (DefaultTierResolver) TierFor(_ context.Context, _ string) (string, error) {
	return "default", nil
}

// HibernateSchedulerOption configures a HibernateScheduler.
type HibernateSchedulerOption func(*HibernateScheduler)

// WithHibernatePolicy overrides the default hibernate policy.
func WithHibernatePolicy(p HibernatePolicy) HibernateSchedulerOption {
	return func(s *HibernateScheduler) { s.policy = p }
}

// WithTierResolver supplies a custom TierResolver.
func WithTierResolver(r TierResolver) HibernateSchedulerOption {
	return func(s *HibernateScheduler) { s.tierResolver = r }
}

// WithSchedulerClock supplies a deterministic clock for tests.
func WithSchedulerClock(clock func() time.Time) HibernateSchedulerOption {
	return func(s *HibernateScheduler) { s.clock = clock }
}

// HibernateScheduler is the periodic loop that hibernates idle sandboxes.
//
// Lifecycle: Start() launches the background goroutine; Stop() cancels it.
// Both methods are idempotent.
type HibernateScheduler struct {
	pool         *SandboxPool
	policy       HibernatePolicy
	tierResolver TierResolver
	clock        func() time.Time

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// NewHibernateScheduler constructs a scheduler with the given pool.
func NewHibernateScheduler(pool *SandboxPool, opts ...HibernateSchedulerOption) *HibernateScheduler {
	s := &HibernateScheduler{
		pool:         pool,
		policy:       DefaultHibernatePolicy(),
		tierResolver: DefaultTierResolver{},
		clock:        time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Policy returns the active policy.
func (s *HibernateScheduler) Policy() HibernatePolicy { return s.policy }

// Start launches the background scan loop. Idempotent.
func (s *HibernateScheduler) Start() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	done := make(chan struct{})
	s.done = done
	s.mu.Unlock()

	go s.run(ctx, done)
}

// Stop cancels the loop and waits for it to exit. Idempotent.
func (s *HibernateScheduler) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	cancel := s.cancel
	done := s.done
	s.cancel = nil
	s.done = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// ScanOnce runs a single scan pass and returns the list of hibernated
// sandbox ids. Tests call this directly with a fast-forward clock.
func (s *HibernateScheduler) ScanOnce(ctx context.Context) []string {
	now := s.clock()
	hibernated := []string{}
	for _, item := range s.pool.ActiveItems() {
		if s.pool.IsPinned(item.SandboxID) {
			continue
		}
		tier, err := s.tierResolver.TierFor(ctx, item.UserID)
		if err != nil {
			tier = ""
		}
		threshold := s.policy.ThresholdFor(tier)
		idle := now.Sub(item.LastActive)
		if idle < threshold {
			continue
		}
		if err := s.pool.Hibernate(ctx, item.SandboxID); err != nil {
			log.Printf("hibernate scheduler: failed to stop sandbox=%s: %v", item.SandboxID, err)
			continue
		}
		hibernated = append(hibernated, item.SandboxID)
		log.Printf("hibernated idle sandbox user=%s id=%s idle=%.1fs tier=%s",
			item.UserID, item.SandboxID, idle.Seconds(), tier)
	}
	return hibernated
}

func (s *HibernateScheduler) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	interval := s.policy.ScanIntervalS
	if interval <= 0 {
		interval = DefaultScanIntervalS
	}
	for {
		s.ScanOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			// next iteration
		}
	}
}
