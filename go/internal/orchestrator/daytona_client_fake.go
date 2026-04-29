// FakeDaytonaClient — in-memory stand-in for the Daytona REST surface.
//
// Used by:
//
//   - unit tests
//   - local dev when DAYTONA_API_KEY is unset (see cmd/orchestrator).
//
// Behaviour is deliberately minimalistic: state transitions are
// deterministic, FindByLabel does a linear scan, and there is no
// persistence across instances.
package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// FakeDaytonaClient is the in-memory implementation of DaytonaClient.
type FakeDaytonaClient struct {
	mu             sync.Mutex
	sandboxes      map[string]*RawSandbox
	volumes        map[string]string
	healthOK       bool
	resumeDelay    time.Duration
	deadSandboxes  map[string]struct{}
	StartCalls     map[string]int
	StopCalls      map[string]int
	DeleteCalls    map[string]int
	CreateCalls    int
}

// NewFakeDaytonaClient constructs an empty fake client.
func NewFakeDaytonaClient() *FakeDaytonaClient {
	return &FakeDaytonaClient{
		sandboxes:     map[string]*RawSandbox{},
		volumes:       map[string]string{},
		healthOK:      true,
		deadSandboxes: map[string]struct{}{},
		StartCalls:    map[string]int{},
		StopCalls:     map[string]int{},
		DeleteCalls:   map[string]int{},
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CreateSandbox implements DaytonaClient.
func (f *FakeDaytonaClient) CreateSandbox(ctx context.Context, p CreateSandboxParams) (*RawSandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "sb-" + randHex(5)
	labels := make(map[string]string, len(p.Labels))
	for k, v := range p.Labels {
		labels[k] = v
	}
	raw := &RawSandbox{ID: id, State: "running", Labels: labels}
	f.sandboxes[id] = raw
	f.CreateCalls++
	return raw, nil
}

// GetSandbox implements DaytonaClient.
func (f *FakeDaytonaClient) GetSandbox(ctx context.Context, sandboxID string) (*RawSandbox, error) {
	f.mu.Lock()
	raw, ok := f.sandboxes[sandboxID]
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("sandbox not found: %s", sandboxID)
	}
	return raw, nil
}

// FindByLabel implements DaytonaClient.
func (f *FakeDaytonaClient) FindByLabel(ctx context.Context, key, value string) (*RawSandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, raw := range f.sandboxes {
		if raw.Labels[key] == value && raw.State != "destroyed" {
			return raw, nil
		}
	}
	return nil, nil
}

// StartSandbox implements DaytonaClient.
func (f *FakeDaytonaClient) StartSandbox(ctx context.Context, sandboxID string) (*RawSandbox, error) {
	if f.resumeDelay > 0 {
		select {
		case <-time.After(f.resumeDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.sandboxes[sandboxID]
	if !ok {
		return nil, fmt.Errorf("sandbox not found: %s", sandboxID)
	}
	updated := &RawSandbox{ID: raw.ID, State: "running", Labels: copyLabels(raw.Labels)}
	f.sandboxes[sandboxID] = updated
	f.StartCalls[sandboxID]++
	return updated, nil
}

// StopSandbox implements DaytonaClient.
func (f *FakeDaytonaClient) StopSandbox(ctx context.Context, sandboxID string) (*RawSandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.sandboxes[sandboxID]
	if !ok {
		return nil, fmt.Errorf("sandbox not found: %s", sandboxID)
	}
	updated := &RawSandbox{ID: raw.ID, State: "hibernated", Labels: copyLabels(raw.Labels)}
	f.sandboxes[sandboxID] = updated
	f.StopCalls[sandboxID]++
	return updated, nil
}

// DeleteSandbox implements DaytonaClient.
func (f *FakeDaytonaClient) DeleteSandbox(ctx context.Context, sandboxID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.sandboxes[sandboxID]
	if !ok {
		return nil
	}
	f.sandboxes[sandboxID] = &RawSandbox{ID: raw.ID, State: "destroyed", Labels: copyLabels(raw.Labels)}
	f.DeleteCalls[sandboxID]++
	delete(f.deadSandboxes, sandboxID)
	return nil
}

// GetOrCreateVolume implements DaytonaClient.
func (f *FakeDaytonaClient) GetOrCreateVolume(ctx context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.volumes[name]; ok {
		return id, nil
	}
	id := "vol-" + randHex(4)
	f.volumes[name] = id
	return id, nil
}

// ExecCommand implements DaytonaClient.
func (f *FakeDaytonaClient) ExecCommand(ctx context.Context, sandboxID string, command []string, p ExecParams) (*RawExecResult, error) {
	f.mu.Lock()
	_, ok := f.sandboxes[sandboxID]
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("sandbox not found: %s", sandboxID)
	}
	zero := 0
	return &RawExecResult{ExitCode: &zero}, nil
}

// Healthz implements DaytonaClient.
func (f *FakeDaytonaClient) Healthz(ctx context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.healthOK
}

// PingHealth implements HealthPinger. Returns false for sandboxes the
// test has marked dead via MarkDead.
func (f *FakeDaytonaClient) PingHealth(ctx context.Context, sandboxID string, timeout time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dead := f.deadSandboxes[sandboxID]; dead {
		return false
	}
	raw, ok := f.sandboxes[sandboxID]
	return ok && raw.State == "running"
}

// MarkDead marks a sandbox as unhealthy from PingHealth's perspective.
func (f *FakeDaytonaClient) MarkDead(sandboxID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadSandboxes[sandboxID] = struct{}{}
}

// MarkAlive removes a sandbox from the dead set.
func (f *FakeDaytonaClient) MarkAlive(sandboxID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.deadSandboxes, sandboxID)
}

// SetResumeDelay sets an artificial latency on StartSandbox to allow
// pool single-flight tests to interleave callers.
func (f *FakeDaytonaClient) SetResumeDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeDelay = d
}

// SetHealthOK toggles the health probe success.
func (f *FakeDaytonaClient) SetHealthOK(ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.healthOK = ok
}

// VolumeNames returns a snapshot of the registered volume names.
func (f *FakeDaytonaClient) VolumeNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.volumes))
	for n := range f.volumes {
		out = append(out, n)
	}
	return out
}

func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Compile-time assertions.
var (
	_ DaytonaClient = (*FakeDaytonaClient)(nil)
	_ HealthPinger  = (*FakeDaytonaClient)(nil)
)
