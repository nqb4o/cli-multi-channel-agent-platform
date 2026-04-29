package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// pipeTransport is an in-memory bidirectional byte pipe acting as a
// DaemonTransport. The orchestrator-side write goes onto toDaemon; the
// daemon-side write goes onto fromDaemon.
type pipeTransport struct {
	mu          sync.Mutex
	toDaemon    chan []byte
	fromDaemon  chan []byte
	closed      bool
	hangReadFn  func() ([]byte, error)
}

func newPipeTransport() *pipeTransport {
	return &pipeTransport{
		toDaemon:   make(chan []byte, 8),
		fromDaemon: make(chan []byte, 8),
	}
}

func (t *pipeTransport) Write(_ context.Context, data []byte) error {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return errors.New("transport closed")
	}
	t.toDaemon <- append([]byte(nil), data...)
	return nil
}

func (t *pipeTransport) ReadLine(_ context.Context) ([]byte, error) {
	t.mu.Lock()
	hang := t.hangReadFn
	t.mu.Unlock()
	if hang != nil {
		return hang()
	}
	line, ok := <-t.fromDaemon
	if !ok {
		return nil, errors.New("EOF")
	}
	// Strip trailing newline if present.
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	return line, nil
}

func (t *pipeTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	close(t.fromDaemon)
	close(t.toDaemon)
	return nil
}

func (t *pipeTransport) sendReply(reply map[string]any) {
	b, _ := json.Marshal(reply)
	t.fromDaemon <- append(b, '\n')
}

// ---------------------------------------------------------------------------
// ExecResult shape
// ---------------------------------------------------------------------------

func TestExecResultDefaultTimedOutFalse(t *testing.T) {
	zero := 0
	r := ExecResult{Stdout: []byte("o"), Stderr: []byte("e"), ExitCode: &zero}
	if r.TimedOut {
		t.Error("default timed_out should be false")
	}
}

func TestExecResultFieldsRoundTrip(t *testing.T) {
	code := 42
	r := ExecResult{Stdout: []byte("hello"), Stderr: []byte("oops"), ExitCode: &code, TimedOut: false}
	if string(r.Stdout) != "hello" || string(r.Stderr) != "oops" || *r.ExitCode != 42 {
		t.Errorf("unexpected: %+v", r)
	}
}

func TestExecResultTimedOutMeansNoExitCode(t *testing.T) {
	r := ExecResult{TimedOut: true}
	if r.ExitCode != nil {
		t.Error("expected nil exit_code on timeout")
	}
}

// ---------------------------------------------------------------------------
// DaemonHandle JSON-RPC roundtrip via in-memory echo daemon.
// ---------------------------------------------------------------------------

// runEchoDaemon reads from pipe.toDaemon and replies based on a simple
// echo protocol. Loops until the pipe is closed.
func runEchoDaemon(pipe *pipeTransport) {
	go func() {
		for raw := range pipe.toDaemon {
			var msg map[string]any
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue
			}
			id, _ := msg["id"].(string)
			method, _ := msg["method"].(string)
			params, _ := msg["params"].(map[string]any)

			if method == "boom" {
				pipe.sendReply(map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"error":   map[string]any{"code": -32000, "message": "boom"},
				})
				continue
			}
			var result map[string]any
			if method == "ping" {
				result = map[string]any{"echo": params}
			} else {
				result = map[string]any{"method": method, "params": params}
			}
			pipe.sendReply(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result":  result,
			})
		}
	}()
}

func TestRPCPingRoundTrip(t *testing.T) {
	pipe := newPipeTransport()
	runEchoDaemon(pipe)
	handle := NewDaemonHandle("d-test", "sb-test", pipe, nil)
	defer handle.Stop(context.Background())

	start := time.Now()
	result, err := handle.RPC(context.Background(), "ping", map[string]any{"hello": "world"}, 2*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	echo, ok := result["echo"].(map[string]any)
	if !ok || echo["hello"] != "world" {
		t.Errorf("unexpected result: %v", result)
	}
	if elapsed > time.Second {
		t.Errorf("rpc took %v", elapsed)
	}
}

func TestRPCUnknownMethodReturnsMethodEcho(t *testing.T) {
	pipe := newPipeTransport()
	runEchoDaemon(pipe)
	handle := NewDaemonHandle("d-test", "sb-test", pipe, nil)
	defer handle.Stop(context.Background())

	result, err := handle.RPC(context.Background(), "anything", map[string]any{"k": float64(1)}, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result["method"] != "anything" {
		t.Errorf("unexpected: %v", result)
	}
	params, _ := result["params"].(map[string]any)
	if params["k"] != float64(1) {
		t.Errorf("unexpected params: %v", params)
	}
}

func TestRPCErrorEnvelopeRaises(t *testing.T) {
	pipe := newPipeTransport()
	runEchoDaemon(pipe)
	handle := NewDaemonHandle("d-test", "sb-test", pipe, nil)
	defer handle.Stop(context.Background())

	_, err := handle.RPC(context.Background(), "boom", nil, 2*time.Second)
	if err == nil {
		t.Fatal("expected error")
	}
	rpcErr, ok := err.(*DaemonRpcError)
	if !ok {
		t.Fatalf("expected DaemonRpcError, got %T: %v", err, err)
	}
	if rpcErr.Code != -32000 {
		t.Errorf("code = %d", rpcErr.Code)
	}
	if rpcErr.Message != "boom" {
		t.Errorf("message = %q", rpcErr.Message)
	}
}

func TestRPCConcurrentCallsCorrelate(t *testing.T) {
	pipe := newPipeTransport()
	runEchoDaemon(pipe)
	handle := NewDaemonHandle("d-test", "sb-test", pipe, nil)
	defer handle.Stop(context.Background())

	type res struct {
		i int
		v float64
	}
	out := make(chan res, 3)
	for i := 1; i <= 3; i++ {
		go func(i int) {
			result, err := handle.RPC(context.Background(), "ping", map[string]any{"i": float64(i)}, 2*time.Second)
			if err != nil {
				out <- res{i: i, v: -1}
				return
			}
			echo := result["echo"].(map[string]any)
			out <- res{i: i, v: echo["i"].(float64)}
		}(i)
	}
	got := map[int]float64{}
	for i := 0; i < 3; i++ {
		r := <-out
		got[r.i] = r.v
	}
	for i := 1; i <= 3; i++ {
		if got[i] != float64(i) {
			t.Errorf("expected %d -> %d, got %v", i, i, got[i])
		}
	}
}

func TestRPCTimeoutFires(t *testing.T) {
	pipe := newPipeTransport()
	pipe.mu.Lock()
	pipe.hangReadFn = func() ([]byte, error) {
		select {} // hang forever
	}
	pipe.mu.Unlock()
	handle := NewDaemonHandle("d-test", "sb-test", pipe, nil)
	defer handle.Stop(context.Background())

	_, err := handle.RPC(context.Background(), "ping", nil, 50*time.Millisecond)
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestRPCAfterStopErrors(t *testing.T) {
	pipe := newPipeTransport()
	handle := NewDaemonHandle("d-test", "sb-test", pipe, nil)
	_ = handle.Stop(context.Background())
	_, err := handle.RPC(context.Background(), "ping", nil, time.Second)
	if err == nil {
		t.Error("expected error after stop")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	pipe := newPipeTransport()
	handle := NewDaemonHandle("d-test", "sb-test", pipe, nil)
	if err := handle.Stop(context.Background()); err != nil {
		t.Errorf("first stop: %v", err)
	}
	if err := handle.Stop(context.Background()); err != nil {
		t.Errorf("second stop: %v", err)
	}
}

func TestOnStopCallbackInvoked(t *testing.T) {
	pipe := newPipeTransport()
	called := 0
	onStop := func(_ context.Context) error {
		called++
		return nil
	}
	handle := NewDaemonHandle("d-test", "sb-test", pipe, onStop)
	if err := handle.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Errorf("on_stop called %d times", called)
	}
}

func TestEnvelopeJSONRPCFieldAndID(t *testing.T) {
	pipe := newPipeTransport()
	handle := NewDaemonHandle("d-test", "sb-test", pipe, nil)
	defer handle.Stop(context.Background())

	// Pre-arm a reader: capture the request envelope.
	capCh := make(chan map[string]any, 1)
	go func() {
		raw := <-pipe.toDaemon
		var env map[string]any
		_ = json.Unmarshal(raw, &env)
		capCh <- env
		// Send the ack.
		id, _ := env["id"].(string)
		pipe.sendReply(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result":  map[string]any{"ack": true},
		})
	}()

	result, err := handle.RPC(context.Background(), "ping", nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result["ack"] != true {
		t.Errorf("unexpected: %v", result)
	}

	env := <-capCh
	if env["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v", env["jsonrpc"])
	}
	if env["method"] != "ping" {
		t.Errorf("method = %v", env["method"])
	}
	id, ok := env["id"].(string)
	if !ok || id == "" {
		t.Errorf("id = %v", env["id"])
	}
}

// ---------------------------------------------------------------------------
// LocalSubprocessDaemonSpawner — spawn /bin/cat and write/read via stdio.
// ---------------------------------------------------------------------------

func TestLocalSubprocessDaemonSpawnerCatRoundTrip(t *testing.T) {
	spawner := NewLocalSubprocessDaemonSpawner()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	transport, err := spawner.Spawn(ctx, "sb-1", []string{"/bin/cat"}, nil)
	if err != nil {
		t.Skipf("/bin/cat unavailable: %v", err)
	}
	defer spawner.Stop(context.Background(), "sb-1", "d-1")

	if err := transport.Write(ctx, []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	line, err := transport.ReadLine(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(line) != "hello" {
		t.Errorf("got %q", line)
	}
}

func TestLocalSubprocessDaemonSpawnerStop(t *testing.T) {
	spawner := NewLocalSubprocessDaemonSpawner()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := spawner.Spawn(ctx, "sb-1", []string{"/bin/sleep", "60"}, nil)
	if err != nil {
		t.Skipf("/bin/sleep unavailable: %v", err)
	}
	if err := spawner.Stop(context.Background(), "sb-1", "d-1"); err != nil {
		t.Errorf("Stop: %v", err)
	}
	// Idempotent — second call should be a no-op.
	if err := spawner.Stop(context.Background(), "sb-1", "d-1"); err != nil {
		t.Errorf("second Stop: %v", err)
	}
}

func TestLocalSubprocessSpawnerRejectsEmptyCmd(t *testing.T) {
	spawner := NewLocalSubprocessDaemonSpawner()
	if _, err := spawner.Spawn(context.Background(), "sb-1", nil, nil); err == nil {
		t.Error("expected error for empty cmd")
	}
}
