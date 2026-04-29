// Span helper API (Go port of decorators.py).
//
// Two entry points:
//
//   - StartSpan(ctx, name, attrs...) returns a (newCtx, end) pair so callers
//     write `ctx, end := StartSpan(...); defer end(nil)`. The end callback
//     accepts an error which, if non-nil, gets recorded on the span (with
//     platform.error.class + platform.error.message) before the span ends.
//     This mirrors Python's `async with traced_span(...):` shape.
//
//   - Traced wraps a function so the span lifecycle is hidden — equivalent
//     to Python's `@traced(name)` decorator. Two flavours: TracedFunc (no
//     return value) and TracedFuncErr (returns an error which feeds the
//     error-class semantics).
package telemetry

import (
	"context"
	"reflect"
	"runtime"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracerName matches Python's "platform_telemetry" tracer scope so all spans
// land under the same instrumentation library, regardless of language.
const tracerName = "platform_telemetry"

func tracer() trace.Tracer {
	return otel.Tracer(tracerName)
}

// Attrs is a convenience map type so call sites read like the Python
// keyword-argument form: telemetry.Attrs{telemetry.PlatformUserID: "u-1"}.
type Attrs map[string]any

func (a Attrs) toKVs() []attribute.KeyValue {
	if len(a) == 0 {
		return nil
	}
	out := make([]attribute.KeyValue, 0, len(a))
	for k, v := range a {
		out = append(out, anyToKV(k, v))
	}
	return out
}

func anyToKV(key string, v any) attribute.KeyValue {
	switch x := v.(type) {
	case string:
		return attribute.String(key, x)
	case bool:
		return attribute.Bool(key, x)
	case int:
		return attribute.Int(key, x)
	case int64:
		return attribute.Int64(key, x)
	case float64:
		return attribute.Float64(key, x)
	case []string:
		return attribute.StringSlice(key, x)
	default:
		// Fallback to a string render so we never panic on unexpected types.
		return attribute.String(key, reflectString(v))
	}
}

func reflectString(v any) string {
	if v == nil {
		return ""
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return ""
	}
	return rv.String()
}

// EndFunc is returned by StartSpan. Pass an error (or nil) to optionally
// record an error on the span before it ends.
type EndFunc func(err error)

// StartSpan opens a span named `name` carrying `attrs`. Use it like:
//
//	ctx, end := telemetry.StartSpan(ctx, telemetry.SpanRuntimeCliTurn,
//	    telemetry.Attrs{telemetry.PlatformUserID: "u-1"})
//	defer end(nil)
//
// If you have an error to surface, call `end(err)` (or `defer end(err)` after
// assigning to a named return). The error is recorded on the span with
// platform.error.class + platform.error.message attributes — the same shape
// the ErrorAwareRatioSampler watches for.
func StartSpan(ctx context.Context, name string, attrs ...Attrs) (context.Context, EndFunc) {
	merged := Attrs{}
	for _, a := range attrs {
		for k, v := range a {
			merged[k] = v
		}
	}
	startOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindInternal),
	}
	if kvs := merged.toKVs(); len(kvs) > 0 {
		startOpts = append(startOpts, trace.WithAttributes(kvs...))
	}
	ctx, span := tracer().Start(ctx, name, startOpts...)
	end := func(err error) {
		if err != nil {
			recordError(span, err)
		}
		span.End()
	}
	return ctx, end
}

// StartSpanWithKind is the variant that accepts an explicit SpanKind (e.g.
// trace.SpanKindServer for inbound HTTP, trace.SpanKindClient for outbound
// RPC).
func StartSpanWithKind(ctx context.Context, name string, kind trace.SpanKind, attrs ...Attrs) (context.Context, EndFunc) {
	merged := Attrs{}
	for _, a := range attrs {
		for k, v := range a {
			merged[k] = v
		}
	}
	startOpts := []trace.SpanStartOption{trace.WithSpanKind(kind)}
	if kvs := merged.toKVs(); len(kvs) > 0 {
		startOpts = append(startOpts, trace.WithAttributes(kvs...))
	}
	ctx, span := tracer().Start(ctx, name, startOpts...)
	end := func(err error) {
		if err != nil {
			recordError(span, err)
		}
		span.End()
	}
	return ctx, end
}

func recordError(span trace.Span, err error) {
	span.SetAttributes(
		attribute.String(PlatformErrorClass, errorClassName(err)),
		attribute.String(PlatformErrorMessage, err.Error()),
	)
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// errorClassName returns the Go type name of err — equivalent to Python's
// `type(exc).__name__` so platform.error.class values are consistent enough
// across languages to drive the same dashboards.
func errorClassName(err error) string {
	if err == nil {
		return ""
	}
	t := reflect.TypeOf(err)
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Name() == "" {
		// Anonymous struct (e.g. errors.New result) — fall back to "errorString".
		return "errorString"
	}
	return t.Name()
}

// Traced wraps fn in a span. If name is empty, the runtime function name is
// used (matching Python's default of `module.qualname`).
func Traced(name string, fn func(ctx context.Context) error) func(ctx context.Context) error {
	if name == "" {
		name = funcName(fn)
	}
	return func(ctx context.Context) error {
		ctx, end := StartSpan(ctx, name)
		err := fn(ctx)
		end(err)
		return err
	}
}

// TracedNoErr is the no-error variant — equivalent to wrapping a sync
// function whose Python signature returns no exception path.
func TracedNoErr(name string, fn func(ctx context.Context)) func(ctx context.Context) {
	if name == "" {
		name = funcName(fn)
	}
	return func(ctx context.Context) {
		ctx, end := StartSpan(ctx, name)
		fn(ctx)
		end(nil)
	}
}

func funcName(fn any) string {
	pc := reflect.ValueOf(fn).Pointer()
	if f := runtime.FuncForPC(pc); f != nil {
		return f.Name()
	}
	return "anonymous"
}

// CurrentSpan returns the active span from ctx (or a no-op span).
func CurrentSpan(ctx context.Context) trace.Span {
	return trace.SpanFromContext(ctx)
}
