package clibackend

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrUnknownBackend is returned by [BackendRegistry.Get] when no backend
// matches the requested id.
var ErrUnknownBackend = errors.New("clibackend: unknown backend id")

// ErrDuplicateBackend is returned by [BackendRegistry.Register] when a
// backend with the same id is already registered.
var ErrDuplicateBackend = errors.New("clibackend: duplicate backend id")

// Factory builds a backend on demand. Used by lazy registration so a
// missing CLI module (e.g. Codex on a Claude-only deployment) does not
// block other backends — mirrors the Python try/except wrap pattern.
type Factory func() (CliBackend, error)

// BackendRegistry is a thread-safe map of provider id → CliBackend.
//
// Two registration modes:
//
//   - [Register] — eager: pass a fully-built backend.
//   - [RegisterLazy] — defer construction until first [Get]. The factory
//     runs once; the result is cached. Errors from the factory surface on
//     [Get] (and the registration is dropped so a future call retries).
//
// The Python tree's BackendRegistry collapses these two modes into a
// single dict; we keep the same semantics with a single internal entry
// type and an instance/factory union.
type BackendRegistry struct {
	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	instance CliBackend
	factory  Factory
}

// NewBackendRegistry builds an empty registry.
func NewBackendRegistry() *BackendRegistry {
	return &BackendRegistry{entries: make(map[string]*entry)}
}

// Register inserts an already-constructed backend. Returns
// ErrDuplicateBackend if id is already taken.
func (r *BackendRegistry) Register(b CliBackend) error {
	if b == nil {
		return errors.New("clibackend: cannot register nil backend")
	}
	id := b.ID()
	if id == "" {
		return errors.New("clibackend: backend ID() must not be empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[id]; ok {
		return fmt.Errorf("%w: %q", ErrDuplicateBackend, id)
	}
	r.entries[id] = &entry{instance: b}
	return nil
}

// RegisterLazy inserts a factory. The factory is invoked at most once,
// the first time [Get] resolves the id.
func (r *BackendRegistry) RegisterLazy(id string, f Factory) error {
	if id == "" {
		return errors.New("clibackend: cannot register lazy backend with empty id")
	}
	if f == nil {
		return errors.New("clibackend: nil factory")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[id]; ok {
		return fmt.Errorf("%w: %q", ErrDuplicateBackend, id)
	}
	r.entries[id] = &entry{factory: f}
	return nil
}

// Get resolves a backend by id. Lazy entries are constructed on demand;
// if the factory errors, the entry is removed so the next Get retries.
func (r *BackendRegistry) Get(id string) (CliBackend, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownBackend, id)
	}
	if e.instance != nil {
		return e.instance, nil
	}
	b, err := e.factory()
	if err != nil {
		// Drop the broken factory so a subsequent call retries.
		delete(r.entries, id)
		return nil, fmt.Errorf("clibackend: factory for %q failed: %w", id, err)
	}
	if b == nil || b.ID() == "" {
		delete(r.entries, id)
		return nil, fmt.Errorf("clibackend: factory for %q returned invalid backend", id)
	}
	e.instance = b
	return b, nil
}

// IDs returns all registered ids (lazy + eager) in lexical order.
func (r *BackendRegistry) IDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.entries))
	for id := range r.entries {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Has reports whether id is registered (lazy or eager).
func (r *BackendRegistry) Has(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.entries[id]
	return ok
}

// Unregister drops a backend by id. Returns false if id was not present.
// Provided for tests; production code does not unregister.
func (r *BackendRegistry) Unregister(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[id]; !ok {
		return false
	}
	delete(r.entries, id)
	return true
}
