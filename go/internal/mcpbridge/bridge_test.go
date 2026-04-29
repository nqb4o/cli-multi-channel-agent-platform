package mcpbridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test helpers

// testdataDir returns the absolute path to the testdata directory so tests
// can be run from any working directory.
func testdataDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "testdata", "skills_with_mcp")
}

// twoFixtureSkills returns the two fixture skill manifests (web-search and
// trend-analysis) with workdirs pointing at the testdata subdirectories.
func twoFixtureSkills() []SkillManifestForBridge {
	td := testdataDir()
	return []SkillManifestForBridge{
		{
			Name:       "web-search",
			MCPEnabled: true,
			MCPCommand: []string{"python3", "mcp_server.py"},
			WorkDir:    filepath.Join(td, "web-search"),
		},
		{
			Name:       "trend-analysis",
			MCPEnabled: true,
			MCPCommand: []string{"python3", "mcp_server.py"},
			WorkDir:    filepath.Join(td, "trend-analysis"),
		},
	}
}

func testScope() McpScope {
	return McpScope{
		UserID:    "user-001",
		ChannelID: "chan-001",
		SessionID: "sess-001",
		AgentID:   "agent-001",
	}
}

func startBridge(t *testing.T, skills []SkillManifestForBridge) *McpBridge {
	t.Helper()
	mgr := &McpBridgeManager{}
	bridge, err := mgr.Start(skills, testScope(), "run-test-001", LoopbackHost, 0)
	require.NoError(t, err)
	t.Cleanup(bridge.Stop)
	return bridge
}

// doPost sends a POST to bridge.URL with the given body and Authorization header.
func doPost(t *testing.T, bridge *McpBridge, body []byte, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, bridge.URL, bytes.NewReader(body))
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func jsonRPCBody(t *testing.T, id any, method string, params map[string]any) []byte {
	t.Helper()
	req := RPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	b, err := MarshalRequest(req)
	require.NoError(t, err)
	return b
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return b
}

func parseRPCResp(t *testing.T, body []byte) RPCResponse {
	t.Helper()
	r, err := ParseResponse(body)
	require.NoError(t, err)
	return r
}

// ---------------------------------------------------------------------------
// McpBridgeManager.Start tests

func TestStart_BindsToLoopback(t *testing.T) {
	bridge := startBridge(t, nil)
	assert.True(t, strings.HasPrefix(bridge.URL, "http://127.0.0.1:"))
	assert.Greater(t, bridge.Port, 0)
}

func TestStart_RefusesNonLoopbackHost(t *testing.T) {
	mgr := &McpBridgeManager{}
	_, err := mgr.Start(nil, testScope(), "run-x", "0.0.0.0", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "127.0.0.1")
}

func TestStart_URLHasMCPPath(t *testing.T) {
	bridge := startBridge(t, nil)
	assert.True(t, strings.HasSuffix(bridge.URL, MCPPath))
}

func TestStart_TokenIs64HexChars(t *testing.T) {
	bridge := startBridge(t, nil)
	assert.Len(t, bridge.Token, 64)
	for _, c := range bridge.Token {
		assert.True(t, (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'),
			"token char %q is not lowercase hex", c)
	}
}

func TestStart_ScopeIsPreserved(t *testing.T) {
	scope := McpScope{
		UserID:    "u-123",
		ChannelID: "c-456",
		SessionID: "s-789",
		AgentID:   "a-abc",
	}
	mgr := &McpBridgeManager{}
	bridge, err := mgr.Start(nil, scope, "run-scope", LoopbackHost, 0)
	require.NoError(t, err)
	defer bridge.Stop()
	assert.Equal(t, scope, bridge.Scope)
}

func TestStart_EnvOverlayContainsToken(t *testing.T) {
	bridge := startBridge(t, nil)
	env := bridge.EnvOverlay()
	assert.Equal(t, bridge.Token, env[ENVTokenVar])
}

// ---------------------------------------------------------------------------
// Health endpoint

func TestHealthEndpoint_ReturnsOKNoAuth(t *testing.T) {
	bridge := startBridge(t, nil)
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", bridge.Port))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// Authentication tests

func TestAuth_MissingBearerReturns401(t *testing.T) {
	bridge := startBridge(t, nil)
	req, _ := http.NewRequest(http.MethodPost, bridge.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAuth_WrongBearerReturns401(t *testing.T) {
	bridge := startBridge(t, nil)
	body := jsonRPCBody(t, 1, "initialize", nil)
	resp := doPost(t, bridge, body, "wrong-token")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAuth_CorrectBearerReturns200(t *testing.T) {
	bridge := startBridge(t, nil)
	body := jsonRPCBody(t, 1, "initialize", nil)
	resp := doPost(t, bridge, body, bridge.Token)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// JSON-RPC: initialize

func TestInitialize_ReturnsMCPVersion(t *testing.T) {
	bridge := startBridge(t, nil)
	body := jsonRPCBody(t, 1, "initialize", nil)
	resp := doPost(t, bridge, body, bridge.Token)
	raw := readBody(t, resp)
	r := parseRPCResp(t, raw)
	require.Nil(t, r.Error)
	result, ok := r.Result.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, MCPProtocolVersion, result["protocolVersion"])
}

func TestInitialize_ResponseIDMatchesRequest(t *testing.T) {
	bridge := startBridge(t, nil)
	body := jsonRPCBody(t, 42, "initialize", nil)
	resp := doPost(t, bridge, body, bridge.Token)
	raw := readBody(t, resp)
	r := parseRPCResp(t, raw)
	// ID comes back as json.Number from UseNumber decoder — compare as string.
	idStr := fmt.Sprintf("%v", r.ID)
	assert.Equal(t, "42", idStr)
}

// ---------------------------------------------------------------------------
// JSON-RPC: tools/list

func TestToolsList_WithTwoSkillsExposesPrefixedTools(t *testing.T) {
	if testing.Short() {
		t.Skip("requires python3 subprocess")
	}
	bridge := startBridge(t, twoFixtureSkills())

	body := jsonRPCBody(t, 2, "tools/list", nil)
	resp := doPost(t, bridge, body, bridge.Token)
	raw := readBody(t, resp)
	r := parseRPCResp(t, raw)
	require.Nil(t, r.Error, "unexpected RPC error: %v", r.Error)

	result, ok := r.Result.(map[string]any)
	require.True(t, ok)
	toolsRaw, ok := result["tools"].([]any)
	require.True(t, ok)

	names := make([]string, 0, len(toolsRaw))
	for _, t := range toolsRaw {
		if m, ok := t.(map[string]any); ok {
			if n, ok := m["name"].(string); ok {
				names = append(names, n)
			}
		}
	}

	assert.Contains(t, names, "web-search.search")
	assert.Contains(t, names, "trend-analysis.analyze_trend")
	assert.Contains(t, names, "trend-analysis.summary")
}

func TestToolsList_EmptySkillsReturnsEmptyList(t *testing.T) {
	bridge := startBridge(t, nil)
	body := jsonRPCBody(t, 3, "tools/list", nil)
	resp := doPost(t, bridge, body, bridge.Token)
	raw := readBody(t, resp)
	r := parseRPCResp(t, raw)
	require.Nil(t, r.Error)
	result, _ := r.Result.(map[string]any)
	tools, _ := result["tools"].([]any)
	assert.Empty(t, tools)
}

// ---------------------------------------------------------------------------
// JSON-RPC: tools/call

func TestToolsCall_WebSearchReturnsResult(t *testing.T) {
	if testing.Short() {
		t.Skip("requires python3 subprocess")
	}
	bridge := startBridge(t, twoFixtureSkills())

	params := map[string]any{
		"name":      "web-search.search",
		"arguments": map[string]any{"query": "golang testing"},
	}
	body := jsonRPCBody(t, 10, "tools/call", params)
	resp := doPost(t, bridge, body, bridge.Token)
	raw := readBody(t, resp)
	r := parseRPCResp(t, raw)
	require.Nil(t, r.Error, "unexpected RPC error: %v", r.Error)
	assert.NotNil(t, r.Result)
}

func TestToolsCall_TrendAnalyzeReturnsResult(t *testing.T) {
	if testing.Short() {
		t.Skip("requires python3 subprocess")
	}
	bridge := startBridge(t, twoFixtureSkills())

	params := map[string]any{
		"name":      "trend-analysis.analyze_trend",
		"arguments": map[string]any{"topic": "AI", "period": "1w"},
	}
	body := jsonRPCBody(t, 11, "tools/call", params)
	resp := doPost(t, bridge, body, bridge.Token)
	raw := readBody(t, resp)
	r := parseRPCResp(t, raw)
	require.Nil(t, r.Error)
	assert.NotNil(t, r.Result)
}

func TestToolsCall_MissingDotInNameReturnsError(t *testing.T) {
	bridge := startBridge(t, nil)
	params := map[string]any{"name": "nodot", "arguments": map[string]any{}}
	body := jsonRPCBody(t, 20, "tools/call", params)
	resp := doPost(t, bridge, body, bridge.Token)
	raw := readBody(t, resp)
	r := parseRPCResp(t, raw)
	require.NotNil(t, r.Error)
	assert.Equal(t, http.StatusOK, resp.StatusCode) // JSON-RPC errors still 200
}

func TestToolsCall_UnknownSkillReturnsError(t *testing.T) {
	bridge := startBridge(t, nil)
	params := map[string]any{"name": "no-such-skill.tool", "arguments": map[string]any{}}
	body := jsonRPCBody(t, 21, "tools/call", params)
	resp := doPost(t, bridge, body, bridge.Token)
	raw := readBody(t, resp)
	r := parseRPCResp(t, raw)
	require.NotNil(t, r.Error)
}

// ---------------------------------------------------------------------------
// Audit log

func TestAuditLog_SuccessfulToolCallWritesEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("requires python3 subprocess")
	}
	bridge := startBridge(t, twoFixtureSkills())

	params := map[string]any{
		"name":      "web-search.search",
		"arguments": map[string]any{"query": "audit-test"},
	}
	body := jsonRPCBody(t, 30, "tools/call", params)
	resp := doPost(t, bridge, body, bridge.Token)
	readBody(t, resp)

	require.Len(t, bridge.ToolCalls, 1)
	entry := bridge.ToolCalls[0]
	assert.Equal(t, "run-test-001", entry.RunID)
	assert.Equal(t, "user-001", entry.UserID)
	assert.Equal(t, "web-search", entry.SkillSlug)
	assert.Equal(t, "search", entry.Tool)
	assert.Len(t, entry.ArgsHash, 16)
	assert.True(t, entry.OK)
	assert.Empty(t, entry.Error)
	assert.Greater(t, entry.LatencyMs, 0.0)
}

func TestAuditLog_FailedToolCallWritesEntry(t *testing.T) {
	bridge := startBridge(t, nil)
	params := map[string]any{"name": "no-skill.tool", "arguments": map[string]any{}}
	body := jsonRPCBody(t, 31, "tools/call", params)
	resp := doPost(t, bridge, body, bridge.Token)
	readBody(t, resp)

	require.Len(t, bridge.ToolCalls, 1)
	entry := bridge.ToolCalls[0]
	assert.False(t, entry.OK)
	assert.NotEmpty(t, entry.Error)
}

// ---------------------------------------------------------------------------
// Scope headers echoed

func TestScopeHeaders_EchoedOnInitialize(t *testing.T) {
	bridge := startBridge(t, nil)
	body := jsonRPCBody(t, 1, "initialize", nil)
	resp := doPost(t, bridge, body, bridge.Token)
	defer resp.Body.Close()
	assert.Equal(t, "user-001", resp.Header.Get("x-openclaw-account-id"))
	assert.Equal(t, "chan-001", resp.Header.Get("x-openclaw-message-channel"))
	assert.Equal(t, "sess-001", resp.Header.Get("x-session-key"))
	assert.Equal(t, "agent-001", resp.Header.Get("x-openclaw-agent-id"))
}

// ---------------------------------------------------------------------------
// Unknown method

func TestUnknownMethod_ReturnsMethodNotFound(t *testing.T) {
	bridge := startBridge(t, nil)
	body := jsonRPCBody(t, 99, "no/such/method", nil)
	resp := doPost(t, bridge, body, bridge.Token)
	raw := readBody(t, resp)
	r := parseRPCResp(t, raw)
	require.NotNil(t, r.Error)
	assert.Equal(t, -32601, r.Error.Code)
}

// ---------------------------------------------------------------------------
// Stop

func TestStop_ReapsChildrenWithin1s(t *testing.T) {
	if testing.Short() {
		t.Skip("requires python3 subprocess")
	}
	mgr := &McpBridgeManager{}
	bridge, err := mgr.Start(twoFixtureSkills(), testScope(), "run-stop", LoopbackHost, 0)
	require.NoError(t, err)

	// Start children via tools/list.
	body := jsonRPCBody(t, 1, "tools/list", nil)
	resp := doPost(t, bridge, body, bridge.Token)
	readBody(t, resp)

	start := time.Now()
	bridge.Stop()
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 2*time.Second, "Stop should reap children within ~1s")
}

func TestStop_Idempotent(t *testing.T) {
	bridge := startBridge(t, nil)
	bridge.Stop()
	bridge.Stop() // should not panic
}

// ---------------------------------------------------------------------------
// Non-POST on /mcp

func TestNonPOST_MethodNotAllowed(t *testing.T) {
	bridge := startBridge(t, nil)
	req, _ := http.NewRequest(http.MethodGet, bridge.URL, nil)
	req.Header.Set("Authorization", "Bearer "+bridge.Token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// hashArgs

func TestHashArgs_IsDeterministic(t *testing.T) {
	args := map[string]any{"b": 2, "a": 1}
	h1 := hashArgs(args)
	h2 := hashArgs(args)
	assert.Equal(t, h1, h2)
	assert.Len(t, h1, 16)
}

// ---------------------------------------------------------------------------
// JSON-RPC parse helpers

func TestParseResponse_ParsesResult(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":1,"result":{"foo":"bar"}}`)
	r, err := ParseResponse(raw)
	require.NoError(t, err)
	assert.Nil(t, r.Error)
	m, ok := r.Result.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "bar", m["foo"])
}

func TestParseResponse_ParsesError(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"not found"}}`)
	r, err := ParseResponse(raw)
	require.NoError(t, err)
	require.NotNil(t, r.Error)
	assert.Equal(t, -32601, r.Error.Code)
}

// ---------------------------------------------------------------------------
// End-to-end flow: tools/list → tools/call

func TestEndToEnd_ToolsListThenCall(t *testing.T) {
	if testing.Short() {
		t.Skip("requires python3 subprocess")
	}
	bridge := startBridge(t, twoFixtureSkills())

	// Step 1: initialize
	initBody := jsonRPCBody(t, 1, "initialize", nil)
	initResp := doPost(t, bridge, initBody, bridge.Token)
	raw := readBody(t, initResp)
	initR := parseRPCResp(t, raw)
	require.Nil(t, initR.Error)

	// Step 2: list tools
	listBody := jsonRPCBody(t, 2, "tools/list", nil)
	listResp := doPost(t, bridge, listBody, bridge.Token)
	raw = readBody(t, listResp)
	listR := parseRPCResp(t, raw)
	require.Nil(t, listR.Error)

	result, _ := listR.Result.(map[string]any)
	toolsRaw, _ := result["tools"].([]any)
	assert.GreaterOrEqual(t, len(toolsRaw), 3, "expected at least 3 tools across 2 skills")

	// Step 3: call web-search.search
	callBody := jsonRPCBody(t, 3, "tools/call", map[string]any{
		"name":      "web-search.search",
		"arguments": map[string]any{"query": "e2e test"},
	})
	callResp := doPost(t, bridge, callBody, bridge.Token)
	raw = readBody(t, callResp)
	callR := parseRPCResp(t, raw)
	require.Nil(t, callR.Error, "call error: %v", callR.Error)
	assert.NotNil(t, callR.Result)

	// Step 4: call trend-analysis.summary
	summaryBody := jsonRPCBody(t, 4, "tools/call", map[string]any{
		"name":      "trend-analysis.summary",
		"arguments": map[string]any{"topic": "climate"},
	})
	summaryResp := doPost(t, bridge, summaryBody, bridge.Token)
	raw = readBody(t, summaryResp)
	summaryR := parseRPCResp(t, raw)
	require.Nil(t, summaryR.Error)
	assert.NotNil(t, summaryR.Result)

	// Audit log should have 2 entries.
	assert.Len(t, bridge.ToolCalls, 2)
}

// ---------------------------------------------------------------------------
// Content-type check

func TestResponseContentType_IsApplicationJSON(t *testing.T) {
	bridge := startBridge(t, nil)
	body := jsonRPCBody(t, 1, "initialize", nil)
	resp := doPost(t, bridge, body, bridge.Token)
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	assert.True(t, strings.HasPrefix(ct, "application/json"), "got %q", ct)
}

// ---------------------------------------------------------------------------
// JSON-RPC: invalid request body

func TestInvalidJSON_ReturnsBadRequest(t *testing.T) {
	bridge := startBridge(t, nil)
	req, _ := http.NewRequest(http.MethodPost, bridge.URL, strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+bridge.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	// Could be 400 (bad request) or 200 with JSON-RPC error; either is acceptable.
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		var r RPCResponse
		_ = json.Unmarshal(body, &r)
		assert.NotNil(t, r.Error)
	} else {
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	}
}
