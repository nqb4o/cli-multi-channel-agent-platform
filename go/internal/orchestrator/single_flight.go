// Generic single-flight by key.
//
// When N goroutines call Do(key, fn) concurrently with the same key, only
// the first runs fn; the rest wait on the same future and observe the
// same result (or error). Once the in-flight call settles, the slot is
// freed. Mirrors golang.org/x/sync/singleflight semantics; we avoid the
// dep so we can use generics over the result type.
package orchestrator

import "sync"

// SingleFlight deduplicates concurrent calls keyed by string.
//
// Used by SandboxPool to deduplicate get_or_resume calls when many
// messages for the same hibernated user arrive at once (F15 acceptance
// criterion #5: 10 concurrent calls = exactly 1 resume).
type SingleFlight[T any] struct {
	mu       sync.Mutex
	inflight map[string]*sfCall[T]
}

type sfCall[T any] struct {
	wg     sync.WaitGroup
	result T
	err    error
}

// NewSingleFlight constructs a fresh SingleFlight.
func NewSingleFlight[T any]() *SingleFlight[T] {
	return &SingleFlight[T]{inflight: map[string]*sfCall[T]{}}
}

// Do runs fn if no other call for key is in flight; otherwise waits for
// the in-flight result. On error, the slot is freed AND the error
// propagates to every waiter.
func (s *SingleFlight[T]) Do(key string, fn func() (T, error)) (T, error) {
	s.mu.Lock()
	if s.inflight == nil {
		s.inflight = map[string]*sfCall[T]{}
	}
	if c, ok := s.inflight[key]; ok {
		s.mu.Unlock()
		c.wg.Wait()
		return c.result, c.err
	}
	c := &sfCall[T]{}
	c.wg.Add(1)
	s.inflight[key] = c
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.inflight, key)
		s.mu.Unlock()
		c.wg.Done()
	}()

	c.result, c.err = fn()
	return c.result, c.err
}

// InFlight reports whether a call for key is currently running.
func (s *SingleFlight[T]) InFlight(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.inflight[key]
	return ok
}

// Keys returns a snapshot of keys currently in flight.
func (s *SingleFlight[T]) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.inflight))
	for k := range s.inflight {
		out = append(out, k)
	}
	return out
}
