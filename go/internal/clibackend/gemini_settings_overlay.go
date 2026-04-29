package clibackend

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
)

// geminiSettingsEnv is the env var the Gemini CLI reads for its system
// settings path. Pointing it at a per-turn temp file lets us overlay an
// MCP config without ever touching the user's real ~/.gemini/settings.json.
const geminiSettingsEnv = "GEMINI_CLI_SYSTEM_SETTINGS_PATH"

// geminiSettingsOverlay is a per-turn settings.json file. Created in a
// private tempdir under settingsRoot. cleanup() removes the file and the
// tempdir; safe to call multiple times.
type geminiSettingsOverlay struct {
	path string
}

// Path returns the absolute path to the temp settings file.
func (o *geminiSettingsOverlay) Path() string {
	if o == nil {
		return ""
	}
	return o.path
}

// cleanup removes the temp settings file + parent tempdir (if empty).
// Errors are logged but not surfaced — cleanup is best-effort.
func (o *geminiSettingsOverlay) cleanup() {
	if o == nil || o.path == "" {
		return
	}
	if err := os.Remove(o.path); err != nil && !os.IsNotExist(err) {
		slog.Debug("failed to remove gemini settings tempfile", "path", o.path, "err", err)
	}
	parent := filepath.Dir(o.path)
	// Best effort; ignore "not empty" / not-exist.
	_ = os.Remove(parent)
}

// buildGeminiSettingsOverlay writes a temp settings.json overlaying
// mcpConfig onto the user's defaults (read from baseSettingsPath, if any).
//
// Mirrors OpenClaw's writeGeminiSystemSettings (bundle-mcp.ts L220-256):
// the base file is read non-destructively, the MCP block is merged in, and
// the result is written into a private tempdir. The caller exposes the
// path via GEMINI_CLI_SYSTEM_SETTINGS_PATH to the spawned subprocess only.
func buildGeminiSettingsOverlay(
	mcpConfig map[string]any,
	baseSettingsPath string,
	settingsRoot string,
	runID string,
) (*geminiSettingsOverlay, error) {
	base := map[string]any{}
	if baseSettingsPath != "" {
		if data, err := os.ReadFile(baseSettingsPath); err == nil {
			var loaded map[string]any
			if jsonErr := json.Unmarshal(data, &loaded); jsonErr == nil && loaded != nil {
				base = loaded
			} else if jsonErr != nil {
				slog.Debug(
					"could not parse base gemini settings; starting empty",
					"path", baseSettingsPath, "err", jsonErr,
				)
			}
		} else if !os.IsNotExist(err) {
			slog.Debug(
				"could not read base gemini settings; starting empty",
				"path", baseSettingsPath, "err", err,
			)
		}
	}

	servers, _ := mcpConfig["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}

	merged := map[string]any{}
	for k, v := range base {
		merged[k] = v
	}

	// merged.mcp.allowed = sorted(servers.keys())
	mcpBlock := map[string]any{}
	if existing, ok := merged["mcp"].(map[string]any); ok {
		for k, v := range existing {
			mcpBlock[k] = v
		}
	}
	allowed := make([]string, 0, len(servers))
	for name := range servers {
		allowed = append(allowed, name)
	}
	sort.Strings(allowed)
	// Always-non-nil slice for stable JSON.
	if allowed == nil {
		allowed = []string{}
	}
	mcpBlock["allowed"] = allowed
	merged["mcp"] = mcpBlock

	// merged.mcpServers = base.mcpServers ∪ new servers (new overrides).
	mergedServers := map[string]any{}
	if existing, ok := merged["mcpServers"].(map[string]any); ok {
		for k, v := range existing {
			mergedServers[k] = v
		}
	}
	for k, v := range servers {
		mergedServers[k] = v
	}
	merged["mcpServers"] = mergedServers

	if err := os.MkdirAll(settingsRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create settings root: %w", err)
	}
	tempDir, err := os.MkdirTemp(settingsRoot, fmt.Sprintf("gemini-mcp-%s-*", runID))
	if err != nil {
		return nil, fmt.Errorf("create overlay tempdir: %w", err)
	}
	settingsPath := filepath.Join(tempDir, "settings.json")
	encoded, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		// Best-effort cleanup so we don't leak the tempdir.
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("marshal merged settings: %w", err)
	}
	if err := os.WriteFile(settingsPath, append(encoded, '\n'), 0o600); err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("write settings overlay: %w", err)
	}
	return &geminiSettingsOverlay{path: settingsPath}, nil
}
