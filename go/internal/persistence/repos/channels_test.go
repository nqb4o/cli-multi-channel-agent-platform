package repos

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openclaw/agent-platform/internal/persistence"
)

func newChannelsRepoWithKey(t *testing.T, pool *pgxpool.Pool) *ChannelsRepo {
	t.Helper()
	cfg := &persistence.Config{
		DSN:              "x",
		EncryptionKeyHex: hex.EncodeToString(fakeKey()),
	}
	r, err := NewChannelsRepo(pool, cfg)
	if err != nil {
		t.Fatalf("NewChannelsRepo: %v", err)
	}
	return r
}

func TestChannelsRegisterAndLookup(t *testing.T) {
	pool := freshPool(t)
	users := NewUsersRepo(pool)
	agents := NewAgentsRepo(pool)
	channels := newChannelsRepoWithKey(t, pool)

	ctx := context.Background()
	user, err := users.Create(ctx, "ch@example.com")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	agent, err := agents.Create(ctx, user.ID, "chbot", "x: 1")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	encrypted, err := persistence.Encrypt(fakeKey(), []byte(`{"webhook_secret": "wh"}`))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	record, err := channels.Register(ctx, user.ID.String(), "telegram", "tg:111:222", encrypted, agent.ID.String())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if record.UserID != user.ID.String() {
		t.Fatalf("user_id mismatch: %s vs %s", record.UserID, user.ID)
	}
	if record.AgentID != agent.ID.String() {
		t.Fatalf("agent_id mismatch: %s vs %s", record.AgentID, agent.ID)
	}

	routing, err := channels.LookupRouting(ctx, "telegram", "tg:111:222")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if routing == nil || routing.ChannelID != record.ChannelID {
		t.Fatalf("routing mismatch: %+v vs %+v", routing, record)
	}

	chanUUID, err := uuid.Parse(record.ChannelID)
	if err != nil {
		t.Fatalf("parse channel id: %v", err)
	}
	plain, err := channels.GetDecryptedConfig(ctx, chanUUID)
	if err != nil {
		t.Fatalf("get decrypted: %v", err)
	}
	if !bytes.Equal(plain, []byte(`{"webhook_secret": "wh"}`)) {
		t.Fatalf("decrypted mismatch: %q", plain)
	}
}

func TestChannelsLookupUnregisteredReturnsNil(t *testing.T) {
	pool := freshPool(t)
	repo, err := NewChannelsRepo(pool, nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	got, err := repo.LookupRouting(context.Background(), "telegram", "tg:404:404")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestChannelsRegisterIdempotentOnConflict(t *testing.T) {
	pool := freshPool(t)
	users := NewUsersRepo(pool)
	agents := NewAgentsRepo(pool)
	channels, err := NewChannelsRepo(pool, nil)
	if err != nil {
		t.Fatalf("new channels: %v", err)
	}

	ctx := context.Background()
	user, _ := users.Create(ctx, "idem@example.com")
	agent, _ := agents.Create(ctx, user.ID, "idem", "x: 1")
	blob, _ := persistence.Encrypt(fakeKey(), []byte(`{}`))

	a, err := channels.Register(ctx, user.ID.String(), "telegram", "tg:dup:1", blob, agent.ID.String())
	if err != nil {
		t.Fatalf("register a: %v", err)
	}
	b, err := channels.Register(ctx, user.ID.String(), "telegram", "tg:dup:1", blob, agent.ID.String())
	if err != nil {
		t.Fatalf("register b: %v", err)
	}
	if a.ChannelID != b.ChannelID {
		t.Fatalf("expected idempotent register; got %s vs %s", a.ChannelID, b.ChannelID)
	}
}
