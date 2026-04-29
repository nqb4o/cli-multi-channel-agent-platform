// Package gateway is the F06 gateway HTTP service: webhooks, signup/login,
// agent + channel CRUD, and the producer side of the agent:runs Redis Stream.
//
// Configuration is environment-driven; LoadConfig reads the same env vars as
// the Python implementation (services/gateway/src/gateway/config.py) so a
// single .env file works for either binary.
package gateway

import (
	"os"
	"strconv"
)

// Config is the runtime configuration for the gateway.
type Config struct {
	// AdminToken is the bearer token gating /admin/*. Empty → admin endpoints
	// fail closed (503).
	AdminToken string

	// RedisURL is used for both idempotency cache and the agent:runs stream.
	RedisURL string

	// PostgresDSN is used by /readyz to ping the DB.
	PostgresDSN string

	// OrchestratorURL is the F01 base URL for synchronous admin proxy calls.
	OrchestratorURL string

	// IdempotencyTTLSeconds is how long a webhook update_id stays in the
	// dedupe cache. 600s = 10min by default.
	IdempotencyTTLSeconds int

	// StreamName is the Redis Stream the agent:runs jobs are XADDed onto.
	// FROZEN at "agent:runs".
	StreamName string

	// ConsumerGroup is the orchestrator-side consumer group name (only relevant
	// here for documentation; the gateway only produces).
	ConsumerGroup string

	// UserJWTSecret is the HS256 signing secret for /auth tokens. Empty →
	// /auth/* returns 503.
	UserJWTSecret string

	// UserJWTTTLSeconds — token lifetime. 86400 (24h) by default.
	UserJWTTTLSeconds int

	// BypassLogin — if true, /auth/login accepts magic_code "BYPASS" and skips
	// the magic-code email round trip. Demo only — never set in production.
	BypassLogin bool

	// DBEncryptionKeyHex — 32-byte AES-GCM key (64 hex chars) used to encrypt
	// channel config blobs at the user-API boundary.
	DBEncryptionKeyHex string

	// HTTPAddr is the bind address for the HTTP server (cmd/gateway only).
	HTTPAddr string

	// IdempotencyNamespace — Redis key prefix for the dedupe cache. Defaults
	// to "gw:idem" matching Python.
	IdempotencyNamespace string
}

// LoadConfig builds a Config from the current environment.
func LoadConfig() Config {
	return Config{
		AdminToken:            getenv("ADMIN_TOKEN", ""),
		RedisURL:              getenv("REDIS_URL", "redis://localhost:6379/0"),
		PostgresDSN:           getenv("POSTGRES_DSN", "postgresql://postgres:postgres@localhost:5432/agent_platform"),
		OrchestratorURL:       getenv("ORCHESTRATOR_URL", "http://localhost:8081"),
		IdempotencyTTLSeconds: getenvInt("IDEMPOTENCY_TTL_SECONDS", 600),
		StreamName:            getenv("AGENT_RUNS_STREAM", "agent:runs"),
		ConsumerGroup:         getenv("AGENT_RUNS_GROUP", "orchestrator"),
		UserJWTSecret:         getenv("USER_JWT_SECRET", ""),
		UserJWTTTLSeconds:     getenvInt("USER_JWT_TTL_SECONDS", 86400),
		BypassLogin:           getenv("BYPASS_LOGIN", "0") == "1",
		DBEncryptionKeyHex:    getenv("DB_ENCRYPTION_KEY", ""),
		HTTPAddr:              getenv("GATEWAY_HTTP_ADDR", ":8080"),
		IdempotencyNamespace:  getenv("IDEMPOTENCY_NAMESPACE", "gw:idem"),
	}
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
