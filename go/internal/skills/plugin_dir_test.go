package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeResolvedWithDir(t *testing.T, name, version, tier string) *ResolvedSkill {
	t.Helper()
	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "---\nname: " + name + "\nversion: " + version + "\ndescription: X\nwhen_to_use: Y\n---\n# Body\n"
	skillMDPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillMDPath, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return &ResolvedSkill{
		Manifest: &SkillManifest{
			Name:       name,
			Version:    version,
			SourcePath: skillMDPath,
		},
		Tier:     SourceTier(tier),
		SkillDir: skillDir,
	}
}

// ---------------------------------------------------------------------------
// GeneratePluginDir — happy paths
// ---------------------------------------------------------------------------

func TestGeneratePluginDirBasicLayout(t *testing.T) {
	tmp := t.TempDir()
	skills := []*ResolvedSkill{
		makeResolvedWithDir(t, "web-search", "1.0.0", "workspace"),
	}
	pd, err := GeneratePluginDir(skills, "run-001", tmp, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Check root exists
	if _, err := os.Stat(pd.Root); err != nil {
		t.Fatalf("plugin root not created: %v", err)
	}
	// Check skills/<slug>/SKILL.md
	skillMD := filepath.Join(pd.PluginRoot, "skills", "web-search", "SKILL.md")
	if _, err := os.Stat(skillMD); err != nil {
		t.Fatalf("SKILL.md not found: %v", err)
	}
	// Check .claude-plugin/plugin.json
	pluginJSON := filepath.Join(pd.PluginRoot, ".claude-plugin", "plugin.json")
	if _, err := os.Stat(pluginJSON); err != nil {
		t.Fatalf("plugin.json not found: %v", err)
	}
}

func TestGeneratePluginDirPluginJSONContents(t *testing.T) {
	tmp := t.TempDir()
	skills := []*ResolvedSkill{makeResolvedWithDir(t, "foo", "1.0.0", "workspace")}
	pd, err := GeneratePluginDir(skills, "run-002", tmp, "my-plugin", "My plugin description")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(pd.PluginRoot, ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("plugin.json not valid JSON: %v", err)
	}
	if meta["name"] != "my-plugin" {
		t.Fatalf("name=%v", meta["name"])
	}
	if meta["description"] != "My plugin description" {
		t.Fatalf("description=%v", meta["description"])
	}
}

func TestGeneratePluginDirDefaultPluginName(t *testing.T) {
	tmp := t.TempDir()
	skills := []*ResolvedSkill{makeResolvedWithDir(t, "foo", "1.0.0", "workspace")}
	pd, err := GeneratePluginDir(skills, "run-003", tmp, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pd.PluginName != DefaultPluginName {
		t.Fatalf("PluginName=%q", pd.PluginName)
	}
}

func TestGeneratePluginDirMultipleSkills(t *testing.T) {
	tmp := t.TempDir()
	skills := []*ResolvedSkill{
		makeResolvedWithDir(t, "aaa", "1.0.0", "workspace"),
		makeResolvedWithDir(t, "bbb", "1.0.0", "managed"),
	}
	pd, err := GeneratePluginDir(skills, "run-004", tmp, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pd.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(pd.Entries))
	}
	// Check both skill dirs exist.
	for _, slug := range []string{"aaa", "bbb"} {
		path := filepath.Join(pd.PluginRoot, "skills", slug, "SKILL.md")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("SKILL.md missing for %q: %v", slug, err)
		}
	}
}

func TestGeneratePluginDirRootNameContainsRunID(t *testing.T) {
	tmp := t.TempDir()
	pd, err := GeneratePluginDir(nil, "my-run-id", tmp, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(pd.Root, "claude_plugins_my-run-id") {
		t.Fatalf("Root should contain run_id, got %q", pd.Root)
	}
}

func TestGeneratePluginDirInvalidRunID(t *testing.T) {
	_, err := GeneratePluginDir(nil, "", "/tmp", "", "")
	if err == nil {
		t.Fatal("expected error for empty run_id")
	}
	_, err = GeneratePluginDir(nil, "bad/run/id", "/tmp", "", "")
	if err == nil {
		t.Fatal("expected error for run_id with /")
	}
}

func TestGeneratePluginDirIdempotentReruns(t *testing.T) {
	tmp := t.TempDir()
	skills := []*ResolvedSkill{makeResolvedWithDir(t, "foo", "1.0.0", "workspace")}
	// Run twice — second run should not fail.
	_, err := GeneratePluginDir(skills, "run-rerun", tmp, "", "")
	if err != nil {
		t.Fatalf("first run error: %v", err)
	}
	_, err = GeneratePluginDir(skills, "run-rerun", tmp, "", "")
	if err != nil {
		t.Fatalf("second run error: %v", err)
	}
}

func TestGeneratePluginDirNoSourceDir(t *testing.T) {
	// Skill with a missing skill dir falls back to serialised manifest.
	tmp := t.TempDir()
	r := &ResolvedSkill{
		Manifest: &SkillManifest{
			Name:        "no-source",
			Version:     "1.0",
			Description: "X",
			WhenToUse:   "Y",
		},
		Tier:     TierWorkspace,
		SkillDir: "/nonexistent/path/no-source",
	}
	pd, err := GeneratePluginDir([]*ResolvedSkill{r}, "run-nosrc", tmp, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	skillMD := filepath.Join(pd.PluginRoot, "skills", "no-source", "SKILL.md")
	data, err := os.ReadFile(skillMD)
	if err != nil {
		t.Fatalf("SKILL.md not created: %v", err)
	}
	if !strings.Contains(string(data), "no-source") {
		t.Fatalf("serialised manifest should contain skill name, got:\n%s", data)
	}
}

func TestGeneratePluginDirEntryPaths(t *testing.T) {
	tmp := t.TempDir()
	r := makeResolvedWithDir(t, "entry-skill", "1.0.0", "workspace")
	pd, err := GeneratePluginDir([]*ResolvedSkill{r}, "run-ent", tmp, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pd.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(pd.Entries))
	}
	entry := pd.Entries[0]
	if entry.Slug != "entry-skill" {
		t.Fatalf("Slug=%q", entry.Slug)
	}
	if !strings.HasSuffix(entry.DestPath, "SKILL.md") {
		t.Fatalf("DestPath should end with SKILL.md, got %q", entry.DestPath)
	}
}

func TestGeneratePluginDirCopiesSiblingFiles(t *testing.T) {
	tmp := t.TempDir()
	// Create a skill dir with a sibling file.
	srcDir := t.TempDir()
	skillDir := filepath.Join(srcDir, "copytest")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "---\nname: copytest\nversion: 1.0\ndescription: X\nwhen_to_use: Y\n---\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "helper.sh"), []byte("#!/bin/bash\necho hello"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &ResolvedSkill{
		Manifest: &SkillManifest{Name: "copytest", Version: "1.0", Description: "X", WhenToUse: "Y",
			SourcePath: filepath.Join(skillDir, "SKILL.md")},
		Tier:     TierWorkspace,
		SkillDir: skillDir,
	}
	pd, err := GeneratePluginDir([]*ResolvedSkill{r}, "run-copy", tmp, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	helperDest := filepath.Join(pd.PluginRoot, "skills", "copytest", "helper.sh")
	if _, err := os.Stat(helperDest); err != nil {
		t.Fatalf("sibling file not copied: %v", err)
	}
}
