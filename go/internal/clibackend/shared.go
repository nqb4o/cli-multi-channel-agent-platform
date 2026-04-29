// Free helpers shared between F02 (Codex), F04 (Claude), and (future)
// F03 (Gemini). Anything provider-specific lives in the per-backend
// file; anything provider-agnostic — JSONL line parsing, env-merge,
// argv[0] resolution, SIGTERM-then-SIGKILL termination — lives here.
//
// Ported from services/runtime/src/runtime/cli_backends/{codex,claude}.py.
// The shapes mirror the Python helpers (`_maybe_load_record`,
// `_collect_text`, `_is_inline_error`, `_resolve_executable`,
// `_terminate`, the env-compose pattern in `CodexBackend._compose_env`).

package clibackend

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// stderrTailBytes is the size of the stderr tail returned in CliTurnError.
const stderrTailBytes = 4096

// terminateGrace is how long to wait after SIGTERM before SIGKILL on
// cancellation. Mirrors Python's `_TERMINATE_GRACE_S = 1.0`.
const terminateGrace = 1 * time.Second

// maybeLoadRecord parses one JSONL line. Tolerates trailing whitespace +
// blank lines + non-object payloads. Mirrors Python's
// `_maybe_load_record` in codex.py / claude.py.
func maybeLoadRecord(line string) map[string]any {
	stripped := strings.TrimSpace(line)
	if stripped == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(stripped), &v); err != nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return m
}

// collectText recursively concatenates string content out of a CLI item
// payload. Mirrors openclaw `collectCliText` (cli-output.ts L162-194)
// with the ordering: response > text > result > content > message.
func collectText(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, entry := range v {
			sb.WriteString(collectText(entry))
		}
		return sb.String()
	case map[string]any:
		for _, key := range []string{"response", "text", "result"} {
			if field, ok := v[key]; ok {
				if s, ok := field.(string); ok {
					return s
				}
			}
		}
		if content, ok := v["content"]; ok {
			switch c := content.(type) {
			case string:
				return c
			case []any:
				var sb strings.Builder
				for _, entry := range c {
					sb.WriteString(collectText(entry))
				}
				return sb.String()
			}
		}
		if msg, ok := v["message"]; ok {
			if m, ok := msg.(map[string]any); ok {
				return collectText(m)
			}
		}
	}
	return ""
}

// extractUsage pulls the documented "common" token-count keys from any
// event that carries a `usage` object. Backend-specific extensions
// (Codex's nested input_tokens_details preservation; Gemini's `stats`
// fallback carrier) live in the per-backend file as separate helpers.
//
// Returns nil if no `usage` map is present or no recognised keys
// resolved.
func extractUsage(event map[string]any) map[string]any {
	rawAny, ok := event["usage"]
	if !ok {
		return nil
	}
	raw, ok := rawAny.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]any{}
	for _, key := range []string{
		"input_tokens",
		"output_tokens",
		"total_tokens",
		"cache_read_input_tokens",
		"cache_creation_input_tokens",
	} {
		if v, ok := raw[key]; ok {
			if n, ok := numericToInt64(v); ok {
				out[key] = n
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isInlineError returns a human-readable message if `event` represents a
// failure (`type:"error"` or a top-level `error` map with `message`).
// Mirrors Python's `_is_inline_error` in codex.py.
func isInlineError(event map[string]any) string {
	if t, ok := event["type"].(string); ok && strings.EqualFold(t, "error") {
		for _, key := range []string{"message", "error", "result", "detail"} {
			val, ok := event[key]
			if !ok {
				continue
			}
			if s, ok := val.(string); ok {
				if t := strings.TrimSpace(s); t != "" {
					return t
				}
			}
			if m, ok := val.(map[string]any); ok {
				if msg, ok := m["message"].(string); ok {
					if t := strings.TrimSpace(msg); t != "" {
						return t
					}
				}
			}
		}
		// Last resort: stringify the event with sorted keys (Go's
		// encoding/json already sorts map[string]any keys, matching
		// Python's `json.dumps(..., sort_keys=True)`).
		if b, err := json.Marshal(event); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", event)
	}
	// Some CLI builds set `error` on a non-error-typed event.
	if errAny, ok := event["error"]; ok {
		if m, ok := errAny.(map[string]any); ok {
			if msg, ok := m["message"].(string); ok {
				if t := strings.TrimSpace(msg); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

// composeEnv builds the env slice for a subprocess: parent process env
// merged with `extra` (extras win on conflict). Mirrors Python's
// `_compose_env`.
func composeEnv(extra map[string]string) []string {
	env := make(map[string]string, len(os.Environ()))
	for _, kv := range os.Environ() {
		idx := strings.IndexByte(kv, '=')
		if idx <= 0 {
			continue
		}
		env[kv[:idx]] = kv[idx+1:]
	}
	for k, v := range extra {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// resolveExecutable reports whether `cmd` is a runnable program — either
// an absolute / relative path that exists with the +x bit, or a
// PATH-resolvable name. Mirrors Python's `_resolve_executable` (returns
// bool form for callers that just want a yes/no).
func resolveExecutable(cmd string) bool {
	if cmd == "" {
		return false
	}
	if filepath.IsAbs(cmd) || strings.HasPrefix(cmd, "./") || strings.HasPrefix(cmd, "../") {
		info, err := os.Stat(cmd)
		if err != nil || info.IsDir() {
			return false
		}
		return info.Mode()&0o111 != 0
	}
	_, err := exec.LookPath(cmd)
	return err == nil
}

// terminateMu serialises Process.Signal calls so we don't race with
// reaping (some Go runtimes are picky about Signal-after-Wait on Linux).
var terminateMu sync.Mutex

// terminateProcess sends SIGTERM to the process (and its process group,
// for child shells), waits up to terminateGrace, then escalates to
// SIGKILL. Mirrors Python's `_terminate` so cancellation semantics are
// uniform across providers.
func terminateProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	terminateMu.Lock()
	defer terminateMu.Unlock()

	pid := cmd.Process.Pid
	pgid := -pid
	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	deadline := time.Now().Add(terminateGrace)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(pgid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

// processAlive probes via signal-0 — Linux returns ESRCH if the pid is
// gone. Returns true if the process is still alive, false otherwise.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return false
		}
		// EPERM etc → process exists, we just can't signal it.
		return true
	}
	return true
}
