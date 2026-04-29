package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisDoer is the narrow Redis surface IdempotencyCache + AgentRunQueue need.
// Both go-redis Client and miniredis-backed clients satisfy this — see tests.
type RedisDoer interface {
	SetNX(ctx context.Context, key string, value any, expiration time.Duration) *redis.BoolCmd
	XAdd(ctx context.Context, args *redis.XAddArgs) *redis.StringCmd
	XLen(ctx context.Context, stream string) *redis.IntCmd
	Ping(ctx context.Context) *redis.StatusCmd
}

// IdempotencyCache is a Redis-backed dedupe cache for inbound webhook updates.
// Same (channel_type, message_id) within TTLSeconds short-circuits to "duplicate".
//
// The key namespace ("gw:idem" by default) is shared with the Python
// implementation so a Go gateway and a Python gateway can coexist on one Redis.
type IdempotencyCache struct {
	client    RedisDoer
	ttl       time.Duration
	namespace string
}

// NewIdempotencyCache builds an IdempotencyCache. Returns an error if
// ttlSeconds is non-positive.
func NewIdempotencyCache(client RedisDoer, ttlSeconds int, namespace string) (*IdempotencyCache, error) {
	if ttlSeconds <= 0 {
		return nil, errors.New("ttlSeconds must be positive")
	}
	if namespace == "" {
		namespace = "gw:idem"
	}
	return &IdempotencyCache{
		client:    client,
		ttl:       time.Duration(ttlSeconds) * time.Second,
		namespace: namespace,
	}, nil
}

// Namespace returns the configured key prefix.
func (c *IdempotencyCache) Namespace() string { return c.namespace }

// Key returns the full Redis key for (channelType, messageID).
func (c *IdempotencyCache) Key(channelType, messageID string) string {
	return fmt.Sprintf("%s:%s:%s", c.namespace, channelType, messageID)
}

// Claim atomically reserves (channelType, messageID) for the configured TTL.
//
// Returns true the first time the key is seen within the TTL window; false for
// duplicates. Returns an error if either input is empty or Redis fails.
func (c *IdempotencyCache) Claim(ctx context.Context, channelType, messageID string) (bool, error) {
	if channelType == "" || messageID == "" {
		return false, errors.New("channel_type and message_id are required")
	}
	key := c.Key(channelType, messageID)
	res, err := c.client.SetNX(ctx, key, "1", c.ttl).Result()
	if err != nil {
		return false, fmt.Errorf("idempotency.Claim: %w", err)
	}
	return res, nil
}
