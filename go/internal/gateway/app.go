package gateway

import (
	"context"
	"errors"
	"time"
)

// App is the gateway's runtime dependency container.
//
// Mirrors the Python `app.state.*` attributes (config, redis, queue,
// idempotency, channels_repo, users_repo, agents_repo, orchestrator,
// db_health). Routes pull dependencies off this struct rather than off a
// global; tests construct an App directly with in-memory fakes.
type App struct {
	Config       Config
	Redis        RedisDoer
	Queue        *AgentRunQueue
	Idempotency  *IdempotencyCache
	ChannelsRepo ChannelsRepo
	UsersRepo    UsersRepo
	AgentsRepo   AgentsRepo
	Orchestrator OrchestratorClient
	DBHealth     HealthCheckable
	Channels     *ChannelRegistry
}

// NewApp validates wiring and constructs an App. All non-Redis deps default
// to in-memory implementations when nil so tests can build minimal Apps with
// just a fake Redis.
func NewApp(
	cfg Config,
	rdb RedisDoer,
	channelsRepo ChannelsRepo,
	usersRepo UsersRepo,
	agentsRepo AgentsRepo,
	orchestrator OrchestratorClient,
	dbHealth HealthCheckable,
	registry *ChannelRegistry,
) (*App, error) {
	if rdb == nil {
		return nil, errors.New("redis client is required")
	}
	queue, err := NewAgentRunQueue(rdb, cfg.StreamName)
	if err != nil {
		return nil, err
	}
	ttl := cfg.IdempotencyTTLSeconds
	if ttl <= 0 {
		ttl = 600
	}
	idem, err := NewIdempotencyCache(rdb, ttl, cfg.IdempotencyNamespace)
	if err != nil {
		return nil, err
	}
	if channelsRepo == nil {
		channelsRepo = NewInMemoryChannelsRepo()
	}
	if usersRepo == nil {
		usersRepo = NewInMemoryUsersRepo()
	}
	if agentsRepo == nil {
		agentsRepo = NewInMemoryAgentsRepo()
	}
	if dbHealth == nil {
		dbHealth = AlwaysHealthy{}
	}
	if registry == nil {
		registry = DefaultChannelRegistry()
	}
	if orchestrator == nil {
		orchestrator = NewHttpOrchestratorClient(cfg.OrchestratorURL, 5*time.Second)
	}
	return &App{
		Config:       cfg,
		Redis:        rdb,
		Queue:        queue,
		Idempotency:  idem,
		ChannelsRepo: channelsRepo,
		UsersRepo:    usersRepo,
		AgentsRepo:   agentsRepo,
		Orchestrator: orchestrator,
		DBHealth:     dbHealth,
		Channels:     registry,
	}, nil
}

// UserJWTTTL returns the configured user JWT TTL as a time.Duration.
func (a *App) UserJWTTTL() time.Duration {
	if a.Config.UserJWTTTLSeconds <= 0 {
		return 24 * time.Hour
	}
	return time.Duration(a.Config.UserJWTTTLSeconds) * time.Second
}

// pingRedis is a small helper for /readyz so handlers don't hard-code the
// boilerplate. Returns false on any error, including context-deadline.
func pingRedis(ctx context.Context, rdb RedisDoer) bool {
	if rdb == nil {
		return false
	}
	cmd := rdb.Ping(ctx)
	if cmd == nil {
		return false
	}
	if err := cmd.Err(); err != nil {
		return false
	}
	return true
}
