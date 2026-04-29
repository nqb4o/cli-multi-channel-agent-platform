// System-prompt catalog renderer.
//
// The system prompt advertises every resolved skill so the model can decide
// when to load one. The format is a markdown bullet list — short and
// deterministic so it caches well across providers.
//
// Adapted from openclaw/src/agents/skills/skill-contract.ts
// formatSkillsForPrompt.
package skills

import "strings"

// CatalogHeader is prepended when at least one skill is available.
const CatalogHeader = "## Available skills\n\n" +
	"The following skills are available for this turn. Use the file at " +
	"`<source path>` to load the skill's instructions when the task matches " +
	"its description.\n"

// CatalogEmpty is the full output when no skills are resolved.
const CatalogEmpty = "## Available skills\n\n" +
	"_No skills are available for this turn._\n"

// RenderCatalog renders the markdown catalog section for the system prompt.
//
// Output is deterministic: skills are pre-sorted by the resolver, and the
// formatter does not interpolate any unsanitised input — the schema parser
// already guarantees name/version/description/when_to_use are non-empty.
func RenderCatalog(skills []*ResolvedSkill) string {
	if len(skills) == 0 {
		return CatalogEmpty
	}
	var lines []string
	lines = append(lines, CatalogHeader)
	for _, resolved := range skills {
		m := resolved.Manifest
		path := m.SourcePath
		if path == "" {
			path = resolved.SkillDir
		}
		lines = append(lines,
			"- **"+m.Name+" (v"+m.Version+")** — "+m.Description,
			"  _When to use:_ "+m.WhenToUse,
			"  _Source:_ "+string(resolved.Tier)+" at `"+path+"`",
		)
	}
	return strings.Join(lines, "\n") + "\n"
}
