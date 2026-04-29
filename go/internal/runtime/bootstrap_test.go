package runtime

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fixtureWorkspace points at testdata/workspace, the Go-side mirror of
// services/runtime/tests/fixtures/workspace.
var fixtureWorkspace = filepath.Join("testdata", "workspace")

func TestBootstrapFilenamesAreInDocumentedOrder(t *testing.T) {
	want := []string{
		"AGENTS.md",
		"SOUL.md",
		"IDENTITY.md",
		"USER.md",
		"BOOTSTRAP.md",
	}
	if !reflect.DeepEqual(BootstrapFilenames, want) {
		t.Fatalf("BootstrapFilenames: %v", BootstrapFilenames)
	}
}

func TestLoadReturnsOneEntryPerCanonicalFilename(t *testing.T) {
	tmp := t.TempDir()
	files := LoadBootstrapFiles(tmp)
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	if !reflect.DeepEqual(names, BootstrapFilenames) {
		t.Fatalf("names: %v", names)
	}
	for _, f := range files {
		if !f.Missing {
			t.Fatalf("expected %s missing in empty workspace", f.Name)
		}
		if f.Content != nil {
			t.Fatalf("expected %s content nil, got %v", f.Name, *f.Content)
		}
	}
}

func TestLoadPresentFilesHaveContent(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "AGENTS.md"), []byte("agents body"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "SOUL.md"), []byte("soul body"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := LoadBootstrapFiles(tmp)
	byName := indexByName(files)
	if byName["AGENTS.md"].Missing {
		t.Fatal("AGENTS.md should be present")
	}
	if byName["AGENTS.md"].Content == nil || *byName["AGENTS.md"].Content != "agents body" {
		t.Fatalf("AGENTS.md content: %v", byName["AGENTS.md"].Content)
	}
	if *byName["SOUL.md"].Content != "soul body" {
		t.Fatalf("SOUL.md: %v", byName["SOUL.md"].Content)
	}
	if !byName["IDENTITY.md"].Missing {
		t.Fatal("IDENTITY.md should be missing")
	}
}

func TestLoadTreatsPresentButEmptyAsPresent(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "AGENTS.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	files := LoadBootstrapFiles(tmp)
	byName := indexByName(files)
	if byName["AGENTS.md"].Missing {
		t.Fatal("present-but-empty file should not be Missing")
	}
	if byName["AGENTS.md"].Content == nil || *byName["AGENTS.md"].Content != "" {
		t.Fatalf("AGENTS.md content: %v", byName["AGENTS.md"].Content)
	}
}

func TestLoadTrimsFilesToBudget(t *testing.T) {
	tmp := t.TempDir()
	huge := strings.Repeat("X", PerFileMaxChars+5_000)
	if err := os.WriteFile(filepath.Join(tmp, "AGENTS.md"), []byte(huge), 0o644); err != nil {
		t.Fatal(err)
	}
	files := LoadBootstrapFiles(tmp)
	byName := indexByName(files)
	c := byName["AGENTS.md"].Content
	if c == nil {
		t.Fatal("expected non-nil content")
	}
	if len(*c) > PerFileMaxChars+200 {
		t.Fatalf("content not trimmed: len=%d", len(*c))
	}
	if !strings.Contains(*c, "truncated by runtime bootstrap") {
		t.Fatal("missing truncation marker")
	}
}

func TestLoadHandlesUnreadableFiles(t *testing.T) {
	tmp := t.TempDir()
	// Invalid UTF-8 bytes — treated as missing rather than crashing.
	if err := os.WriteFile(filepath.Join(tmp, "AGENTS.md"), []byte{0xff, 0xfe, 0x00, 0x01, 0x80}, 0o644); err != nil {
		t.Fatal(err)
	}
	files := LoadBootstrapFiles(tmp)
	byName := indexByName(files)
	if !byName["AGENTS.md"].Missing {
		t.Fatal("invalid UTF-8 should be treated as missing")
	}
	if byName["AGENTS.md"].Content != nil {
		t.Fatal("expected nil content for invalid UTF-8")
	}
}

func TestLoadUsesDefaultWorkspaceWhenArgIsEmpty(t *testing.T) {
	// Using "" should fall through to DefaultWorkspaceDir. We can't
	// control /home/user/workspace from a test, but the loader must
	// not crash — it should return one entry per filename, all missing.
	files := LoadBootstrapFiles("")
	if len(files) != len(BootstrapFilenames) {
		t.Fatalf("len=%d, want %d", len(files), len(BootstrapFilenames))
	}
}

// ---------------------------------------------------------------------------
// build_system_prompt
// ---------------------------------------------------------------------------

func TestBuildSystemPromptIdentityOnlyWhenNoFiles(t *testing.T) {
	identity := IdentityConfig{Name: "Stub Buddy"}
	prompt := BuildSystemPrompt(identity, nil, "")
	if !strings.Contains(prompt, "You are Stub Buddy.") {
		t.Fatalf("prompt missing identity line: %q", prompt)
	}
	if strings.Contains(prompt, "# AGENTS.md") {
		t.Fatal("no files → no headers expected")
	}
	if !strings.HasSuffix(prompt, "\n") {
		t.Fatal("prompt should end with newline")
	}
}

func TestBuildSystemPromptEmitsFilesInOrder(t *testing.T) {
	identity := IdentityConfig{Name: "Stub Buddy"}
	a := "A1"
	s := "S1"
	i := "I1"
	files := []BootstrapFile{
		{Name: "AGENTS.md", Content: &a},
		{Name: "SOUL.md", Content: &s},
		{Name: "IDENTITY.md", Content: &i},
	}
	prompt := BuildSystemPrompt(identity, files, "")
	pa := strings.Index(prompt, "# AGENTS.md")
	ps := strings.Index(prompt, "# SOUL.md")
	pi := strings.Index(prompt, "# IDENTITY.md")
	if pa < 0 || ps < 0 || pi < 0 {
		t.Fatalf("missing headers: %q", prompt)
	}
	if !(pa < ps && ps < pi) {
		t.Fatalf("section order wrong: AGENTS=%d SOUL=%d IDENTITY=%d", pa, ps, pi)
	}
}

func TestBuildSystemPromptSkipsMissingAndEmptyFiles(t *testing.T) {
	identity := IdentityConfig{Name: "X"}
	empty := ""
	body := "i-body"
	files := []BootstrapFile{
		{Name: "AGENTS.md", Missing: true},
		{Name: "SOUL.md", Content: &empty},
		{Name: "IDENTITY.md", Content: &body},
	}
	prompt := BuildSystemPrompt(identity, files, "")
	if strings.Contains(prompt, "# AGENTS.md") {
		t.Fatal("missing AGENTS.md should not be in prompt")
	}
	if strings.Contains(prompt, "# SOUL.md") {
		t.Fatal("empty SOUL.md should not be in prompt")
	}
	if !strings.Contains(prompt, "# IDENTITY.md") {
		t.Fatal("non-empty IDENTITY.md should be in prompt")
	}
	if !strings.Contains(prompt, "i-body") {
		t.Fatal("IDENTITY.md body missing")
	}
}

func TestBuildSystemPromptExtraPreambleAfterIdentity(t *testing.T) {
	identity := IdentityConfig{Name: "Buddy"}
	prompt := BuildSystemPrompt(identity, nil, "(do not blink)")
	idx := strings.Index(prompt, "You are Buddy.")
	pre := strings.Index(prompt, "(do not blink)")
	if idx < 0 || pre < 0 {
		t.Fatalf("missing pieces: %q", prompt)
	}
	if !(idx < pre) {
		t.Fatalf("identity must come before preamble: idx=%d pre=%d", idx, pre)
	}
}

func TestBuildSystemPromptIdentityName(t *testing.T) {
	p1 := BuildSystemPrompt(IdentityConfig{Name: "Atlas"}, nil, "")
	p2 := BuildSystemPrompt(IdentityConfig{Name: "Bee"}, nil, "")
	if !strings.Contains(p1, "You are Atlas.") {
		t.Fatal("p1 should mention Atlas")
	}
	if !strings.Contains(p2, "You are Bee.") {
		t.Fatal("p2 should mention Bee")
	}
	if strings.Contains(p1, "Bee") || strings.Contains(p2, "Atlas") {
		t.Fatal("names crossed over")
	}
}

// ---------------------------------------------------------------------------
// End-to-end against the testdata fixture.
// ---------------------------------------------------------------------------

func TestLoadRealFixtureWorkspace(t *testing.T) {
	files := LoadBootstrapFiles(fixtureWorkspace)
	byName := indexByName(files)
	if byName["AGENTS.md"].Missing {
		t.Fatal("fixture should have AGENTS.md")
	}
	if byName["SOUL.md"].Missing {
		t.Fatal("fixture should have SOUL.md")
	}
	if byName["IDENTITY.md"].Missing {
		t.Fatal("fixture should have IDENTITY.md")
	}
	if byName["USER.md"].Missing {
		t.Fatal("fixture should have USER.md")
	}
	if !byName["BOOTSTRAP.md"].Missing {
		t.Fatal("fixture should NOT have BOOTSTRAP.md")
	}
}

func TestBuildSystemPromptConcatRealFixture(t *testing.T) {
	files := LoadBootstrapFiles(fixtureWorkspace)
	prompt := BuildSystemPrompt(IdentityConfig{Name: "Stub Buddy"}, files, "")
	if !strings.Contains(prompt, "You are Stub Buddy.") {
		t.Fatal("missing identity line")
	}
	for _, h := range []string{"# AGENTS.md", "# SOUL.md", "# IDENTITY.md", "# USER.md"} {
		if !strings.Contains(prompt, h) {
			t.Fatalf("prompt missing %q", h)
		}
	}
	if strings.Contains(prompt, "# BOOTSTRAP.md") {
		t.Fatal("BOOTSTRAP.md should not appear (missing in fixture)")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func indexByName(files []BootstrapFile) map[string]BootstrapFile {
	m := make(map[string]BootstrapFile, len(files))
	for _, f := range files {
		m[f.Name] = f
	}
	return m
}
