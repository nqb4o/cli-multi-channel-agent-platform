package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func testdataSkillsDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "testdata", "skills")
}

// ---------------------------------------------------------------------------
// LoadAll — happy paths
// ---------------------------------------------------------------------------

func TestLoadAllWorkspaceRoot(t *testing.T) {
	base := testdataSkillsDir(t)
	roots := []SkillRoot{
		{Path: filepath.Join(base, "workspace"), Tier: TierWorkspace},
	}
	result := LoadAll(roots)
	if len(result.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", result.Warnings)
	}
	if len(result.Skills) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(result.Skills))
	}
}

func TestLoadAllMultipleRoots(t *testing.T) {
	base := testdataSkillsDir(t)
	roots := []SkillRoot{
		{Path: filepath.Join(base, "workspace"), Tier: TierWorkspace},
		{Path: filepath.Join(base, "managed"), Tier: TierManaged},
		{Path: filepath.Join(base, "bundled"), Tier: TierBundled},
	}
	result := LoadAll(roots)
	if len(result.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", result.Warnings)
	}
	if len(result.Skills) != 4 {
		t.Fatalf("expected 4 skills, got %d: %v", len(result.Skills), skillNames(result.Skills))
	}
}

func TestLoadAllTierTaggedCorrectly(t *testing.T) {
	base := testdataSkillsDir(t)
	roots := []SkillRoot{
		{Path: filepath.Join(base, "workspace"), Tier: TierWorkspace},
		{Path: filepath.Join(base, "bundled"), Tier: TierBundled},
	}
	result := LoadAll(roots)
	for _, s := range result.Skills {
		if s.Manifest.Name == "web-search" && s.Tier != TierWorkspace {
			t.Fatalf("web-search should be workspace tier, got %q", s.Tier)
		}
		if s.Manifest.Name == "image-describe" && s.Tier != TierBundled {
			t.Fatalf("image-describe should be bundled tier, got %q", s.Tier)
		}
	}
}

func TestLoadAllNonExistentRootIsEmpty(t *testing.T) {
	roots := []SkillRoot{
		{Path: "/nonexistent/path/that/does/not/exist", Tier: TierWorkspace},
	}
	result := LoadAll(roots)
	if len(result.Skills) != 0 {
		t.Fatalf("expected 0 skills for non-existent root, got %d", len(result.Skills))
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("non-existent root should not produce warnings, got %v", result.Warnings)
	}
}

func TestLoadAllRootIsFileProducesWarning(t *testing.T) {
	tmp := t.TempDir()
	filePath := filepath.Join(tmp, "not-a-dir.txt")
	if err := os.WriteFile(filePath, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	roots := []SkillRoot{{Path: filePath, Tier: TierWorkspace}}
	result := LoadAll(roots)
	if len(result.Warnings) == 0 {
		t.Fatal("expected warning for file-as-root")
	}
	if result.Warnings[0].Code != "root_not_dir" {
		t.Fatalf("warning code=%q", result.Warnings[0].Code)
	}
}

func TestLoadAllSkipsDotDirs(t *testing.T) {
	tmp := t.TempDir()
	dotDir := filepath.Join(tmp, ".hidden-skill")
	if err := os.MkdirAll(dotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillMD := "---\nname: hidden\nversion: 1.0\ndescription: X\nwhen_to_use: Y\n---\n"
	if err := os.WriteFile(filepath.Join(dotDir, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		t.Fatal(err)
	}
	roots := []SkillRoot{{Path: tmp, Tier: TierWorkspace}}
	result := LoadAll(roots)
	if len(result.Skills) != 0 {
		t.Fatalf("expected 0 skills, hidden dir should be skipped")
	}
}

func TestLoadAllSkipsNodeModules(t *testing.T) {
	tmp := t.TempDir()
	nmDir := filepath.Join(tmp, "node_modules", "some-skill")
	if err := os.MkdirAll(nmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillMD := "---\nname: nm-skill\nversion: 1.0\ndescription: X\nwhen_to_use: Y\n---\n"
	if err := os.WriteFile(filepath.Join(nmDir, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		t.Fatal(err)
	}
	roots := []SkillRoot{{Path: tmp, Tier: TierWorkspace}}
	result := LoadAll(roots)
	if len(result.Skills) != 0 {
		t.Fatalf("expected 0 skills, node_modules should be skipped")
	}
}

func TestLoadAllMissingSkillMDProducesWarning(t *testing.T) {
	tmp := t.TempDir()
	emptySkillDir := filepath.Join(tmp, "empty-skill")
	if err := os.MkdirAll(emptySkillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	roots := []SkillRoot{{Path: tmp, Tier: TierWorkspace}}
	result := LoadAll(roots)
	if len(result.Skills) != 0 {
		t.Fatalf("expected 0 skills")
	}
	if len(result.Warnings) == 0 {
		t.Fatal("expected warning for missing SKILL.md")
	}
	if result.Warnings[0].Code != "missing_skill_md" {
		t.Fatalf("warning code=%q", result.Warnings[0].Code)
	}
}

func TestLoadAllBadSkillMDProducesWarning(t *testing.T) {
	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, "bad-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// No frontmatter.
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Just markdown\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	roots := []SkillRoot{{Path: tmp, Tier: TierWorkspace}}
	result := LoadAll(roots)
	if len(result.Skills) != 0 {
		t.Fatalf("expected 0 skills")
	}
	if len(result.Warnings) == 0 {
		t.Fatal("expected warning for bad SKILL.md")
	}
	if result.Warnings[0].Code != "parse_error" {
		t.Fatalf("warning code=%q", result.Warnings[0].Code)
	}
}

func TestLoadAllLexicalOrder(t *testing.T) {
	tmp := t.TempDir()
	names := []string{"zzz", "aaa", "mmm"}
	for _, n := range names {
		dir := filepath.Join(tmp, n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		text := "---\nname: " + n + "\nversion: 1.0\ndescription: X\nwhen_to_use: Y\n---\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	roots := []SkillRoot{{Path: tmp, Tier: TierWorkspace}}
	result := LoadAll(roots)
	if len(result.Skills) != 3 {
		t.Fatalf("expected 3 skills, got %d", len(result.Skills))
	}
	if result.Skills[0].Manifest.Name != "aaa" {
		t.Fatalf("first skill should be aaa (lexical), got %q", result.Skills[0].Manifest.Name)
	}
	if result.Skills[2].Manifest.Name != "zzz" {
		t.Fatalf("last skill should be zzz (lexical), got %q", result.Skills[2].Manifest.Name)
	}
}

func TestLoadAllSourcePathStamped(t *testing.T) {
	base := testdataSkillsDir(t)
	roots := []SkillRoot{{Path: filepath.Join(base, "workspace"), Tier: TierWorkspace}}
	result := LoadAll(roots)
	for _, s := range result.Skills {
		if s.Manifest.SourcePath == "" {
			t.Fatalf("SourcePath not stamped for %q", s.Manifest.Name)
		}
	}
}

// ---------------------------------------------------------------------------
// LoadOne
// ---------------------------------------------------------------------------

func TestLoadOneHappyPath(t *testing.T) {
	base := testdataSkillsDir(t)
	result := LoadOne(filepath.Join(base, "workspace", "web-search"), TierWorkspace)
	if len(result.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", result.Warnings)
	}
	if len(result.Skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(result.Skills))
	}
	if result.Skills[0].Manifest.Name != "web-search" {
		t.Fatalf("Name=%q", result.Skills[0].Manifest.Name)
	}
}

// ---------------------------------------------------------------------------
// Merged
// ---------------------------------------------------------------------------

func TestLoadResultMerged(t *testing.T) {
	a := LoadResult{
		Skills:   []*LoadedSkill{{Manifest: &SkillManifest{Name: "a"}, Tier: TierWorkspace}},
		Warnings: []LoadWarning{{Code: "w1"}},
	}
	b := LoadResult{
		Skills:   []*LoadedSkill{{Manifest: &SkillManifest{Name: "b"}, Tier: TierBundled}},
		Warnings: []LoadWarning{{Code: "w2"}},
	}
	merged := a.Merged(b)
	if len(merged.Skills) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(merged.Skills))
	}
	if len(merged.Warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %d", len(merged.Warnings))
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func skillNames(skills []*LoadedSkill) []string {
	names := make([]string, len(skills))
	for i, s := range skills {
		names[i] = s.Manifest.Name
	}
	return names
}
