// Inline-TOML serialiser for Codex `-c key=value` overrides.
//
// Codex's `-c` flag takes a TOML-formatted value. We serialise primitives,
// strings, lists, and nested maps to inline TOML (`[a, b]` / `{ k = v }`)
// so the whole value fits on one argv entry.
//
// Ported from services/runtime/src/runtime/cli_backends/codex.py
// (`serialize_toml_inline_value` / `format_toml_config_override`), which
// in turn mirrors openclaw `cli-runner/toml-inline.ts` (MIT,
// openclaw/LICENSE, @cb4ec1265f8b2e3bb78a20fb2ee83285b9076e7e).

package clibackend

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// tomlBareKey matches a TOML "bare key" — letters, digits, underscores,
// and hyphens. Anything else is quoted.
var tomlBareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func escapeTOMLString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

func formatTOMLKey(key string) string {
	if tomlBareKey.MatchString(key) {
		return key
	}
	return `"` + escapeTOMLString(key) + `"`
}

// SerializeTOMLInlineValue serialises value as an inline-TOML literal.
//
// Mirrors openclaw `serializeTomlInlineValue` (toml-inline.ts L13-32) and
// the Python `serialize_toml_inline_value`. Supports strings, numbers,
// booleans, slices, and maps. Nested maps and slices use TOML's inline
// syntax so the whole value can ride a single argv entry.
//
// Map iteration order is sorted by key for deterministic output (matches
// the Python implementation's insertion order for tests, but also gives
// stable Go output regardless of hash seed).
func SerializeTOMLInlineValue(value any) (string, error) {
	switch v := value.(type) {
	case nil:
		return "", fmt.Errorf("toml inline: nil values are not supported")
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	case string:
		return `"` + escapeTOMLString(v) + `"`, nil
	case int:
		return strconv.Itoa(v), nil
	case int8:
		return strconv.FormatInt(int64(v), 10), nil
	case int16:
		return strconv.FormatInt(int64(v), 10), nil
	case int32:
		return strconv.FormatInt(int64(v), 10), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case uint:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint8:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint64:
		return strconv.FormatUint(v, 10), nil
	case float32:
		// Match Python's str(float) for ints-as-floats; otherwise fall through to %g.
		return formatFloat(float64(v)), nil
	case float64:
		return formatFloat(v), nil
	case []any:
		parts := make([]string, 0, len(v))
		for _, entry := range v {
			s, err := SerializeTOMLInlineValue(entry)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case []string:
		parts := make([]string, 0, len(v))
		for _, entry := range v {
			parts = append(parts, `"`+escapeTOMLString(entry)+`"`)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			s, err := SerializeTOMLInlineValue(v[k])
			if err != nil {
				return "", err
			}
			parts = append(parts, formatTOMLKey(k)+" = "+s)
		}
		return "{ " + strings.Join(parts, ", ") + " }", nil
	default:
		return "", fmt.Errorf("toml inline: unsupported value type %T", value)
	}
}

// formatFloat mirrors Python's `str(float)` for values used in TOML
// overrides. Whole-number floats render as "1" not "1.0" in Python's
// `str(1)` path (we hit that because Python's `int` and `float` are both
// numbers); for non-integer floats we use the smallest-round-trip 'g'
// form. This is mostly cosmetic — Codex parses either.
func formatFloat(f float64) string {
	if f == float64(int64(f)) && f >= -1e15 && f <= 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// FormatTOMLConfigOverride builds the `key=<inline-toml>` payload for
// `codex -c`. Mirrors openclaw `formatTomlConfigOverride` / Python's
// `format_toml_config_override`.
func FormatTOMLConfigOverride(key string, value any) (string, error) {
	encoded, err := SerializeTOMLInlineValue(value)
	if err != nil {
		return "", err
	}
	return key + "=" + encoded, nil
}
