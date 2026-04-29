package gateway

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/openclaw/agent-platform/internal/gateway/channels"
)

// ChannelRegistry holds the process-wide map of registered channel adapters.
// Adapters call Register at startup; webhook routes call Get to dispatch.
//
// Mirrors the Python channel_registry module's threaded dict.
type ChannelRegistry struct {
	mu       sync.RWMutex
	adapters map[string]channels.ChannelAdapter
}

// NewChannelRegistry constructs an empty registry. Most callers should use the
// process-wide DefaultChannelRegistry instead.
func NewChannelRegistry() *ChannelRegistry {
	return &ChannelRegistry{adapters: map[string]channels.ChannelAdapter{}}
}

// Register adds an adapter under channelType. Returns an error if channelType
// is empty or disagrees with adapter.Type() (defensive — prevents routing
// surprises later).
func (r *ChannelRegistry) Register(channelType string, adapter channels.ChannelAdapter) error {
	if channelType == "" {
		return errors.New("channelType must be non-empty")
	}
	if adapter == nil {
		return errors.New("adapter must be non-nil")
	}
	if adapter.Type() != channelType {
		return fmt.Errorf("adapter.Type()=%q does not match channelType=%q",
			adapter.Type(), channelType)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[channelType] = adapter
	return nil
}

// Get returns the adapter for channelType, or nil if unregistered.
func (r *ChannelRegistry) Get(channelType string) channels.ChannelAdapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.adapters[channelType]
}

// List returns the sorted list of registered channel types.
func (r *ChannelRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.adapters))
	for k := range r.adapters {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Clear wipes the registry. Test helper — not used in production paths.
func (r *ChannelRegistry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters = map[string]channels.ChannelAdapter{}
}

// ---------------------------------------------------------------------------
// Process-wide singleton (mirrors Python module-level _registry).
// ---------------------------------------------------------------------------

var (
	defaultChannelRegistry   *ChannelRegistry
	defaultChannelRegistryMu sync.Mutex
)

// DefaultChannelRegistry returns the process-wide registry, lazily allocated.
func DefaultChannelRegistry() *ChannelRegistry {
	defaultChannelRegistryMu.Lock()
	defer defaultChannelRegistryMu.Unlock()
	if defaultChannelRegistry == nil {
		defaultChannelRegistry = NewChannelRegistry()
	}
	return defaultChannelRegistry
}

// RegisterChannel is a package-level shortcut for DefaultChannelRegistry().Register.
func RegisterChannel(channelType string, adapter channels.ChannelAdapter) error {
	return DefaultChannelRegistry().Register(channelType, adapter)
}

// ClearChannelRegistry wipes the process-wide registry (test helper).
func ClearChannelRegistry() {
	DefaultChannelRegistry().Clear()
}
