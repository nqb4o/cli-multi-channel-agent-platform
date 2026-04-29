// Sandbox pool: active-set tracking, single-flight resume, LRU eviction.
//
// Sits on top of Orchestrator (F01). The Orchestrator owns lifecycle CRUD;
// the pool layers in:
//
//   - Single-flight resume: N concurrent GetOrResume calls for the same
//     hibernated user issue exactly one start_sandbox.
//   - LRU active-set tracking with a configurable capacity (default 100).
//     When at capacity the LRU non-pinned sandbox is hibernated.
//   - Last-active timestamps used by HibernateScheduler.
package orchestrator

import (
	"container/list"
	"context"
	"log"
	"sort"
	"sync"
	"time"
)

// DefaultPoolCapacity matches the Python brief: "default 100".
const DefaultPoolCapacity = 100

// PoolOption configures a SandboxPool.
type PoolOption func(*SandboxPool)

// WithPoolCapacity overrides the active-set capacity.
func WithPoolCapacity(n int) PoolOption {
	return func(p *SandboxPool) { p.capacity = n }
}

// WithPoolClock injects a deterministic clock; tests use this to
// fast-forward time without sleeping.
func WithPoolClock(clock func() time.Time) PoolOption {
	return func(p *SandboxPool) { p.clock = clock }
}

// SandboxPool is the in-memory active-set + single-flight wrapper around
// Orchestrator.
type SandboxPool struct {
	orch     *Orchestrator
	capacity int
	clock    func() time.Time

	mu         sync.Mutex
	active     *list.List               // FIFO LRU; front = LRU, back = MRU. Each elem stores activeEntry.
	byUser     map[string]*list.Element // user_id -> element in active.
	lastActive map[string]time.Time     // sandbox_id -> last_active_at.
	pinned     map[string]struct{}      // sandbox_id set.
	sf         *SingleFlight[*Sandbox]
}

type activeEntry struct {
	userID    string
	sandboxID string
}

// NewSandboxPool constructs a pool wrapping the given Orchestrator.
func NewSandboxPool(orch *Orchestrator, opts ...PoolOption) *SandboxPool {
	p := &SandboxPool{
		orch:       orch,
		capacity:   DefaultPoolCapacity,
		clock:      time.Now,
		active:     list.New(),
		byUser:     map[string]*list.Element{},
		lastActive: map[string]time.Time{},
		pinned:     map[string]struct{}{},
		sf:         NewSingleFlight[*Sandbox](),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Orchestrator returns the underlying orchestrator.
func (p *SandboxPool) Orchestrator() *Orchestrator { return p.orch }

// Capacity returns the configured active-set capacity.
func (p *SandboxPool) Capacity() int { return p.capacity }

// GetOrResume is single-flight resume + LRU touch.
//
// N concurrent calls for the same user_id issue exactly one underlying
// Orchestrator.GetOrResume.
func (p *SandboxPool) GetOrResume(ctx context.Context, userID string) (*Sandbox, error) {
	key := "resume:" + userID
	return p.sf.Do(key, func() (*Sandbox, error) {
		sandbox, err := p.orch.GetOrResume(ctx, userID)
		if err != nil {
			return nil, err
		}
		p.touchInternal(userID, sandbox.ID)
		p.maybeEvict(ctx)
		return sandbox, nil
	})
}

// MaybeEvict triggers an eviction pass; safe to call externally for tests.
func (p *SandboxPool) MaybeEvict(ctx context.Context) { p.maybeEvict(ctx) }

// maybeEvict hibernates the LRU non-pinned sandbox if at capacity.
func (p *SandboxPool) maybeEvict(ctx context.Context) {
	p.mu.Lock()
	if p.active.Len() <= p.capacity {
		p.mu.Unlock()
		return
	}
	var victim *list.Element
	for e := p.active.Front(); e != nil; e = e.Next() {
		entry := e.Value.(activeEntry)
		if _, pinned := p.pinned[entry.sandboxID]; pinned {
			continue
		}
		victim = e
		break
	}
	if victim == nil {
		log.Printf("pool.maybe_evict: at capacity (%d) but every sandbox is pinned", p.capacity)
		p.mu.Unlock()
		return
	}
	victimEntry := victim.Value.(activeEntry)
	p.active.Remove(victim)
	delete(p.byUser, victimEntry.userID)
	delete(p.lastActive, victimEntry.sandboxID)
	p.mu.Unlock()

	if err := p.orch.Hibernate(ctx, victimEntry.sandboxID); err != nil {
		log.Printf("pool.evict failed sandbox=%s: %v", victimEntry.sandboxID, err)
		return
	}
	log.Printf("pool.evicted user=%s sandbox=%s (lru, capacity=%d)",
		victimEntry.userID, victimEntry.sandboxID, p.capacity)
}

// Pin marks a sandbox as protected from LRU eviction. Used by the warm pool.
func (p *SandboxPool) Pin(sandboxID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pinned[sandboxID] = struct{}{}
}

// Unpin removes the pin (no-op if missing).
func (p *SandboxPool) Unpin(sandboxID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pinned, sandboxID)
}

// IsPinned reports whether a sandbox is currently pinned.
func (p *SandboxPool) IsPinned(sandboxID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.pinned[sandboxID]
	return ok
}

// Touch is a public hook for adapters to mark a sandbox active without
// going through GetOrResume.
func (p *SandboxPool) Touch(sandboxID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastActive[sandboxID] = p.clock()
	for uid, elem := range p.byUser {
		if elem.Value.(activeEntry).sandboxID == sandboxID {
			p.active.MoveToBack(elem)
			// Refresh entry value (in case caller updated mapping).
			elem.Value = activeEntry{userID: uid, sandboxID: sandboxID}
			break
		}
	}
}

// LastActive returns the recorded last_active_at for sandboxID. Zero
// means "unknown".
func (p *SandboxPool) LastActive(sandboxID string) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastActive[sandboxID]
}

// ActiveCount returns the number of sandboxes currently in the active set.
func (p *SandboxPool) ActiveCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active.Len()
}

// ActiveItem represents one row of ActiveItems output.
type ActiveItem struct {
	UserID     string
	SandboxID  string
	LastActive time.Time
}

// ActiveItems returns a snapshot of (user_id, sandbox_id, last_active_at)
// in LRU-first order.
func (p *SandboxPool) ActiveItems() []ActiveItem {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ActiveItem, 0, p.active.Len())
	for e := p.active.Front(); e != nil; e = e.Next() {
		entry := e.Value.(activeEntry)
		out = append(out, ActiveItem{
			UserID:     entry.userID,
			SandboxID:  entry.sandboxID,
			LastActive: p.lastActive[entry.sandboxID],
		})
	}
	return out
}

// Remove drops a user from the active set (e.g. after explicit hibernate).
func (p *SandboxPool) Remove(userID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if elem, ok := p.byUser[userID]; ok {
		entry := elem.Value.(activeEntry)
		p.active.Remove(elem)
		delete(p.byUser, userID)
		delete(p.lastActive, entry.sandboxID)
	}
}

// Hibernate hibernates a sandbox via the Orchestrator and removes it from
// the active set. Pinned sandboxes are NOT skipped here — pinning only
// affects MaybeEvict.
func (p *SandboxPool) Hibernate(ctx context.Context, sandboxID string) error {
	if err := p.orch.Hibernate(ctx, sandboxID); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for uid, elem := range p.byUser {
		if elem.Value.(activeEntry).sandboxID == sandboxID {
			p.active.Remove(elem)
			delete(p.byUser, uid)
			break
		}
	}
	delete(p.lastActive, sandboxID)
	return nil
}

// touchInternal marks a (user, sandbox) pair as MRU + records last_active.
func (p *SandboxPool) touchInternal(userID, sandboxID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	p.lastActive[sandboxID] = now
	if elem, ok := p.byUser[userID]; ok {
		elem.Value = activeEntry{userID: userID, sandboxID: sandboxID}
		p.active.MoveToBack(elem)
		return
	}
	elem := p.active.PushBack(activeEntry{userID: userID, sandboxID: sandboxID})
	p.byUser[userID] = elem
}

// LruCandidates returns items sorted ascending by LastActive (oldest first).
func LruCandidates(items []ActiveItem) []ActiveItem {
	out := make([]ActiveItem, len(items))
	copy(out, items)
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastActive.Before(out[j].LastActive)
	})
	return out
}
