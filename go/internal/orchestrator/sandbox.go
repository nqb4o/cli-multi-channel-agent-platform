// Orchestrator: the public face of F01.
//
// Other features (F02 CLI backends, F05 ADK runtime, F06 gateway) consume
// only this file's surface. Internally everything delegates to a
// DaytonaClient (live or fake).
//
// State persistence
// -----------------
// F12 (persistence) eventually owns the user_id -> sandbox_id mapping and
// last_active_at timestamps. Until then, we keep an in-memory stub. The
// SandboxStore interface is the contract F12 satisfies — there is no
// direct DB access from this file.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// SandboxState is the four-state enum the platform exposes externally.
type SandboxState string

const (
	StateProvisioning SandboxState = "provisioning"
	StateRunning      SandboxState = "running"
	StateHibernated   SandboxState = "hibernated"
	StateDestroyed    SandboxState = "destroyed"
)

// userLabel is stamped on every sandbox so get_or_resume can recover
// from a missing store entry by scanning provider labels.
const userLabel = "platform.user_id"

// Sandbox is the platform's view of a per-user sandbox.
//
// Other features may import this struct but must NOT mutate it — the
// orchestrator keeps the source of truth and refreshes the value on every
// state-changing call.
type Sandbox struct {
	ID     string
	UserID string
	State  SandboxState
	Labels map[string]string
}

// SandboxStore is the persistence boundary F12 will satisfy. The
// orchestrator looks up "does this user already have a sandbox?" via
// FindByUser. The in-memory stub below is fine for unit tests + local dev.
type SandboxStore interface {
	FindByUser(ctx context.Context, userID string) (string, error)
	Upsert(ctx context.Context, userID, sandboxID string) error
	Touch(ctx context.Context, sandboxID string) error
	Remove(ctx context.Context, sandboxID string) error
}

// inMemorySandboxStore is the default SandboxStore used until F12 lands.
type inMemorySandboxStore struct {
	mu            sync.Mutex
	userToSandbox map[string]string
	lastActive    map[string]time.Time
}

// NewInMemorySandboxStore constructs an empty in-memory store.
func NewInMemorySandboxStore() SandboxStore {
	return &inMemorySandboxStore{
		userToSandbox: map[string]string{},
		lastActive:    map[string]time.Time{},
	}
}

func (s *inMemorySandboxStore) FindByUser(_ context.Context, userID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.userToSandbox[userID], nil
}

func (s *inMemorySandboxStore) Upsert(_ context.Context, userID, sandboxID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userToSandbox[userID] = sandboxID
	s.lastActive[sandboxID] = time.Now()
	return nil
}

func (s *inMemorySandboxStore) Touch(_ context.Context, sandboxID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastActive[sandboxID] = time.Now()
	return nil
}

func (s *inMemorySandboxStore) Remove(_ context.Context, sandboxID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.lastActive, sandboxID)
	for uid, sid := range s.userToSandbox {
		if sid == sandboxID {
			delete(s.userToSandbox, uid)
		}
	}
	return nil
}

// LastActive returns the recorded last-active timestamp for sandboxID
// (mostly used by tests to assert touch behaviour). Zero means unknown.
func (s *inMemorySandboxStore) LastActive(sandboxID string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastActive[sandboxID]
}

// ---------------------------------------------------------------------------
// Orchestrator
// ---------------------------------------------------------------------------

// OrchestratorOption configures the Orchestrator constructor.
type OrchestratorOption func(*Orchestrator)

// WithSandboxStore overrides the default in-memory SandboxStore.
func WithSandboxStore(s SandboxStore) OrchestratorOption {
	return func(o *Orchestrator) { o.store = s }
}

// WithDaemonSpawner sets the DaemonSpawner used by StartDaemon.
func WithDaemonSpawner(d DaemonSpawner) OrchestratorOption {
	return func(o *Orchestrator) { o.daemonSpawner = d }
}

// WithAutoStopIntervalM overrides the default 5-minute auto-stop interval.
func WithAutoStopIntervalM(m int) OrchestratorOption {
	return func(o *Orchestrator) { o.autoStopM = m }
}

// Orchestrator is the high-level sandbox CRUD + exec + daemon RPC entry
// point. All methods are safe for concurrent callers.
type Orchestrator struct {
	client        DaytonaClient
	image         string
	autoStopM     int
	store         SandboxStore
	daemonSpawner DaemonSpawner
}

// NewOrchestrator constructs a fresh Orchestrator.
func NewOrchestrator(client DaytonaClient, image string, opts ...OrchestratorOption) *Orchestrator {
	o := &Orchestrator{
		client:    client,
		image:     image,
		autoStopM: 5,
		store:     NewInMemorySandboxStore(),
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// Client returns the underlying DaytonaClient. Mostly used by API.healthz.
func (o *Orchestrator) Client() DaytonaClient { return o.client }

// Store returns the SandboxStore in use.
func (o *Orchestrator) Store() SandboxStore { return o.store }

// Create provisions a brand-new sandbox for userID.
//
// Each user gets their own copy of the four standard volumes (codex,
// gemini, claude, workspace). Volume IDs are stable across destroy/
// recreate so auth + workspace data survive sandbox replacement.
func (o *Orchestrator) Create(ctx context.Context, userID string) (*Sandbox, error) {
	if userID == "" {
		return nil, errors.New("user_id is required")
	}

	// 1. Resolve / create per-user volumes.
	volumeMounts := make([]RawVolumeMount, 0, len(UserVolumeSpecs))
	for _, spec := range UserVolumeSpecs {
		volName, err := VolumeNameFor(userID, spec.LogicalName)
		if err != nil {
			return nil, err
		}
		volID, err := o.client.GetOrCreateVolume(ctx, volName)
		if err != nil {
			return nil, fmt.Errorf("get or create volume %s: %w", volName, err)
		}
		volumeMounts = append(volumeMounts, RawVolumeMount{
			VolumeID:  volID,
			MountPath: spec.MountPath,
		})
	}

	// 2. Create the sandbox itself. Retry up to 6 times when Daytona reports
	// a volume is not yet ready (newly created volumes take a few seconds).
	labels := map[string]string{userLabel: userID}
	name := fmt.Sprintf("u-%s-%s", userID, shortHex(uuid.NewString(), 6))
	params := CreateSandboxParams{
		Name:              name,
		Image:             o.image,
		Labels:            labels,
		Volumes:           volumeMounts,
		AutoStopIntervalM: o.autoStopM,
	}
	var raw *RawSandbox
	var createErr error
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt*5) * time.Second):
			}
		}
		raw, createErr = o.client.CreateSandbox(ctx, params)
		if createErr == nil {
			break
		}
		if !strings.Contains(createErr.Error(), "not in a ready state") {
			return nil, fmt.Errorf("create sandbox: %w", createErr)
		}
	}
	if createErr != nil {
		return nil, fmt.Errorf("create sandbox: %w", createErr)
	}
	_ = raw // used below

	if err := o.store.Upsert(ctx, userID, raw.ID); err != nil {
		return nil, fmt.Errorf("store upsert: %w", err)
	}
	return o.wrap(raw, userID), nil
}

// GetOrResume returns a running sandbox for userID — resume if hibernated,
// create if missing.
//
// Lookup order:
//  1. Store (fast path).
//  2. Provider label search (recover from a stale store entry).
//  3. Create a new sandbox.
func (o *Orchestrator) GetOrResume(ctx context.Context, userID string) (*Sandbox, error) {
	if userID == "" {
		return nil, errors.New("user_id is required")
	}
	sandboxID, _ := o.store.FindByUser(ctx, userID)
	var raw *RawSandbox
	if sandboxID != "" {
		got, err := o.client.GetSandbox(ctx, sandboxID)
		if err == nil {
			raw = got
		}
	}
	if raw == nil {
		// Recover from a stale store entry by searching provider labels.
		got, err := o.client.FindByLabel(ctx, userLabel, userID)
		if err == nil && got != nil {
			raw = got
			_ = o.store.Upsert(ctx, userID, got.ID)
		}
	}
	if raw == nil {
		return o.Create(ctx, userID)
	}
	if raw.State == string(StateDestroyed) {
		return o.Create(ctx, userID)
	}
	if raw.State == string(StateHibernated) {
		started, err := o.client.StartSandbox(ctx, raw.ID)
		if err != nil {
			return nil, fmt.Errorf("start sandbox: %w", err)
		}
		raw = started
	}
	_ = o.store.Touch(ctx, raw.ID)
	return o.wrap(raw, userID), nil
}

// Hibernate stops the sandbox so Daytona can hibernate it. Idempotent on
// already-hibernated; rejects destroyed.
func (o *Orchestrator) Hibernate(ctx context.Context, sandboxID string) error {
	raw, err := o.client.GetSandbox(ctx, sandboxID)
	if err != nil {
		return err
	}
	if raw.State == string(StateHibernated) {
		return nil
	}
	if raw.State == string(StateDestroyed) {
		return fmt.Errorf("cannot hibernate destroyed sandbox %s", sandboxID)
	}
	if _, err := o.client.StopSandbox(ctx, sandboxID); err != nil {
		return err
	}
	return nil
}

// Destroy tears down the sandbox. Idempotent. Volumes survive — they're
// keyed on user_id and outlive any single sandbox.
func (o *Orchestrator) Destroy(ctx context.Context, sandboxID string) error {
	if err := o.client.DeleteSandbox(ctx, sandboxID); err != nil {
		return err
	}
	return o.store.Remove(ctx, sandboxID)
}

// Get looks up a sandbox by id.
func (o *Orchestrator) Get(ctx context.Context, sandboxID string) (*Sandbox, error) {
	raw, err := o.client.GetSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	userID := raw.Labels[userLabel]
	return o.wrap(raw, userID), nil
}

// Exec runs a one-shot command inside the sandbox. For long-running
// processes use StartDaemon instead.
func (o *Orchestrator) Exec(ctx context.Context, sandboxID string, cmd []string, stdin []byte, env map[string]string, timeoutS int) (*ExecResult, error) {
	if len(cmd) == 0 {
		return nil, errors.New("cmd must be non-empty")
	}
	raw, err := o.client.ExecCommand(ctx, sandboxID, cmd, ExecParams{
		Env:      env,
		Stdin:    stdin,
		TimeoutS: timeoutS,
	})
	if err != nil {
		return nil, err
	}
	_ = o.store.Touch(ctx, sandboxID)
	return &ExecResult{
		Stdout:   raw.Stdout,
		Stderr:   raw.Stderr,
		ExitCode: raw.ExitCode,
		TimedOut: raw.TimedOut,
	}, nil
}

// StartDaemon spawns a long-running process and returns a JSON-RPC handle.
// Requires WithDaemonSpawner to have been provided.
func (o *Orchestrator) StartDaemon(ctx context.Context, sandboxID string, cmd []string, env map[string]string) (*DaemonHandle, error) {
	if o.daemonSpawner == nil {
		return nil, errors.New("no DaemonSpawner configured. Wire one via WithDaemonSpawner(...)")
	}
	transport, err := o.daemonSpawner.Spawn(ctx, sandboxID, cmd, env)
	if err != nil {
		return nil, fmt.Errorf("spawn: %w", err)
	}
	daemonID := "daemon-" + shortHex(uuid.NewString(), 12)
	spawner := o.daemonSpawner
	onStop := func(ctx context.Context) error {
		return spawner.Stop(ctx, sandboxID, daemonID)
	}
	handle := NewDaemonHandle(daemonID, sandboxID, transport, onStop)
	_ = o.store.Touch(ctx, sandboxID)
	return handle, nil
}

func (o *Orchestrator) wrap(raw *RawSandbox, userID string) *Sandbox {
	uid := userID
	if uid == "" {
		uid = raw.Labels[userLabel]
	}
	labels := make(map[string]string, len(raw.Labels))
	for k, v := range raw.Labels {
		labels[k] = v
	}
	return &Sandbox{
		ID:     raw.ID,
		UserID: uid,
		State:  SandboxState(raw.State),
		Labels: labels,
	}
}

// shortHex strips the dashes from a uuid string and returns the first n
// hex chars — used for compact sandbox names.
func shortHex(s string, n int) string {
	clean := strings.ReplaceAll(s, "-", "")
	if len(clean) >= n {
		return clean[:n]
	}
	return clean
}
