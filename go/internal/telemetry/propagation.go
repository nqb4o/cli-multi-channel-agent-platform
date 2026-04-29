// W3C trace-context propagation helpers (Go port of propagation.py).
//
// The Python implementation accepts both string and bytes carrier values so
// it can read traceparents fanned in from message brokers (Redis Streams,
// NATS, Kafka). The Go counterpart exposes the same contract by accepting
// any map[string]any (or map[string][]byte) and coercing values to UTF-8
// strings before delegating to the underlying TraceContext propagator.
package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/propagation"
)

// W3C trace-context header names.
const (
	TraceparentHeader = "traceparent"
	TracestateHeader  = "tracestate"
)

// Use a single TraceContext propagator across the package — matches the
// Python module-global _propagator.
var propagator = propagation.TraceContext{}

// InjectTraceparent writes the active span's W3C headers into carrier and
// returns it for chaining. ctx supplies the span context (use
// context.Background() if you need to inject from the active span via
// otel.GetTextMapPropagator default — but our shim keeps the semantics
// equivalent to Python's inject(carrier, context=context_or_current)).
func InjectTraceparent(ctx context.Context, carrier map[string]string) map[string]string {
	if carrier == nil {
		carrier = map[string]string{}
	}
	propagator.Inject(ctx, propagation.MapCarrier(carrier))
	return carrier
}

// CoerceCarrierToStr returns a copy of carrier with all keys + values
// coerced to string. Values that are []byte are UTF-8 decoded; any value
// that fails decoding is dropped (matching the Python implementation, which
// silently skips on UnicodeDecodeError).
func CoerceCarrierToStr(carrier map[string]any) map[string]string {
	out := make(map[string]string, len(carrier))
	for k, v := range carrier {
		key := k
		switch x := v.(type) {
		case string:
			out[key] = x
		case []byte:
			if isValidUTF8(x) {
				out[key] = string(x)
			}
		case fmt.Stringer:
			out[key] = x.String()
		default:
			out[key] = fmt.Sprintf("%v", v)
		}
	}
	return out
}

// CoerceBytesCarrier is a convenience for the common Redis Streams shape
// where both keys and values come back as []byte.
func CoerceBytesCarrier(carrier map[string][]byte) map[string]string {
	out := make(map[string]string, len(carrier))
	for k, v := range carrier {
		if isValidUTF8(v) {
			out[k] = string(v)
		}
	}
	return out
}

// ExtractTraceparent reads W3C headers from carrier and returns a context
// with the remote span context attached. Pass the returned context as the
// parent of the next span.
func ExtractTraceparent(ctx context.Context, carrier map[string]string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return propagator.Extract(ctx, propagation.MapCarrier(carrier))
}

// ExtractTraceparentFromAny is the carrier-agnostic variant: it coerces
// values into strings (mimicking the Python extract_traceparent that
// accepts bytes-valued mappings).
func ExtractTraceparentFromAny(ctx context.Context, carrier map[string]any) context.Context {
	return ExtractTraceparent(ctx, CoerceCarrierToStr(carrier))
}

// ExtractTraceparentFromBytes is the Redis-Streams convenience variant.
func ExtractTraceparentFromBytes(ctx context.Context, carrier map[string][]byte) context.Context {
	return ExtractTraceparent(ctx, CoerceBytesCarrier(carrier))
}

// isValidUTF8 mirrors Python's UnicodeDecodeError check — Go strings are
// already byte sequences so we only need to verify the data decodes.
func isValidUTF8(b []byte) bool {
	for i := 0; i < len(b); {
		r, size := decodeRune(b[i:])
		if r == 0xFFFD && size == 1 {
			return false
		}
		i += size
	}
	return true
}

func decodeRune(b []byte) (rune, int) {
	// Inline a tiny utf8 check — calling into encoding/utf8 would be cleaner
	// but adds an import on a hot path.
	if len(b) == 0 {
		return 0, 0
	}
	c := b[0]
	switch {
	case c < 0x80:
		return rune(c), 1
	case c < 0xC2:
		return 0xFFFD, 1
	case c < 0xE0:
		if len(b) < 2 || b[1]&0xC0 != 0x80 {
			return 0xFFFD, 1
		}
		return rune(c&0x1F)<<6 | rune(b[1]&0x3F), 2
	case c < 0xF0:
		if len(b) < 3 || b[1]&0xC0 != 0x80 || b[2]&0xC0 != 0x80 {
			return 0xFFFD, 1
		}
		return rune(c&0x0F)<<12 | rune(b[1]&0x3F)<<6 | rune(b[2]&0x3F), 3
	case c < 0xF5:
		if len(b) < 4 || b[1]&0xC0 != 0x80 || b[2]&0xC0 != 0x80 || b[3]&0xC0 != 0x80 {
			return 0xFFFD, 1
		}
		return rune(c&0x07)<<18 | rune(b[1]&0x3F)<<12 | rune(b[2]&0x3F)<<6 | rune(b[3]&0x3F), 4
	}
	return 0xFFFD, 1
}
