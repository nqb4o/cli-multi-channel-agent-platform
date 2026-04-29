package orchestrator

import (
	"context"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Volume contract — frozen with F02/F03/F04.
// ---------------------------------------------------------------------------

func TestVolumeMountPathsAreFrozen(t *testing.T) {
	if CodexAuthPath != "/home/user/.codex" {
		t.Errorf("CodexAuthPath = %q", CodexAuthPath)
	}
	if GeminiAuthPath != "/home/user/.gemini" {
		t.Errorf("GeminiAuthPath = %q", GeminiAuthPath)
	}
	if ClaudeAuthPath != "/home/user/.claude" {
		t.Errorf("ClaudeAuthPath = %q", ClaudeAuthPath)
	}
	if WorkspacePath != "/home/user/workspace" {
		t.Errorf("WorkspacePath = %q", WorkspacePath)
	}
}

func TestUserVolumeSpecsCoverAllProviders(t *testing.T) {
	paths := map[string]bool{}
	for _, spec := range UserVolumeSpecs {
		paths[spec.MountPath] = true
	}
	want := []string{CodexAuthPath, GeminiAuthPath, ClaudeAuthPath, WorkspacePath}
	for _, p := range want {
		if !paths[p] {
			t.Errorf("missing volume spec for %s", p)
		}
	}
	if len(UserVolumeSpecs) != 4 {
		t.Errorf("len(UserVolumeSpecs) = %d, want 4", len(UserVolumeSpecs))
	}
}

func TestVolumeNameForIsDeterministic(t *testing.T) {
	a, err := VolumeNameFor("alice", "codex-auth")
	if err != nil {
		t.Fatalf("VolumeNameFor: %v", err)
	}
	if a != "u-alice-codex-auth" {
		t.Errorf("got %q, want %q", a, "u-alice-codex-auth")
	}
	b, _ := VolumeNameFor("alice", "workspace")
	if b != "u-alice-workspace" {
		t.Errorf("got %q", b)
	}
	c, _ := VolumeNameFor("alice", "codex-auth")
	if c != a {
		t.Errorf("non-deterministic: %q vs %q", c, a)
	}
}

func TestVolumeNameForRejectsInvalidUserID(t *testing.T) {
	cases := []string{"", "with space", "with/slash", "with:colon"}
	for _, c := range cases {
		if _, err := VolumeNameFor(c, "workspace"); err == nil {
			t.Errorf("expected error for user_id=%q", c)
		}
	}
}

// ---------------------------------------------------------------------------
// Orchestrator.Create
// ---------------------------------------------------------------------------

func TestCreateSandboxProvisionsAllVolumes(t *testing.T) {
	orch, fake := newTestOrchestrator()
	ctx := context.Background()

	sandbox, err := orch.Create(ctx, "alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sandbox.UserID != "alice" {
		t.Errorf("UserID = %q", sandbox.UserID)
	}
	if sandbox.State != StateRunning {
		t.Errorf("State = %s", sandbox.State)
	}
	want := map[string]bool{}
	for _, spec := range UserVolumeSpecs {
		name, _ := VolumeNameFor("alice", spec.LogicalName)
		want[name] = true
	}
	got := map[string]bool{}
	for _, n := range fake.VolumeNames() {
		got[n] = true
	}
	for n := range want {
		if !got[n] {
			t.Errorf("missing volume %q in %v", n, fake.VolumeNames())
		}
	}
}

func TestCreateStampsUserLabel(t *testing.T) {
	orch, fake := newTestOrchestrator()
	ctx := context.Background()
	sandbox, err := orch.Create(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := fake.GetSandbox(ctx, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Labels["platform.user_id"] != "alice" {
		t.Errorf("label not stamped: %v", raw.Labels)
	}
}

func TestCreateRejectsEmptyUserID(t *testing.T) {
	orch, _ := newTestOrchestrator()
	if _, err := orch.Create(context.Background(), ""); err == nil {
		t.Error("expected error for empty user_id")
	}
}

func TestCreateDistinctUsersGetDistinctSandboxes(t *testing.T) {
	orch, _ := newTestOrchestrator()
	ctx := context.Background()
	a, _ := orch.Create(ctx, "alice")
	b, _ := orch.Create(ctx, "bob")
	if a.ID == b.ID {
		t.Errorf("expected distinct ids, got %s == %s", a.ID, b.ID)
	}
	if a.UserID != "alice" || b.UserID != "bob" {
		t.Errorf("user_ids: %s %s", a.UserID, b.UserID)
	}
}

// ---------------------------------------------------------------------------
// Orchestrator.GetOrResume
// ---------------------------------------------------------------------------

func TestGetOrResumeReturnsExistingRunning(t *testing.T) {
	orch, fake := newTestOrchestrator()
	ctx := context.Background()
	first, _ := orch.Create(ctx, "alice")
	second, err := orch.GetOrResume(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Errorf("expected reuse, got %s vs %s", second.ID, first.ID)
	}
	if second.State != StateRunning {
		t.Errorf("state = %s", second.State)
	}
	if fake.CreateCalls != 1 {
		t.Errorf("create_calls = %d", fake.CreateCalls)
	}
}

func TestGetOrResumeCreatesWhenMissing(t *testing.T) {
	orch, fake := newTestOrchestrator()
	ctx := context.Background()
	sandbox, err := orch.GetOrResume(ctx, "brand-new")
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.UserID != "brand-new" {
		t.Errorf("user_id = %s", sandbox.UserID)
	}
	if fake.CreateCalls != 1 {
		t.Errorf("create_calls = %d", fake.CreateCalls)
	}
}

func TestGetOrResumeResumesHibernated(t *testing.T) {
	orch, fake := newTestOrchestrator()
	ctx := context.Background()
	sandbox, _ := orch.Create(ctx, "alice")
	if err := orch.Hibernate(ctx, sandbox.ID); err != nil {
		t.Fatal(err)
	}
	resumed, err := orch.GetOrResume(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ID != sandbox.ID {
		t.Errorf("expected reuse, got %s vs %s", resumed.ID, sandbox.ID)
	}
	if resumed.State != StateRunning {
		t.Errorf("state = %s", resumed.State)
	}
	if fake.StartCalls[sandbox.ID] != 1 {
		t.Errorf("start_calls = %v", fake.StartCalls)
	}
}

func TestGetOrResumeRecoversFromStaleStore(t *testing.T) {
	orch, fake := newTestOrchestrator()
	ctx := context.Background()
	// Create a sandbox out-of-band with the user label.
	raw, _ := fake.CreateSandbox(ctx, CreateSandboxParams{
		Name:   "orphan",
		Image:  "img",
		Labels: map[string]string{"platform.user_id": "alice"},
	})
	sandbox, err := orch.GetOrResume(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.ID != raw.ID {
		t.Errorf("expected label recovery, got %s vs %s", sandbox.ID, raw.ID)
	}
	if sandbox.UserID != "alice" {
		t.Errorf("user_id = %s", sandbox.UserID)
	}
}

func TestGetOrResumeCreatesWhenDestroyed(t *testing.T) {
	orch, _ := newTestOrchestrator()
	ctx := context.Background()
	sandbox, _ := orch.Create(ctx, "alice")
	_ = orch.Destroy(ctx, sandbox.ID)
	fresh, err := orch.GetOrResume(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == sandbox.ID {
		t.Error("expected new sandbox after destroy")
	}
	if fresh.State != StateRunning {
		t.Errorf("state = %s", fresh.State)
	}
}

// ---------------------------------------------------------------------------
// Hibernate / Destroy
// ---------------------------------------------------------------------------

func TestHibernateStopsRunning(t *testing.T) {
	orch, fake := newTestOrchestrator()
	ctx := context.Background()
	sandbox, _ := orch.Create(ctx, "alice")
	if err := orch.Hibernate(ctx, sandbox.ID); err != nil {
		t.Fatal(err)
	}
	raw, _ := fake.GetSandbox(ctx, sandbox.ID)
	if raw.State != "hibernated" {
		t.Errorf("state = %s", raw.State)
	}
}

func TestHibernateIdempotent(t *testing.T) {
	orch, fake := newTestOrchestrator()
	ctx := context.Background()
	sandbox, _ := orch.Create(ctx, "alice")
	_ = orch.Hibernate(ctx, sandbox.ID)
	before := fake.StopCalls[sandbox.ID]
	_ = orch.Hibernate(ctx, sandbox.ID)
	after := fake.StopCalls[sandbox.ID]
	if before != after {
		t.Errorf("expected idempotent, got before=%d after=%d", before, after)
	}
}

func TestHibernateRejectsDestroyed(t *testing.T) {
	orch, _ := newTestOrchestrator()
	ctx := context.Background()
	sandbox, _ := orch.Create(ctx, "alice")
	_ = orch.Destroy(ctx, sandbox.ID)
	err := orch.Hibernate(ctx, sandbox.ID)
	if err == nil || !strings.Contains(err.Error(), "destroyed") {
		t.Errorf("expected destroyed error, got %v", err)
	}
}

func TestDestroyClearsStore(t *testing.T) {
	orch, _ := newTestOrchestrator()
	ctx := context.Background()
	sandbox, _ := orch.Create(ctx, "alice")
	_ = orch.Destroy(ctx, sandbox.ID)
	next, _ := orch.GetOrResume(ctx, "alice")
	if next.ID == sandbox.ID {
		t.Error("expected fresh sandbox after destroy")
	}
}

func TestDestroyIdempotent(t *testing.T) {
	orch, _ := newTestOrchestrator()
	ctx := context.Background()
	sandbox, _ := orch.Create(ctx, "alice")
	_ = orch.Destroy(ctx, sandbox.ID)
	if err := orch.Destroy(ctx, sandbox.ID); err != nil {
		t.Errorf("second destroy: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Exec / StartDaemon guards
// ---------------------------------------------------------------------------

func TestExecRunsCommand(t *testing.T) {
	orch, _ := newTestOrchestrator()
	ctx := context.Background()
	sandbox, _ := orch.Create(ctx, "alice")
	result, err := orch.Exec(ctx, sandbox.ID, []string{"true"}, nil, nil, 60)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Errorf("exit_code = %v", result.ExitCode)
	}
	if result.TimedOut {
		t.Error("timed_out should be false")
	}
}

func TestExecRejectsEmptyCmd(t *testing.T) {
	orch, _ := newTestOrchestrator()
	ctx := context.Background()
	sandbox, _ := orch.Create(ctx, "alice")
	if _, err := orch.Exec(ctx, sandbox.ID, []string{}, nil, nil, 60); err == nil {
		t.Error("expected error for empty cmd")
	}
}

func TestStartDaemonWithoutSpawnerErrors(t *testing.T) {
	orch, _ := newTestOrchestrator()
	ctx := context.Background()
	sandbox, _ := orch.Create(ctx, "alice")
	_, err := orch.StartDaemon(ctx, sandbox.ID, []string{"agent-runtime"}, nil)
	if err == nil || !strings.Contains(err.Error(), "DaemonSpawner") {
		t.Errorf("expected DaemonSpawner error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// inMemorySandboxStore — round-trip
// ---------------------------------------------------------------------------

func TestInMemoryStoreFindByUserRoundTrips(t *testing.T) {
	store := NewInMemorySandboxStore()
	ctx := context.Background()
	id, _ := store.FindByUser(ctx, "alice")
	if id != "" {
		t.Errorf("expected empty, got %q", id)
	}
	_ = store.Upsert(ctx, "alice", "sb-1")
	id, _ = store.FindByUser(ctx, "alice")
	if id != "sb-1" {
		t.Errorf("got %q", id)
	}
}

func TestInMemoryStoreRemoveClearsMapping(t *testing.T) {
	store := NewInMemorySandboxStore()
	ctx := context.Background()
	_ = store.Upsert(ctx, "alice", "sb-1")
	_ = store.Remove(ctx, "sb-1")
	id, _ := store.FindByUser(ctx, "alice")
	if id != "" {
		t.Errorf("expected empty, got %q", id)
	}
}

func TestInMemoryStoreTouchUpdatesLastActive(t *testing.T) {
	s := NewInMemorySandboxStore().(*inMemorySandboxStore)
	ctx := context.Background()
	_ = s.Upsert(ctx, "alice", "sb-1")
	_ = s.Touch(ctx, "sb-1")
	if s.LastActive("sb-1").IsZero() {
		t.Error("expected non-zero last active")
	}
}

// ---------------------------------------------------------------------------
// Get
// ---------------------------------------------------------------------------

func TestGetWrapsRawIntoSandbox(t *testing.T) {
	orch, _ := newTestOrchestrator()
	ctx := context.Background()
	sandbox, _ := orch.Create(ctx, "alice")
	fetched, err := orch.Get(ctx, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fetched.ID != sandbox.ID || fetched.UserID != "alice" || fetched.State != StateRunning {
		t.Errorf("unexpected fetched: %+v", fetched)
	}
}
