// Exec results + long-running daemon helpers.
//
// Two responsibilities:
//
//  1. ExecResult — typed return for one-shot exec calls.
//  2. DaemonHandle — JSON-RPC 2.0 over a bidirectional byte stream.
//     F05 (the runtime daemon) speaks JSON-RPC over stdio; the orchestrator
//     spawns it via DaemonSpawner and routes RPC calls through DaemonHandle.
//
// The handle exposes only RPC and Stop. Adding richer surface (cancel,
// streaming, backpressure) needs an interface change request — same rule
// as the Python source.
package orchestrator

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ExecResult is the return value of Orchestrator.Exec.
//
// Stdout/Stderr are raw bytes — callers decode based on what they spawned.
// ExitCode == nil means the command was killed by timeout (TimedOut == true).
type ExecResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode *int
	TimedOut bool
}

// DaemonTransport is a bidirectional line-oriented byte stream the JSON-RPC
// layer writes to.
//
// Implementations:
//   - LocalSubprocessDaemonSpawner constructs subprocessTransport.
//   - Tests inject an in-memory pipe.
type DaemonTransport interface {
	Write(ctx context.Context, data []byte) error
	ReadLine(ctx context.Context) ([]byte, error)
	Close() error
}

// DaemonSpawner spawns a long-running process and returns a DaemonTransport.
type DaemonSpawner interface {
	Spawn(ctx context.Context, sandboxID string, cmd []string, env map[string]string) (DaemonTransport, error)
	Stop(ctx context.Context, sandboxID, daemonID string) error
}

// DaemonRpcError is the typed error returned when a daemon emits a
// JSON-RPC error envelope.
type DaemonRpcError struct {
	Code    int
	Message string
	Data    any
}

func (e *DaemonRpcError) Error() string {
	return fmt.Sprintf("daemon rpc error %d: %s", e.Code, e.Message)
}

// DaemonHandle wraps a DaemonTransport plus a single inbound reader
// goroutine. RPC calls are correlated by id.
type DaemonHandle struct {
	DaemonID  string
	SandboxID string

	transport DaemonTransport
	onStop    func(context.Context) error

	mu         sync.Mutex
	pending    map[string]chan rpcReply
	readerOnce sync.Once
	readerDone chan struct{}
	stopped    bool
	writeMu    sync.Mutex
}

type rpcReply struct {
	result map[string]any
	err    error
}

// NewDaemonHandle constructs a handle. onStop (optional) fires after Stop()
// closes the transport — used to tell the spawner to clean up its side.
func NewDaemonHandle(daemonID, sandboxID string, transport DaemonTransport, onStop func(context.Context) error) *DaemonHandle {
	return &DaemonHandle{
		DaemonID:   daemonID,
		SandboxID:  sandboxID,
		transport:  transport,
		onStop:     onStop,
		pending:    map[string]chan rpcReply{},
		readerDone: make(chan struct{}),
	}
}

// RPC sends a JSON-RPC 2.0 request and waits for the matching response.
func (h *DaemonHandle) RPC(ctx context.Context, method string, params map[string]any, timeout time.Duration) (map[string]any, error) {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return nil, fmt.Errorf("daemon %s is stopped", h.DaemonID)
	}
	h.mu.Unlock()

	h.ensureReader()

	id := uuid.NewString()
	ch := make(chan rpcReply, 1)

	h.mu.Lock()
	h.pending[id] = ch
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
	}()

	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	if params == nil {
		envelope["params"] = map[string]any{}
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("marshal envelope: %w", err)
	}
	payload = append(payload, '\n')

	h.writeMu.Lock()
	werr := h.transport.Write(ctx, payload)
	h.writeMu.Unlock()
	if werr != nil {
		return nil, fmt.Errorf("transport write: %w", werr)
	}

	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case reply := <-ch:
		if reply.err != nil {
			return nil, reply.err
		}
		return reply.result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, context.DeadlineExceeded
	}
}

// Stop closes the transport and tears down the reader goroutine.
// Idempotent — safe to call repeatedly.
func (h *DaemonHandle) Stop(ctx context.Context) error {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return nil
	}
	h.stopped = true
	pending := h.pending
	h.pending = map[string]chan rpcReply{}
	h.mu.Unlock()

	_ = h.transport.Close()
	// Best-effort wait for reader goroutine to exit (it observes EOF
	// from the closed transport).
	select {
	case <-h.readerDone:
	case <-time.After(500 * time.Millisecond):
	}

	for _, ch := range pending {
		select {
		case ch <- rpcReply{err: errors.New("daemon stopped")}:
		default:
		}
	}

	if h.onStop != nil {
		return h.onStop(ctx)
	}
	return nil
}

// ensureReader starts the reader goroutine on first RPC call.
func (h *DaemonHandle) ensureReader() {
	h.readerOnce.Do(func() {
		go h.readerLoop()
	})
}

func (h *DaemonHandle) readerLoop() {
	defer close(h.readerDone)
	for {
		h.mu.Lock()
		stopped := h.stopped
		h.mu.Unlock()
		if stopped {
			return
		}
		line, err := h.transport.ReadLine(context.Background())
		if err != nil {
			return
		}
		if len(line) == 0 {
			return
		}
		var msg map[string]any
		if jerr := json.Unmarshal(line, &msg); jerr != nil {
			// Skip malformed frames — daemon must speak proper JSON-RPC.
			continue
		}
		idAny, ok := msg["id"]
		if !ok {
			continue
		}
		idStr, ok := idAny.(string)
		if !ok {
			continue
		}
		h.mu.Lock()
		ch, ok := h.pending[idStr]
		h.mu.Unlock()
		if !ok {
			continue
		}
		if errVal, hasErr := msg["error"]; hasErr {
			if errMap, ok := errVal.(map[string]any); ok {
				code := 0
				if c, ok := errMap["code"].(float64); ok {
					code = int(c)
				}
				message, _ := errMap["message"].(string)
				select {
				case ch <- rpcReply{err: &DaemonRpcError{Code: code, Message: message, Data: errMap["data"]}}:
				default:
				}
				continue
			}
		}
		var result map[string]any
		if r, ok := msg["result"].(map[string]any); ok {
			result = r
		} else {
			result = map[string]any{}
		}
		select {
		case ch <- rpcReply{result: result}:
		default:
		}
	}
}

// ---------------------------------------------------------------------------
// LocalSubprocessDaemonSpawner — production-style spawner using os/exec.
// ---------------------------------------------------------------------------

// LocalSubprocessDaemonSpawner spawns daemons as local subprocesses with
// stdio pipes. Used when the runtime daemon must run on the orchestrator
// host (single-node dev, smoke tests). Inside a real Daytona sandbox, F05
// would inject a session-backed spawner.
type LocalSubprocessDaemonSpawner struct {
	mu        sync.Mutex
	processes map[string]*localProcess // key: sandboxID + ":" + daemonID (we just track by sandboxID for now)
}

type localProcess struct {
	cmd       *exec.Cmd
	sandboxID string
}

// NewLocalSubprocessDaemonSpawner constructs an empty spawner.
func NewLocalSubprocessDaemonSpawner() *LocalSubprocessDaemonSpawner {
	return &LocalSubprocessDaemonSpawner{
		processes: map[string]*localProcess{},
	}
}

// Spawn launches the command and returns a transport tied to its stdio.
func (s *LocalSubprocessDaemonSpawner) Spawn(ctx context.Context, sandboxID string, cmd []string, env map[string]string) (DaemonTransport, error) {
	if len(cmd) == 0 {
		return nil, errors.New("cmd must be non-empty")
	}
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	if env != nil {
		envSlice := make([]string, 0, len(env))
		for k, v := range env {
			envSlice = append(envSlice, k+"="+v)
		}
		c.Env = envSlice
	}
	stdin, err := c.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := c.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start: %w", err)
	}

	s.mu.Lock()
	s.processes[sandboxID] = &localProcess{cmd: c, sandboxID: sandboxID}
	s.mu.Unlock()

	return &subprocessTransport{
		stdin:   stdin,
		stdout:  bufio.NewReader(stdout),
		cmd:     c,
		stdoutC: stdout,
	}, nil
}

// Stop terminates the daemon process for the given sandbox.
func (s *LocalSubprocessDaemonSpawner) Stop(ctx context.Context, sandboxID, daemonID string) error {
	s.mu.Lock()
	proc, ok := s.processes[sandboxID]
	if ok {
		delete(s.processes, sandboxID)
	}
	s.mu.Unlock()
	if !ok || proc.cmd.Process == nil {
		return nil
	}
	_ = proc.cmd.Process.Kill()
	_, _ = proc.cmd.Process.Wait()
	return nil
}

// subprocessTransport is the DaemonTransport over an exec.Cmd's stdio pipes.
type subprocessTransport struct {
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	stdoutC io.Closer
	cmd     *exec.Cmd
	mu      sync.Mutex
	closed  bool
}

func (t *subprocessTransport) Write(_ context.Context, data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errors.New("transport closed")
	}
	_, err := t.stdin.Write(data)
	return err
}

func (t *subprocessTransport) ReadLine(_ context.Context) ([]byte, error) {
	line, err := t.stdout.ReadBytes('\n')
	if len(line) > 0 {
		// Strip trailing newline.
		if line[len(line)-1] == '\n' {
			line = line[:len(line)-1]
		}
		return line, nil
	}
	if err != nil {
		return nil, err
	}
	return line, nil
}

func (t *subprocessTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	_ = t.stdin.Close()
	_ = t.stdoutC.Close()
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
		_, _ = t.cmd.Process.Wait()
	}
	return nil
}

var _ DaemonSpawner = (*LocalSubprocessDaemonSpawner)(nil)
var _ DaemonTransport = (*subprocessTransport)(nil)
