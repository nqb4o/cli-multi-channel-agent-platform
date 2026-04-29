package telemetry

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
)

func TestInjectThenExtractRoundTrips(t *testing.T) {
	initIsolated(t, "propagation-tests")

	carrier := map[string]string{}
	ctx, end := StartSpan(context.Background(), SpanGatewayHandleWebhook)
	InjectTraceparent(ctx, carrier)
	end(nil)

	if _, ok := carrier["traceparent"]; !ok {
		t.Fatalf("expected traceparent in carrier, got %v", carrier)
	}

	// Round-trip: extract and start a downstream span, verify same trace_id.
	downstreamCtx := ExtractTraceparent(context.Background(), carrier)
	tracer := otel.Tracer("test")
	_, downstream := tracer.Start(downstreamCtx, "downstream")
	originalTraceID := strings.Split(carrier["traceparent"], "-")[1]
	got := downstream.SpanContext().TraceID().String()
	if got != originalTraceID {
		t.Fatalf("trace id mismatch: got %s, want %s", got, originalTraceID)
	}
	downstream.End()
}

func TestCarrierAcceptsBytesValues(t *testing.T) {
	initIsolated(t, "propagation-tests")

	carrier := map[string]string{}
	ctx, end := StartSpan(context.Background(), SpanGatewayHandleWebhook)
	InjectTraceparent(ctx, carrier)
	end(nil)

	bytesCarrier := map[string][]byte{}
	for k, v := range carrier {
		bytesCarrier[k] = []byte(v)
	}
	coerced := CoerceBytesCarrier(bytesCarrier)
	if len(coerced) != len(carrier) {
		t.Fatalf("coerced size %d != carrier size %d", len(coerced), len(carrier))
	}
	for k, v := range carrier {
		if coerced[k] != v {
			t.Fatalf("key %q: got %q, want %q", k, coerced[k], v)
		}
	}

	downstreamCtx := ExtractTraceparentFromBytes(context.Background(), bytesCarrier)
	tracer := otel.Tracer("test")
	_, downstream := tracer.Start(downstreamCtx, "downstream")
	defer downstream.End()
	if !downstream.SpanContext().IsValid() {
		t.Fatal("downstream context should be valid after extract from bytes carrier")
	}
	originalTraceID := strings.Split(carrier["traceparent"], "-")[1]
	if downstream.SpanContext().TraceID().String() != originalTraceID {
		t.Fatal("trace id should round-trip through bytes carrier")
	}
}

func TestTwoPropagationHopsShareTraceID(t *testing.T) {
	initIsolated(t, "propagation-tests")
	exp := GetInMemorySpanExporter()
	exp.Reset()

	// Hop 1: gateway emits + injects.
	gatewayCarrier := map[string]string{}
	ctx, end := StartSpan(context.Background(), SpanGatewayHandleWebhook)
	InjectTraceparent(ctx, gatewayCarrier)
	end(nil)

	// Bytes-encoded transit (Redis Streams).
	bytesCarrier1 := map[string][]byte{}
	for k, v := range gatewayCarrier {
		bytesCarrier1[k] = []byte(v)
	}

	// Hop 2: orchestrator extracts + runs + injects again.
	orchCtx := ExtractTraceparentFromBytes(context.Background(), bytesCarrier1)
	runtimeCarrier := map[string]string{}
	orchSpanCtx, end2 := StartSpan(orchCtx, SpanOrchestratorRun)
	InjectTraceparent(orchSpanCtx, runtimeCarrier)
	end2(nil)

	// Hop 3: runtime extracts + runs.
	runtimeCtx := ExtractTraceparent(context.Background(), runtimeCarrier)
	_, end3 := StartSpan(runtimeCtx, SpanRuntimeRunLoop)
	end3(nil)

	spans := exp.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("expected 3 spans, got %d", len(spans))
	}
	traceIDs := map[string]struct{}{}
	for _, s := range spans {
		traceIDs[s.SpanContext.TraceID().String()] = struct{}{}
	}
	if len(traceIDs) != 1 {
		t.Fatalf("expected 1 trace id across 3 hops, got %v", traceIDs)
	}
}

func TestExtractWithNoHeadersReturnsEmptyContext(t *testing.T) {
	initIsolated(t, "propagation-tests")
	ctx := ExtractTraceparent(context.Background(), map[string]string{})
	tracer := otel.Tracer("test")
	_, span := tracer.Start(ctx, "root")
	defer span.End()
	if !span.SpanContext().IsValid() {
		t.Fatal("root span should still be valid (fresh trace id)")
	}
}

func TestCoerceCarrierToStrAcceptsAnyValueTypes(t *testing.T) {
	carrier := map[string]any{
		"a": "string",
		"b": []byte("bytes"),
		"c": 42,
	}
	coerced := CoerceCarrierToStr(carrier)
	if coerced["a"] != "string" {
		t.Fatalf("string value: %v", coerced["a"])
	}
	if coerced["b"] != "bytes" {
		t.Fatalf("bytes value: %v", coerced["b"])
	}
	if coerced["c"] != "42" {
		t.Fatalf("int value: %v", coerced["c"])
	}
}

func TestCoerceCarrierDropsInvalidUTF8(t *testing.T) {
	carrier := map[string]any{
		"good": []byte("ok"),
		"bad":  []byte{0xff, 0xfe, 0xfd}, // invalid UTF-8
	}
	coerced := CoerceCarrierToStr(carrier)
	if _, has := coerced["bad"]; has {
		t.Fatal("invalid UTF-8 entry should be dropped")
	}
	if coerced["good"] != "ok" {
		t.Fatal("good entry should survive")
	}
}
