// Per-provider MCP config builders.
//
// F09 owns the shape of per-provider configs that point at the F11 loopback
// bridge. F11 owns the runtime those configs point at. Every provider has a
// slightly different on-disk config format.
//
// MCPBridge is declared locally as an interface so F09 ships without a
// runtime dependency on F11 (which is implemented in a parallel sub-agent).
//
// References:
//   - Codex: inline mcp_servers table in config.toml
//   - Claude: .mcp.json plus --plugin-dir
//   - Gemini: settings.json overlay
//
// Adapted from openclaw/src/cli/{codex,claude,gemini}-config.ts (MIT).
package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// MCPBridge is the minimal interface F09 needs from the F11 loopback bridge.
// F11's concrete McpBridge satisfies this via duck typing — no import of F11
// is required.
type MCPBridge interface {
	URL() string
	Token() string
	EnvOverlay() map[string]string
}

const (
	// ENVTokenVar is the env-var name the spawned CLI reads the bearer token from.
	ENVTokenVar = "OPENCLAW_MCP_TOKEN"
	// ENVTokenPlaceholder is the placeholder inserted into config files so the
	// literal token never lands on disk.
	ENVTokenPlaceholder = "${OPENCLAW_MCP_TOKEN}"
	// DefaultServerKey is the MCP server entry name used in all provider configs.
	DefaultServerKey = "openclaw-runtime"
)

// ---------------------------------------------------------------------------
// Codex
// ---------------------------------------------------------------------------

// CodexInlineConfig returns the dict to merge into Codex's config.toml
// mcp_servers table.
//
//	[mcp_servers.openclaw-runtime]
//	transport = "http"
//	url = "http://127.0.0.1:54321/mcp"
//	bearer_token_env = "OPENCLAW_MCP_TOKEN"
func CodexInlineConfig(bridge MCPBridge, serverKey string) map[string]any {
	if serverKey == "" {
		serverKey = DefaultServerKey
	}
	return map[string]any{
		"mcp_servers": map[string]any{
			serverKey: map[string]any{
				"transport":        "http",
				"url":              bridge.URL(),
				"bearer_token_env": ENVTokenVar,
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Claude
// ---------------------------------------------------------------------------

// ClaudeMCPConfig returns the .mcp.json payload Claude Code accepts as plugin
// MCP config.
//
//	{
//	  "mcpServers": {
//	    "openclaw-runtime": {
//	      "type": "http",
//	      "url": "...",
//	      "headers": {"Authorization": "Bearer ${OPENCLAW_MCP_TOKEN}"}
//	    }
//	  }
//	}
func ClaudeMCPConfig(bridge MCPBridge, serverKey string) map[string]any {
	if serverKey == "" {
		serverKey = DefaultServerKey
	}
	return map[string]any{
		"mcpServers": map[string]any{
			serverKey: map[string]any{
				"type": "http",
				"url":  bridge.URL(),
				"headers": map[string]any{
					"Authorization": "Bearer " + ENVTokenPlaceholder,
				},
			},
		},
	}
}

// ClaudeConfigFile serialises ClaudeMCPConfig to a JSON string ready for disk.
func ClaudeConfigFile(bridge MCPBridge, serverKey string) (string, error) {
	cfg := ClaudeMCPConfig(bridge, serverKey)
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("ClaudeConfigFile marshal: %w", err)
	}
	return string(b) + "\n", nil
}

// ---------------------------------------------------------------------------
// Gemini
// ---------------------------------------------------------------------------

// GeminiSettingsPayload returns the dict to merge into Gemini CLI's
// settings.json.
func GeminiSettingsPayload(bridge MCPBridge, serverKey string) map[string]any {
	if serverKey == "" {
		serverKey = DefaultServerKey
	}
	return map[string]any{
		"mcpServers": map[string]any{
			serverKey: map[string]any{
				"transport": "http",
				"url":       bridge.URL(),
				"headers": map[string]any{
					"Authorization": "Bearer " + ENVTokenPlaceholder,
				},
			},
		},
	}
}

// GeminiSettingsOverlay merges GeminiSettingsPayload into an existing settings
// dict. The overlay is non-destructive: it preserves other top-level keys and
// other MCP servers. Only the entry under serverKey is overwritten.
func GeminiSettingsOverlay(bridge MCPBridge, existing map[string]any, serverKey string) map[string]any {
	if serverKey == "" {
		serverKey = DefaultServerKey
	}
	result := make(map[string]any, len(existing)+1)
	for k, v := range existing {
		result[k] = v
	}
	payload := GeminiSettingsPayload(bridge, serverKey)
	// Merge mcpServers.
	mergedServers := map[string]any{}
	if existingServers, ok := result["mcpServers"].(map[string]any); ok {
		for k, v := range existingServers {
			mergedServers[k] = v
		}
	}
	if newServers, ok := payload["mcpServers"].(map[string]any); ok {
		for k, v := range newServers {
			mergedServers[k] = v
		}
	}
	result["mcpServers"] = mergedServers
	return result
}

// ---------------------------------------------------------------------------
// Loopback layering
// ---------------------------------------------------------------------------

// LayerLoopbackIntoPluginDir drops a .mcp.json into the F09 plugin dir
// pointing at F11. Returns the absolute path to the written .mcp.json.
func LayerLoopbackIntoPluginDir(bridge MCPBridge, pluginDir *PluginDir, serverKey string) (string, error) {
	if pluginDir == nil {
		return "", fmt.Errorf("LayerLoopbackIntoPluginDir: pluginDir is nil")
	}
	info, err := os.Stat(pluginDir.PluginRoot)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("plugin_dir.PluginRoot does not exist: %s", pluginDir.PluginRoot)
	}
	content, err := ClaudeConfigFile(bridge, serverKey)
	if err != nil {
		return "", err
	}
	target := filepath.Join(pluginDir.PluginRoot, ".mcp.json")
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("LayerLoopbackIntoPluginDir write: %w", err)
	}
	return target, nil
}

// ---------------------------------------------------------------------------
// Env helpers
// ---------------------------------------------------------------------------

// TokenEnv builds the env-var bag F11 should set on the spawned CLI.
// Kept here so the name of the env var is owned by the same module that hands
// it to every provider config.
func TokenEnv(bridge MCPBridge) map[string]string {
	return map[string]string{ENVTokenVar: bridge.Token()}
}

// EnvWithToken returns baseEnv merged with the bridge token env. If baseEnv is
// nil it starts from the process environment.
func EnvWithToken(bridge MCPBridge, baseEnv map[string]string) map[string]string {
	merged := make(map[string]string)
	if baseEnv == nil {
		baseEnv = osEnvironMap()
	}
	for k, v := range baseEnv {
		merged[k] = v
	}
	for k, v := range TokenEnv(bridge) {
		merged[k] = v
	}
	return merged
}
