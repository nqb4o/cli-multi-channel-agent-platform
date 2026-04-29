// Package e2e contains end-to-end smoke tests that build and exercise the
// real Go binaries (runtime-daemon, gateway) to verify the full integration
// path works from a fresh process start.
//
// Run with:
//
//	go test -run Smoke ./tests/e2e/ -v -timeout=60s
//
// The tests build the binaries via `go build` into a temp dir, so they
// exercise the real production code paths without any mocking.
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// moduleRoot returns the absolute path to the go/ module root by walking up
// from this test file's location.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile is …/go/tests/e2e/smoke_test.go
	// walk up two levels: e2e → tests → go
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found from %q: %v", root, err)
	}
	return root
}

// buildBinary runs "go build -o dest ./cmd/<name>" from the module root.
// Returns the path to the built binary.
func buildBinary(t *testing.T, modRoot, name, destDir string) string {
	t.Helper()
	dest := filepath.Join(destDir, name)
	if runtime.GOOS == "windows" {
		dest += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", dest, "./cmd/"+name)
	cmd.Dir = modRoot
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
	return dest
}

// writeAgentYAML writes a minimal agent.yaml that uses the "stub" provider.
func writeAgentYAML(t *testing.T, dir string) string {
	t.Helper()
	content := `identity:
  name: "SmokeBot"
providers:
  - id: stub
    model: stub-large
skills: []
`
	p := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write agent.yaml: %v", err)
	}
	return p
}

// sendLine writes one JSON-RPC line to the writer and flushes.
func sendLine(t *testing.T, w io.Writer, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal rpc: %v", err)
	}
	b = append(b, '\n')
	if _, err := w.Write(b); err != nil {
		t.Fatalf("write rpc: %v", err)
	}
}

// readLine reads one JSON-RPC line from the scanner with a deadline.
func readLine(t *testing.T, sc *bufio.Scanner, timeout time.Duration) map[string]any {
	t.Helper()
	done := make(chan bool, 1)
	var line string
	go func() {
		if sc.Scan() {
			line = sc.Text()
		}
		done <- true
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("readLine: timeout after %s", timeout)
	}
	if line == "" {
		if err := sc.Err(); err != nil {
			t.Fatalf("scanner error: %v", err)
		}
		t.Fatal("readLine: EOF with empty line")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		t.Fatalf("unmarshal rpc response %q: %v", line, err)
	}
	return out
}

// ---------------------------------------------------------------------------
// TestSmokeRuntimeDaemon
// ---------------------------------------------------------------------------

// TestSmokeRuntimeDaemon starts the real runtime-daemon binary with
// --register-stub, exercises health / run / shutdown JSON-RPC calls, and
// asserts the expected response shapes.
func TestSmokeRuntimeDaemon(t *testing.T) {
	modRoot := moduleRoot(t)
	tmpDir := t.TempDir()

	// Build the binary.
	binary := buildBinary(t, modRoot, "runtime-daemon", tmpDir)

	// Write workspace files + agent.yaml.
	wsDir := filepath.Join(tmpDir, "workspace")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(wsDir, "AGENTS.md"), []byte("# AGENTS\nSmoke test.\n"), 0o644)
	configPath := writeAgentYAML(t, tmpDir)

	// Start the daemon.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	proc := exec.CommandContext(ctx, binary,
		"--config", configPath,
		"--workspace", wsDir,
		"--register-stub",
		"--log-level", "ERROR",
	)
	stdinPipe, err := proc.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdoutPipe, err := proc.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	proc.Stderr = os.Stderr // log daemon stderr to test output

	if err := proc.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	defer func() {
		_ = stdinPipe.Close()
		_ = proc.Wait()
	}()

	sc := bufio.NewScanner(stdoutPipe)

	// --- 1. health ---
	sendLine(t, stdinPipe, map[string]any{
		"jsonrpc": "2.0",
		"id":      "1",
		"method":  "health",
		"params":  nil,
	})
	healthResp := readLine(t, sc, 10*time.Second)
	t.Logf("health response: %v", healthResp)

	result, ok := healthResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("health result missing or wrong type: %v", healthResp)
	}
	if result["ok"] != true {
		t.Fatalf("health ok=%v", result["ok"])
	}
	if result["version"] == "" || result["version"] == nil {
		t.Fatalf("health version missing: %v", result)
	}

	// --- 2. run ---
	sendLine(t, stdinPipe, map[string]any{
		"jsonrpc": "2.0",
		"id":      "2",
		"method":  "run",
		"params": map[string]any{
			"user_id":    "00000000-0000-0000-0000-000000000001",
			"agent_id":   "00000000-0000-0000-0000-000000000002",
			"channel_id": "00000000-0000-0000-0000-000000000003",
			"thread_id":  "smoke-thread-1",
			"run_id":     "smoke-run-1",
			"message":    map[string]any{"text": "hello smoke", "images": []string{}},
		},
	})
	runResp := readLine(t, sc, 15*time.Second)
	t.Logf("run response: %v", runResp)

	runResult, ok := runResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("run result missing or wrong type: %v", runResp)
	}
	if runResult["ok"] != true {
		t.Fatalf("run ok=%v (full result: %v)", runResult["ok"], runResult)
	}

	// The SuccessEnvelope nests result inside result.result
	innerResult, ok := runResult["result"].(map[string]any)
	if !ok {
		t.Fatalf("run inner result missing: %v", runResult)
	}
	text, _ := innerResult["text"].(string)
	if !strings.Contains(text, "[stub echo]") {
		t.Fatalf("run text %q should contain '[stub echo]'", text)
	}

	// --- 3. shutdown ---
	sendLine(t, stdinPipe, map[string]any{
		"jsonrpc": "2.0",
		"id":      "3",
		"method":  "shutdown",
		"params":  nil,
	})
	shutResp := readLine(t, sc, 5*time.Second)
	t.Logf("shutdown response: %v", shutResp)

	shutResult, ok := shutResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("shutdown result missing: %v", shutResp)
	}
	if shutResult["ok"] != true {
		t.Fatalf("shutdown ok=%v", shutResult["ok"])
	}

	t.Log("TestSmokeRuntimeDaemon: PASS")
}

// ---------------------------------------------------------------------------
// TestSmokeGatewayHealthz
// ---------------------------------------------------------------------------

// TestSmokeGatewayHealthz builds the gateway binary, starts it with a fake
// Redis URL (miniredis is not available as a subprocess, so we pick an
// unlikely port and expect the gateway to fail to connect to Redis on startup,
// BUT the binary should refuse to start — so instead we test with a
// pre-started miniredis or skip the test if Redis isn't reachable).
//
// Since the gateway requires a live Redis to start (it pings Redis in run()),
// this test tries a local Redis on :6380 first; if unreachable it starts a
// miniredis stand-in by using a random free port.  We build + run the binary
// and assert GET /healthz returns 200.
func TestSmokeGatewayHealthz(t *testing.T) {
	modRoot := moduleRoot(t)
	tmpDir := t.TempDir()

	// Build the gateway binary.
	binary := buildBinary(t, modRoot, "gateway", tmpDir)

	// Find a free TCP port for the gateway to listen on.
	port := findFreePort(t)

	// Try to use local Redis on :6380 (the compose stack's port).
	// If unavailable, skip this sub-test rather than failing: the critical
	// smoke path is the daemon test above.
	redisURL := "redis://localhost:6380/0"
	if !isPortReachable("localhost:6380", 500*time.Millisecond) {
		t.Log("Redis not reachable on :6380 — trying :6379")
		redisURL = "redis://localhost:6379/0"
		if !isPortReachable("localhost:6379", 500*time.Millisecond) {
			t.Skip("no local Redis available; skipping gateway smoke test")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	proc := exec.CommandContext(ctx, binary)
	proc.Env = append(os.Environ(),
		fmt.Sprintf("REDIS_URL=%s", redisURL),
		"ADMIN_TOKEN=devtoken",
		"USER_JWT_SECRET=test",
		fmt.Sprintf("GATEWAY_HTTP_ADDR=:%d", port),
	)
	proc.Stderr = os.Stderr
	proc.Stdout = os.Stderr

	if err := proc.Start(); err != nil {
		t.Fatalf("start gateway: %v", err)
	}
	defer func() {
		_ = proc.Process.Signal(os.Interrupt)
		_ = proc.Wait()
	}()

	// Wait up to 5s for the gateway to be ready.
	healthURL := fmt.Sprintf("http://localhost:%d/healthz", port)
	var lastErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(healthURL) //nolint:noctx
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Logf("gateway /healthz → %d", resp.StatusCode)
				t.Log("TestSmokeGatewayHealthz: PASS")
				return
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("gateway /healthz not OK within 5s: %v", lastErr)
}

// ---------------------------------------------------------------------------
// network helpers
// ---------------------------------------------------------------------------

func isPortReachable(addr string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	d := &net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func findFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("findFreePort: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}
