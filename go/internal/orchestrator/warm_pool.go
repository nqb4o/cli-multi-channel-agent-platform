// Warm-pool manager.
//
// Keeps the top-N most-active users' sandboxes resumed so they avoid
// cold-start. Top-N is recomputed every refresh_interval (default 1h)
// from a TopActiveUsersSource.
package orchestrator

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// F15 brief defaults.
const (
	DefaultTopN              = 100
	DefaultWarmWindow        = 7 * 24 * time.Hour
	DefaultRefreshInterval   = 1 * time.Hour
)

// TopActiveUsersSource returns user ids ordered by recent activity.
//
// The full F12 RunsRepoP covers many concerns; the warm-pool needs only
// this one method.
type TopActiveUsersSource interface {
	TopActiveUsers(ctx context.Context, since time.Time, limit int) ([]string, error)
}

// WarmPoolStatus is a point-in-time snapshot exposed via the admin API.
type WarmPoolStatus struct {
	TopN                   int        `json:"top_n"`
	WindowSeconds          int        `json:"window_seconds"`
	RefreshIntervalSeconds int        `json:"refresh_interval_seconds"`
	PinnedUserIDs          []string   `json:"pinned_user_ids"`
	LastRefreshAt          *time.Time `json:"last_refresh_at"`
	LastRefreshSize        int        `json:"last_refresh_size"`
	LastError              string     `json:"last_error,omitempty"`
}

// WarmPoolOption configures a WarmPoolManager.
type WarmPoolOption func(*WarmPoolManager)

// WithWarmPoolTopN overrides the top-N count.
func WithWarmPoolTopN(n int) WarmPoolOption {
	return func(w *WarmPoolManager) { w.topN = n }
}

// WithWarmPoolWindow overrides the activity window.
func WithWarmPoolWindow(d time.Duration) WarmPoolOption {
	return func(w *WarmPoolManager) { w.window = d }
}

// WithWarmPoolRefreshInterval overrides the periodic refresh interval.
func WithWarmPoolRefreshInterval(d time.Duration) WarmPoolOption {
	return func(w *WarmPoolManager) { w.refreshInterval = d }
}

// WarmPoolManager recomputes top-N from a TopActiveUsersSource and
// pins/unpins SandboxPool entries accordingly.
type WarmPoolManager struct {
	pool            *SandboxPool
	source          TopActiveUsersSource
	topN            int
	window          time.Duration
	refreshInterval time.Duration

	mu              sync.Mutex
	current         map[string]string // user_id -> sandbox_id
	lastRefreshAt   *time.Time
	lastRefreshSize int
	lastError       string

	runMu   sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// NewWarmPoolManager constructs a warm-pool wired to source.
func NewWarmPoolManager(pool *SandboxPool, source TopActiveUsersSource, opts ...WarmPoolOption) *WarmPoolManager {
	w := &WarmPoolManager{
		pool:            pool,
		source:          source,
		topN:            DefaultTopN,
		window:          DefaultWarmWindow,
		refreshInterval: DefaultRefreshInterval,
		current:         map[string]string{},
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// TopN returns the configured top-N.
func (w *WarmPoolManager) TopN() int { return w.topN }

// Window returns the activity window.
func (w *WarmPoolManager) Window() time.Duration { return w.window }

// RefreshInterval returns the periodic refresh cadence.
func (w *WarmPoolManager) RefreshInterval() time.Duration { return w.refreshInterval }

// PinnedUsers returns a snapshot of user_id -> sandbox_id for the current
// pinned set.
func (w *WarmPoolManager) PinnedUsers() map[string]string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]string, len(w.current))
	for k, v := range w.current {
		out[k] = v
	}
	return out
}

// Status returns the current snapshot.
func (w *WarmPoolManager) Status() WarmPoolStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.statusLocked()
}

func (w *WarmPoolManager) statusLocked() WarmPoolStatus {
	users := make([]string, 0, len(w.current))
	for u := range w.current {
		users = append(users, u)
	}
	sort.Strings(users)
	var lastRefresh *time.Time
	if w.lastRefreshAt != nil {
		t := *w.lastRefreshAt
		lastRefresh = &t
	}
	return WarmPoolStatus{
		TopN:                   w.topN,
		WindowSeconds:          int(w.window.Seconds()),
		RefreshIntervalSeconds: int(w.refreshInterval.Seconds()),
		PinnedUserIDs:          users,
		LastRefreshAt:          lastRefresh,
		LastRefreshSize:        w.lastRefreshSize,
		LastError:              w.lastError,
	}
}

// Refresh recomputes the top-N: prewarm new entries, hibernate evictees.
func (w *WarmPoolManager) Refresh(ctx context.Context) (WarmPoolStatus, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	since := time.Now().UTC().Add(-w.window)
	topUsers, err := w.source.TopActiveUsers(ctx, since, w.topN)
	if err != nil {
		w.lastError = fmt.Sprintf("%T: %v", err, err)
		log.Printf("warm-pool refresh: source query failed: %v", err)
		return w.statusLocked(), err
	}

	newSet := make(map[string]struct{}, len(topUsers))
	for _, u := range topUsers {
		newSet[u] = struct{}{}
	}

	// Determine adds + carries.
	toAdd := []string{}
	for _, u := range topUsers {
		if _, ok := w.current[u]; !ok {
			toAdd = append(toAdd, u)
		}
	}
	toRemove := []string{}
	for u := range w.current {
		if _, ok := newSet[u]; !ok {
			toRemove = append(toRemove, u)
		}
	}

	newCurrent := make(map[string]string, len(topUsers))

	// Prewarm new users.
	for _, userID := range toAdd {
		sandbox, err := w.pool.GetOrResume(ctx, userID)
		if err != nil {
			log.Printf("warm-pool: prewarm failed user=%s: %v", userID, err)
			continue
		}
		w.pool.Pin(sandbox.ID)
		newCurrent[userID] = sandbox.ID
	}

	// Carry still-pinned users.
	for _, userID := range topUsers {
		if sid, ok := w.current[userID]; ok {
			newCurrent[userID] = sid
		}
	}

	// Evict users that fell out.
	for _, userID := range toRemove {
		sandboxID, ok := w.current[userID]
		if !ok {
			continue
		}
		w.pool.Unpin(sandboxID)
		if err := w.pool.Hibernate(ctx, sandboxID); err != nil {
			log.Printf("warm-pool: hibernate-on-evict failed sandbox=%s: %v", sandboxID, err)
		}
	}

	w.current = newCurrent
	now := time.Now().UTC()
	w.lastRefreshAt = &now
	w.lastRefreshSize = len(w.current)
	w.lastError = ""
	log.Printf("warm-pool refresh: pinned=%d added=%d removed=%d",
		len(w.current), len(toAdd), len(toRemove))
	return w.statusLocked(), nil
}

// Start launches the background refresh goroutine. Idempotent.
func (w *WarmPoolManager) Start() {
	w.runMu.Lock()
	if w.running {
		w.runMu.Unlock()
		return
	}
	w.running = true
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	done := make(chan struct{})
	w.done = done
	w.runMu.Unlock()

	go w.run(ctx, done)
}

// Stop cancels the refresh loop. Idempotent.
func (w *WarmPoolManager) Stop() {
	w.runMu.Lock()
	if !w.running {
		w.runMu.Unlock()
		return
	}
	w.running = false
	cancel := w.cancel
	done := w.done
	w.cancel = nil
	w.done = nil
	w.runMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (w *WarmPoolManager) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	for {
		_, _ = w.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.refreshInterval):
		}
	}
}
