// ADR-007 precedence + version-pin + eligibility resolver.
//
// Inputs:
//   - available: every successfully loaded skill (across all tiers).
//   - requested: the agent's skill list. Each entry is either a bare slug
//     ("web-search") or "slug@version" to pin. nil means "load every
//     available skill" (admin/dev mode).
//   - env: the env-var snapshot used for required_env eligibility.
//
// Output: ResolvedSkills.
package skills

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

var requestRE = regexp.MustCompile(`^(?P<slug>[a-z0-9][a-z0-9_-]*)(?:@(?P<version>.+))?$`)

// SkillRef is a parsed entry from agent.yaml's skills list.
type SkillRef struct {
	Slug    string
	Version string // empty means no pin
	Raw     string
}

// ParseSkillRef parses "web-search" or "web-search@1.2.0".
func ParseSkillRef(raw string) (SkillRef, error) {
	cleaned := raw
	if cleaned == "" {
		return SkillRef{}, fmt.Errorf("skill ref must be a non-empty string")
	}
	match := requestRE.FindStringSubmatch(cleaned)
	if match == nil {
		return SkillRef{}, fmt.Errorf("invalid skill ref %q (expected 'slug' or 'slug@version')", raw)
	}
	slugIdx := requestRE.SubexpIndex("slug")
	versionIdx := requestRE.SubexpIndex("version")
	return SkillRef{
		Slug:    match[slugIdx],
		Version: match[versionIdx],
		Raw:     cleaned,
	}, nil
}

// ResolvedSkill is the winning manifest + provenance for one slug.
type ResolvedSkill struct {
	Manifest         *SkillManifest
	Tier             SourceTier
	RequestedVersion string // empty if no pin was requested
	SkillDir         string
}

// RejectedSkill is a loaded skill excluded from the final set.
//
// Reason values:
//   - not_requested: skill exists but agent did not ask for it.
//   - shadowed: outranked by a higher-tier skill of the same slug.
//   - version_mismatch: pin did not match.
//   - missing_env: required_env not satisfied.
//   - duplicate_in_tier: two entries with the same slug and tier.
type RejectedSkill struct {
	Slug             string
	Tier             SourceTier
	RequestedVersion string
	Reason           string
	Detail           string
	SkillDir         string
}

// ResolvedSkills is the result of one Resolve call.
type ResolvedSkills struct {
	// Selected is sorted by slug for determinism.
	Selected []*ResolvedSkill
	Rejected []RejectedSkill
	// Unknown holds refs that were requested but not found.
	Unknown []SkillRef
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// Resolve applies precedence + version pin + eligibility.
//
// available should come from LoadResult.Skills. requested may be nil (load
// every available skill) or a slice of slug strings / slug@version strings.
// env is used for required_env checks; pass nil to use os.Environ.
func Resolve(available []*LoadedSkill, requested []string, env map[string]string) ResolvedSkills {
	if env == nil {
		env = osEnvironMap()
	}

	// Group by slug, detect intra-tier duplicates.
	bySlug := map[string][]*LoadedSkill{}
	var duplicates []RejectedSkill
	seenPerTier := map[[2]string]*LoadedSkill{} // [slug, tier] → first winner
	for _, cand := range available {
		key := [2]string{cand.Manifest.Name, string(cand.Tier)}
		if prev, seen := seenPerTier[key]; seen {
			_ = prev
			duplicates = append(duplicates, RejectedSkill{
				Slug:   cand.Manifest.Name,
				Tier:   cand.Tier,
				Reason: "duplicate_in_tier",
				Detail: fmt.Sprintf("slug %q appears more than once in tier %q", cand.Manifest.Name, cand.Tier),
				SkillDir: cand.SkillDir,
			})
			continue
		}
		seenPerTier[key] = cand
		bySlug[cand.Manifest.Name] = append(bySlug[cand.Manifest.Name], cand)
	}

	// Sort each group by tier rank ascending (highest precedence first).
	for _, group := range bySlug {
		sort.Slice(group, func(i, j int) bool {
			return tierRank[group[i].Tier] < tierRank[group[j].Tier]
		})
	}

	var selected []*ResolvedSkill
	rejected := append([]RejectedSkill(nil), duplicates...)
	var unknown []SkillRef

	if requested == nil {
		// "All available" mode — apply precedence + eligibility.
		for _, group := range bySlug {
			chosen, groupRejected := pickWithPrecedence(group, "", env)
			rejected = append(rejected, groupRejected...)
			if chosen != nil {
				selected = append(selected, chosen)
			}
		}
	} else {
		// Parse refs.
		refs := make([]SkillRef, 0, len(requested))
		for _, raw := range requested {
			ref, err := ParseSkillRef(raw)
			if err != nil {
				unknown = append(unknown, SkillRef{Raw: raw})
				continue
			}
			refs = append(refs, ref)
		}

		// Mark unrequested skills as not_requested.
		requestedSlugs := map[string]bool{}
		for _, ref := range refs {
			requestedSlugs[ref.Slug] = true
		}
		for slug, group := range bySlug {
			if !requestedSlugs[slug] {
				top := group[0]
				rejected = append(rejected, RejectedSkill{
					Slug:     slug,
					Tier:     top.Tier,
					Reason:   "not_requested",
					Detail:   fmt.Sprintf("skill %q is not listed in agent.yaml skills:", slug),
					SkillDir: top.SkillDir,
				})
			}
		}

		// Resolve each ref.
		for _, ref := range refs {
			group, found := bySlug[ref.Slug]
			if !found {
				unknown = append(unknown, ref)
				continue
			}
			chosen, groupRejected := pickWithPrecedence(group, ref.Version, env)
			rejected = append(rejected, groupRejected...)
			if chosen != nil {
				selected = append(selected, chosen)
			} else {
				unknown = append(unknown, ref)
			}
		}
	}

	// Sort selected by slug for determinism.
	sort.Slice(selected, func(i, j int) bool {
		return selected[i].Manifest.Name < selected[j].Manifest.Name
	})

	return ResolvedSkills{
		Selected: selected,
		Rejected: rejected,
		Unknown:  unknown,
	}
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

// pickWithPrecedence picks the first valid candidate from a tier-sorted group.
// "Valid" means: matches the version pin (if any) AND has every required_env
// set in env. Every loser collects a structured rejection reason.
func pickWithPrecedence(group []*LoadedSkill, requestedVersion string, env map[string]string) (*ResolvedSkill, []RejectedSkill) {
	var rejected []RejectedSkill
	for _, cand := range group {
		manifest := cand.Manifest
		if requestedVersion != "" && manifest.Version != requestedVersion {
			rejected = append(rejected, RejectedSkill{
				Slug:             manifest.Name,
				Tier:             cand.Tier,
				RequestedVersion: requestedVersion,
				Reason:           "version_mismatch",
				Detail: fmt.Sprintf("requested version %q but tier %q ships %q",
					requestedVersion, cand.Tier, manifest.Version),
				SkillDir: cand.SkillDir,
			})
			continue
		}
		var missing []string
		for _, k := range manifest.RequiredEnv {
			if env[k] == "" {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			detail := "missing required env vars: "
			for i, m := range missing {
				if i > 0 {
					detail += ", "
				}
				detail += m
			}
			rejected = append(rejected, RejectedSkill{
				Slug:             manifest.Name,
				Tier:             cand.Tier,
				RequestedVersion: requestedVersion,
				Reason:           "missing_env",
				Detail:           detail,
				SkillDir:         cand.SkillDir,
			})
			continue
		}
		// First matching candidate wins; everything below it is shadowed.
		chosen := &ResolvedSkill{
			Manifest:         manifest,
			Tier:             cand.Tier,
			RequestedVersion: requestedVersion,
			SkillDir:         cand.SkillDir,
		}
		for _, shadow := range group {
			if shadow == cand {
				continue
			}
			if tierRank[shadow.Tier] <= tierRank[cand.Tier] {
				// Already considered + rejected above.
				continue
			}
			rejected = append(rejected, RejectedSkill{
				Slug:             shadow.Manifest.Name,
				Tier:             shadow.Tier,
				RequestedVersion: requestedVersion,
				Reason:           "shadowed",
				Detail:           fmt.Sprintf("outranked by tier %q (selected) — ADR-007 precedence", cand.Tier),
				SkillDir:         shadow.SkillDir,
			})
		}
		return chosen, rejected
	}
	return nil, rejected
}

// osEnvironMap returns a copy of the process environment as a map.
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
