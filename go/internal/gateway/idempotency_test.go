package gateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newMiniredisClient(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

func TestIdempotency_FirstClaimReturnsTrue(t *testing.T) {
	c, _ := newMiniredisClient(t)
	cache, err := NewIdempotencyCache(c, 60, "")
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	ok, err := cache.Claim(context.Background(), "telegram", "msg-1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !ok {
		t.Fatal("first claim should return true")
	}
}

func TestIdempotency_SecondClaimReturnsFalse(t *testing.T) {
	c, _ := newMiniredisClient(t)
	cache, _ := NewIdempotencyCache(c, 60, "")
	ctx := context.Background()
	if ok, _ := cache.Claim(ctx, "telegram", "msg-1"); !ok {
		t.Fatal("expected first true")
	}
	if ok, _ := cache.Claim(ctx, "telegram", "msg-1"); ok {
		t.Fatal("expected second false")
	}
}

func TestIdempotency_DistinctMessageIDsIndependent(t *testing.T) {
	c, _ := newMiniredisClient(t)
	cache, _ := NewIdempotencyCache(c, 60, "")
	ctx := context.Background()
	if ok, _ := cache.Claim(ctx, "telegram", "msg-1"); !ok {
		t.Fatal("msg-1 first")
	}
	if ok, _ := cache.Claim(ctx, "telegram", "msg-2"); !ok {
		t.Fatal("msg-2 should be independent")
	}
}

func TestIdempotency_DistinctChannelTypesIndependent(t *testing.T) {
	c, _ := newMiniredisClient(t)
	cache, _ := NewIdempotencyCache(c, 60, "")
	ctx := context.Background()
	if ok, _ := cache.Claim(ctx, "telegram", "msg-1"); !ok {
		t.Fatal("telegram first")
	}
	if ok, _ := cache.Claim(ctx, "zalo", "msg-1"); !ok {
		t.Fatal("zalo independent of telegram")
	}
}

func TestIdempotency_ConcurrentClaimsOnlyOneWins(t *testing.T) {
	c, _ := newMiniredisClient(t)
	cache, _ := NewIdempotencyCache(c, 60, "")
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make([]bool, 2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			defer wg.Done()
			ok, _ := cache.Claim(ctx, "telegram", "race-1")
			results[i] = ok
		}()
	}
	wg.Wait()
	wins := 0
	for _, r := range results {
		if r {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one race winner expected, got %d", wins)
	}
}

func TestIdempotency_TTLIsSetOnClaim(t *testing.T) {
	c, mr := newMiniredisClient(t)
	cache, _ := NewIdempotencyCache(c, 600, "gw:idem")
	if _, err := cache.Claim(context.Background(), "telegram", "msg-1"); err != nil {
		t.Fatal(err)
	}
	ttl := mr.TTL("gw:idem:telegram:msg-1")
	if ttl <= 0 || ttl > 600*time.Second {
		t.Fatalf("ttl=%v out of range", ttl)
	}
}

func TestIdempotency_NamespacePrefixUsed(t *testing.T) {
	c, mr := newMiniredisClient(t)
	cache, _ := NewIdempotencyCache(c, 60, "custom-ns")
	if _, err := cache.Claim(context.Background(), "telegram", "msg-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := mr.Get("custom-ns:telegram:msg-1"); err != nil {
		t.Fatalf("expected key under custom-ns prefix: %v", err)
	}
}

func TestIdempotency_ZeroOrNegativeTTLRejected(t *testing.T) {
	c, _ := newMiniredisClient(t)
	if _, err := NewIdempotencyCache(c, 0, ""); err == nil {
		t.Fatal("ttl=0 should error")
	}
	if _, err := NewIdempotencyCache(c, -1, ""); err == nil {
		t.Fatal("ttl=-1 should error")
	}
}

func TestIdempotency_EmptyKeysRejected(t *testing.T) {
	c, _ := newMiniredisClient(t)
	cache, _ := NewIdempotencyCache(c, 60, "")
	if _, err := cache.Claim(context.Background(), "", "msg-1"); err == nil {
		t.Fatal("empty channelType should error")
	}
	if _, err := cache.Claim(context.Background(), "telegram", ""); err == nil {
		t.Fatal("empty messageID should error")
	}
}

func TestIdempotency_DefaultNamespaceWhenEmpty(t *testing.T) {
	c, mr := newMiniredisClient(t)
	cache, _ := NewIdempotencyCache(c, 60, "")
	if cache.Namespace() != "gw:idem" {
		t.Fatalf("default namespace, got %q", cache.Namespace())
	}
	if _, err := cache.Claim(context.Background(), "telegram", "msg-x"); err != nil {
		t.Fatal(err)
	}
	if _, err := mr.Get("gw:idem:telegram:msg-x"); err != nil {
		t.Fatalf("default key prefix: %v", err)
	}
}
