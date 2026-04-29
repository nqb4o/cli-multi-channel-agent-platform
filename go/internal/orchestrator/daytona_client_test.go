package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// State normalization
// ---------------------------------------------------------------------------

func TestStateTableCoversEveryDocumentedState(t *testing.T) {
	expected := map[string]string{
		"creating":      "provisioning",
		"pending_build": "provisioning",
		"starting":      "provisioning",
		"started":       "running",
		"stopping":      "running",
		"stopped":       "hibernated",
		"archiving":     "hibernated",
		"archived":      "hibernated",
		"restoring":     "provisioning",
		"destroyed":     "destroyed",
		"destroying":    "destroyed",
		"error":         "destroyed",
		"build_failed":  "destroyed",
		"unknown":       "destroyed",
	}
	for k, want := range expected {
		got := daytonaToOurState[k]
		if got != want {
			t.Errorf("state[%q] = %q, want %q", k, got, want)
		}
	}
}

func TestNormalizeStateHandlesEnumStringification(t *testing.T) {
	if NormalizeState("SandboxState.STARTED") != "running" {
		t.Errorf("STARTED enum")
	}
	if NormalizeState("SandboxState.STOPPED") != "hibernated" {
		t.Errorf("STOPPED enum")
	}
	if NormalizeState("SandboxState.ARCHIVED") != "hibernated" {
		t.Errorf("ARCHIVED enum")
	}
}

func TestNormalizeStateLowercasesInput(t *testing.T) {
	if NormalizeState("STARTED") != "running" {
		t.Error("STARTED")
	}
	if NormalizeState("Stopped") != "hibernated" {
		t.Error("Stopped")
	}
}

func TestNormalizeStateUnknownIsDestroyed(t *testing.T) {
	if NormalizeState("flying-saucer") != "destroyed" {
		t.Error("unknown should normalize to destroyed")
	}
	if NormalizeState("") != "destroyed" {
		t.Error("empty should normalize to destroyed")
	}
}

// ---------------------------------------------------------------------------
// Raw struct shapes
// ---------------------------------------------------------------------------

func TestRawSandboxFields(t *testing.T) {
	r := RawSandbox{ID: "sb-1", State: "running", Labels: map[string]string{"k": "v"}}
	if r.ID != "sb-1" || r.State != "running" || r.Labels["k"] != "v" {
		t.Errorf("unexpected: %+v", r)
	}
}

func TestRawExecResultDefaultTimedOut(t *testing.T) {
	zero := 0
	r := RawExecResult{ExitCode: &zero}
	if r.TimedOut {
		t.Error("default timed_out should be false")
	}
}

func TestRawVolumeMountFields(t *testing.T) {
	vm := RawVolumeMount{VolumeID: "vol-x", MountPath: "/home/user/.codex"}
	if vm.VolumeID != "vol-x" || vm.MountPath != "/home/user/.codex" {
		t.Errorf("unexpected: %+v", vm)
	}
}

// ---------------------------------------------------------------------------
// LiveDaytonaClient — driven against an httptest server.
// ---------------------------------------------------------------------------

func TestLiveCreateSandbox(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/sandboxes" {
			_ = json.NewDecoder(r.Body).Decode(&captured)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     "sb-1",
				"state":  "started",
				"labels": map[string]string{"platform.user_id": "alice"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := NewLiveDaytonaClient("test-key", srv.URL, "")
	raw, err := client.CreateSandbox(context.Background(), CreateSandboxParams{
		Name:              "my-box",
		Image:             "ubuntu:24.04",
		Env:               map[string]string{"FOO": "bar"},
		Labels:            map[string]string{"platform.user_id": "alice"},
		Volumes:           []RawVolumeMount{{VolumeID: "v1", MountPath: "/home/user/.codex"}},
		AutoStopIntervalM: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if raw.State != "running" {
		t.Errorf("state = %s", raw.State)
	}
	if captured["name"] != "my-box" {
		t.Errorf("name = %v", captured["name"])
	}
	if captured["image"] != "ubuntu:24.04" {
		t.Errorf("image = %v", captured["image"])
	}
	if captured["auto_stop_interval"].(float64) != 5 {
		t.Errorf("auto_stop_interval = %v", captured["auto_stop_interval"])
	}
	vols, _ := captured["volumes"].([]any)
	if len(vols) != 1 {
		t.Fatalf("volumes = %v", vols)
	}
	vm := vols[0].(map[string]any)
	if vm["volume_id"] != "v1" || vm["mount_path"] != "/home/user/.codex" {
		t.Errorf("volume mount: %v", vm)
	}
}

func TestLiveGetSandboxNormalizesState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "sb-1",
			"state":  "archived",
			"labels": map[string]string{"k": "v"},
		})
	}))
	defer srv.Close()
	client := NewLiveDaytonaClient("k", srv.URL, "")
	raw, err := client.GetSandbox(context.Background(), "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	if raw.State != "hibernated" {
		t.Errorf("state = %s", raw.State)
	}
	if raw.Labels["k"] != "v" {
		t.Errorf("labels = %v", raw.Labels)
	}
}

func TestLiveFindByLabelReturnsFirst(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"id": "sb-1", "state": "started", "labels": map[string]string{"platform.user_id": "alice"}},
				{"id": "sb-2", "state": "started", "labels": map[string]string{"platform.user_id": "bob"}},
			},
		})
	}))
	defer srv.Close()
	client := NewLiveDaytonaClient("k", srv.URL, "")
	raw, err := client.FindByLabel(context.Background(), "platform.user_id", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if raw == nil || raw.ID != "sb-1" {
		t.Errorf("got %+v", raw)
	}
}

func TestLiveFindByLabelNoneOnEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	}))
	defer srv.Close()
	client := NewLiveDaytonaClient("k", srv.URL, "")
	raw, err := client.FindByLabel(context.Background(), "platform.user_id", "nope")
	if err != nil {
		t.Fatal(err)
	}
	if raw != nil {
		t.Errorf("expected nil, got %+v", raw)
	}
}

func TestLiveStartStopDelete(t *testing.T) {
	calls := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"id":"sb-1","state":"started","labels":{}}`))
		case strings.HasSuffix(r.URL.Path, "/stop"):
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"id":"sb-1","state":"stopped","labels":{}}`))
		case r.Method == "GET":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"id":"sb-1","state":"started","labels":{}}`))
		default:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	client := NewLiveDaytonaClient("k", srv.URL, "")

	started, err := client.StartSandbox(context.Background(), "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	if started.State != "running" {
		t.Errorf("started.state = %s", started.State)
	}

	stopped, err := client.StopSandbox(context.Background(), "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	_ = stopped

	if err := client.DeleteSandbox(context.Background(), "sb-1"); err != nil {
		t.Fatal(err)
	}
	hasDelete := false
	for _, c := range calls {
		if strings.HasPrefix(c, "DELETE ") {
			hasDelete = true
		}
	}
	if !hasDelete {
		t.Errorf("expected DELETE call, got %v", calls)
	}
}

func TestLiveExecCommand(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result":    "ok\n",
			"exit_code": 0,
		})
	}))
	defer srv.Close()
	client := NewLiveDaytonaClient("k", srv.URL, "")

	res, err := client.ExecCommand(context.Background(), "sb-1", []string{"echo", "hi there"}, ExecParams{
		Env:      map[string]string{"X": "1"},
		TimeoutS: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit_code = %v", res.ExitCode)
	}
	if string(res.Stdout) != "ok\n" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	cmd, _ := capturedBody["command"].(string)
	if cmd != "echo 'hi there'" {
		t.Errorf("cmd = %q", cmd)
	}
}

func TestLiveExecCommandPipesStdin(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"result": "", "exit_code": 0})
	}))
	defer srv.Close()
	client := NewLiveDaytonaClient("k", srv.URL, "")

	_, err := client.ExecCommand(context.Background(), "sb-1",
		[]string{"python", "-c", "import sys; print(sys.stdin.read())"},
		ExecParams{Stdin: []byte("hello stdin")})
	if err != nil {
		t.Fatal(err)
	}
	cmd, _ := capturedBody["command"].(string)
	if !strings.HasPrefix(cmd, "echo '") {
		t.Errorf("cmd = %q", cmd)
	}
	if !strings.Contains(cmd, "base64 -d |") {
		t.Errorf("cmd = %q (no base64 -d)", cmd)
	}
	if !strings.Contains(cmd, "python -c") {
		t.Errorf("cmd = %q (no python)", cmd)
	}
}

func TestLiveGetOrCreateVolume(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "vol-1"})
	}))
	defer srv.Close()
	client := NewLiveDaytonaClient("k", srv.URL, "")
	id, err := client.GetOrCreateVolume(context.Background(), "u-alice-codex-auth")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "vol-") {
		t.Errorf("id = %q", id)
	}
}

func TestLiveHealthzOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	}))
	defer srv.Close()
	client := NewLiveDaytonaClient("k", srv.URL, "")
	if !client.Healthz(context.Background()) {
		t.Error("expected healthz=true")
	}
}

func TestLiveHealthzFalseOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	client := NewLiveDaytonaClient("k", srv.URL, "")
	if client.Healthz(context.Background()) {
		t.Error("expected healthz=false")
	}
}

// ---------------------------------------------------------------------------
// FakeDaytonaClient round-trip
// ---------------------------------------------------------------------------

func TestFakeCreateGetStartStopDelete(t *testing.T) {
	fake := NewFakeDaytonaClient()
	ctx := context.Background()
	raw, err := fake.CreateSandbox(ctx, CreateSandboxParams{
		Name:   "x",
		Image:  "ubuntu",
		Labels: map[string]string{"platform.user_id": "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if raw.State != "running" {
		t.Errorf("state = %s", raw.State)
	}

	got, err := fake.GetSandbox(ctx, raw.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != raw.ID {
		t.Errorf("id mismatch")
	}

	stopped, _ := fake.StopSandbox(ctx, raw.ID)
	if stopped.State != "hibernated" {
		t.Errorf("after stop = %s", stopped.State)
	}

	started, _ := fake.StartSandbox(ctx, raw.ID)
	if started.State != "running" {
		t.Errorf("after start = %s", started.State)
	}

	if err := fake.DeleteSandbox(ctx, raw.ID); err != nil {
		t.Fatal(err)
	}
}

func TestFakeFindByLabel(t *testing.T) {
	fake := NewFakeDaytonaClient()
	ctx := context.Background()
	a, _ := fake.CreateSandbox(ctx, CreateSandboxParams{Labels: map[string]string{"platform.user_id": "alice"}})
	b, _ := fake.CreateSandbox(ctx, CreateSandboxParams{Labels: map[string]string{"platform.user_id": "bob"}})
	got, err := fake.FindByLabel(ctx, "platform.user_id", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != a.ID {
		t.Errorf("got %+v want id=%s", got, a.ID)
	}
	if got != nil && got.ID == b.ID {
		t.Error("returned wrong sandbox")
	}

	none, _ := fake.FindByLabel(ctx, "platform.user_id", "missing")
	if none != nil {
		t.Errorf("expected nil, got %+v", none)
	}
}

func TestFakeHealthzAndPingHealth(t *testing.T) {
	fake := NewFakeDaytonaClient()
	ctx := context.Background()
	if !fake.Healthz(ctx) {
		t.Error("expected healthy")
	}
	fake.SetHealthOK(false)
	if fake.Healthz(ctx) {
		t.Error("expected unhealthy after toggle")
	}

	fake.SetHealthOK(true)
	raw, _ := fake.CreateSandbox(ctx, CreateSandboxParams{})
	if !fake.PingHealth(ctx, raw.ID, 0) {
		t.Error("expected ping ok")
	}
	fake.MarkDead(raw.ID)
	if fake.PingHealth(ctx, raw.ID, 0) {
		t.Error("expected ping false after MarkDead")
	}
	fake.MarkAlive(raw.ID)
	if !fake.PingHealth(ctx, raw.ID, 0) {
		t.Error("expected ping ok after MarkAlive")
	}
}

func TestFakeExecCommandReturnsZero(t *testing.T) {
	fake := NewFakeDaytonaClient()
	ctx := context.Background()
	raw, _ := fake.CreateSandbox(ctx, CreateSandboxParams{})
	res, err := fake.ExecCommand(ctx, raw.ID, []string{"true"}, ExecParams{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit_code = %v", res.ExitCode)
	}
}

func TestFakeGetOrCreateVolumeStable(t *testing.T) {
	fake := NewFakeDaytonaClient()
	ctx := context.Background()
	a, _ := fake.GetOrCreateVolume(ctx, "v1")
	b, _ := fake.GetOrCreateVolume(ctx, "v1")
	if a != b {
		t.Errorf("expected stable id, got %q vs %q", a, b)
	}
}
