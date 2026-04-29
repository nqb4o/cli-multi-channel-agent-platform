// Package runtime is the Go port of services/runtime/src/runtime/* —
// the JSON-RPC daemon that lives inside a per-user sandbox and drives the
// agent loop.
//
// The Go port intentionally drops three pieces from the Python tree:
//
//   - The ADK harness (RUNTIME_USE_ADK / google-adk wrap). It was a Python-only
//     Phase-0 spike and the Go runtime always takes the bare CLI path.
//   - F09 skill resolution + F11 MCP loopback bridge wiring. Wave G-D ports
//     those into the Go tree separately.
//   - F16 multi-provider fallback chain. The Go loop ships only the
//     single-provider path; Wave G-D extends it.
//
// The JSON-RPC envelope shapes (health/run/shutdown), the agent.yaml schema
// (minus skills/mcp wiring), and the bootstrap-file ordering are wire- and
// behaviour-compatible with the Python F05 implementation. The demo pipeline
// can swap the Python daemon binary for cmd/runtime-daemon and continue to
// work.
package runtime

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultAgentYAMLPath is the production location of the per-sandbox agent
// config. The orchestrator writes it before launching the daemon.
const DefaultAgentYAMLPath = "/home/user/agent.yaml"

// Phase-0 known top-level keys. Anything else is reported as a warning, not a
// hard error — Phase 1 (skills, MCP filters) will add fields here.
var knownTopLevel = map[string]struct{}{
	"identity":  {},
	"providers": {},
	"skills":    {},
	"mcp":       {},
}

// IdentityConfig is the agent identity block.
//
// Both fields are runtime hints — the daemon does not enforce them. The
// persona file is read by the bootstrap loader if present in the workspace.
type IdentityConfig struct {
	Name        string
	PersonaFile string // optional; relative to workspace
}

// ProviderConfig is one provider entry from agent.yaml. The id matches a
// CliBackend.ID() registered on the BackendRegistry.
type ProviderConfig struct {
	ID           string
	Model        string // optional override; "" means the CLI picks default
	FallbackOnly bool   // F16 — for now parsed but not consumed
}

// McpConfig is the MCP loopback bridge config. Phase 0 only carries the
// boolean hint; F11 wires it up.
type McpConfig struct {
	Bundle bool
}

// AgentConfig is the parsed contents of agent.yaml.
type AgentConfig struct {
	Identity  IdentityConfig
	Providers []ProviderConfig
	Skills    []string
	Mcp       McpConfig
	Raw       map[string]any
}

// PrimaryProvider returns the first non-fallback_only provider.
//
// Validation guarantees this exists, so the only way this returns an error is
// if AgentConfig was hand-constructed (skipping the parser).
func (c AgentConfig) PrimaryProvider() (ProviderConfig, error) {
	if len(c.Providers) == 0 {
		return ProviderConfig{}, errors.New("agent.yaml has no providers configured")
	}
	for _, p := range c.Providers {
		if !p.FallbackOnly {
			return p, nil
		}
	}
	return ProviderConfig{}, errors.New(
		"agent.yaml: at least one provider must not be fallback_only",
	)
}

// HasFallbackChain returns true when more than one provider is configured.
// Wave G-D's fallback port engages on this; the single-provider path is
// what the Go runtime ships in F05.
func (c AgentConfig) HasFallbackChain() bool {
	return len(c.Providers) > 1
}

// ConfigWarning is a non-fatal config issue (unknown key, deprecated field).
type ConfigWarning struct {
	Code    string
	Message string
}

// ConfigError marks a hard validation failure (missing required field, bad
// type). Surface as a Go error.
type ConfigError struct {
	Msg string
}

func (e *ConfigError) Error() string { return e.Msg }

func newConfigErr(format string, a ...any) *ConfigError {
	return &ConfigError{Msg: fmt.Sprintf(format, a...)}
}

// LoadAgentConfig parses agent.yaml from disk.
//
// Pass path = "" to use DefaultAgentYAMLPath. The text variant skips the file
// read and parses the supplied YAML directly — used by tests.
func LoadAgentConfig(path string) (*AgentConfig, []ConfigWarning, error) {
	resolved := path
	if resolved == "" {
		resolved = DefaultAgentYAMLPath
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, nil, err
	}
	return LoadAgentConfigFromBytes(data)
}

// LoadAgentConfigFromBytes parses agent.yaml from raw bytes.
func LoadAgentConfigFromBytes(data []byte) (*AgentConfig, []ConfigWarning, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, nil, newConfigErr("agent.yaml is not valid YAML: %v", err)
	}
	if raw == nil {
		raw = map[string]any{}
	}

	warnings := make([]ConfigWarning, 0)
	for k := range raw {
		if _, ok := knownTopLevel[k]; !ok {
			warnings = append(warnings, ConfigWarning{
				Code:    "unknown_key",
				Message: fmt.Sprintf("unknown top-level key in agent.yaml: %q", k),
			})
		}
	}

	identity, err := parseIdentity(raw["identity"])
	if err != nil {
		return nil, warnings, err
	}
	providers, err := parseProviders(raw["providers"])
	if err != nil {
		return nil, warnings, err
	}
	skills, err := parseSkills(raw["skills"])
	if err != nil {
		return nil, warnings, err
	}
	mcp, err := parseMcp(raw["mcp"])
	if err != nil {
		return nil, warnings, err
	}

	return &AgentConfig{
		Identity:  identity,
		Providers: providers,
		Skills:    skills,
		Mcp:       mcp,
		Raw:       raw,
	}, warnings, nil
}

// ---------------------------------------------------------------------------
// Section parsers.
// ---------------------------------------------------------------------------

func parseIdentity(node any) (IdentityConfig, error) {
	if node == nil {
		return IdentityConfig{}, newConfigErr("agent.yaml: missing required 'identity' section")
	}
	m, ok := node.(map[string]any)
	if !ok {
		return IdentityConfig{}, newConfigErr("agent.yaml: 'identity' must be a mapping")
	}
	nameRaw, _ := m["name"]
	name, ok := nameRaw.(string)
	if !ok || strings.TrimSpace(name) == "" {
		return IdentityConfig{}, newConfigErr("agent.yaml: 'identity.name' must be a non-empty string")
	}
	personaFile := ""
	if pf, present := m["persona_file"]; present && pf != nil {
		s, ok := pf.(string)
		if !ok {
			return IdentityConfig{}, newConfigErr("agent.yaml: 'identity.persona_file' must be a string")
		}
		personaFile = strings.TrimSpace(s)
	}
	return IdentityConfig{
		Name:        strings.TrimSpace(name),
		PersonaFile: personaFile,
	}, nil
}

func parseProviders(node any) ([]ProviderConfig, error) {
	if node == nil {
		return nil, newConfigErr("agent.yaml: 'providers' list is required and must be non-empty")
	}
	list, ok := node.([]any)
	if !ok {
		return nil, newConfigErr("agent.yaml: 'providers' must be a list")
	}
	if len(list) == 0 {
		return nil, newConfigErr("agent.yaml: 'providers' list is required and must be non-empty")
	}
	out := make([]ProviderConfig, 0, len(list))
	seen := make(map[string]struct{})
	for idx, entry := range list {
		em, ok := entry.(map[string]any)
		if !ok {
			return nil, newConfigErr("agent.yaml: providers[%d] must be a mapping", idx)
		}
		idRaw, _ := em["id"]
		id, ok := idRaw.(string)
		if !ok || strings.TrimSpace(id) == "" {
			return nil, newConfigErr("agent.yaml: providers[%d].id must be a non-empty string", idx)
		}
		id = strings.TrimSpace(id)
		if _, dup := seen[id]; dup {
			return nil, newConfigErr("agent.yaml: providers[%d].id=%q appears more than once", idx, id)
		}
		seen[id] = struct{}{}

		model := ""
		if mr, present := em["model"]; present && mr != nil {
			s, ok := mr.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return nil, newConfigErr("agent.yaml: providers[%d].model must be a non-empty string", idx)
			}
			model = strings.TrimSpace(s)
		}
		fallbackOnly := false
		if fr, present := em["fallback_only"]; present && fr != nil {
			b, ok := fr.(bool)
			if !ok {
				return nil, newConfigErr("agent.yaml: providers[%d].fallback_only must be a boolean", idx)
			}
			fallbackOnly = b
		}
		out = append(out, ProviderConfig{ID: id, Model: model, FallbackOnly: fallbackOnly})
	}
	hasNonFallback := false
	for _, p := range out {
		if !p.FallbackOnly {
			hasNonFallback = true
			break
		}
	}
	if !hasNonFallback {
		return nil, newConfigErr(
			"agent.yaml: at least one provider must be non-fallback_only " +
				"(every entry was marked fallback_only=true)")
	}
	return out, nil
}

func parseSkills(node any) ([]string, error) {
	if node == nil {
		return nil, nil
	}
	list, ok := node.([]any)
	if !ok {
		return nil, newConfigErr("agent.yaml: 'skills' must be a list of strings")
	}
	out := make([]string, 0, len(list))
	for idx, entry := range list {
		s, ok := entry.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, newConfigErr("agent.yaml: skills[%d] must be a non-empty string", idx)
		}
		out = append(out, strings.TrimSpace(s))
	}
	return out, nil
}

func parseMcp(node any) (McpConfig, error) {
	if node == nil {
		return McpConfig{}, nil
	}
	m, ok := node.(map[string]any)
	if !ok {
		return McpConfig{}, newConfigErr("agent.yaml: 'mcp' must be a mapping")
	}
	bundle := false
	if br, present := m["bundle"]; present && br != nil {
		b, ok := br.(bool)
		if !ok {
			return McpConfig{}, newConfigErr("agent.yaml: 'mcp.bundle' must be a boolean")
		}
		bundle = b
	}
	return McpConfig{Bundle: bundle}, nil
}
