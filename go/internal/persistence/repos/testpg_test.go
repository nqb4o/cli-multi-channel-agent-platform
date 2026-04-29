package repos

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/openclaw/agent-platform/internal/persistence"
)

// pgContext is a once-per-test-binary shared Postgres testcontainer.
type pgContext struct {
	dsn       string
	container testcontainers.Container
}

var (
	pgOnce sync.Once
	pgRef  *pgContext
	pgErr  error
)

func startPostgres(t *testing.T) *pgContext {
	t.Helper()
	pgOnce.Do(func() {
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

// freshPool — shared per-test fixture: migrations applied (idempotent),
// every business table truncated. Pool auto-closed at end of test.
func freshPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pgc := startPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, pgc.dsn)
	if err != nil {
		t.Fatalf("pgx.Connect: %v", err)
	}
	if _, err := persistence.Up(ctx, conn, nil); err != nil {
		conn.Close(ctx)
		t.Fatalf("migrate.Up: %v", err)
	}
	conn.Close(ctx)

	pool, err := persistence.NewPool(ctx, pgc.dsn, persistence.PoolOptions{
		MinConns: 1, MaxConns: 4, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	if _, err := pool.Exec(ctx, `
        TRUNCATE TABLE
            skill_signatures, skill_releases, skill_packages, skill_publishers,
            skills_installed, runs, sessions, sandboxes, channels, agents, users
        RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// fakeKey — same convention as the Python conftest: 32 zero bytes.
func fakeKey() []byte { return make([]byte, persistence.KeyLen) }
