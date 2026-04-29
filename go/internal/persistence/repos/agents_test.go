package repos

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestAgentsCreateListUpdate(t *testing.T) {
	pool := freshPool(t)
	users := NewUsersRepo(pool)
	agents := NewAgentsRepo(pool)

	ctx := context.Background()
	user, err := users.Create(ctx, "agent-owner@example.com")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	a1, err := agents.Create(ctx, user.ID, "alpha", "providers: []\n")
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	a2, err := agents.Create(ctx, user.ID, "beta", "providers: [a]\n")
	if err != nil {
		t.Fatalf("create beta: %v", err)
	}

	listing, err := agents.ListForUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listing) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(listing))
	}
	gotIDs := map[uuid.UUID]bool{}
	for _, a := range listing {
		gotIDs[a.ID] = true
	}
	if !gotIDs[a1.ID] || !gotIDs[a2.ID] {
		t.Fatalf("expected ids %v %v in listing, got %v", a1.ID, a2.ID, listing)
	}
	// Order: oldest first (created_at ASC). a1 should be first.
	if listing[0].Name != "alpha" || listing[1].Name != "beta" {
		t.Fatalf("expected [alpha, beta], got [%s, %s]", listing[0].Name, listing[1].Name)
	}

	updated, err := agents.UpdateConfig(ctx, a1.ID, "providers: [b]\n")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated == nil {
		t.Fatal("expected updated agent")
	}
	if updated.ConfigYAML != "providers: [b]\n" {
		t.Fatalf("expected new yaml, got %q", updated.ConfigYAML)
	}
	if updated.UpdatedAt.Before(updated.CreatedAt) {
		t.Fatalf("UpdatedAt %v should be >= CreatedAt %v", updated.UpdatedAt, updated.CreatedAt)
	}
}

func TestAgentsUpdateMissingReturnsNil(t *testing.T) {
	pool := freshPool(t)
	repo := NewAgentsRepo(pool)
	got, err := repo.UpdateConfig(context.Background(), uuid.New(), "x: 1")
	if err != nil {
		t.Fatalf("update missing: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}
