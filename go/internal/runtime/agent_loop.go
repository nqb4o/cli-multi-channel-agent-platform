package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/openclaw/agent-platform/internal/clibackend"
	"github.com/openclaw/agent-platform/internal/mcpbridge"
	"github.com/openclaw/agent-platform/internal/skills"
)

// claudeSkillPluginDirEnv is the env-var the Claude CLI reads to find the
// per-run plugin directory (mirrors CLAUDE_SKILL_PLUGIN_DIR in the Python
// tree).
const claudeSkillPluginDirEnv = "CLAUDE_SKILL_PLUGIN_DIR"

// bridgeAdapter wraps *mcpbridge.McpBridge to satisfy the skills.MCPBridge
// interface. skills.MCPBridge requires URL() and Token() as methods but
// mcpbridge.McpBridge exposes them as public fields (different design choice).
// This adapter is internal — callers just pass *mcpbridge.McpBridge through
// the resolveSkills/maybeStartBridge helpers.
type bridgeAdapter struct {
	b *mcpbridge.McpBridge
}

func (a *bridgeAdapter) URL() string                  { return a.b.URL }
func (a *bridgeAdapter) Token() string                { return a.b.Token }
func (a *bridgeAdapter) EnvOverlay() map[string]string { return a.b.EnvOverlay() }

// RunMessage is the inbound message payload passed into a `run` request.
type RunMessage struct {
	Text   string
	Images []string
}

// RunRequest is the parameters block of the JSON-RPC `run` method.
//
// Mirrors services/runtime/src/runtime/agent_loop.py RunRequest. Field
// names + JSON shapes are wire-compatible.
type RunRequest struct {
	UserID       uuid.UUID
	AgentID      uuid.UUID
	ChannelID    uuid.UUID
	ThreadID     string
	Message      RunMessage
	RunID        string
	WorkspaceDir string // optional — falls back to the daemon's --workspace flag
}

// RunResult is a successful turn result.
type RunResult struct {
	Text      string
	Telemetry map[string]any
	Errors    []string
}

// RunErrorResult is the structured error returned to the orchestrator.
//
// This matches the JSON-RPC error envelope shape from the F05 brief:
// the outer JSON-RPC envelope is still a normal Response with ok=false
// in result; the application error lives inside.
type RunErrorResult struct {
	ErrorClass string
	Message    string
	UserFacing string
	Details    map[string]any
}

// defaultUserFacing maps each error class to a user-facing string.
// Mirrors the Python tree's _DEFAULT_USER_FACING.
var defaultUserFacing = map[string]string{
	"auth_expired":     "Your provider login has expired. Please re-authenticate.",
	"rate_limit":       "The provider hit a rate limit. Try again in a moment.",
	"transient":        "The provider hiccuped. Try again in a moment.",
	"unknown_provider": "This agent is configured for a provider that is not available.",
	"config_error":     "This agent's configuration could not be loaded.",
	"internal":         "Something went wrong on the runtime side. Please try again.",
}

// userFacing returns the canonical string for a class, falling back to
// the "internal" message for unknown classes.
func userFacing(class string) string {
	if msg, ok := defaultUserFacing[class]; ok {
		return msg
	}
	return defaultUserFacing["internal"]
}

// AgentLoop is the stateless per-turn orchestrator.
//
// One AgentLoop is constructed per Daemon; Run is called concurrently from
// the JSON-RPC dispatcher. The loop holds no per-turn state — every call
// passes a RunRequest and the loop reads/writes through its injected
// dependencies (config, registry, DAL).
type AgentLoop struct {
	cfg          *AgentConfig
	registry     *clibackend.BackendRegistry
	sessionDal   SessionDal
	workspaceDir string
	logger       *slog.Logger
	// chain is non-nil when HasFallbackChain() is true. Wave G-D wires this.
	chain *ProviderChain
	// F09/F11: skill roots and MCP bridge manager.
	// nil/empty skillRoots → legacy behaviour (no skills, no bridge).
	skillRoots []skills.SkillRoot
	mcpManager *mcpbridge.McpBridgeManager
}

// AgentLoopOption configures an AgentLoop.
type AgentLoopOption func(*AgentLoop)

// WithWorkspaceDir sets the default workspace directory used when the
// inbound RunRequest does not carry one.
func WithWorkspaceDir(dir string) AgentLoopOption {
	return func(l *AgentLoop) { l.workspaceDir = dir }
}

// WithProviderChain sets the fallback chain. When set and HasFallbackChain()
// is true the loop uses runWithFallback instead of the single-provider path.
// Wave G-D wires this automatically based on the agent config.
func WithProviderChain(chain *ProviderChain) AgentLoopOption {
	return func(l *AgentLoop) { l.chain = chain }
}

// WithLogger overrides the slog.Logger the loop uses (defaults to
// slog.Default).
func WithLogger(logger *slog.Logger) AgentLoopOption {
	return func(l *AgentLoop) { l.logger = logger }
}

// WithSkillRoots sets the F09 skill root directories.  When nil or empty the
// loop takes the legacy no-skills path (existing tests unaffected).
func WithSkillRoots(roots []skills.SkillRoot) AgentLoopOption {
	return func(l *AgentLoop) { l.skillRoots = roots }
}

// WithMcpManager overrides the F11 MCP bridge manager.  When nil a default
// manager is used.
func WithMcpManager(mgr *mcpbridge.McpBridgeManager) AgentLoopOption {
	return func(l *AgentLoop) { l.mcpManager = mgr }
}

// NewAgentLoop builds an AgentLoop. Callers pass the parsed agent config,
// the populated backend registry, and a SessionDal (in-memory or DB-backed).
func NewAgentLoop(
	cfg *AgentConfig,
	registry *clibackend.BackendRegistry,
	dal SessionDal,
	opts ...AgentLoopOption,
) *AgentLoop {
	l := &AgentLoop{
		cfg:        cfg,
		registry:   registry,
		sessionDal: dal,
		logger:     slog.Default(),
		mcpManager: &mcpbridge.McpBridgeManager{},
	}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Run executes one agent turn end-to-end.
//
// Returns either a RunResult (success) or a RunErrorResult (classified
// failure). Both the Python tree and this Go port distinguish those by
// type so the daemon's envelope layer never has to inspect a sentinel
// error code.
//
// When HasFallbackChain() is true and a ProviderChain has been wired via
// WithProviderChain, the loop delegates to runWithFallback. Otherwise it
// uses the single-provider path.
func (l *AgentLoop) Run(ctx context.Context, req RunRequest) (*RunResult, *RunErrorResult) {
	if l.cfg.HasFallbackChain() && l.chain != nil {
		return l.runWithFallback(ctx, req)
	}
	return l.runSingle(ctx, req)
}

// runSingle is the original single-provider path.
func (l *AgentLoop) runSingle(ctx context.Context, req RunRequest) (*RunResult, *RunErrorResult) {
	started := time.Now()
	provider, err := l.cfg.PrimaryProvider()
	if err != nil {
		return nil, &RunErrorResult{
			ErrorClass: "config_error",
			Message:    err.Error(),
			UserFacing: userFacing("config_error"),
			Details:    map[string]any{"stage": "config"},
		}
	}

	// 4. Look up cached session id + bootstrap-injection state.
	cachedSID, _, err := l.sessionDal.LookupSessionID(
		ctx, req.UserID, req.ChannelID, req.ThreadID, provider.ID,
	)
	if err != nil {
		l.logger.Error("session DAL lookup failed", "err", err)
		return nil, &RunErrorResult{
			ErrorClass: "internal",
			Message:    fmt.Sprintf("session lookup failed: %v", err),
			UserFacing: userFacing("internal"),
			Details:    map[string]any{"stage": "session_lookup"},
		}
	}
	initialized, err := l.sessionDal.IsInitialized(
		ctx, req.UserID, req.ChannelID, req.ThreadID, provider.ID,
	)
	if err != nil {
		l.logger.Error("session DAL is_initialized failed", "err", err)
		return nil, &RunErrorResult{
			ErrorClass: "internal",
			Message:    fmt.Sprintf("session lookup failed: %v", err),
			UserFacing: userFacing("internal"),
			Details:    map[string]any{"stage": "session_lookup"},
		}
	}

	// 3. System prompt — bootstrap files only on the first turn.
	var bootstrapFiles []BootstrapFile
	if !initialized {
		ws := req.WorkspaceDir
		if ws == "" {
			ws = l.workspaceDir
		}
		bootstrapFiles = LoadBootstrapFiles(ws)
	}
	systemPrompt := BuildSystemPrompt(l.cfg.Identity, bootstrapFiles, "")

	// F09 — resolve skills (legacy flow returns empty when no roots).
	resolved, pluginDir := l.resolveSkills(provider.ID, req)
	defer cleanupPluginDir(pluginDir)

	// F11 — start the per-turn MCP bridge iff at least one resolved skill
	// exposes an MCP server. Otherwise keep mcp_config=nil (no port opened).
	var bridge *mcpbridge.McpBridge
	var bridgeWarning string
	if len(l.skillRoots) > 0 {
		bridge, err = l.maybeStartBridge(ctx, resolved, req)
		if err != nil {
			l.logger.Warn("MCP bridge failed to start", "err", err)
			bridgeWarning = fmt.Sprintf("mcp_bridge_start_failed: %v", err)
			bridge = nil
		}
	}
	if bridge != nil {
		defer bridge.Stop()
	}

	// Build provider-specific mcp_config + extra_env.
	mcpConfig, extraEnv := buildProviderOverlay(provider.ID, bridge, pluginDir, l.logger)

	// 5. Build CliTurnInput.
	turnInput := clibackend.CliTurnInput{
		SystemPrompt: systemPrompt,
		UserPrompt:   req.Message.Text,
		Images:       append([]string(nil), req.Message.Images...),
		ExtraEnv:     extraEnv,
		MCPConfig:    mcpConfig,
	}
	if cachedSID != "" {
		sid := cachedSID
		turnInput.SessionID = &sid
	}
	if provider.Model != "" {
		m := provider.Model
		turnInput.Model = &m
	}
	if req.RunID != "" {
		rid := req.RunID
		turnInput.RunID = &rid
	}

	// 6. Backend lookup.
	backend, err := l.registry.Get(provider.ID)
	if err != nil {
		if errors.Is(err, clibackend.ErrUnknownBackend) {
			l.logger.Error("unknown provider", "id", provider.ID)
			return nil, &RunErrorResult{
				ErrorClass: "unknown_provider",
				Message:    err.Error(),
				UserFacing: userFacing("unknown_provider"),
				Details:    map[string]any{"provider": provider.ID},
			}
		}
		l.logger.Error("backend lookup failed", "err", err)
		return nil, &RunErrorResult{
			ErrorClass: "internal",
			Message:    fmt.Sprintf("backend lookup failed: %v", err),
			UserFacing: userFacing("internal"),
			Details:    map[string]any{"stage": "registry_get", "provider": provider.ID},
		}
	}

	// 7. Execute the CLI turn.
	out, cliErr := safeTurn(ctx, backend, turnInput)
	latencyMS := int(time.Since(started) / time.Millisecond)

	// safeTurn wraps panics → cliErr with class=internal so the daemon never
	// crashes on a misbehaving backend. The Go runtime drops the ADK harness
	// (was a Python-only spike) so this is the only safety net we keep.
	if cliErr != nil {
		class := string(cliErr.Class)
		if class == "" {
			class = "internal"
		}
		return nil, &RunErrorResult{
			ErrorClass: class,
			Message:    cliErr.Message,
			UserFacing: userFacing(class),
			Details: map[string]any{
				"provider":    provider.ID,
				"exit_code":   cliErr.ExitCode,
				"stderr_tail": cliErr.StderrTail,
				"latency_ms":  latencyMS,
			},
		}
	}
	if out == nil {
		// Backend returned nil/nil — invalid contract; treat as internal.
		return nil, &RunErrorResult{
			ErrorClass: "internal",
			Message:    "backend returned nil output and nil error",
			UserFacing: userFacing("internal"),
			Details:    map[string]any{"provider": provider.ID, "latency_ms": latencyMS},
		}
	}

	// 9. Success — persist new session id, return text + telemetry.
	if out.NewSessionID != nil && *out.NewSessionID != "" {
		if err := l.sessionDal.RecordTurn(
			ctx, req.UserID, req.ChannelID, req.ThreadID, provider.ID, *out.NewSessionID,
		); err != nil {
			// We still return the user-facing reply — losing the session
			// cache only means the next turn will be a fresh conversation.
			l.logger.Error("failed to persist session id", "err", err)
		}
	}

	usage := out.Usage
	if usage == nil {
		usage = map[string]any{}
	}
	telemetry := map[string]any{
		"provider":           provider.ID,
		"model":              orEmpty(provider.Model),
		"cli_session_id":     ptrString(out.NewSessionID),
		"latency_ms":         latencyMS,
		"usage":              usage,
		"bootstrap_injected": !initialized,
		"adk_harness":        false, // Go runtime does not ship the ADK harness.
		"run_id":             req.RunID,
	}
	attachSkillTelemetry(telemetry, resolved, bridge, bridgeWarning)

	return &RunResult{
		Text:      out.Text,
		Telemetry: telemetry,
		Errors:    nil,
	}, nil
}

// runWithFallback executes one agent turn using the multi-provider fallback
// chain. It builds a per-provider session-aware TurnDispatcher and hands
// control to ProviderChain.Turn.
//
// On overall success the telemetry carries a "fallback_attempts" key with the
// full attempt list. On failure it surfaces the terminal error with
// "fallback_chain_exhausted": true.
func (l *AgentLoop) runWithFallback(ctx context.Context, req RunRequest) (*RunResult, *RunErrorResult) {
	started := time.Now()

	// Build the system prompt based on whether the first-turn bootstrap has
	// been injected for the primary provider.
	primary, err := l.cfg.PrimaryProvider()
	if err != nil {
		return nil, &RunErrorResult{
			ErrorClass: "config_error",
			Message:    err.Error(),
			UserFacing: userFacing("config_error"),
			Details:    map[string]any{"stage": "config"},
		}
	}
	initialized, err := l.sessionDal.IsInitialized(
		ctx, req.UserID, req.ChannelID, req.ThreadID, primary.ID,
	)
	if err != nil {
		l.logger.Error("session DAL is_initialized failed", "err", err)
		return nil, &RunErrorResult{
			ErrorClass: "internal",
			Message:    fmt.Sprintf("session lookup failed: %v", err),
			UserFacing: userFacing("internal"),
			Details:    map[string]any{"stage": "session_lookup"},
		}
	}
	var bootstrapFiles []BootstrapFile
	if !initialized {
		ws := req.WorkspaceDir
		if ws == "" {
			ws = l.workspaceDir
		}
		bootstrapFiles = LoadBootstrapFiles(ws)
	}
	systemPrompt := BuildSystemPrompt(l.cfg.Identity, bootstrapFiles, "")

	// Base turn input — session ID is resolved per-provider inside the dispatcher.
	baseInput := clibackend.CliTurnInput{
		SystemPrompt: systemPrompt,
		UserPrompt:   req.Message.Text,
		Images:       append([]string(nil), req.Message.Images...),
		ExtraEnv:     map[string]string{},
	}
	if req.RunID != "" {
		rid := req.RunID
		baseInput.RunID = &rid
	}

	// Per-provider TurnDispatcher. Hydrates session ID + model from the config,
	// then delegates to the registered backend.
	dispatcher := func(dctx context.Context, entry ProviderEntry, inp clibackend.CliTurnInput) (*clibackend.CliTurnOutput, *clibackend.CliTurnError) {
		// Session ID is per-provider.
		cachedSID, _, serr := l.sessionDal.LookupSessionID(
			dctx, req.UserID, req.ChannelID, req.ThreadID, entry.ID,
		)
		if serr != nil {
			l.logger.Error("session DAL lookup failed in fallback", "provider", entry.ID, "err", serr)
			return nil, &clibackend.CliTurnError{
				Class:   clibackend.Transient,
				Message: fmt.Sprintf("session lookup failed: %v", serr),
			}
		}
		inp.SessionID = nil
		if cachedSID != "" {
			sid := cachedSID
			inp.SessionID = &sid
		}
		if entry.Model != "" {
			m := entry.Model
			inp.Model = &m
		} else {
			inp.Model = nil
		}

		backend, berr := l.registry.Get(entry.ID)
		if berr != nil {
			if errors.Is(berr, clibackend.ErrUnknownBackend) {
				return nil, &clibackend.CliTurnError{
					Class:   clibackend.Unknown,
					Message: berr.Error(),
				}
			}
			return nil, &clibackend.CliTurnError{
				Class:   clibackend.Transient,
				Message: fmt.Sprintf("backend lookup failed: %v", berr),
			}
		}
		return safeTurn(dctx, backend, inp)
	}

	chainResult := l.chain.Turn(ctx, baseInput, dispatcher)
	latencyMS := int(time.Since(started) / time.Millisecond)

	if !chainResult.Succeeded() {
		class := string(chainResult.Error.Class)
		if class == "" {
			class = "internal"
		}
		return nil, &RunErrorResult{
			ErrorClass: class,
			Message:    chainResult.Error.Message,
			UserFacing: userFacing(class),
			Details: map[string]any{
				"fallback_chain_exhausted": true,
				"attempts":                chainResult.Attempts,
				"latency_ms":              latencyMS,
			},
		}
	}

	out := chainResult.Output
	selectedID := chainResult.SelectedProviderID

	// Persist session id for the winning provider.
	if out.NewSessionID != nil && *out.NewSessionID != "" {
		if rerr := l.sessionDal.RecordTurn(
			ctx, req.UserID, req.ChannelID, req.ThreadID, selectedID, *out.NewSessionID,
		); rerr != nil {
			l.logger.Error("failed to persist session id (fallback)", "provider", selectedID, "err", rerr)
		}
	}

	usage := out.Usage
	if usage == nil {
		usage = map[string]any{}
	}

	// Find the selected provider config (for model name).
	modelName := ""
	for _, p := range l.cfg.Providers {
		if p.ID == selectedID {
			modelName = p.Model
			break
		}
	}

	telemetry := map[string]any{
		"provider":           selectedID,
		"model":              orEmpty(modelName),
		"cli_session_id":     ptrString(out.NewSessionID),
		"latency_ms":         latencyMS,
		"usage":              usage,
		"bootstrap_injected": !initialized,
		"adk_harness":        false,
		"run_id":             req.RunID,
		"fallback_attempts":  chainResult.Attempts,
	}

	return &RunResult{
		Text:      out.Text,
		Telemetry: telemetry,
		Errors:    nil,
	}, nil
}

// ---------------------------------------------------------------------------
// F09 + F11 helpers
// ---------------------------------------------------------------------------

// resolveSkills resolves skills for one provider from the configured skill
// roots.  When skillRoots is nil/empty, it returns an empty ResolvedSkills
// and a nil PluginDir so callers can take the legacy no-bridge path.
//
// The plugin dir is only generated for the "claude-cli" provider: Claude
// reads skills from a per-run directory tree.
func (l *AgentLoop) resolveSkills(
	providerID string,
	req RunRequest,
) (*skills.ResolvedSkills, *skills.PluginDir) {
	if len(l.skillRoots) == 0 {
		empty := &skills.ResolvedSkills{}
		return empty, nil
	}

	loadResult := skills.LoadAll(l.skillRoots)
	for _, w := range loadResult.Warnings {
		l.logger.Info("skill load warning", "code", w.Code, "path", w.Path, "message", w.Message)
	}

	// Build env map from process environment for required_env checks.
	envMap := osEnvironMap()

	// requested comes from agent.yaml's skills list.
	var requested []string
	if len(l.cfg.Skills) > 0 {
		requested = l.cfg.Skills
	}
	resolved := skills.Resolve(loadResult.Skills, requested, envMap)
	result := &resolved

	var pluginDir *skills.PluginDir
	if providerID == "claude-cli" && len(resolved.Selected) > 0 {
		runID := req.RunID
		if runID == "" {
			runID = "no-run-id"
		}
		pd, err := skills.GeneratePluginDir(resolved.Selected, runID, "", "", "")
		if err != nil {
			l.logger.Warn("failed to generate Claude plugin dir", "err", err)
		} else {
			pluginDir = pd
		}
	}

	return result, pluginDir
}

// maybeStartBridge starts the F11 MCP loopback bridge when at least one
// resolved skill has an MCP server configured.  Returns nil when no MCP
// skills are present so the caller can take the no-bridge path.
func (l *AgentLoop) maybeStartBridge(
	ctx context.Context,
	resolved *skills.ResolvedSkills,
	req RunRequest,
) (*mcpbridge.McpBridge, error) {
	if resolved == nil || len(resolved.Selected) == 0 {
		return nil, nil
	}

	var mcpSkills []mcpbridge.SkillManifestForBridge
	for _, r := range resolved.Selected {
		if r.Manifest.Mcp.Enabled && len(r.Manifest.Mcp.Command) > 0 {
			mcpSkills = append(mcpSkills, mcpbridge.SkillManifestForBridge{
				Name:       r.Manifest.Name,
				MCPEnabled: true,
				MCPCommand: r.Manifest.Mcp.Command,
				WorkDir:    r.SkillDir,
			})
		}
	}
	if len(mcpSkills) == 0 {
		return nil, nil
	}

	runID := req.RunID
	if runID == "" {
		runID = "no-run-id"
	}
	scope := mcpbridge.McpScope{
		UserID:    req.UserID.String(),
		ChannelID: req.ChannelID.String(),
		SessionID: req.ThreadID,
		AgentID:   req.AgentID.String(),
	}
	return l.mcpManager.Start(mcpSkills, scope, runID, mcpbridge.LoopbackHost, 0)
}

// buildProviderOverlay returns (mcpConfig, extraEnv) for the given provider
// and bridge.  When bridge is nil it returns (nil, {}) so the backend takes
// the legacy no-MCP path.
func buildProviderOverlay(
	providerID string,
	bridge *mcpbridge.McpBridge,
	pluginDir *skills.PluginDir,
	logger *slog.Logger,
) (map[string]any, map[string]string) {
	if bridge == nil {
		return nil, map[string]string{}
	}

	ba := &bridgeAdapter{b: bridge}
	env := bridge.EnvOverlay()

	switch providerID {
	case "claude-cli":
		if pluginDir != nil {
			if _, err := skills.LayerLoopbackIntoPluginDir(ba, pluginDir, ""); err != nil {
				logger.Warn("failed to layer loopback into Claude plugin dir", "err", err)
			}
			env[claudeSkillPluginDirEnv] = pluginDir.Root
		}
		return nil, env // Claude reads .mcp.json from plugin dir

	case "codex-cli":
		return skills.CodexInlineConfig(ba, ""), env

	case "google-gemini-cli":
		return skills.GeminiSettingsPayload(ba, ""), env

	default:
		// Unknown provider — pass env overlay but no structured mcp_config.
		return nil, env
	}
}

// attachSkillTelemetry appends F09/F11 fields to the telemetry map.
func attachSkillTelemetry(
	telemetry map[string]any,
	resolved *skills.ResolvedSkills,
	bridge *mcpbridge.McpBridge,
	bridgeWarning string,
) {
	if resolved != nil && len(resolved.Selected) > 0 {
		slugs := make([]string, 0, len(resolved.Selected))
		for _, r := range resolved.Selected {
			slugs = append(slugs, r.Manifest.Name)
		}
		telemetry["skills_resolved"] = slugs
	}
	if bridge != nil {
		calls := make([]map[string]any, 0, len(bridge.ToolCalls))
		for _, entry := range bridge.ToolCalls {
			calls = append(calls, map[string]any{
				"run_id":     entry.RunID,
				"user_id":    entry.UserID,
				"skill_slug": entry.SkillSlug,
				"tool":       entry.Tool,
				"args_hash":  entry.ArgsHash,
				"latency_ms": entry.LatencyMs,
				"ok":         entry.OK,
				"timestamp":  entry.Timestamp,
				"error":      entry.Error,
			})
		}
		telemetry["mcp_tool_calls"] = calls
	}
	if bridgeWarning != "" {
		telemetry["mcp_warnings"] = []string{bridgeWarning}
	}
}

// cleanupPluginDir removes the per-run Claude plugin tree. Best-effort.
func cleanupPluginDir(pluginDir *skills.PluginDir) {
	if pluginDir == nil {
		return
	}
	_ = os.RemoveAll(pluginDir.Root)
}

// osEnvironMap returns the current process environment as a map.
func osEnvironMap() map[string]string {
	environ := os.Environ()
	out := make(map[string]string, len(environ))
	for _, kv := range environ {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			out[parts[0]] = parts[1]
		}
	}
	return out
}

// safeTurn calls backend.Turn and converts panics into an "internal" error
// result so a misbehaving backend cannot crash the daemon.
//
// In addition to wrapping panics, this is also the spot the Python tree
// catches generic exceptions and folds them into RunErrorResult. The Go
// equivalent: backends are not expected to return Go errors (the contract
// uses *CliTurnError only), but we still recover any panics they throw.
func safeTurn(ctx context.Context, backend clibackend.CliBackend, in clibackend.CliTurnInput) (out *clibackend.CliTurnOutput, errResult *clibackend.CliTurnError) {
	defer func() {
		if r := recover(); r != nil {
			out = nil
			errResult = &clibackend.CliTurnError{
				Class:   clibackend.ErrorClass("internal"),
				Message: fmt.Sprintf("backend panicked: %v", r),
			}
		}
	}()
	out, errResult = backend.Turn(ctx, in)
	return
}

// orEmpty returns "" for an empty model string so telemetry consumers see
// a consistent type (rather than a missing key vs. empty string).
func orEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return s
}

func ptrString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}
