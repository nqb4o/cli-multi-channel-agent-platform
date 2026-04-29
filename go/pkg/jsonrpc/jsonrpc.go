// Package jsonrpc implements line-delimited JSON-RPC 2.0 over stdio.
//
// This is the FROZEN transport layer used by the runtime daemon
// (services/runtime/src/runtime/daemon.py in the Python tree). The Go and
// Python implementations MUST stay wire-compatible:
//
//   - One JSON object per line (terminated by '\n').
//   - Standard JSON-RPC 2.0 envelope shape.
//   - Application-level errors (auth_expired, rate_limit, ...) are returned
//     as ok=false inside the result field rather than as JSON-RPC error
//     codes — see [ApplicationErrorToEnvelope]. JSON-RPC -32xxx error codes
//     are reserved for transport / parse / dispatch failures.
//
// The Reader uses bufio.Scanner with MaxScanTokenSize = 16 MiB so that
// large prompts (skill output, image-bearing turns) fit on one line.
//
// The Writer serialises concurrent responses through a mutex so multiple
// in-flight handlers can write without interleaving JSON.
package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// MaxLineSize is the maximum size of a single JSON-RPC line (16 MiB).
// This matches the Python daemon's capacity for large prompts/payloads.
const MaxLineSize = 16 << 20

// Standard JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// ProtocolVersion is the JSON-RPC version string ("2.0").
const ProtocolVersion = "2.0"

// drainTimeout is how long [Server.Serve] waits for in-flight handlers
// after the input stream closes (or shutdown is called).
const drainTimeout = 5 * time.Second

// Request is an incoming JSON-RPC envelope.
//
// ID may be string, float64, or nil — JSON-RPC permits any of these.
// Notifications (id absent) are accepted; the handler is run but no reply
// is written.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a successful JSON-RPC reply.
type Response struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Result  any    `json:"result"`
}

// ErrorResponse is a JSON-RPC error reply (transport-level error codes).
//
// Application-level errors travel inside a [Response] result with
// ok=false — see [ApplicationErrorToEnvelope].
type ErrorResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Error   Error  `json:"error"`
}

// Error is the body of an [ErrorResponse].
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Handler is the user-supplied dispatcher. It returns either a result value
// (any JSON-serialisable) or an [Error]. A nil Error means success.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, *Error)

// Server is a line-delimited JSON-RPC 2.0 server over an io.Reader/Writer
// pair (typically stdin/stdout).
type Server struct {
	reader  io.Reader
	writer  io.Writer
	handler Handler

	writeMu  sync.Mutex
	flushW   *bufio.Writer
	inflight sync.WaitGroup

	shuttingDown chan struct{}
	shutdownOnce sync.Once
}

// NewServer builds a server. The handler must be safe to call concurrently:
// each incoming line spawns a goroutine that invokes it.
func NewServer(r io.Reader, w io.Writer, h Handler) *Server {
	return &Server{
		reader:       r,
		writer:       w,
		handler:      h,
		flushW:       bufio.NewWriter(w),
		shuttingDown: make(chan struct{}),
	}
}

// Serve reads requests until EOF or [Server.Shutdown], dispatching each
// one in its own goroutine. Returns nil on clean EOF. Errors from the
// underlying reader (other than io.EOF) are returned.
//
// On exit, Serve waits up to 5s for in-flight handlers to finish, then
// flushes the writer.
func (s *Server) Serve(ctx context.Context) error {
	scanner := bufio.NewScanner(s.reader)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, MaxLineSize)

	dispatchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		select {
		case <-s.shuttingDown:
			cancel()
		case <-ctx.Done():
			cancel()
		}
	}()

	for scanner.Scan() {
		select {
		case <-s.shuttingDown:
			return s.drain()
		case <-ctx.Done():
			return s.drain()
		default:
		}
		// Copy line bytes — Scanner reuses its buffer.
		raw := append([]byte(nil), scanner.Bytes()...)
		s.inflight.Add(1)
		go func(line []byte) {
			defer s.inflight.Done()
			s.handleLine(dispatchCtx, line)
		}(raw)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		// Drain anyway so we don't lose responses already in-flight.
		_ = s.drain()
		return fmt.Errorf("jsonrpc: scanner: %w", err)
	}
	return s.drain()
}

// Shutdown signals Serve to stop reading new requests and drain in-flight
// handlers. Safe to call multiple times.
func (s *Server) Shutdown() {
	s.shutdownOnce.Do(func() { close(s.shuttingDown) })
}

func (s *Server) drain() error {
	done := make(chan struct{})
	go func() {
		s.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(drainTimeout):
		// Timed out — handlers may still be running; we still flush.
	}
	s.writeMu.Lock()
	err := s.flushW.Flush()
	s.writeMu.Unlock()
	return err
}

func (s *Server) handleLine(ctx context.Context, line []byte) {
	if len(line) == 0 {
		s.write(buildParseError(nil, "empty line"))
		return
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		s.write(buildParseError(nil, fmt.Sprintf("json parse error: %v", err)))
		return
	}
	if req.JSONRPC != "" && req.JSONRPC != ProtocolVersion {
		s.write(buildInvalidRequest(req.ID, "jsonrpc must be '2.0'"))
		return
	}
	if req.Method == "" {
		s.write(buildInvalidRequest(req.ID, "missing or invalid 'method'"))
		return
	}

	result, jerr := s.handler(ctx, req.Method, req.Params)

	// JSON-RPC notifications (id absent / null) get no reply.
	if req.ID == nil {
		return
	}

	if jerr != nil {
		s.write(ErrorResponse{
			JSONRPC: ProtocolVersion,
			ID:      req.ID,
			Error:   *jerr,
		})
		return
	}
	s.write(Response{
		JSONRPC: ProtocolVersion,
		ID:      req.ID,
		Result:  result,
	})
}

// write serialises an envelope and pushes a line to the writer.
// Concurrent callers are serialised by writeMu.
func (s *Server) write(envelope any) {
	payload, err := json.Marshal(envelope)
	if err != nil {
		// Last-ditch internal error envelope.
		fallback, _ := json.Marshal(ErrorResponse{
			JSONRPC: ProtocolVersion,
			ID:      nil,
			Error:   Error{Code: CodeInternalError, Message: "marshal error"},
		})
		payload = fallback
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, _ = s.flushW.Write(payload)
	_, _ = s.flushW.Write([]byte{'\n'})
	_ = s.flushW.Flush()
}

// ApplicationErrorToEnvelope formats an application-level error as a
// successful JSON-RPC reply with ok=false in the result, matching the
// Python `_application_error_to_envelope` shape.
//
// Use this when a method ran to completion but the application reports
// an error (auth_expired, rate_limit, etc.) — JSON-RPC -32xxx codes are
// reserved for transport/parse/dispatch failures.
func ApplicationErrorToEnvelope(class, message, userFacing string, details map[string]any) map[string]any {
	return map[string]any{
		"ok": false,
		"error": map[string]any{
			"class":       class,
			"message":     message,
			"user_facing": userFacing,
			"details":     details,
		},
	}
}

// SuccessEnvelope formats a successful run-style result with ok=true.
// Mirrors Python's `_ok_run_to_envelope` for the runtime daemon's `run`
// method.
func SuccessEnvelope(result any) map[string]any {
	return map[string]any{
		"ok":     true,
		"result": result,
	}
}

func buildParseError(id any, msg string) ErrorResponse {
	return ErrorResponse{
		JSONRPC: ProtocolVersion,
		ID:      id,
		Error:   Error{Code: CodeParseError, Message: msg},
	}
}

func buildInvalidRequest(id any, msg string) ErrorResponse {
	return ErrorResponse{
		JSONRPC: ProtocolVersion,
		ID:      id,
		Error:   Error{Code: CodeInvalidRequest, Message: msg},
	}
}
