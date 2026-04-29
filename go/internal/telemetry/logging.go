// Structured logging with trace correlation (Go port of logging.py).
//
// JSON-line slog handler that injects trace_id + span_id from the active
// span context. Use it via NewLogger(name) — each record gets the same
// trace_id / span_id Python emits, so logs line up across languages in Loki.
package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/trace"
)

// Sentinel zero IDs used outside any span — matches the Python null markers
// so dashboards can filter them with a single string predicate.
const (
	traceIDNull = "00000000000000000000000000000000"
	spanIDNull  = "0000000000000000"
)

// JSONHandler renders slog records as one JSON line per event, with
// trace_id / span_id pulled from the slog.Record context (set by passing a
// context through Logger.Log* methods or by using LogAttrs/InfoContext).
//
// The output schema matches Python's JsonFormatter:
//
//	{"level":"info","logger":"orchestrator.run","message":"...",
//	 "trace_id":"<32 hex>","span_id":"<16 hex>","trace_flags":"01", ...}
type JSONHandler struct {
	mu     *sync.Mutex
	out    io.Writer
	level  slog.Level
	name   string
	attrs  []slog.Attr
	groups []string
}

// NewJSONHandler builds a handler writing to out.
func NewJSONHandler(out io.Writer, name string, level slog.Level) *JSONHandler {
	if out == nil {
		out = os.Stderr
	}
	return &JSONHandler{out: out, level: level, name: name, mu: &sync.Mutex{}}
}

// Enabled implements slog.Handler.
func (h *JSONHandler) Enabled(_ context.Context, lvl slog.Level) bool {
	return lvl >= h.level
}

// Handle implements slog.Handler.
func (h *JSONHandler) Handle(ctx context.Context, r slog.Record) error {
	payload := map[string]any{
		"level":   strings.ToLower(r.Level.String()),
		"logger":  h.name,
		"message": r.Message,
	}
	traceID, spanID, traceFlags := traceIDsFromContext(ctx)
	payload["trace_id"] = traceID
	payload["span_id"] = spanID
	if traceFlags != "" {
		payload["trace_flags"] = traceFlags
	}

	for _, a := range h.attrs {
		assignAttr(payload, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		assignAttr(payload, a)
		return true
	})

	buf, err := jsonMarshal(payload)
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err = h.out.Write(buf)
	return err
}

// WithAttrs implements slog.Handler.
func (h *JSONHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append([]slog.Attr{}, h.attrs...)
	clone.attrs = append(clone.attrs, attrs...)
	return &clone
}

// WithGroup implements slog.Handler.
func (h *JSONHandler) WithGroup(name string) slog.Handler {
	clone := *h
	clone.groups = append([]string{}, h.groups...)
	clone.groups = append(clone.groups, name)
	return &clone
}

// NewLogger returns a *slog.Logger pre-configured with the JSON handler.
// The trace correlation works automatically when callers use the *Context
// variants (logger.InfoContext, etc.) — see test_logging.go.
func NewLogger(name string) *slog.Logger {
	return slog.New(NewJSONHandler(os.Stderr, name, slog.LevelInfo))
}

// NewLoggerTo is the same as NewLogger but writes to a custom sink — used in
// tests to capture into bytes.Buffer.
func NewLoggerTo(name string, out io.Writer, level slog.Level) *slog.Logger {
	return slog.New(NewJSONHandler(out, name, level))
}

// traceIDsFromContext pulls the active span ids out of ctx. Outside any
// span, returns the canonical all-zero placeholders.
func traceIDsFromContext(ctx context.Context) (traceID, spanID, traceFlags string) {
	if ctx == nil {
		return traceIDNull, spanIDNull, ""
	}
	sc := trace.SpanFromContext(ctx).SpanContext()
	if !sc.IsValid() {
		return traceIDNull, spanIDNull, "00"
	}
	return sc.TraceID().String(), sc.SpanID().String(), sc.TraceFlags().String()
}

// assignAttr converts an slog.Attr into a JSON-friendly key/value pair.
//
// Mirrors Python's JsonFormatter loop: scalars + lists go through verbatim,
// anything that doesn't render via json.Marshal falls back to fmt.Stringer
// or repr-equivalent. Crucially, Stringer-implementing values (the slog
// equivalent of Python's __repr__-only objects) get their String() captured
// even when json.Marshal would happily serialise an empty struct as "{}".
func assignAttr(dst map[string]any, a slog.Attr) {
	if a.Key == "" {
		return
	}
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		// Flatten one level — matches the Python "extra={...}" surface.
		for _, sub := range v.Group() {
			assignAttr(dst, sub)
		}
		return
	}
	raw := v.Any()
	// Prefer Stringer, then error, before falling through to JSON. Matches
	// Python's repr() fallback for objects whose default JSON shape would
	// hide useful information.
	if s, ok := raw.(interface{ String() string }); ok {
		switch raw.(type) {
		case slog.LogValuer:
			// LogValuer was already resolved above; treat it as a normal value.
		case string, []byte:
			dst[a.Key] = raw
			return
		default:
			dst[a.Key] = s.String()
			return
		}
	}
	if e, ok := raw.(error); ok {
		dst[a.Key] = e.Error()
		return
	}
	dst[a.Key] = raw
}

// jsonMarshal renders payload, falling back to a repr-ish string for any
// value that doesn't serialise (matching Python's repr fallback).
func jsonMarshal(payload map[string]any) ([]byte, error) {
	for k, v := range payload {
		if !canMarshal(v) {
			payload[k] = stringerFallback(v)
		}
	}
	return json.Marshal(payload)
}

func canMarshal(v any) bool {
	if v == nil {
		return true
	}
	_, err := json.Marshal(v)
	return err == nil
}

func stringerFallback(v any) string {
	if s, ok := v.(interface{ String() string }); ok {
		return s.String()
	}
	if s, ok := v.(error); ok {
		return s.Error()
	}
	// Fallback render — best-effort representation.
	b, err := json.Marshal(strings.ToValidUTF8(toGoString(v), "?"))
	if err == nil {
		return string(b)
	}
	return "<unrenderable>"
}

func toGoString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
