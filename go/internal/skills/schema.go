// Package skills is the Go port of services/runtime/src/runtime/skills/* (F09).
//
// It implements the FROZEN skill frontmatter schema (version 1), a filesystem
// loader, an ADR-007 precedence resolver, a catalog renderer for the system
// prompt, a Claude Code plugin-dir generator, and per-provider MCP config
// builders.
//
// The schema is frozen at SKILL_SCHEMA_VERSION = 1. The tripwire test
// (TestSkillSchemaLocked) fails loudly if any field is added, renamed, or
// removed from SkillManifest / SkillMcpConfig / SkillSigning.
//
// Adapted from openclaw/src/agents/skills/frontmatter.ts (MIT licence).
// Original TypeScript copyright OpenClaw contributors; this is a fresh Go
// rewrite that keeps only the fields the platform uses.
package skills

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// anyToString converts a YAML scalar to a string. YAML may deserialise
// semver-ish strings like "1.0" as float64 unless they are quoted; we handle
// this gracefully so authors can write version: 1.0 without quotes.
func anyToString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case int:
		return fmt.Sprintf("%d", t), true
	case int64:
		return fmt.Sprintf("%d", t), true
	case float64:
		// Format without trailing zeros to match what the user wrote.
		s := fmt.Sprintf("%g", t)
		return s, true
	case bool:
		if t {
			return "true", true
		}
		return "false", true
	default:
		return "", false
	}
}

// SkillSchemaVersion is the FROZEN schema version. Bumping this is a
// deliberate, breaking action. The tripwire test reads this constant and the
// field sets below and fails on any drift.
const SkillSchemaVersion = 1

// FrozenManifestFields is the set of field names TestSkillSchemaLocked pins.
// It must stay in sync with SkillManifest.
var FrozenManifestFields = []string{
	"Name",
	"Version",
	"Description",
	"WhenToUse",
	"AllowedTools",
	"RequiredEnv",
	"Mcp",
	"Signing",
	"Body",
	"SourcePath",
}

// FrozenMcpFields is the set of field names TestSkillSchemaLocked pins for
// SkillMcpConfig.
var FrozenMcpFields = []string{"Enabled", "Command", "Transport"}

// FrozenSigningFields is the set of field names TestSkillSchemaLocked pins for
// SkillSigning.
var FrozenSigningFields = []string{"Publisher", "Sig"}

var (
	validTransports = map[string]bool{"stdio": true, "http": true}
	slugPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	frontmatterRE   = regexp.MustCompile(`(?s)\A---\s*\n(?P<frontmatter>.*?)\n---\s*(?:\n(?P<body>.*))?\z`)
)

// SkillMcpConfig is the per-skill MCP server config block. FROZEN.
type SkillMcpConfig struct {
	// Enabled toggles whether the skill ships an MCP server.
	Enabled bool
	// Command is the argv used to spawn the MCP server.
	Command []string
	// Transport is how F11 talks to the child: "stdio" or "http".
	Transport string
}

// SkillSigning carries optional signature metadata. F13 owns verification;
// F09 only stores the bytes. FROZEN.
type SkillSigning struct {
	Publisher string // empty string means absent/null
	Sig       string // empty string means absent/null
}

// SkillManifest is the parsed SKILL.md — the canonical in-memory
// representation. All required fields are guaranteed non-empty.
// Body is the markdown content after the closing ---. SourcePath is set by
// the loader to the absolute SKILL.md path (empty for in-memory parses).
//
// FROZEN at SkillSchemaVersion = 1.
type SkillManifest struct {
	// Required fields.
	Name        string
	Version     string
	Description string
	WhenToUse   string

	// Optional fields.
	AllowedTools []string
	RequiredEnv  []string
	Mcp          SkillMcpConfig
	Signing      SkillSigning

	// Loader-stamped fields.
	Body       string
	SourcePath string // empty for in-memory manifests
}

// SkillSchemaError is returned when SKILL.md frontmatter is missing or
// malformed. The loader catches this and converts it to a warning.
type SkillSchemaError struct {
	msg string
}

func (e *SkillSchemaError) Error() string { return e.msg }

func schemaErrorf(format string, args ...any) *SkillSchemaError {
	return &SkillSchemaError{msg: fmt.Sprintf(format, args...)}
}

// ParseSkillMD parses a SKILL.md text into a SkillManifest.
//
// fallbackSlug is used as Name if the frontmatter omits it — the loader
// passes the folder's basename so a SKILL.md that forgot to set name still
// gets a usable slug (mirrors openclaw/local-loader.ts behaviour).
//
// Returns *SkillSchemaError on any parse/validation failure.
func ParseSkillMD(text string, fallbackSlug string) (*SkillManifest, error) {
	match := frontmatterRE.FindStringSubmatch(text)
	if match == nil {
		return nil, schemaErrorf("SKILL.md is missing the YAML frontmatter block (must start with '---')")
	}
	// Named group indices.
	fmIdx := frontmatterRE.SubexpIndex("frontmatter")
	bodyIdx := frontmatterRE.SubexpIndex("body")

	frontmatterRaw := match[fmIdx]
	body := strings.TrimRight(match[bodyIdx], "\n")

	var data map[string]any
	if err := yaml.Unmarshal([]byte(frontmatterRaw), &data); err != nil {
		return nil, schemaErrorf("SKILL.md frontmatter is not valid YAML: %v", err)
	}
	if data == nil {
		data = map[string]any{}
	}

	name, err := readRequiredStr(data, "name", fallbackSlug)
	if err != nil {
		return nil, err
	}
	if !slugPattern.MatchString(name) {
		return nil, schemaErrorf("skill 'name' must be a slug ([a-z0-9_-]+), got %q", name)
	}

	version, err := readRequiredStr(data, "version", "")
	if err != nil {
		return nil, err
	}
	description, err := readRequiredStr(data, "description", "")
	if err != nil {
		return nil, err
	}
	whenToUse, err := readRequiredStr(data, "when_to_use", "")
	if err != nil {
		return nil, err
	}

	allowedTools, err := readOptionalStrList(data, "allowed_tools")
	if err != nil {
		return nil, err
	}
	requiredEnv, err := readOptionalStrList(data, "required_env")
	if err != nil {
		return nil, err
	}

	mcp, err := parseMcpBlock(data["mcp"])
	if err != nil {
		return nil, err
	}
	signing, err := parseSigningBlock(data["signing"])
	if err != nil {
		return nil, err
	}

	return &SkillManifest{
		Name:         name,
		Version:      version,
		Description:  description,
		WhenToUse:    whenToUse,
		AllowedTools: allowedTools,
		RequiredEnv:  requiredEnv,
		Mcp:          mcp,
		Signing:      signing,
		Body:         body,
	}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func readRequiredStr(data map[string]any, key, fallback string) (string, error) {
	raw, present := data[key]
	if !present || raw == nil {
		if fallback != "" {
			return strings.TrimSpace(fallback), nil
		}
		return "", schemaErrorf("SKILL.md frontmatter is missing required field %q", key)
	}
	s, ok := anyToString(raw)
	if !ok || strings.TrimSpace(s) == "" {
		if fallback != "" {
			return strings.TrimSpace(fallback), nil
		}
		return "", schemaErrorf("SKILL.md frontmatter is missing required field %q", key)
	}
	return strings.TrimSpace(s), nil
}

func readOptionalStrList(data map[string]any, key string) ([]string, error) {
	raw, present := data[key]
	if !present || raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, schemaErrorf("SKILL.md frontmatter field %q must be a list of strings", key)
	}
	out := make([]string, 0, len(list))
	for idx, entry := range list {
		s, ok := anyToString(entry)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, schemaErrorf("SKILL.md frontmatter field %q[%d] must be a non-empty string", key, idx)
		}
		out = append(out, strings.TrimSpace(s))
	}
	return out, nil
}

func parseMcpBlock(node any) (SkillMcpConfig, error) {
	if node == nil {
		return SkillMcpConfig{Transport: "stdio"}, nil
	}
	m, ok := node.(map[string]any)
	if !ok {
		return SkillMcpConfig{}, schemaErrorf("SKILL.md frontmatter 'mcp' must be a mapping")
	}
	enabled := false
	if v, ok := m["enabled"]; ok && v != nil {
		b, ok := v.(bool)
		if !ok {
			return SkillMcpConfig{}, schemaErrorf("SKILL.md frontmatter 'mcp.enabled' must be a boolean")
		}
		enabled = b
	}
	var command []string
	if v, ok := m["command"]; ok && v != nil {
		list, ok := v.([]any)
		if !ok {
			return SkillMcpConfig{}, schemaErrorf("SKILL.md frontmatter 'mcp.command' must be a list of strings")
		}
		command = make([]string, 0, len(list))
		for idx, entry := range list {
			s, ok := entry.(string)
			if !ok || s == "" {
				return SkillMcpConfig{}, schemaErrorf("SKILL.md frontmatter 'mcp.command[%d]' must be a non-empty string", idx)
			}
			command = append(command, s)
		}
	}
	transport := "stdio"
	if v, ok := m["transport"]; ok && v != nil {
		s, ok := v.(string)
		if !ok {
			return SkillMcpConfig{}, schemaErrorf("SKILL.md frontmatter 'mcp.transport' must be a string")
		}
		if !validTransports[s] {
			return SkillMcpConfig{}, schemaErrorf("SKILL.md frontmatter 'mcp.transport' must be one of [stdio http], got %q", s)
		}
		transport = s
	}
	return SkillMcpConfig{Enabled: enabled, Command: command, Transport: transport}, nil
}

func parseSigningBlock(node any) (SkillSigning, error) {
	if node == nil {
		return SkillSigning{}, nil
	}
	m, ok := node.(map[string]any)
	if !ok {
		return SkillSigning{}, schemaErrorf("SKILL.md frontmatter 'signing' must be a mapping")
	}
	var publisher, sig string
	if v, ok := m["publisher"]; ok && v != nil {
		s, ok := v.(string)
		if !ok {
			return SkillSigning{}, schemaErrorf("SKILL.md frontmatter 'signing.publisher' must be a string or null")
		}
		publisher = s
	}
	if v, ok := m["sig"]; ok && v != nil {
		s, ok := v.(string)
		if !ok {
			return SkillSigning{}, schemaErrorf("SKILL.md frontmatter 'signing.sig' must be a string or null")
		}
		sig = s
	}
	return SkillSigning{Publisher: publisher, Sig: sig}, nil
}
