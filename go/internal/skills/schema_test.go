package skills

import (
	"reflect"
	"sort"
	"testing"
)

// ---------------------------------------------------------------------------
// Tripwire: TestSkillSchemaLocked pins the FROZEN field set.
// This test MUST fail if any field is added, renamed, or removed from
// SkillManifest / SkillMcpConfig / SkillSigning without a deliberate schema
// version bump.
// ---------------------------------------------------------------------------

func TestSkillSchemaLocked(t *testing.T) {
	t.Run("SkillSchemaVersion", func(t *testing.T) {
		if SkillSchemaVersion != 1 {
			t.Fatalf("SkillSchemaVersion changed to %d — update this test intentionally", SkillSchemaVersion)
		}
	})

	t.Run("SkillManifest fields", func(t *testing.T) {
		rt := reflect.TypeOf(SkillManifest{})
		got := fieldNames(rt)
		want := sorted(FrozenManifestFields)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("SkillManifest field set drifted!\ngot:  %v\nwant: %v", got, want)
		}
	})

	t.Run("SkillMcpConfig fields", func(t *testing.T) {
		rt := reflect.TypeOf(SkillMcpConfig{})
		got := fieldNames(rt)
		want := sorted(FrozenMcpFields)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("SkillMcpConfig field set drifted!\ngot:  %v\nwant: %v", got, want)
		}
	})

	t.Run("SkillSigning fields", func(t *testing.T) {
		rt := reflect.TypeOf(SkillSigning{})
		got := fieldNames(rt)
		want := sorted(FrozenSigningFields)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("SkillSigning field set drifted!\ngot:  %v\nwant: %v", got, want)
		}
	})
}

func fieldNames(rt reflect.Type) []string {
	names := make([]string, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		names[i] = rt.Field(i).Name
	}
	return sorted(names)
}

func sorted(ss []string) []string {
	out := make([]string, len(ss))
	copy(out, ss)
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// ParseSkillMD — happy paths
// ---------------------------------------------------------------------------

const minimalSkillMD = `---
name: web-search
version: 1.0.0
description: Search the web
when_to_use: When the user asks about current events
---
`

func TestParseSkillMDMinimal(t *testing.T) {
	m, err := ParseSkillMD(minimalSkillMD, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Name != "web-search" {
		t.Fatalf("Name=%q", m.Name)
	}
	if m.Version != "1.0.0" {
		t.Fatalf("Version=%q", m.Version)
	}
	if m.Description != "Search the web" {
		t.Fatalf("Description=%q", m.Description)
	}
	if m.WhenToUse != "When the user asks about current events" {
		t.Fatalf("WhenToUse=%q", m.WhenToUse)
	}
	if len(m.AllowedTools) != 0 {
		t.Fatalf("AllowedTools=%v", m.AllowedTools)
	}
	if len(m.RequiredEnv) != 0 {
		t.Fatalf("RequiredEnv=%v", m.RequiredEnv)
	}
	if m.Mcp.Enabled {
		t.Fatal("Mcp.Enabled should be false by default")
	}
	if m.Mcp.Transport != "stdio" {
		t.Fatalf("Mcp.Transport=%q (default should be 'stdio')", m.Mcp.Transport)
	}
}

const fullSkillMD = `---
name: image-describe
version: 2.1.0
description: Describe image content
when_to_use: When the user provides an image
allowed_tools: [bash, python]
required_env: [OPENAI_API_KEY, ANTHROPIC_API_KEY]
mcp:
  enabled: true
  command: [python, -m, server]
  transport: http
signing:
  publisher: openclaw-official
  sig: abc123
---
# Image Describe

Detailed image description skill.
`

func TestParseSkillMDFull(t *testing.T) {
	m, err := ParseSkillMD(fullSkillMD, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Name != "image-describe" {
		t.Fatalf("Name=%q", m.Name)
	}
	if m.Version != "2.1.0" {
		t.Fatalf("Version=%q", m.Version)
	}
	if len(m.AllowedTools) != 2 || m.AllowedTools[0] != "bash" || m.AllowedTools[1] != "python" {
		t.Fatalf("AllowedTools=%v", m.AllowedTools)
	}
	if len(m.RequiredEnv) != 2 {
		t.Fatalf("RequiredEnv=%v", m.RequiredEnv)
	}
	if !m.Mcp.Enabled {
		t.Fatal("Mcp.Enabled should be true")
	}
	if m.Mcp.Transport != "http" {
		t.Fatalf("Mcp.Transport=%q", m.Mcp.Transport)
	}
	if len(m.Mcp.Command) != 3 {
		t.Fatalf("Mcp.Command=%v", m.Mcp.Command)
	}
	if m.Signing.Publisher != "openclaw-official" {
		t.Fatalf("Signing.Publisher=%q", m.Signing.Publisher)
	}
	if m.Signing.Sig != "abc123" {
		t.Fatalf("Signing.Sig=%q", m.Signing.Sig)
	}
	if m.Body == "" {
		t.Fatal("Body should not be empty")
	}
}

func TestParseSkillMDFallbackSlug(t *testing.T) {
	text := `---
version: 1.0.0
description: A skill
when_to_use: When needed
---
`
	m, err := ParseSkillMD(text, "fallback-slug")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Name != "fallback-slug" {
		t.Fatalf("Name should be fallback-slug, got %q", m.Name)
	}
}

func TestParseSkillMDBodyParsed(t *testing.T) {
	text := "---\nname: foo\nversion: 1.0.0\ndescription: X\nwhen_to_use: Y\n---\n# Hello\n\nWorld\n"
	m, err := ParseSkillMD(text, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Body == "" {
		t.Fatal("Body should not be empty")
	}
}

// ---------------------------------------------------------------------------
// ParseSkillMD — error paths
// ---------------------------------------------------------------------------

func TestParseSkillMDMissingFrontmatter(t *testing.T) {
	_, err := ParseSkillMD("# Just markdown\n\nNo frontmatter here.", "")
	if err == nil {
		t.Fatal("expected error for missing frontmatter")
	}
}

func TestParseSkillMDMissingRequiredName(t *testing.T) {
	text := "---\nversion: 1.0.0\ndescription: X\nwhen_to_use: Y\n---\n"
	_, err := ParseSkillMD(text, "") // no fallback
	if err == nil {
		t.Fatal("expected error for missing 'name'")
	}
}

func TestParseSkillMDInvalidSlug(t *testing.T) {
	text := "---\nname: My Invalid Slug!\nversion: 1.0\ndescription: X\nwhen_to_use: Y\n---\n"
	_, err := ParseSkillMD(text, "")
	if err == nil {
		t.Fatal("expected error for invalid slug")
	}
}

func TestParseSkillMDInvalidTransport(t *testing.T) {
	text := "---\nname: foo\nversion: 1.0\ndescription: X\nwhen_to_use: Y\nmcp:\n  transport: grpc\n---\n"
	_, err := ParseSkillMD(text, "")
	if err == nil {
		t.Fatal("expected error for invalid mcp.transport")
	}
}

func TestParseSkillMDAllowedToolsMustBeList(t *testing.T) {
	text := "---\nname: foo\nversion: 1.0\ndescription: X\nwhen_to_use: Y\nallowed_tools: not-a-list\n---\n"
	_, err := ParseSkillMD(text, "")
	if err == nil {
		t.Fatal("expected error for non-list allowed_tools")
	}
}

func TestParseSkillMDMcpMustBeMapping(t *testing.T) {
	text := "---\nname: foo\nversion: 1.0\ndescription: X\nwhen_to_use: Y\nmcp: true\n---\n"
	_, err := ParseSkillMD(text, "")
	if err == nil {
		t.Fatal("expected error for non-mapping mcp")
	}
}

func TestParseSkillMDSigningMustBeMapping(t *testing.T) {
	text := "---\nname: foo\nversion: 1.0\ndescription: X\nwhen_to_use: Y\nsigning: [a, b]\n---\n"
	_, err := ParseSkillMD(text, "")
	if err == nil {
		t.Fatal("expected error for non-mapping signing")
	}
}

func TestParseSkillMDMcpCommandMustBeList(t *testing.T) {
	text := "---\nname: foo\nversion: 1.0\ndescription: X\nwhen_to_use: Y\nmcp:\n  command: python\n---\n"
	_, err := ParseSkillMD(text, "")
	if err == nil {
		t.Fatal("expected error for non-list mcp.command")
	}
}

func TestParseSkillMDMissingVersion(t *testing.T) {
	text := "---\nname: foo\ndescription: X\nwhen_to_use: Y\n---\n"
	_, err := ParseSkillMD(text, "")
	if err == nil {
		t.Fatal("expected error for missing version")
	}
}

func TestParseSkillMDMissingDescription(t *testing.T) {
	text := "---\nname: foo\nversion: 1.0\nwhen_to_use: Y\n---\n"
	_, err := ParseSkillMD(text, "")
	if err == nil {
		t.Fatal("expected error for missing description")
	}
}

func TestParseSkillMDMissingWhenToUse(t *testing.T) {
	text := "---\nname: foo\nversion: 1.0\ndescription: X\n---\n"
	_, err := ParseSkillMD(text, "")
	if err == nil {
		t.Fatal("expected error for missing when_to_use")
	}
}

func TestParseSkillMDSlugWithUnderscore(t *testing.T) {
	text := "---\nname: my_skill\nversion: 1.0.0\ndescription: X\nwhen_to_use: Y\n---\n"
	m, err := ParseSkillMD(text, "")
	if err != nil {
		t.Fatalf("underscore slug should be valid: %v", err)
	}
	if m.Name != "my_skill" {
		t.Fatalf("Name=%q", m.Name)
	}
}

func TestParseSkillMDSlugWithHyphen(t *testing.T) {
	text := "---\nname: my-skill-v2\nversion: 1.0.0\ndescription: X\nwhen_to_use: Y\n---\n"
	m, err := ParseSkillMD(text, "")
	if err != nil {
		t.Fatalf("hyphen slug should be valid: %v", err)
	}
	if m.Name != "my-skill-v2" {
		t.Fatalf("Name=%q", m.Name)
	}
}

func TestParseSkillMDSigningNullFields(t *testing.T) {
	text := "---\nname: foo\nversion: 1.0.0\ndescription: X\nwhen_to_use: Y\nsigning:\n  publisher: null\n  sig: null\n---\n"
	m, err := ParseSkillMD(text, "")
	if err != nil {
		t.Fatalf("null signing fields should be valid: %v", err)
	}
	if m.Signing.Publisher != "" {
		t.Fatalf("Publisher=%q", m.Signing.Publisher)
	}
	if m.Signing.Sig != "" {
		t.Fatalf("Sig=%q", m.Signing.Sig)
	}
}

func TestParseSkillMDMcpDefaultTransport(t *testing.T) {
	text := "---\nname: foo\nversion: 1.0.0\ndescription: X\nwhen_to_use: Y\nmcp:\n  enabled: true\n  command: [node, server.js]\n---\n"
	m, err := ParseSkillMD(text, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Mcp.Transport != "stdio" {
		t.Fatalf("default transport should be stdio, got %q", m.Mcp.Transport)
	}
}
