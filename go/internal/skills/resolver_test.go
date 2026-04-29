package skills

import (
	"testing"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func makeLoaded(name, version string, tier SourceTier) *LoadedSkill {
	return &LoadedSkill{
		Manifest: &SkillManifest{
			Name:    name,
			Version: version,
		},
		Tier:     tier,
		SkillDir: "/fake/" + name,
	}
}

func makeLoadedWithEnv(name, version string, tier SourceTier, requiredEnv []string) *LoadedSkill {
	s := makeLoaded(name, version, tier)
	s.Manifest.RequiredEnv = requiredEnv
	return s
}

// ---------------------------------------------------------------------------
// ParseSkillRef
// ---------------------------------------------------------------------------

func TestParseSkillRefBareSlug(t *testing.T) {
	ref, err := ParseSkillRef("web-search")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.Slug != "web-search" {
		t.Fatalf("Slug=%q", ref.Slug)
	}
	if ref.Version != "" {
		t.Fatalf("Version should be empty, got %q", ref.Version)
	}
}

func TestParseSkillRefWithVersion(t *testing.T) {
	ref, err := ParseSkillRef("web-search@1.2.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.Slug != "web-search" {
		t.Fatalf("Slug=%q", ref.Slug)
	}
	if ref.Version != "1.2.0" {
		t.Fatalf("Version=%q", ref.Version)
	}
}

func TestParseSkillRefInvalid(t *testing.T) {
	_, err := ParseSkillRef("My Invalid Slug!")
	if err == nil {
		t.Fatal("expected error for invalid ref")
	}
}

func TestParseSkillRefEmpty(t *testing.T) {
	_, err := ParseSkillRef("")
	if err == nil {
		t.Fatal("expected error for empty ref")
	}
}

// ---------------------------------------------------------------------------
// Precedence: workspace wins over project/managed/bundled
// ---------------------------------------------------------------------------

func TestResolvePrecedenceWorkspaceWinsOverManaged(t *testing.T) {
	available := []*LoadedSkill{
		makeLoaded("web-search", "1.0.0", TierManaged),
		makeLoaded("web-search", "2.0.0", TierWorkspace),
	}
	result := Resolve(available, nil, map[string]string{})
	if len(result.Selected) != 1 {
		t.Fatalf("expected 1 selected, got %d", len(result.Selected))
	}
	if result.Selected[0].Tier != TierWorkspace {
		t.Fatalf("workspace should win, got tier %q", result.Selected[0].Tier)
	}
	if result.Selected[0].Manifest.Version != "2.0.0" {
		t.Fatalf("workspace version should be 2.0.0, got %q", result.Selected[0].Manifest.Version)
	}
}

func TestResolvePrecedenceProjectWinsOverPersonal(t *testing.T) {
	available := []*LoadedSkill{
		makeLoaded("foo", "1.0.0", TierPersonal),
		makeLoaded("foo", "1.0.0", TierProject),
	}
	result := Resolve(available, nil, map[string]string{})
	if len(result.Selected) != 1 {
		t.Fatalf("expected 1 selected, got %d", len(result.Selected))
	}
	if result.Selected[0].Tier != TierProject {
		t.Fatalf("project should win, got tier %q", result.Selected[0].Tier)
	}
}

func TestResolvePrecedenceAllTiers(t *testing.T) {
	available := []*LoadedSkill{
		makeLoaded("foo", "bundled", TierBundled),
		makeLoaded("foo", "managed", TierManaged),
		makeLoaded("foo", "personal", TierPersonal),
		makeLoaded("foo", "project", TierProject),
		makeLoaded("foo", "workspace", TierWorkspace),
	}
	result := Resolve(available, nil, map[string]string{})
	if len(result.Selected) != 1 {
		t.Fatalf("expected 1 selected, got %d", len(result.Selected))
	}
	if result.Selected[0].Manifest.Version != "workspace" {
		t.Fatalf("workspace should win, got version %q", result.Selected[0].Manifest.Version)
	}
}

// ---------------------------------------------------------------------------
// Version pinning
// ---------------------------------------------------------------------------

func TestResolveVersionPinHonoured(t *testing.T) {
	available := []*LoadedSkill{
		makeLoaded("web-search", "2.0.0", TierWorkspace),
		makeLoaded("web-search", "1.0.0", TierManaged),
	}
	// Pin to 1.0.0 — workspace doesn't have it, managed does.
	result := Resolve(available, []string{"web-search@1.0.0"}, map[string]string{})
	if len(result.Selected) != 1 {
		t.Fatalf("expected 1 selected, got %d: unknown=%v rejected=%v", len(result.Selected), result.Unknown, result.Rejected)
	}
	if result.Selected[0].Tier != TierManaged {
		t.Fatalf("managed should be selected (has pinned version), got %q", result.Selected[0].Tier)
	}
}

func TestResolveVersionPinNotFoundGoesToUnknown(t *testing.T) {
	available := []*LoadedSkill{
		makeLoaded("web-search", "1.0.0", TierWorkspace),
	}
	result := Resolve(available, []string{"web-search@9.9.9"}, map[string]string{})
	if len(result.Selected) != 0 {
		t.Fatalf("expected 0 selected, got %d", len(result.Selected))
	}
	if len(result.Unknown) != 1 {
		t.Fatalf("expected 1 unknown, got %d", len(result.Unknown))
	}
}

// ---------------------------------------------------------------------------
// required_env eligibility
// ---------------------------------------------------------------------------

func TestResolveMissingEnvFiltersSkill(t *testing.T) {
	available := []*LoadedSkill{
		makeLoadedWithEnv("trend-analysis", "1.0.0", TierManaged, []string{"TIINGO_API_KEY"}),
	}
	result := Resolve(available, nil, map[string]string{}) // no env
	if len(result.Selected) != 0 {
		t.Fatalf("expected 0 selected (missing env), got %d", len(result.Selected))
	}
	if len(result.Rejected) == 0 {
		t.Fatal("expected at least one rejected skill")
	}
	found := false
	for _, r := range result.Rejected {
		if r.Reason == "missing_env" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected missing_env reason, got %v", result.Rejected)
	}
}

func TestResolveEnvPresentAllowsSkill(t *testing.T) {
	available := []*LoadedSkill{
		makeLoadedWithEnv("trend-analysis", "1.0.0", TierManaged, []string{"TIINGO_API_KEY"}),
	}
	result := Resolve(available, nil, map[string]string{"TIINGO_API_KEY": "tok"})
	if len(result.Selected) != 1 {
		t.Fatalf("expected 1 selected, got %d", len(result.Selected))
	}
}

// ---------------------------------------------------------------------------
// Requested mode
// ---------------------------------------------------------------------------

func TestResolveRequestedModeFiltersUnrequested(t *testing.T) {
	available := []*LoadedSkill{
		makeLoaded("web-search", "1.0.0", TierWorkspace),
		makeLoaded("summarize-url", "1.0.0", TierWorkspace),
	}
	result := Resolve(available, []string{"web-search"}, map[string]string{})
	if len(result.Selected) != 1 {
		t.Fatalf("expected 1 selected, got %d", len(result.Selected))
	}
	if result.Selected[0].Manifest.Name != "web-search" {
		t.Fatalf("Name=%q", result.Selected[0].Manifest.Name)
	}
	// summarize-url is available but not requested → not_requested
	found := false
	for _, r := range result.Rejected {
		if r.Slug == "summarize-url" && r.Reason == "not_requested" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected not_requested for summarize-url, got %v", result.Rejected)
	}
}

func TestResolveUnknownSlugGoesToUnknown(t *testing.T) {
	available := []*LoadedSkill{
		makeLoaded("web-search", "1.0.0", TierWorkspace),
	}
	result := Resolve(available, []string{"does-not-exist"}, map[string]string{})
	if len(result.Unknown) != 1 {
		t.Fatalf("expected 1 unknown, got %d", len(result.Unknown))
	}
}

func TestResolveNilRequestedLoadsAll(t *testing.T) {
	available := []*LoadedSkill{
		makeLoaded("web-search", "1.0.0", TierWorkspace),
		makeLoaded("summarize-url", "1.0.0", TierManaged),
	}
	result := Resolve(available, nil, map[string]string{})
	if len(result.Selected) != 2 {
		t.Fatalf("expected 2 selected, got %d", len(result.Selected))
	}
}

func TestResolveEmptyRequestedLoadsNothing(t *testing.T) {
	available := []*LoadedSkill{
		makeLoaded("web-search", "1.0.0", TierWorkspace),
	}
	result := Resolve(available, []string{}, map[string]string{})
	if len(result.Selected) != 0 {
		t.Fatalf("expected 0 selected, got %d", len(result.Selected))
	}
	// web-search is available but not requested → not_requested
	if len(result.Rejected) == 0 {
		t.Fatal("expected not_requested rejection")
	}
}

// ---------------------------------------------------------------------------
// Duplicate in tier
// ---------------------------------------------------------------------------

func TestResolveDuplicateInTierRejected(t *testing.T) {
	available := []*LoadedSkill{
		makeLoaded("web-search", "1.0.0", TierWorkspace),
		makeLoaded("web-search", "1.0.0", TierWorkspace), // same slug + tier
	}
	result := Resolve(available, nil, map[string]string{})
	found := false
	for _, r := range result.Rejected {
		if r.Reason == "duplicate_in_tier" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected duplicate_in_tier rejection")
	}
}

// ---------------------------------------------------------------------------
// Sorted output
// ---------------------------------------------------------------------------

func TestResolveSelectedSortedBySlug(t *testing.T) {
	available := []*LoadedSkill{
		makeLoaded("zzz-skill", "1.0.0", TierWorkspace),
		makeLoaded("aaa-skill", "1.0.0", TierWorkspace),
		makeLoaded("mmm-skill", "1.0.0", TierWorkspace),
	}
	result := Resolve(available, nil, map[string]string{})
	if len(result.Selected) != 3 {
		t.Fatalf("expected 3 selected, got %d", len(result.Selected))
	}
	if result.Selected[0].Manifest.Name != "aaa-skill" {
		t.Fatalf("first should be aaa-skill, got %q", result.Selected[0].Manifest.Name)
	}
	if result.Selected[2].Manifest.Name != "zzz-skill" {
		t.Fatalf("last should be zzz-skill, got %q", result.Selected[2].Manifest.Name)
	}
}

// ---------------------------------------------------------------------------
// Shadowing reason
// ---------------------------------------------------------------------------

func TestResolveLowerTierShadowed(t *testing.T) {
	available := []*LoadedSkill{
		makeLoaded("foo", "1.0.0", TierWorkspace),
		makeLoaded("foo", "1.0.0", TierBundled),
	}
	result := Resolve(available, nil, map[string]string{})
	if len(result.Selected) != 1 {
		t.Fatalf("expected 1 selected, got %d", len(result.Selected))
	}
	found := false
	for _, r := range result.Rejected {
		if r.Reason == "shadowed" && r.Tier == TierBundled {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected bundled to be shadowed, got %v", result.Rejected)
	}
}
