package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/openclaw/agent-platform/pkg/jsonrpc"
)

// RuntimeVersion is the daemon's reported version. Bumped per release.
//
// The acceptance test only checks the field is present + non-empty in
// `health()` output. Keep this in sync with the Python RUNTIME_VERSION
// in services/runtime/src/runtime/daemon.py.
const RuntimeVersion = "0.1.0"

// Daemon serves the JSON-RPC over stdio loop:
//
//   - health   → {ok, version, ts}
//   - run      → ok envelope wrapping {text, telemetry, errors} or an
//                application-level {ok=false, error{...}}
//   - shutdown → replies, sets the shutdown flag, drains in-flight calls
//
// Construct with [BuildDaemon] for the production wiring (parses
// agent.yaml, plumbs the registry/DAL into an [AgentLoop]); tests
// construct Daemon directly with their own AgentLoop.
type Daemon struct {
	loop   *AgentLoop
	logger *slog.Logger

	// server is filled in by [Daemon.Serve]. Tests that drive Daemon
	// purely through Handle() never touch it.
	server *jsonrpc.Server

	// shutdownOnce guards Daemon.Shutdown.
	shutdownOnce sync.Once
}

// DaemonOption configures a Daemon at construction time.
type DaemonOption func(*Daemon)

// WithDaemonLogger overrides the slog.Logger the daemon uses.
func WithDaemonLogger(logger *slog.Logger) DaemonOption {
	return func(d *Daemon) { d.logger = logger }
}

// NewDaemon wraps an AgentLoop. The loop must already have its registry
// + DAL plumbed in.
func NewDaemon(loop *AgentLoop, opts ...DaemonOption) *Daemon {
	d := &Daemon{
		loop:   loop,
		logger: slog.Default(),
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Serve runs the JSON-RPC loop on the supplied reader/writer pair.
// In production these are os.Stdin / os.Stdout; tests pass io.Pipe pairs.
//
// Returns when the input stream closes, [Daemon.Shutdown] is called, or
// the context is cancelled. In-flight handlers are drained (bounded by
// the jsonrpc package's drain timeout, currently 5s).
func (d *Daemon) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	d.server = jsonrpc.NewServer(r, w, d.dispatch)
	return d.server.Serve(ctx)
}

// Shutdown signals Serve to stop reading new requests + drain. Safe to
// call multiple times. No-op until [Daemon.Serve] has been called.
func (d *Daemon) Shutdown() {
	d.shutdownOnce.Do(func() {
		if d.server != nil {
			d.server.Shutdown()
		}
	})
}

// Handle dispatches a single Request. Public so unit tests can call it
// without going through the JSON-RPC framing.
//
// Returns either a value (for an OK reply) or a *jsonrpc.Error (for a
// transport-level failure). Application errors travel inside the OK
// value as ok=false envelopes.
func (d *Daemon) Handle(ctx context.Context, method string, params json.RawMessage) (any, *jsonrpc.Error) {
	switch method {
	case "health":
		return d.handleHealth(ctx, params)
	case "shutdown":
		return d.handleShutdown(ctx, params)
	case "run":
		return d.handleRun(ctx, params)
	default:
		return nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeMethodNotFound,
			Message: "unknown method: " + method,
		}
	}
}

// dispatch is the handler shape the jsonrpc.Server wants.
func (d *Daemon) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *jsonrpc.Error) {
	return d.Handle(ctx, method, params)
}

// ---------------------------------------------------------------------------
// health
// ---------------------------------------------------------------------------

func (d *Daemon) handleHealth(_ context.Context, _ json.RawMessage) (any, *jsonrpc.Error) {
	// Acceptance: must answer within 100ms. We avoid any I/O here.
	return map[string]any{
		"ok":      true,
		"version": RuntimeVersion,
		"ts":      time.Now().Unix(),
	}, nil
}

// ---------------------------------------------------------------------------
// shutdown
// ---------------------------------------------------------------------------

func (d *Daemon) handleShutdown(_ context.Context, _ json.RawMessage) (any, *jsonrpc.Error) {
	d.Shutdown()
	return map[string]any{"ok": true}, nil
}

// ---------------------------------------------------------------------------
// run
// ---------------------------------------------------------------------------

func (d *Daemon) handleRun(ctx context.Context, params json.RawMessage) (any, *jsonrpc.Error) {
	req, perr := parseRunRequest(params)
	if perr != nil {
		return nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeInvalidParams,
			Message: perr.Error(),
		}
	}

	// We always return an OK JSON-RPC envelope; application errors
	// travel inside the result body per the F05 brief.
	defer func() {
		// Defensive: any panic inside loop.Run is converted into an
		// "internal" application error via safeTurn already, but if a
		// deeper bug surfaces here we still don't want to take down the
		// daemon.
		if r := recover(); r != nil {
			d.logger.Error("daemon.handleRun panic", "panic", r)
		}
	}()

	result, errResult := d.loop.Run(ctx, *req)
	if errResult != nil {
		return jsonrpc.ApplicationErrorToEnvelope(
			errResult.ErrorClass,
			errResult.Message,
			errResult.UserFacing,
			errResult.Details,
		), nil
	}

	return jsonrpc.SuccessEnvelope(map[string]any{
		"text":      result.Text,
		"telemetry": result.Telemetry,
		"errors":    sliceOrEmpty(result.Errors),
	}), nil
}

// sliceOrEmpty replaces a nil string slice with an empty one so the
// JSON encodes `"errors": []` rather than `"errors": null` (matches the
// Python `list(result.errors)` shape).
func sliceOrEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// ---------------------------------------------------------------------------
// run param parsing
// ---------------------------------------------------------------------------

// parseRunRequest casts the JSON `params` payload into a RunRequest.
//
// Strict on required fields; tolerant of unknown fields (the
// orchestrator may forward telemetry headers etc.). Mirrors
// _parse_run_request in services/runtime/src/runtime/daemon.py.
func parseRunRequest(raw json.RawMessage) (*RunRequest, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("missing 'params'")
	}
	var p struct {
		UserID       string          `json:"user_id"`
		AgentID      string          `json:"agent_id"`
		ChannelID    string          `json:"channel_id"`
		ThreadID     string          `json:"thread_id"`
		Message      json.RawMessage `json:"message"`
		RunID        string          `json:"run_id"`
		WorkspaceDir *string         `json:"workspace_dir"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("'params' must be a JSON object: %v", err)
	}

	userID, err := uuid.Parse(p.UserID)
	if err != nil {
		return nil, fmt.Errorf("missing or invalid uuid field: 'user_id'")
	}
	agentID, err := uuid.Parse(p.AgentID)
	if err != nil {
		return nil, fmt.Errorf("missing or invalid uuid field: 'agent_id'")
	}
	channelID, err := uuid.Parse(p.ChannelID)
	if err != nil {
		return nil, fmt.Errorf("missing or invalid uuid field: 'channel_id'")
	}

	if strings.TrimSpace(p.ThreadID) == "" {
		return nil, fmt.Errorf("'thread_id' must be a non-empty string")
	}
	if strings.TrimSpace(p.RunID) == "" {
		return nil, fmt.Errorf("'run_id' must be a non-empty string")
	}

	msg, err := parseRunMessage(p.Message)
	if err != nil {
		return nil, err
	}

	wd := ""
	if p.WorkspaceDir != nil {
		wd = *p.WorkspaceDir
	}

	return &RunRequest{
		UserID:       userID,
		AgentID:      agentID,
		ChannelID:    channelID,
		ThreadID:     p.ThreadID,
		Message:      msg,
		RunID:        p.RunID,
		WorkspaceDir: wd,
	}, nil
}

func parseRunMessage(raw json.RawMessage) (RunMessage, error) {
	if len(raw) == 0 {
		// Python's `params.get("message") or {}` — accept missing.
		return RunMessage{}, nil
	}
	var m struct {
		Text   *string  `json:"text"`
		Images []string `json:"images"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return RunMessage{}, fmt.Errorf("'message' must be a JSON object")
	}
	out := RunMessage{}
	if m.Text != nil {
		out.Text = *m.Text
	}
	if m.Images != nil {
		out.Images = append([]string(nil), m.Images...)
	}
	return out, nil
}
