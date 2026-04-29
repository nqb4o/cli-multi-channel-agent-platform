package repos

import (
	"context"
	"testing"
)

func TestUsersCreateAndGetByEmail(t *testing.T) {
	pool := freshPool(t)
	repo := NewUsersRepo(pool)

	ctx := context.Background()
	u, err := repo.Create(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if u.Email != "alice@example.com" || u.ID.String() == "" {
		t.Fatalf("unexpected user: %+v", u)
	}

	got, err := repo.GetByEmail(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.ID != u.ID {
		t.Fatalf("expected same user, got %+v", got)
	}
}

func TestUsersGetByEmailMissing(t *testing.T) {
	pool := freshPool(t)
	repo := NewUsersRepo(pool)
	got, err := repo.GetByEmail(context.Background(), "nope@example.com")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}
