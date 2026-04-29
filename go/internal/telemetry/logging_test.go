package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func parseLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("no log line emitted")
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		t.Fatalf("decode log line %q: %v", line, err)
	}
	return record
}

func TestLogInsideSpanCarriesTraceAndSpanID(t *testing.T) {
	initIsolated(t, "logging-tests")
	buf := &bytes.Buffer{}
	log := NewLoggerTo("test.span_log", buf, slog.LevelDebug)

	ctx, end := StartSpan(context.Background(), SpanRuntimeRunLoop)
	span := CurrentSpan(ctx).SpanContext()
	log.InfoContext(ctx, "hello", "agent_id", "a-1")
	end(nil)

	rec := parseLine(t, buf)
	if rec["trace_id"] != span.TraceID().String() {
		t.Fatalf("trace_id: got %v, want %s", rec["trace_id"], span.TraceID().String())
	}
	if rec["span_id"] != span.SpanID().String() {
		t.Fatalf("span_id: got %v, want %s", rec["span_id"], span.SpanID().String())
	}
	if rec["message"] != "hello" {
		t.Fatalf("message: got %v", rec["message"])
	}
	if rec["agent_id"] != "a-1" {
		t.Fatalf("agent_id: got %v", rec["agent_id"])
	}
	if rec["level"] != "info" {
		t.Fatalf("level: got %v", rec["level"])
	}
	if rec["logger"] != "test.span_log" {
		t.Fatalf("logger: got %v", rec["logger"])
	}
}

func TestLogOutsideSpanUsesZeroIDs(t *testing.T) {
	initIsolated(t, "logging-tests")
	buf := &bytes.Buffer{}
	log := NewLoggerTo("test.no_span_log", buf, slog.LevelDebug)

	log.InfoContext(context.Background(), "orphan")
	rec := parseLine(t, buf)
	if rec["trace_id"] != strings.Repeat("0", 32) {
		t.Fatalf("trace_id outside span: got %v", rec["trace_id"])
	}
	if rec["span_id"] != strings.Repeat("0", 16) {
		t.Fatalf("span_id outside span: got %v", rec["span_id"])
	}
}

func TestMultipleSpansEmitDistinctSpanIDs(t *testing.T) {
	initIsolated(t, "logging-tests")
	buf := &bytes.Buffer{}
	log := NewLoggerTo("test.multi_span_log", buf, slog.LevelDebug)

	ctx1, end1 := StartSpan(context.Background(), SpanRuntimeRunLoop)
	log.InfoContext(ctx1, "first")
	end1(nil)
	ctx2, end2 := StartSpan(context.Background(), SpanRuntimeRunLoop)
	log.InfoContext(ctx2, "second")
	end2(nil)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 log lines, got %d", len(lines))
	}
	var a, b map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &b); err != nil {
		t.Fatal(err)
	}
	if a["span_id"] == b["span_id"] {
		t.Fatal("two spans should have distinct span_ids")
	}
	if a["trace_id"] == b["trace_id"] {
		t.Fatal("two distinct spans should have distinct trace_ids")
	}
}

func TestJSONFormatterHandlesNonSerialisableExtras(t *testing.T) {
	initIsolated(t, "logging-tests")
	buf := &bytes.Buffer{}
	log := NewLoggerTo("test.weird", buf, slog.LevelDebug)

	type Weird struct{}
	weird := &weirdStringer{}
	log.InfoContext(context.Background(), "payload", "obj", weird)
	rec := parseLine(t, buf)
	if rec["obj"] != "<Weird>" {
		t.Fatalf("obj should fall back to String(): got %v", rec["obj"])
	}
}

type weirdStringer struct{}

func (w *weirdStringer) String() string { return "<Weird>" }
