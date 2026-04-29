package repos

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openclaw/agent-platform/internal/persistence"
)

func TestSessionsUpsertPreservesInitializedAt(t *testing.T) {
	pool := freshPool(t)
	users := NewUsersRepo(pool)
	agents := NewAgentsRepo(pool)
	channels, _ := NewChannelsRepo(pool, nil)
	sessions := NewSessionsRepo(pool)

	ctx := context.Background()
	user, _ := users.Create(ctx, "sess@example.com")
	agent, _ := agents.Create(ctx, user.ID, "sa", "x: 1")
	blob, _ := persistence.Encrypt(fakeKey(), []byte(`{}`))
	chan1, err := channels.Register(ctx, user.ID.String(), "telegram", "tg:sess:1", blob, agent.ID.String())
	if err != nil {
		t.Fatalf("register channel: %v", err)
	}
	chanUUID, _ := uuid.Parse(chan1.ChannelID)

	if err := sessions.UpsertAfterTurn(ctx, user.ID, chanUUID, "thread-A", "claude-cli", "cli-sid-1"); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	first, err := sessions.Get(ctx, user.ID, chanUUID, "thread-A", "claude-cli")
	if err != nil {
		t.Fatalf("get 1: %v", err)
	}
	if first == nil || first.CLISessionID == nil || *first.CLISessionID != "cli-sid-1" {
		t.Fatalf("expected cli-sid-1, got %+v", first)
	}
	if first.InitializedAt == nil {
		t.Fatal("InitializedAt should be set after first turn")
	}
	initFirst := *first.InitializedAt
	lastFirst := first.LastUsedAt

	time.Sleep(20 * time.Millisecond) // advance Postgres now()

	if err := sessions.UpsertAfterTurn(ctx, user.ID, chanUUID, "thread-A", "claude-cli", "cli-sid-2"); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	second, err := sessions.Get(ctx, user.ID, chanUUID, "thread-A", "claude-cli")
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if *second.CLISessionID != "cli-sid-2" {
		t.Fatalf("expected cli-sid-2, got %v", second.CLISessionID)
	}
	if !second.InitializedAt.Equal(initFirst) {
		t.Fatalf("InitializedAt changed across turns: %v vs %v", *second.InitializedAt, initFirst)
	}
	if second.LastUsedAt.Before(lastFirst) {
		t.Fatalf("LastUsedAt regressed: %v vs %v", second.LastUsedAt, lastFirst)
	}
}

func TestSessionsDropForProviderIsolated(t *testing.T) {
	pool := freshPool(t)
	users := NewUsersRepo(pool)
	agents := NewAgentsRepo(pool)
	channels, _ := NewChannelsRepo(pool, nil)
	sessions := NewSessionsRepo(pool)

	ctx := context.Background()
	u1, _ := users.Create(ctx, "u1@example.com")
	u2, _ := users.Create(ctx, "u2@example.com")
	a1, _ := agents.Create(ctx, u1.ID, "n1", "x: 1")
	a2, _ := agents.Create(ctx, u2.ID, "n2", "x: 1")

	blob, _ := persistence.Encrypt(fakeKey(), []byte(`{}`))
	c1, _ := channels.Register(ctx, u1.ID.String(), "telegram", "tg:scope:1", blob, a1.ID.String())
	c2, _ := channels.Register(ctx, u2.ID.String(), "telegram", "tg:scope:2", blob, a2.ID.String())
	c1u, _ := uuid.Parse(c1.ChannelID)
	c2u, _ := uuid.Parse(c2.ChannelID)

	must := func(err error, label string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}
	must(sessions.UpsertAfterTurn(ctx, u1.ID, c1u, "t", "codex-cli", "u1-cd"), "u1 cd")
	must(sessions.UpsertAfterTurn(ctx, u1.ID, c1u, "t", "claude-cli", "u1-cl"), "u1 cl")
	must(sessions.UpsertAfterTurn(ctx, u2.ID, c2u, "t", "codex-cli", "u2-cd"), "u2 cd")
	must(sessions.UpsertAfterTurn(ctx, u2.ID, c2u, "t", "claude-cli", "u2-cl"), "u2 cl")

	dropped, err := sessions.DropForProvider(ctx, u1.ID, "codex-cli")
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("expected 1 row dropped, got %d", dropped)
	}

	check := func(uid, cid uuid.UUID, provider string, wantNil bool, label string) {
		t.Helper()
		got, err := sessions.Get(ctx, uid, cid, "t", provider)
		if err != nil {
			t.Fatalf("get %s: %v", label, err)
		}
		if wantNil && got != nil {
			t.Fatalf("%s: expected nil, got %+v", label, got)
		}
		if !wantNil && got == nil {
			t.Fatalf("%s: expected row, got nil", label)
		}
	}
	check(u1.ID, c1u, "claude-cli", false, "u1 claude survives")
	check(u1.ID, c1u, "codex-cli", true, "u1 codex gone")
	check(u2.ID, c2u, "codex-cli", false, "u2 codex untouched")
	check(u2.ID, c2u, "claude-cli", false, "u2 claude untouched")
}
