// Periodic health-probe loop.
//
// Per F15 brief: "every 30s, ping the runtime daemon's health RPC for
// active sandboxes; flag stuck ones for force-recreate".
//
// Failure model:
//   1. Each scan, ping every active sandbox via the configured HealthProber.
//   2. On failure, increment a per-sandbox failure counter.
//   3. After failure_grace consecutive failures (default 3), force-recreate:
//      destroy followed by GetOrResume for the same user.
//   4. Successful probes reset the counter.
package orchestrator

import (
	"context"
	"log"
	"sync"
	"time"
)

// Defaults match the F15 brief.
const (
	DefaultProbeIntervalS = 30 * time.Second
	DefaultProbeTimeoutS  = 5 * time.Second
	DefaultFailureGrace   = 3
)

// HealthProber probes a single sandbox's runtime daemon.
type HealthProber interface {
	PingHealth(ctx context.Context, sandboxID string, timeout time.Duration) bool
}

// HealthProbeStats reports cumulative probe activity.
type HealthProbeStats struct {
	Probes              int
	Failures            int
	Recreates           int
	ConsecutiveFailures map[string]int
}

// HealthProbeOption configures a HealthProbeLoop.
type HealthProbeOption func(*HealthProbeLoop)

// WithProbeInterval overrides the scan cadence.
func WithProbeInterval(d time.Duration) HealthProbeOption {
	return func(l *HealthProbeLoop) { l.interval = d }
}

// WithProbeTimeout overrides the per-probe timeout.
func WithProbeTimeout(d time.Duration) HealthProbeOption {
	return func(l *HealthProbeLoop) { l.timeout = d }
}

// WithFailureGrace sets the consecutive-failure threshold for recreate.
func WithFailureGrace(n int) HealthProbeOption {
	return func(l *HealthProbeLoop) { l.failureGrace = n }
}

// HealthProbeLoop is the background task that pings active sandboxes and
// force-recreates stuck ones.
type HealthProbeLoop struct {
	pool         *SandboxPool
	prober       HealthProber
	interval     time.Duration
	timeout      time.Duration
	failureGrace int

	mu                 sync.Mutex
	stats              HealthProbeStats
	consecutiveFailures map[string]int

	runMu   sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// NewHealthProbeLoop constructs a probe loop.
func NewHealthProbeLoop(pool *SandboxPool, prober HealthProber, opts ...HealthProbeOption) *HealthProbeLoop {
	l := &HealthProbeLoop{
		pool:                pool,
		prober:              prober,
		interval:            DefaultProbeIntervalS,
		timeout:             DefaultProbeTimeoutS,
		failureGrace:        DefaultFailureGrace,
		consecutiveFailures: map[string]int{},
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// ProbeInterval returns the configured scan cadence.
func (l *HealthProbeLoop) ProbeInterval() time.Duration { return l.interval }

// FailureGrace returns the failure-grace count.
func (l *HealthProbeLoop) FailureGrace() int { return l.failureGrace }

// Stats returns a snapshot of cumulative probe activity.
func (l *HealthProbeLoop) Stats() HealthProbeStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	cf := make(map[string]int, len(l.consecutiveFailures))
	for k, v := range l.consecutiveFailures {
		cf[k] = v
	}
	return HealthProbeStats{
		Probes:              l.stats.Probes,
		Failures:            l.stats.Failures,
		Recreates:           l.stats.Recreates,
		ConsecutiveFailures: cf,
	}
}

// Start launches the periodic loop. Idempotent.
func (l *HealthProbeLoop) Start() {
	l.runMu.Lock()
	if l.running {
		l.runMu.Unlock()
		return
	}
	l.running = true
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	done := make(chan struct{})
	l.done = done
	l.runMu.Unlock()
	go l.run(ctx, done)
}

// Stop cancels the loop. Idempotent.
func (l *HealthProbeLoop) Stop() {
	l.runMu.Lock()
	if !l.running {
		l.runMu.Unlock()
		return
	}
	l.running = false
	cancel := l.cancel
	done := l.done
	l.cancel = nil
	l.done = nil
	l.runMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// ScanOnce probes every active sandbox. Returns ids of sandboxes recreated
// this pass.
func (l *HealthProbeLoop) ScanOnce(ctx context.Context) []string {
	recreated := []string{}
	for _, item := range l.pool.ActiveItems() {
		// Run probe with timeout — defensive in case the prober blocks.
		probeCtx, cancel := context.WithTimeout(ctx, l.timeout+time.Second)
		ok := l.prober.PingHealth(probeCtx, item.SandboxID, l.timeout)
		cancel()

		l.mu.Lock()
		l.stats.Probes++
		if ok {
			delete(l.consecutiveFailures, item.SandboxID)
			l.mu.Unlock()
			continue
		}
		count := l.consecutiveFailures[item.SandboxID] + 1
		l.consecutiveFailures[item.SandboxID] = count
		l.stats.Failures++
		shouldRecreate := count >= l.failureGrace
		l.mu.Unlock()

		if !shouldRecreate {
			continue
		}
		log.Printf("health-probe: force-recreating sandbox=%s after %d consecutive failures",
			item.SandboxID, count)
		if err := l.forceRecreate(ctx, item.UserID, item.SandboxID); err != nil {
			log.Printf("health-probe: force-recreate failed user=%s sandbox=%s: %v",
				item.UserID, item.SandboxID, err)
			continue
		}
		recreated = append(recreated, item.SandboxID)
		l.mu.Lock()
		l.stats.Recreates++
		delete(l.consecutiveFailures, item.SandboxID)
		l.mu.Unlock()
	}
	return recreated
}

func (l *HealthProbeLoop) forceRecreate(ctx context.Context, userID, sandboxID string) error {
	// Drop from the active set so subsequent scans skip during the
	// destroy/create window.
	l.pool.Remove(userID)
	if err := l.pool.Orchestrator().Destroy(ctx, sandboxID); err != nil {
		log.Printf("health-probe: destroy failed sandbox=%s: %v — proceeding to resume", sandboxID, err)
	}
	if _, err := l.pool.GetOrResume(ctx, userID); err != nil {
		return err
	}
	return nil
}

func (l *HealthProbeLoop) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	for {
		l.ScanOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(l.interval):
		}
	}
}
