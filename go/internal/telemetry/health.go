// Health probe + provider health tracker (Go port of health.py).
//
// Exposed for F16's fallback chain. Three primitives:
//
//   - HealthOutcome (HEALTHY / DEGRADED / UNHEALTHY) — same enum values as
//     the Python implementation, since Grafana dashboards filter on them.
//   - HealthSnapshot — point-in-time view (failure rate + sample size +
//     last-success/failure timestamps).
//   - HealthProbe — the Go interface F16 wires its fallback chain against.
//   - ProviderHealthTracker — concrete probe with a rolling time window.
//   - HealthRegistry — process-wide map provider_id -> HealthProbe.
package telemetry

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// HealthOutcome enumerates the three states. Values match the Python enum
// so cross-language services agree on dashboard filters.
type HealthOutcome string

const (
	HealthOutcomeHealthy   HealthOutcome = "healthy"
	HealthOutcomeDegraded  HealthOutcome = "degraded"
	HealthOutcomeUnhealthy HealthOutcome = "unhealthy"
)

// HealthSnapshot is the wire-shape returned by HealthProbe.Snapshot().
//
// FailureRate is in [0.0, 1.0]. With zero samples the tracker reports
// HealthOutcomeHealthy and FailureRate == 0.0. LastSuccessAt / LastFailureAt
// are wall-clock seconds (Unix epoch) — nil-pointer-equivalent (zero) means
// "no event yet".
type HealthSnapshot struct {
	ProviderID     string
	Outcome        HealthOutcome
	FailureRate    float64
	SampleSize     int
	LastSuccessAt  float64 // 0 means no success recorded yet
	LastFailureAt  float64 // 0 means no failure recorded yet
	WindowSeconds  float64
	HasSuccess     bool
	HasFailure     bool
}

// HealthProbe is the Go interface F16's fallback chain consumes.
// Implementations report which provider they belong to and a current
// snapshot. ProviderID must be unique across the process (the
// HealthRegistry rejects collisions).
type HealthProbe interface {
	ProviderID() string
	Snapshot() HealthSnapshot
}

// ProviderHealthTrackerConfig groups the optional knobs for the tracker.
type ProviderHealthTrackerConfig struct {
	// WindowSeconds is the rolling-window length. Default 60s.
	WindowSeconds float64
	// DegradedThreshold — failure rate at/above which the tracker reports
	// DEGRADED. Default 0.1.
	DegradedThreshold float64
	// UnhealthyThreshold — failure rate at/above which the tracker reports
	// UNHEALTHY (provided MinSamplesForUnhealthy is also met). Default 0.5.
	UnhealthyThreshold float64
	// MinSamplesForUnhealthy — minimum sample count before UNHEALTHY can be
	// reported (so a single failure doesn't take the tracker offline).
	// Default 3.
	MinSamplesForUnhealthy int
	// Clock is the time source. Tests inject a deterministic clock; nil
	// defaults to time.Now().Unix() in float seconds.
	Clock func() float64
}

func (c *ProviderHealthTrackerConfig) defaults() {
	if c.WindowSeconds == 0 {
		c.WindowSeconds = 60.0
	}
	if c.DegradedThreshold == 0 {
		c.DegradedThreshold = 0.1
	}
	if c.UnhealthyThreshold == 0 {
		c.UnhealthyThreshold = 0.5
	}
	if c.MinSamplesForUnhealthy == 0 {
		c.MinSamplesForUnhealthy = 3
	}
	if c.Clock == nil {
		c.Clock = func() float64 {
			t := time.Now()
			return float64(t.Unix()) + float64(t.Nanosecond())/1e9
		}
	}
}

// ProviderHealthTracker is a rolling-window failure-rate tracker for a single
// provider. It implements HealthProbe.
type ProviderHealthTracker struct {
	providerID    string
	cfg           ProviderHealthTrackerConfig
	mu            sync.Mutex
	samples       []sample
	lastSuccessAt float64
	lastFailureAt float64
	hasSuccess    bool
	hasFailure    bool
}

type sample struct {
	at     float64
	failed bool
}

// NewProviderHealthTracker builds a tracker with default thresholds.
func NewProviderHealthTracker(providerID string) (*ProviderHealthTracker, error) {
	return NewProviderHealthTrackerWithConfig(providerID, ProviderHealthTrackerConfig{})
}

// NewProviderHealthTrackerWithConfig builds a tracker with custom thresholds.
func NewProviderHealthTrackerWithConfig(providerID string, cfg ProviderHealthTrackerConfig) (*ProviderHealthTracker, error) {
	cfg.defaults()
	if cfg.WindowSeconds <= 0 {
		return nil, errors.New("WindowSeconds must be > 0")
	}
	if !(cfg.DegradedThreshold >= 0.0 && cfg.DegradedThreshold <= cfg.UnhealthyThreshold && cfg.UnhealthyThreshold <= 1.0) {
		return nil, errors.New("thresholds must satisfy 0 <= degraded <= unhealthy <= 1")
	}
	return &ProviderHealthTracker{
		providerID: providerID,
		cfg:        cfg,
	}, nil
}

// ProviderID implements HealthProbe.
func (t *ProviderHealthTracker) ProviderID() string { return t.providerID }

// RecordSuccess registers a successful invocation.
func (t *ProviderHealthTracker) RecordSuccess() {
	now := t.cfg.Clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.samples = append(t.samples, sample{at: now, failed: false})
	t.lastSuccessAt = now
	t.hasSuccess = true
	t.evictLocked(now)
}

// RecordFailure registers a failed invocation.
func (t *ProviderHealthTracker) RecordFailure() {
	now := t.cfg.Clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.samples = append(t.samples, sample{at: now, failed: true})
	t.lastFailureAt = now
	t.hasFailure = true
	t.evictLocked(now)
}

// Reset drops all samples + last-event timestamps. Tests-friendly.
func (t *ProviderHealthTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.samples = nil
	t.lastSuccessAt = 0
	t.lastFailureAt = 0
	t.hasSuccess = false
	t.hasFailure = false
}

// Snapshot returns the current health view.
func (t *ProviderHealthTracker) Snapshot() HealthSnapshot {
	now := t.cfg.Clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.evictLocked(now)
	sampleSize := len(t.samples)
	failureRate := 0.0
	if sampleSize > 0 {
		failures := 0
		for _, s := range t.samples {
			if s.failed {
				failures++
			}
		}
		failureRate = float64(failures) / float64(sampleSize)
	}
	return HealthSnapshot{
		ProviderID:    t.providerID,
		Outcome:       t.classify(failureRate, sampleSize),
		FailureRate:   failureRate,
		SampleSize:    sampleSize,
		LastSuccessAt: t.lastSuccessAt,
		LastFailureAt: t.lastFailureAt,
		WindowSeconds: t.cfg.WindowSeconds,
		HasSuccess:    t.hasSuccess,
		HasFailure:    t.hasFailure,
	}
}

func (t *ProviderHealthTracker) evictLocked(now float64) {
	cutoff := now - t.cfg.WindowSeconds
	idx := 0
	for idx < len(t.samples) && t.samples[idx].at < cutoff {
		idx++
	}
	if idx > 0 {
		t.samples = append(t.samples[:0], t.samples[idx:]...)
	}
}

func (t *ProviderHealthTracker) classify(failureRate float64, sampleSize int) HealthOutcome {
	if sampleSize == 0 {
		return HealthOutcomeHealthy
	}
	if failureRate >= t.cfg.UnhealthyThreshold && sampleSize >= t.cfg.MinSamplesForUnhealthy {
		return HealthOutcomeUnhealthy
	}
	if failureRate >= t.cfg.DegradedThreshold {
		return HealthOutcomeDegraded
	}
	return HealthOutcomeHealthy
}

// ---------------------------------------------------------------------------
// HealthRegistry
// ---------------------------------------------------------------------------

// HealthRegistry is a process-wide map provider_id -> HealthProbe.
type HealthRegistry struct {
	mu     sync.Mutex
	probes map[string]HealthProbe
}

// NewHealthRegistry constructs an empty registry.
func NewHealthRegistry() *HealthRegistry {
	return &HealthRegistry{probes: map[string]HealthProbe{}}
}

// Register adds probe to the registry. Re-registering the SAME probe object
// is a no-op; registering a different probe with the same provider_id is an
// error.
func (r *HealthRegistry) Register(probe HealthProbe) error {
	if probe.ProviderID() == "" {
		return errors.New("probe.ProviderID must be non-empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.probes[probe.ProviderID()]
	if ok && existing != probe {
		return fmt.Errorf("probe already registered for %q", probe.ProviderID())
	}
	r.probes[probe.ProviderID()] = probe
	return nil
}

// Unregister removes a probe by provider_id (no-op if missing).
func (r *HealthRegistry) Unregister(providerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.probes, providerID)
}

// Get returns the probe for provider_id, or nil if absent.
func (r *HealthRegistry) Get(providerID string) HealthProbe {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.probes[providerID]
}

// Contains reports whether provider_id has a registered probe.
func (r *HealthRegistry) Contains(providerID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.probes[providerID]
	return ok
}

// Len returns the number of registered probes.
func (r *HealthRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.probes)
}

// SnapshotAll returns a snapshot of every probe at the current instant.
func (r *HealthRegistry) SnapshotAll() map[string]HealthSnapshot {
	r.mu.Lock()
	probes := make([]HealthProbe, 0, len(r.probes))
	for _, p := range r.probes {
		probes = append(probes, p)
	}
	r.mu.Unlock()
	out := make(map[string]HealthSnapshot, len(probes))
	for _, p := range probes {
		out[p.ProviderID()] = p.Snapshot()
	}
	return out
}

// ---------------------------------------------------------------------------
// Default registry singleton
// ---------------------------------------------------------------------------

var (
	defaultRegistryOnce sync.Once
	defaultRegistry     *HealthRegistry
	defaultRegistryMu   sync.Mutex
)

// DefaultRegistry lazily creates and returns the process-wide singleton.
func DefaultRegistry() *HealthRegistry {
	defaultRegistryMu.Lock()
	defer defaultRegistryMu.Unlock()
	if defaultRegistry == nil {
		defaultRegistry = NewHealthRegistry()
	}
	return defaultRegistry
}

// ResetDefaultRegistry drops the singleton. Tests only.
func ResetDefaultRegistry() {
	defaultRegistryMu.Lock()
	defer defaultRegistryMu.Unlock()
	defaultRegistry = nil
	defaultRegistryOnce = sync.Once{}
}
