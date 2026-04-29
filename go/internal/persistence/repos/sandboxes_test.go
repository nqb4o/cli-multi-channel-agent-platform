package repos

import (
	"context"
	"testing"
	"time"
)

func TestSandboxesUpsertAndGetForUser(t *testing.T) {
	pool := freshPool(t)
	users := NewUsersRepo(pool)
	sb := NewSandboxesRepo(pool)

	ctx := context.Background()
	user, err := users.Create(ctx, "sb@example.com")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	created, err := sb.Upsert(ctx, user.ID, "dy-1", "provisioning")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if created.DaytonaID != "dy-1" || created.State != "provisioning" {
		t.Fatalf("unexpected sandbox: %+v", created)
	}

	// Upsert again with new daytona id; same row (user_id UNIQUE).
	bumped, err := sb.Upsert(ctx, user.ID, "dy-2", "running")
	if err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	if bumped.ID != created.ID {
		t.Fatalf("expected same row id; got %s vs %s", bumped.ID, created.ID)
	}
	if bumped.DaytonaID != "dy-2" {
		t.Fatalf("expected dy-2, got %s", bumped.DaytonaID)
	}

	got, err := sb.GetForUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.ID != bumped.ID {
		t.Fatalf("get returned %+v", got)
	}
}

func TestSandboxesUpdateStateBumpsLastActiveAt(t *testing.T) {
	pool := freshPool(t)
	users := NewUsersRepo(pool)
	sb := NewSandboxesRepo(pool)

	ctx := context.Background()
	user, _ := users.Create(ctx, "us@example.com")
	if _, err := sb.Upsert(ctx, user.ID, "dy-1", "provisioning"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	now := time.Now().UTC()
	updated, err := sb.UpdateState(ctx, user.ID, "running", &now)
	if err != nil {
		t.Fatalf("update_state: %v", err)
	}
	if updated == nil || updated.State != "running" {
		t.Fatalf("expected running, got %+v", updated)
	}
	if updated.LastActiveAt == nil {
		t.Fatal("expected LastActiveAt to be set")
	}
}
