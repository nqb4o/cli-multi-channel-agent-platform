package gateway

import (
	"context"
	"testing"
	"time"
)

func TestNewApp_RequiresRedis(t *testing.T) {
	cfg := Config{StreamName: "agent:runs", IdempotencyTTLSeconds: 60}
	if _, err := NewApp(cfg, nil, nil, nil, nil, nil, nil, nil); err == nil {
		t.Fatal("nil redis should error")
	}
}

func TestNewApp_PopulatesDefaults(t *testing.T) {
	c, _ := newMiniredisClient(t)
	cfg := Config{StreamName: "agent:runs", IdempotencyTTLSeconds: 60}
	app, err := NewApp(cfg, c, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if app.UsersRepo == nil || app.AgentsRepo == nil || app.ChannelsRepo == nil {
		t.Fatal("default repos not wired")
	}
	if app.Channels == nil {
		t.Fatal("default registry not wired")
	}
	if app.Orchestrator == nil {
		t.Fatal("default orchestrator client not wired")
	}
	if app.DBHealth == nil {
		t.Fatal("default db_health not wired")
	}
	if !app.DBHealth.Ping(context.Background()) {
		t.Fatal("AlwaysHealthy default should ping ok")
	}
	if got := app.UserJWTTTL(); got != 24*time.Hour {
		t.Fatalf("default ttl should be 24h, got %v", got)
	}
}

func TestNewApp_HonoursConfiguredTTL(t *testing.T) {
	c, _ := newMiniredisClient(t)
	cfg := Config{
		StreamName:            "agent:runs",
		IdempotencyTTLSeconds: 60,
		UserJWTTTLSeconds:     7200,
	}
	app, err := NewApp(cfg, c, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := app.UserJWTTTL(); got != 2*time.Hour {
		t.Fatalf("expected 2h, got %v", got)
	}
}

func TestNewApp_DefaultIdempotencyTTLWhenZero(t *testing.T) {
	c, _ := newMiniredisClient(t)
	cfg := Config{StreamName: "agent:runs"} // TTL=0
	app, err := NewApp(cfg, c, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("zero TTL should fall back to 600s, got %v", err)
	}
	if app.Idempotency == nil {
		t.Fatal("idempotency cache missing")
	}
}
