package orchestrator

import (
	"sync"
	"time"
)

// FakeClock is a deterministic monotonic clock for tests.
//
// Tests advance the clock with Tick; the value returned by Now() is what
// the pool / scheduler reads.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFakeClock builds a clock starting at year 2000.
func NewFakeClock() *FakeClock {
	return &FakeClock{
		now: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// Now returns the current fake time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Tick advances the clock by d.
func (c *FakeClock) Tick(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newTestOrchestrator builds an Orchestrator with the FakeDaytonaClient.
func newTestOrchestrator() (*Orchestrator, *FakeDaytonaClient) {
	fake := NewFakeDaytonaClient()
	orch := NewOrchestrator(fake, "ubuntu:24.04", WithAutoStopIntervalM(5))
	return orch, fake
}

// newTestPool wires Orchestrator + FakeClock + SandboxPool.
func newTestPool(capacity int) (*SandboxPool, *Orchestrator, *FakeDaytonaClient, *FakeClock) {
	orch, fake := newTestOrchestrator()
	clock := NewFakeClock()
	pool := NewSandboxPool(orch, WithPoolCapacity(capacity), WithPoolClock(clock.Now))
	return pool, orch, fake, clock
}
