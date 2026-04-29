package persistence

// pgxpool factory + ping/transaction helpers.
//
// Mirrors the asyncpg create_pool/transaction/ping helpers in
// packages/persistence/src/persistence/pool.py.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolOptions tweak pgxpool config without forcing callers through pgx APIs.
type PoolOptions struct {
	MinConns int32
	MaxConns int32
	Timeout  time.Duration // applied as ConnectTimeout on the pool config
}

// DefaultPoolOptions matches the Python defaults (min 1, max 10, 30s timeout).
func DefaultPoolOptions() PoolOptions {
	return PoolOptions{MinConns: 1, MaxConns: 10, Timeout: 30 * time.Second}
}

// NewPool builds a pgxpool.Pool configured similarly to the asyncpg pool.
func NewPool(ctx context.Context, dsn string, opts PoolOptions) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, fmt.Errorf("persistence.NewPool: empty DSN")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("persistence.NewPool: parse DSN: %w", err)
	}
	if opts.MinConns > 0 {
		cfg.MinConns = opts.MinConns
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.Timeout > 0 {
		cfg.ConnConfig.ConnectTimeout = opts.Timeout
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("persistence.NewPool: %w", err)
	}
	return pool, nil
}

// Ping does a SELECT 1 round-trip with a short deadline. Suitable for /readyz.
func Ping(ctx context.Context, pool *pgxpool.Pool) bool {
	if pool == nil {
		return false
	}
	subCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var n int
	if err := pool.QueryRow(subCtx, "SELECT 1").Scan(&n); err != nil {
		return false
	}
	return n == 1
}

// WithTransaction acquires a connection, opens a transaction, and runs fn.
// The transaction is committed if fn returns nil; otherwise rolled back.
func WithTransaction(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("persistence.WithTransaction begin: %w", err)
	}
	defer func() {
		// Best-effort rollback if Commit was not called.
		_ = tx.Rollback(ctx)
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
