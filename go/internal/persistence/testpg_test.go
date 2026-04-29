package persistence

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// pgContext is shared across tests in this package: one container, one DSN,
// migrations applied once. Functions get fresh pools (TRUNCATE between tests).
type pgContext struct {
	dsn       string
	container testcontainers.Container
}

var (
	pgOnce sync.Once
	pgRef  *pgContext
	pgErr  error
)

// startPostgres spins up a single shared Postgres container for tests in this
// package. Skips the suite if Docker is unavailable.
func startPostgres(t *testing.T) *pgContext {
	t.Helper()
	pgOnce.Do(func() {
		// If the user has provided a DSN explicitly, reuse it (faster local loop).
		if dsn := os.Getenv("PERSISTENCE_TEST_DSN"); dsn != "" {
			pgRef = &pgContext{dsn: dsn}
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		c, err := tcpostgres.Run(ctx, "postgres:16-alpine",
			tcpostgres.WithDatabase("agent_platform_test"),
			tcpostgres.WithUsername("postgres"),
			tcpostgres.WithPassword("postgres"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(60*time.Second),
			),
		)
		if err != nil {
			pgErr = err
			return
		}
		dsn, err := c.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			pgErr = err
			return
		}
		pgRef = &pgContext{dsn: dsn, container: c}
	})
	if pgErr != nil {
		t.Skipf("docker / postgres testcontainer unavailable: %v", pgErr)
	}
	return pgRef
}

// freshPool creates a small pgx pool, applies migrations once (cheap when
// already applied), and TRUNCATEs every test table so each test starts on a
// known canvas.
func freshPool(t *testing.T) (*pgxpool.Pool, *pgContext) {
	t.Helper()
	pgc := startPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Apply migrations using a one-shot pgx.Conn.
	if err := applyMigrations(ctx, pgc.dsn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	pool, err := NewPool(ctx, pgc.dsn, PoolOptions{MinConns: 1, MaxConns: 4, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	if _, err := pool.Exec(ctx, `
        TRUNCATE TABLE
            skill_signatures, skill_releases, skill_packages, skill_publishers,
            skills_installed, runs, sessions, sandboxes, channels, agents, users
        RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool, pgc
}
