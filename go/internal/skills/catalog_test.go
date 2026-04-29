package skills

import (
	"strings"
	"testing"
)

func makeResolved(name, version, tier string, sourcePath string) *ResolvedSkill {
	return &ResolvedSkill{
		Manifest: &SkillManifest{
			Name:        name,
			Version:     version,
			Description: "Does " + name,
			WhenToUse:   "When you need " + name,
			SourcePath:  sourcePath,
		},
		Tier:     SourceTier(tier),
		SkillDir: "/skills/" + name,
	}
}

// ---------------------------------------------------------------------------
// RenderCatalog
// ---------------------------------------------------------------------------

func TestRenderCatalogEmpty(t *testing.T) {
	out := RenderCatalog(nil)
	if out != CatalogEmpty {
		t.Fatalf("empty catalog should return CatalogEmpty constant, got:\n%q", out)
	}
}

func TestRenderCatalogEmptySlice(t *testing.T) {
	out := RenderCatalog([]*ResolvedSkill{})
	if out != CatalogEmpty {
		t.Fatalf("empty slice catalog should return CatalogEmpty, got:\n%q", out)
	}
}

func TestRenderCatalogSingleSkill(t *testing.T) {
	skills := []*ResolvedSkill{
		makeResolved("web-search", "1.0.0", "workspace", "/path/to/SKILL.md"),
	}
	out := RenderCatalog(skills)
	if !strings.Contains(out, "## Available skills") {
		t.Fatal("output should contain '## Available skills'")
	}
	if !strings.Contains(out, "**web-search (v1.0.0)**") {
		t.Fatal("output should contain skill name+version")
	}
	if !strings.Contains(out, "Does web-search") {
		t.Fatal("output should contain description")
	}
	if !strings.Contains(out, "When you need web-search") {
		t.Fatal("output should contain when_to_use")
	}
	if !strings.Contains(out, "workspace at") {
		t.Fatal("output should contain source tier")
	}
	if !strings.Contains(out, "/path/to/SKILL.md") {
		t.Fatal("output should contain source path")
	}
}

func TestRenderCatalogMultipleSkills(t *testing.T) {
	skills := []*ResolvedSkill{
		makeResolved("aaa", "1.0.0", "workspace", "/aaa/SKILL.md"),
		makeResolved("bbb", "2.0.0", "managed", "/bbb/SKILL.md"),
	}
	out := RenderCatalog(skills)
	aaaPos := strings.Index(out, "**aaa")
	bbbPos := strings.Index(out, "**bbb")
	if aaaPos < 0 || bbbPos < 0 {
		t.Fatalf("both skills should appear in catalog:\n%s", out)
	}
}

func TestRenderCatalogFallsBackToSkillDir(t *testing.T) {
	// When SourcePath is empty, use SkillDir.
	r := &ResolvedSkill{
		Manifest: &SkillManifest{
			Name:        "fallback-skill",
			Version:     "1.0",
			Description: "X",
			WhenToUse:   "Y",
			SourcePath:  "", // empty
		},
		Tier:     TierWorkspace,
		SkillDir: "/fallback/dir",
	}
	out := RenderCatalog([]*ResolvedSkill{r})
	if !strings.Contains(out, "/fallback/dir") {
		t.Fatalf("should fall back to SkillDir, got:\n%s", out)
	}
}

func TestRenderCatalogHeaderPresent(t *testing.T) {
	skills := []*ResolvedSkill{makeResolved("x", "1.0", "bundled", "/x")}
	out := RenderCatalog(skills)
	if !strings.HasPrefix(out, "## Available skills") {
		t.Fatalf("catalog should start with header, got:\n%s", out)
	}
}

func TestRenderCatalogEndsWithNewline(t *testing.T) {
	skills := []*ResolvedSkill{makeResolved("x", "1.0", "bundled", "/x")}
	out := RenderCatalog(skills)
	if !strings.HasSuffix(out, "\n") {
		t.Fatal("catalog output should end with newline")
	}
}
