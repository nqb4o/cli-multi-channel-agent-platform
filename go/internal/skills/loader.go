// Loader walks skill root directories and parses SKILL.md files.
//
// A skill root is a directory containing one subdirectory per skill, each
// holding a SKILL.md. Each root carries a SourceTier tag that the resolver
// uses to enforce ADR-007 precedence.
//
// Adapted from openclaw/src/agents/skills/local-loader.ts. The Go port
// keeps the same fail-soft behaviour: a malformed SKILL.md surfaces as a
// LoadWarning; the rest of the root keeps loading.
package skills

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
)

// SourceTier identifies the provenance tier of a skill root (ADR-007).
type SourceTier string

const (
	TierWorkspace SourceTier = "workspace"
	TierProject   SourceTier = "project"
	TierPersonal  SourceTier = "personal"
	TierManaged   SourceTier = "managed"
	TierBundled   SourceTier = "bundled"
)

// TierPrecedence lists tiers in highest-first order. The resolver uses the
// slice index as a rank (0 = highest).
var TierPrecedence = []SourceTier{
	TierWorkspace,
	TierProject,
	TierPersonal,
	TierManaged,
	TierBundled,
}

// tierRank maps tier → rank (0 = highest precedence).
var tierRank map[SourceTier]int

func init() {
	tierRank = make(map[SourceTier]int, len(TierPrecedence))
	for i, t := range TierPrecedence {
		tierRank[t] = i
	}
}

// SkillRoot is a directory + its tier tag.
type SkillRoot struct {
	Path string
	Tier SourceTier
}

// LoadedSkill is a successfully-parsed SKILL.md plus its provenance.
type LoadedSkill struct {
	Manifest *SkillManifest
	Tier     SourceTier
	RootPath string
	SkillDir string
}

// LoadWarning is a non-fatal load issue.
type LoadWarning struct {
	Code    string
	Path    string
	Message string
}

// LoadResult bundles every skill found across every root plus warnings.
type LoadResult struct {
	Skills   []*LoadedSkill
	Warnings []LoadWarning
}

// Merged concatenates two LoadResults (used to fold per-root walks).
func (r LoadResult) Merged(other LoadResult) LoadResult {
	return LoadResult{
		Skills:   append(r.Skills, other.Skills...),
		Warnings: append(r.Warnings, other.Warnings...),
	}
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// LoadAll walks every root and returns all parsed skills + warnings.
// Roots are walked in iteration order. Within each root subdirectories are
// visited in lexical order. Hidden directories and node_modules are skipped.
func LoadAll(roots []SkillRoot) LoadResult {
	acc := LoadResult{}
	for _, root := range roots {
		acc = acc.Merged(loadRoot(root))
	}
	return acc
}

// LoadOne loads a single skill folder. Convenience wrapper used by tests.
func LoadOne(skillDir string, tier SourceTier) LoadResult {
	absSkillDir, err := filepath.Abs(skillDir)
	if err != nil {
		absSkillDir = skillDir
	}
	return loadSkillDir(absSkillDir, tier, filepath.Dir(absSkillDir))
}

// ---------------------------------------------------------------------------
// Internal walkers
// ---------------------------------------------------------------------------

var ignoredDirNames = map[string]bool{
	".git":        true,
	"__pycache__": true,
	"node_modules": true,
	".venv":       true,
}

func loadRoot(root SkillRoot) LoadResult {
	info, err := os.Stat(root.Path)
	if os.IsNotExist(err) {
		// A non-existent root is not a warning — roots are configured
		// liberally and the loader treats them as empty.
		return LoadResult{}
	}
	if err != nil {
		return LoadResult{Warnings: []LoadWarning{{
			Code:    "stat_error",
			Path:    root.Path,
			Message: err.Error(),
		}}}
	}
	if !info.IsDir() {
		return LoadResult{Warnings: []LoadWarning{{
			Code:    "root_not_dir",
			Path:    root.Path,
			Message: "skill root is not a directory: " + root.Path,
		}}}
	}

	entries, err := os.ReadDir(root.Path)
	if err != nil {
		return LoadResult{Warnings: []LoadWarning{{
			Code:    "read_error",
			Path:    root.Path,
			Message: err.Error(),
		}}}
	}

	// Sort lexically for determinism.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	acc := LoadResult{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if len(name) > 0 && name[0] == '.' {
			continue
		}
		if ignoredDirNames[name] {
			continue
		}
		childPath := filepath.Join(root.Path, name)
		acc = acc.Merged(loadSkillDir(childPath, root.Tier, root.Path))
	}
	return acc
}

func loadSkillDir(skillDir string, tier SourceTier, rootPath string) LoadResult {
	skillMD := filepath.Join(skillDir, "SKILL.md")
	data, err := os.ReadFile(skillMD)
	if os.IsNotExist(err) {
		return LoadResult{Warnings: []LoadWarning{{
			Code:    "missing_skill_md",
			Path:    skillMD,
			Message: "no SKILL.md in " + skillDir,
		}}}
	}
	if err != nil {
		return LoadResult{Warnings: []LoadWarning{{
			Code:    "read_error",
			Path:    skillMD,
			Message: "failed to read SKILL.md: " + err.Error(),
		}}}
	}

	fallback := filepath.Base(skillDir)
	manifest, parseErr := ParseSkillMD(string(data), fallback)
	if parseErr != nil {
		slog.Warn("skill parse error", "path", skillMD, "err", parseErr)
		return LoadResult{Warnings: []LoadWarning{{
			Code:    "parse_error",
			Path:    skillMD,
			Message: parseErr.Error(),
		}}}
	}

	// Stamp the absolute source path back into the manifest.
	absSkillMD, err := filepath.Abs(skillMD)
	if err != nil {
		absSkillMD = skillMD
	}
	manifest.SourcePath = absSkillMD

	absSkillDir, err := filepath.Abs(skillDir)
	if err != nil {
		absSkillDir = skillDir
	}

	return LoadResult{
		Skills: []*LoadedSkill{{
			Manifest: manifest,
			Tier:     tier,
			RootPath: rootPath,
			SkillDir: absSkillDir,
		}},
	}
}
