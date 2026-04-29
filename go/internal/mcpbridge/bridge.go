// Package mcpbridge implements F11 — the per-turn MCP Loopback Bridge.
//
// A McpBridgeManager starts a tiny net/http server bound exclusively to
// 127.0.0.1. The server validates a per-session 64-hex-char bearer token
// (constant-time via crypto/subtle), dispatches JSON-RPC 2.0 methods
// (initialize / tools/list / tools/call) to a pool of per-skill stdio
// MCP child processes, and records one ToolCallAuditEntry per call.
//
// Lifecycle:
//
//	bridge, err := mgr.Start(skills, scope, runID)
//	// → McpBridge.URL  = "http://127.0.0.1:<port>/mcp"
//	// → McpBridge.Token for OPENCLAW_MCP_TOKEN env var
//	bridge.Stop() // reaps children ≤1s
//
// Ported from services/runtime/src/runtime/cli_backends/mcp_bridge.py (Python).
package mcpbridge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Constants

const (
	// ENVTokenVar is the env var the spawned CLI reads for the bearer token.
	// Mirrors F09's mcp_config_gen.ENV_TOKEN_VAR.
	ENVTokenVar = "OPENCLAW_MCP_TOKEN"

	// LoopbackHost is the only address the bridge will bind to.
	LoopbackHost = "127.0.0.1"

	// MCPPath is the JSON-RPC endpoint served by the bridge.
	MCPPath = "/mcp"

	// MCPProtocolVersion is the MCP spec date string we advertise on initialize.
	MCPProtocolVersion = "2024-11-05"

	// terminateGraceS is how long to wait after SIGTERM before SIGKILL on child reap.
	terminateGraceS = 1.0

	// childRPCTimeoutS is the tools/list + tools/call child response timeout.
	childRPCTimeoutS = 30.0
)

// ---------------------------------------------------------------------------
// Public types

// McpScope carries (user_id, channel_id, session_id, agent_id). It is
// immutable once created and echoed back in HTTP response headers so test
// assertions can verify a request matched the expected scope.
type McpScope struct {
	UserID    string
	ChannelID string
	SessionID string
	AgentID   string
}

// Headers returns the OpenClaw-style scoping headers for HTTP echo-back.
// Header names mirror openclaw/gateway/mcp-http*.ts.
func (s McpScope) Headers() map[string]string {
	return map[string]string{
		"x-openclaw-account-id":      s.UserID,
		"x-openclaw-message-channel": s.ChannelID,
		"x-session-key":              s.SessionID,
		"x-openclaw-agent-id":        s.AgentID,
	}
}

// ToolCallAuditEntry is one audit-log entry written per tools/call dispatch.
// args_hash is a 16-hex prefix of SHA-256(JSON(arguments)) — plaintext
// arguments never appear in the log. F14 will ship these to the central audit
// pipeline; for now they accumulate in McpBridge.ToolCalls.
type ToolCallAuditEntry struct {
	RunID     string
	UserID    string
	SkillSlug string
	Tool      string
	ArgsHash  string  // 16-char hex prefix of SHA-256(canonical JSON(args))
	LatencyMs float64
	OK        bool
	Timestamp time.Time
	Error     string // empty on success
}

// ---------------------------------------------------------------------------
// McpBridge

// McpBridge is the per-turn loopback bridge handle returned by McpBridgeManager.Start.
// Callers use URL + Token, then call Stop to reap children.
type McpBridge struct {
	// URL is "http://127.0.0.1:<port>/mcp" — what the CLI POSTs to.
	URL string
	// Token is the 64-hex-char bearer token for OPENCLAW_MCP_TOKEN.
	Token string
	// Port is the kernel-assigned TCP port.
	Port int
	// Scope is the (user_id, channel_id, session_id, agent_id) tuple.
	Scope McpScope
	// RunID is the trace-correlation id; surfaces in every audit entry.
	RunID string
	// ToolCalls is the in-memory audit log (append-only during the turn).
	ToolCalls []ToolCallAuditEntry

	server   *http.Server
	listener net.Listener
	pool     *skillChildPool
	stopped  bool
}

// EnvOverlay returns the env-var bag callers should merge onto the spawned CLI's env.
func (b *McpBridge) EnvOverlay() map[string]string {
	return map[string]string{ENVTokenVar: b.Token}
}

// Stop reaps all child processes and closes the HTTP server. Idempotent.
func (b *McpBridge) Stop() {
	if b.stopped {
		return
	}
	b.stopped = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(terminateGraceS*float64(time.Second)))
	defer cancel()
	// Close HTTP server first so no new connections come in.
	_ = b.server.Shutdown(ctx)
	b.pool.stopAll()
}

// ---------------------------------------------------------------------------
// McpBridgeManager

// McpBridgeManager is the factory + lifecycle owner for McpBridge instances.
type McpBridgeManager struct{}

// SkillManifestForBridge is the subset of a skill manifest that the bridge
// needs. Callers supply a slice of these to McpBridgeManager.Start.
type SkillManifestForBridge struct {
	// Name is the skill slug (e.g. "web-search").
	Name string
	// MCPEnabled signals that this skill advertises an MCP server.
	MCPEnabled bool
	// MCPCommand is the argv to spawn the skill's MCP server subprocess.
	MCPCommand []string
	// WorkDir is the cwd for the child process (defaults to os.Getwd()).
	WorkDir string
}

// Start boots a bridge for one turn and returns its handle.
//
// host must be "127.0.0.1" (LoopbackHost); any other value returns an error.
// port=0 lets the kernel pick the port (recommended to avoid collisions).
func (m *McpBridgeManager) Start(
	skills []SkillManifestForBridge,
	scope McpScope,
	runID string,
	host string,
	port int,
) (*McpBridge, error) {
	if host == "" {
		host = LoopbackHost
	}
	if host != LoopbackHost {
		return nil, fmt.Errorf("MCP bridge must bind to %q; refused %q", LoopbackHost, host)
	}

	// Build skill pool.
	manifests := make(map[string]SkillManifestForBridge, len(skills))
	for _, s := range skills {
		manifests[s.Name] = s
	}
	pool := newSkillChildPool(manifests)

	// Generate 64-hex-char bearer token (32 random bytes).
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("mcpbridge: generate token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)

	// Audit list — shared between handler and bridge handle.
	var auditLog []ToolCallAuditEntry

	// Build bridge (we need its pointer for the audit append closure).
	bridge := &McpBridge{
		Token: token,
		Scope: scope,
		RunID: runID,
		pool:  pool,
	}
	bridge.ToolCalls = auditLog[:0:0] // zero-len but same backing

	dispatcher := &rpcDispatcher{
		token:  token,
		scope:  scope,
		pool:   pool,
		runID:  runID,
		bridge: bridge,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", dispatcher.handleHealth)
	mux.HandleFunc(MCPPath, dispatcher.handleMCP)

	srv := &http.Server{Handler: mux}

	addr := fmt.Sprintf("%s:%d", host, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mcpbridge: listen %s: %w", addr, err)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port

	bridge.server = srv
	bridge.listener = ln
	bridge.Port = actualPort
	bridge.URL = fmt.Sprintf("http://%s:%d%s", host, actualPort, MCPPath)

	go func() { _ = srv.Serve(ln) }()

	return bridge, nil
}

// ---------------------------------------------------------------------------
// HTTP / RPC dispatcher

type rpcDispatcher struct {
	token  string
	scope  McpScope
	pool   *skillChildPool
	runID  string
	bridge *McpBridge
}

func (d *rpcDispatcher) handleHealth(w http.ResponseWriter, r *http.Request) {
	d.writeScope(w)
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (d *rpcDispatcher) handleMCP(w http.ResponseWriter, r *http.Request) {
	d.writeScope(w)

	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": fmt.Sprintf("method %q not allowed on %s", r.Method, MCPPath),
		})
		return
	}

	// Authentication.
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing bearer token"})
		return
	}
	presented := strings.TrimSpace(authHeader[len("bearer "):])
	// crypto/subtle.ConstantTimeCompare requires equal-length slices to be timing-safe.
	if subtle.ConstantTimeCompare([]byte(presented), []byte(d.token)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "bearer token mismatch"})
		return
	}

	// Parse JSON-RPC body.
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, jsonrpcError(nil, -32700, "read error"))
		return
	}

	var req map[string]any
	if len(bodyBytes) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(bodyBytes)))
		dec.UseNumber()
		if err := dec.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, jsonrpcError(nil, -32700, "parse error"))
			return
		}
	}
	if req == nil {
		req = map[string]any{}
	}

	reqID := req["id"]
	method, _ := req["method"].(string)
	if method == "" {
		writeJSON(w, http.StatusBadRequest, jsonrpcError(reqID, -32600, "missing method"))
		return
	}

	params, _ := req["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	var result map[string]any
	switch method {
	case "initialize":
		result = jsonrpcResult(reqID, map[string]any{
			"protocolVersion": MCPProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "openclaw-runtime-mcp", "version": "0.1.0"},
		})
	case "tools/list":
		tools, err := d.pool.listAllTools(r.Context())
		if err != nil {
			result = jsonrpcError(reqID, -32000, err.Error())
		} else {
			result = jsonrpcResult(reqID, map[string]any{"tools": tools})
		}
	case "tools/call":
		result = d.dispatchToolsCall(r.Context(), reqID, params)
	default:
		result = jsonrpcError(reqID, -32601, fmt.Sprintf("method not found: %q", method))
	}

	writeJSON(w, http.StatusOK, result)
}

func (d *rpcDispatcher) dispatchToolsCall(ctx context.Context, reqID any, params map[string]any) map[string]any {
	name, _ := params["name"].(string)
	if name == "" || !strings.Contains(name, ".") {
		return jsonrpcError(reqID, -32602, "tools/call params.name must be 'slug.tool'")
	}
	dotIdx := strings.Index(name, ".")
	slug := name[:dotIdx]
	tool := name[dotIdx+1:]
	if slug == "" || tool == "" {
		return jsonrpcError(reqID, -32602, fmt.Sprintf("tools/call name %q did not split into slug.tool", name))
	}

	var arguments map[string]any
	if args, ok := params["arguments"].(map[string]any); ok {
		arguments = args
	} else {
		arguments = map[string]any{}
	}

	argsHash := hashArgs(arguments)
	started := time.Now()
	var callErr error
	var toolResult any

	toolResult, callErr = d.pool.callTool(ctx, slug, tool, arguments)

	latencyMs := float64(time.Since(started).Nanoseconds()) / 1e6
	entry := ToolCallAuditEntry{
		RunID:     d.runID,
		UserID:    d.scope.UserID,
		SkillSlug: slug,
		Tool:      tool,
		ArgsHash:  argsHash,
		LatencyMs: latencyMs,
		OK:        callErr == nil,
		Timestamp: time.Now(),
	}
	if callErr != nil {
		entry.Error = callErr.Error()
	}
	d.bridge.ToolCalls = append(d.bridge.ToolCalls, entry)

	if callErr != nil {
		return jsonrpcError(reqID, -32000, callErr.Error())
	}
	return jsonrpcResult(reqID, toolResult)
}

// writeScope writes all McpScope headers to the response.
func (d *rpcDispatcher) writeScope(w http.ResponseWriter) {
	for k, v := range d.scope.Headers() {
		w.Header().Set(k, v)
	}
}

// ---------------------------------------------------------------------------
// JSON-RPC helpers

func jsonrpcError(id any, code int, message string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	}
}

func jsonrpcResult(id any, result any) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

// ---------------------------------------------------------------------------
// Errors

// ErrNonLoopback is returned when Start is called with a non-loopback host.
var ErrNonLoopback = errors.New("mcpbridge: host must be 127.0.0.1")
