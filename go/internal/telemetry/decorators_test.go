package telemetry

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/codes"
)

func TestStartSpanEmitsNamedSpanWithAttrs(t *testing.T) {
	initIsolated(t, "decorator-tests")
	exp := GetInMemorySpanExporter()
	exp.Reset()

	ctx, end := StartSpan(context.Background(), SpanRuntimeCliTurn,
		Attrs{PlatformUserID: "u-1"})
	_ = ctx
	end(nil)

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Name != SpanRuntimeCliTurn {
		t.Fatalf("span name: got %q, want %q", spans[0].Name, SpanRuntimeCliTurn)
	}
	if got := attrString(spans[0].Attributes, PlatformUserID); got != "u-1" {
		t.Fatalf("platform.user.id: got %q, want u-1", got)
	}
}

func TestStartSpanRecordsErrorWithErrorClass(t *testing.T) {
	initIsolated(t, "decorator-tests")
	exp := GetInMemorySpanExporter()
	exp.Reset()

	type myErr struct{ msg string }
	// myErr's Error method makes it an error type.
	_, end := StartSpan(context.Background(), SpanRuntimeCliTurn)
	end(&myErrConcrete{msg: "boom"})

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if got := attrString(spans[0].Attributes, PlatformErrorClass); got != "myErrConcrete" {
		t.Fatalf("platform.error.class: got %q, want myErrConcrete", got)
	}
	if spans[0].Status.Code != codes.Error {
		t.Fatalf("span status: got %v, want Error", spans[0].Status.Code)
	}
}

type myErrConcrete struct{ msg string }

func (e *myErrConcrete) Error() string { return e.msg }

func TestStartSpanErrorsAreOptional(t *testing.T) {
	initIsolated(t, "decorator-tests")
	exp := GetInMemorySpanExporter()
	exp.Reset()
	_, end := StartSpan(context.Background(), "ok.span")
	end(nil)
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code == codes.Error {
		t.Fatal("span should not have error status when end(nil)")
	}
}

func TestTracedWraperAroundFunc(t *testing.T) {
	initIsolated(t, "decorator-tests")
	exp := GetInMemorySpanExporter()
	exp.Reset()

	wrapped := Traced("custom.fn", func(ctx context.Context) error {
		return nil
	})
	if err := wrapped(context.Background()); err != nil {
		t.Fatalf("wrapped fn: %v", err)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 || spans[0].Name != "custom.fn" {
		t.Fatalf("expected one span named custom.fn, got %v", spans)
	}
}

func TestTracedPropagatesError(t *testing.T) {
	initIsolated(t, "decorator-tests")
	exp := GetInMemorySpanExporter()
	exp.Reset()

	sentinel := errors.New("nope")
	wrapped := Traced("err.fn", func(ctx context.Context) error {
		return sentinel
	})
	if err := wrapped(context.Background()); err != sentinel {
		t.Fatalf("expected sentinel error to propagate, got %v", err)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code != codes.Error {
		t.Fatal("span should have error status when wrapped fn returns err")
	}
}

func TestNestedSpansAreParentChild(t *testing.T) {
	initIsolated(t, "decorator-tests")
	exp := GetInMemorySpanExporter()
	exp.Reset()

	ctx, endParent := StartSpan(context.Background(), "parent")
	parentSpan := CurrentSpan(ctx)
	parentSC := parentSpan.SpanContext()
	_, endChild := StartSpan(ctx, "child")
	endChild(nil)
	endParent(nil)

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(spans))
	}
	byName := map[string]int{}
	for i, s := range spans {
		byName[s.Name] = i
	}
	child := spans[byName["child"]]
	parent := spans[byName["parent"]]
	if child.SpanContext.TraceID() != parent.SpanContext.TraceID() {
		t.Fatal("child + parent should share trace id")
	}
	if child.Parent.SpanID() != parent.SpanContext.SpanID() {
		t.Fatalf("child.Parent (%s) should equal parent.SpanID (%s)",
			child.Parent.SpanID(), parent.SpanContext.SpanID())
	}
	if parent.SpanContext.SpanID() != parentSC.SpanID() {
		t.Fatal("captured parent span id should match exported span id")
	}
}

func TestStartSpanWithAttrsMerge(t *testing.T) {
	initIsolated(t, "decorator-tests")
	exp := GetInMemorySpanExporter()
	exp.Reset()
	_, end := StartSpan(context.Background(), SpanRuntimeCliTurn,
		Attrs{PlatformAgentID: "a-1"},
		Attrs{PlatformUserID: "u-2"})
	end(nil)
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if attrString(spans[0].Attributes, PlatformAgentID) != "a-1" {
		t.Fatal("agent id missing")
	}
	if attrString(spans[0].Attributes, PlatformUserID) != "u-2" {
		t.Fatal("user id missing")
	}
}
