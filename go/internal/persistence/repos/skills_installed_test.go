package repos

import (
	"context"
	"testing"
)

func TestSkillsInstallUninstallList(t *testing.T) {
	pool := freshPool(t)
	users := NewUsersRepo(pool)
	skills := NewSkillsInstalledRepo(pool)

	ctx := context.Background()
	user, err := users.Create(ctx, "skills@example.com")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := skills.Install(ctx, user.ID, "trend-analysis", "0.1.0", "registry"); err != nil {
		t.Fatalf("install ta: %v", err)
	}
	if _, err := skills.Install(ctx, user.ID, "summarize-url", "0.2.0", "local"); err != nil {
		t.Fatalf("install su: %v", err)
	}

	listing, err := skills.ListForUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]bool{}
	for _, s := range listing {
		got[s.Slug] = true
	}
	if !got["trend-analysis"] || !got["summarize-url"] {
		t.Fatalf("missing skills in listing: %v", listing)
	}

	bumped, err := skills.Install(ctx, user.ID, "trend-analysis", "0.1.1", "registry")
	if err != nil {
		t.Fatalf("re-install: %v", err)
	}
	if bumped.Version != "0.1.1" {
		t.Fatalf("expected 0.1.1, got %q", bumped.Version)
	}

	removed, err := skills.Uninstall(ctx, user.ID, "summarize-url")
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !removed {
		t.Fatal("expected uninstall true")
	}

	listing2, err := skills.ListForUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("list 2: %v", err)
	}
	if len(listing2) != 1 || listing2[0].Slug != "trend-analysis" {
		t.Fatalf("expected only trend-analysis, got %+v", listing2)
	}

	removed2, err := skills.Uninstall(ctx, user.ID, "nonexistent")
	if err != nil {
		t.Fatalf("uninstall missing: %v", err)
	}
	if removed2 {
		t.Fatal("expected uninstall false for missing slug")
	}
}
