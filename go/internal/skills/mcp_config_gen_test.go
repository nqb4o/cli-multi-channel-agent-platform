package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubBridge is a minimal MCPBridge for tests.
type stubBridge struct {
	url   string
	token string
}

func (s *stubBridge) URL() string                  { return s.url }
func (s *stubBridge) Token() string                { return s.token }
func (s *stubBridge) EnvOverlay() map[string]string { return map[string]string{} }

var _ MCPBridge = (*stubBridge)(nil)

func newBridge() *stubBridge {
	return &stubBridge{url: "http://127.0.0.1:54321/mcp", token: "secret-token"}
}

// ---------------------------------------------------------------------------
// Codex
// ---------------------------------------------------------------------------

func TestCodexInlineConfigShape(t *testing.T) {
	b := newBridge()
	cfg := CodexInlineConfig(b, "")
	servers, ok := cfg["mcp_servers"].(map[string]any)
	if !ok {
		t.Fatal("mcp_servers should be a map")
	}
	entry, ok := servers[DefaultServerKey].(map[string]any)
	if !ok {
		t.Fatalf("entry for %q not found", DefaultServerKey)
	}
	if entry["transport"] != "http" {
		t.Fatalf("transport=%v", entry["transport"])
	}
	if entry["url"] != "http://127.0.0.1:54321/mcp" {
		t.Fatalf("url=%v", entry["url"])
	}
	if entry["bearer_token_env"] != ENVTokenVar {
		t.Fatalf("bearer_token_env=%v", entry["bearer_token_env"])
	}
}

func TestCodexInlineConfigCustomKey(t *testing.T) {
	b := newBridge()
	cfg := CodexInlineConfig(b, "my-server")
	servers := cfg["mcp_servers"].(map[string]any)
	if _, ok := servers["my-server"]; !ok {
		t.Fatal("custom key not found in codex config")
	}
}

func TestCodexInlineConfigTokenNotLiteral(t *testing.T) {
	b := newBridge()
	raw, err := json.Marshal(CodexInlineConfig(b, ""))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), b.token) {
		t.Fatal("literal token must NOT appear in codex config")
	}
}

// ---------------------------------------------------------------------------
// Claude
// ---------------------------------------------------------------------------

func TestClaudeMCPConfigShape(t *testing.T) {
	b := newBridge()
	cfg := ClaudeMCPConfig(b, "")
	servers := cfg["mcpServers"].(map[string]any)
	entry := servers[DefaultServerKey].(map[string]any)
	if entry["type"] != "http" {
		t.Fatalf("type=%v", entry["type"])
	}
	if entry["url"] != "http://127.0.0.1:54321/mcp" {
		t.Fatalf("url=%v", entry["url"])
	}
	headers := entry["headers"].(map[string]any)
	auth := headers["Authorization"].(string)
	if !strings.Contains(auth, ENVTokenPlaceholder) {
		t.Fatalf("Authorization should contain placeholder, got %q", auth)
	}
	if strings.Contains(auth, b.token) {
		t.Fatal("literal token must NOT appear in claude config")
	}
}

func TestClaudeConfigFileIsValidJSON(t *testing.T) {
	b := newBridge()
	s, err := ClaudeConfigFile(b, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("ClaudeConfigFile output is not valid JSON: %v", err)
	}
}

func TestClaudeConfigFileEndsWithNewline(t *testing.T) {
	b := newBridge()
	s, err := ClaudeConfigFile(b, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(s, "\n") {
		t.Fatal("ClaudeConfigFile should end with newline")
	}
}

// ---------------------------------------------------------------------------
// Gemini
// ---------------------------------------------------------------------------

func TestGeminiSettingsPayloadShape(t *testing.T) {
	b := newBridge()
	cfg := GeminiSettingsPayload(b, "")
	servers := cfg["mcpServers"].(map[string]any)
	entry := servers[DefaultServerKey].(map[string]any)
	if entry["transport"] != "http" {
		t.Fatalf("transport=%v", entry["transport"])
	}
	if entry["url"] != "http://127.0.0.1:54321/mcp" {
		t.Fatalf("url=%v", entry["url"])
	}
}

func TestGeminiSettingsOverlayPreservesExisting(t *testing.T) {
	b := newBridge()
	existing := map[string]any{
		"theme":      "dark",
		"mcpServers": map[string]any{"other-server": map[string]any{"url": "http://other"}},
	}
	result := GeminiSettingsOverlay(b, existing, "")
	if result["theme"] != "dark" {
		t.Fatal("existing 'theme' should be preserved")
	}
	servers := result["mcpServers"].(map[string]any)
	if _, ok := servers["other-server"]; !ok {
		t.Fatal("other-server should be preserved")
	}
	if _, ok := servers[DefaultServerKey]; !ok {
		t.Fatalf("%q should be added", DefaultServerKey)
	}
}

func TestGeminiSettingsOverlayNilExisting(t *testing.T) {
	b := newBridge()
	result := GeminiSettingsOverlay(b, nil, "")
	if result["mcpServers"] == nil {
		t.Fatal("mcpServers should be set")
	}
}

// ---------------------------------------------------------------------------
// LayerLoopbackIntoPluginDir
// ---------------------------------------------------------------------------

func TestLayerLoopbackIntoPluginDir(t *testing.T) {
	tmp := t.TempDir()
	r := makeResolvedWithDir(t, "foo", "1.0.0", "workspace")
	pd, err := GeneratePluginDir([]*ResolvedSkill{r}, "run-loop", tmp, "", "")
	if err != nil {
		t.Fatalf("GeneratePluginDir: %v", err)
	}
	b := newBridge()
	mcpPath, err := LayerLoopbackIntoPluginDir(b, pd, "")
	if err != nil {
		t.Fatalf("LayerLoopbackIntoPluginDir: %v", err)
	}
	if !strings.HasSuffix(mcpPath, ".mcp.json") {
		t.Fatalf("returned path should end with .mcp.json, got %q", mcpPath)
	}
	data, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatalf("read .mcp.json: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf(".mcp.json not valid JSON: %v", err)
	}
	if _, ok := out["mcpServers"]; !ok {
		t.Fatal(".mcp.json should contain mcpServers")
	}
}

func TestLayerLoopbackIntoPluginDirMissingRoot(t *testing.T) {
	b := newBridge()
	fakePD := &PluginDir{
		PluginRoot: "/nonexistent/plugin/root",
	}
	_, err := LayerLoopbackIntoPluginDir(b, fakePD, "")
	if err == nil {
		t.Fatal("expected error for missing plugin root")
	}
}

func TestLayerLoopbackIntoPluginDirNilPluginDir(t *testing.T) {
	b := newBridge()
	_, err := LayerLoopbackIntoPluginDir(b, nil, "")
	if err == nil {
		t.Fatal("expected error for nil plugin dir")
	}
}

// ---------------------------------------------------------------------------
// Env helpers
// ---------------------------------------------------------------------------

func TestTokenEnv(t *testing.T) {
	b := newBridge()
	env := TokenEnv(b)
	if env[ENVTokenVar] != b.token {
		t.Fatalf("TokenEnv should contain token under %q", ENVTokenVar)
	}
}

func TestEnvWithToken(t *testing.T) {
	b := newBridge()
	base := map[string]string{"SOME_VAR": "val"}
	result := EnvWithToken(b, base)
	if result["SOME_VAR"] != "val" {
		t.Fatal("base env should be preserved")
	}
	if result[ENVTokenVar] != b.token {
		t.Fatalf("token env should be set, got %v", result[ENVTokenVar])
	}
}

func TestENVTokenPlaceholderDoesNotContainLiteral(t *testing.T) {
	b := newBridge()
	cfg, _ := ClaudeConfigFile(b, "")
	// The file must NOT contain the token literal but MUST contain the placeholder.
	if strings.Contains(cfg, b.token) {
		t.Fatal("config file must not contain literal token")
	}
	if !strings.Contains(cfg, ENVTokenPlaceholder) {
		t.Fatalf("config file must contain env placeholder %q", ENVTokenPlaceholder)
	}
}

func TestMCPConfigFileWrittenToPluginRoot(t *testing.T) {
	tmp := t.TempDir()
	r := makeResolvedWithDir(t, "bar", "1.0.0", "workspace")
	pd, err := GeneratePluginDir([]*ResolvedSkill{r}, "run-mcp-file", tmp, "", "")
	if err != nil {
		t.Fatal(err)
	}
	b := newBridge()
	mcpPath, err := LayerLoopbackIntoPluginDir(b, pd, "")
	if err != nil {
		t.Fatal(err)
	}
	expectedPath := filepath.Join(pd.PluginRoot, ".mcp.json")
	if mcpPath != expectedPath {
		t.Fatalf("returned path %q != expected %q", mcpPath, expectedPath)
	}
}
