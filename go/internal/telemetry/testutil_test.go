package telemetry

import (
	"go.opentelemetry.io/otel/attribute"
)

// attrString returns the string-typed attribute named key, or "" when
// missing. Used by the *_test.go files for compact assertions.
func attrString(attrs []attribute.KeyValue, key string) string {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value.AsString()
		}
	}
	return ""
}

// attrAny returns the attribute as `any` (or nil when missing).
func attrAny(attrs []attribute.KeyValue, key string) any {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value.AsInterface()
		}
	}
	return nil
}
