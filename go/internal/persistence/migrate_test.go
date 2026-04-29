package persistence

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestLoadEmbeddedMigrations is a pure-Go test (no DB) that verifies the
// embedded migration set is sane.
func TestLoadEmbeddedMigrations(t *testing.T) {
	files, err := LoadEmbeddedMigrations()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(files) < 3 {
		t.Fatalf("expected at least 3 migrations, got %d", len(files))
	}

	wantNames := []string{"0001_init.sql", "0002_indexes.sql", "0003_skill_registry.sql"}
	gotNames := make([]string, 0, len(files))
	for _, f := range files {
		gotNames = append(gotNames, f.Name)
	}

	for _, want := range wantNames {
		found := false
		for _, n := range gotNames {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing migration %q (got %v)", want, gotNames)
		}
	}

	// Lexicographic order is the contract.
	sorted := append([]string(nil), gotNames...)
	sort.Strings(sorted)
	for i, n := range gotNames {
		if n != sorted[i] {
			t.Errorf("migration order broken: pos %d = %q, want %q", i, n, sorted[i])
		}
	}
}

// TestUpAppliesEveryMigration uses a real Postgres testcontainer.
func TestUpAppliesEveryMigration(t *testing.T) {
	pgc := startPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, pgc.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	// Up may have run before; status must report all applied either way.
	if _, err := Up(ctx, conn, nil); err != nil {
		t.Fatalf("up: %v", err)
	}
	rows, err := Status(ctx, conn, nil)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(rows) < 3 {
		t.Fatalf("expected >=3 migrations in status, got %d", len(rows))
	}
	for _, r := range rows {
		if !r.Applied {
			t.Errorf("migration %s not applied after up()", r.Version)
		}
	}
}

// TestUpIsIdempotent — applying a second time is a no-op.
func TestUpIsIdempotent(t *testing.T) {
	pgc := startPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, pgc.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	// Run once to ensure ledger is fully populated.
	if _, err := Up(ctx, conn, nil); err != nil {
		t.Fatalf("up #1: %v", err)
	}
	// Run again — must apply zero migrations.
	applied, err := Up(ctx, conn, nil)
	if err != nil {
		t.Fatalf("up #2: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("expected no migrations applied on second up(), got %v", applied)
	}
}

// TestStatusReportsLexicographicOrder — `status` returns versions in file order.
func TestStatusReportsLexicographicOrder(t *testing.T) {
	pgc := startPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, pgc.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := Up(ctx, conn, nil); err != nil {
		t.Fatalf("up: %v", err)
	}

	rows, err := Status(ctx, conn, nil)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	versions := make([]string, len(rows))
	for i, r := range rows {
		versions[i] = r.Version
	}
	sorted := append([]string(nil), versions...)
	sort.Strings(sorted)
	for i, v := range versions {
		if v != sorted[i] {
			t.Errorf("status not lex-sorted: pos %d = %q, want %q", i, v, sorted[i])
		}
	}
}

// TestSchemaMigrationsLedgerExists — the ledger table is created up-front.
func TestSchemaMigrationsLedgerExists(t *testing.T) {
	pgc := startPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, pgc.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := Up(ctx, conn, nil); err != nil {
		t.Fatalf("up: %v", err)
	}
	var rel *string
	if err := conn.QueryRow(ctx, "SELECT to_regclass('public.schema_migrations')::text").Scan(&rel); err != nil {
		t.Fatalf("regclass: %v", err)
	}
	if rel == nil || *rel == "" {
		t.Fatal("schema_migrations not created")
	}
}

// TestInitMigrationCreatesCoreTables — F12 init creates the core tables.
func TestInitMigrationCreatesCoreTables(t *testing.T) {
	pgc := startPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, pgc.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := Up(ctx, conn, nil); err != nil {
		t.Fatalf("up: %v", err)
	}
	want := map[string]bool{
		"users": true, "agents": true, "channels": true,
		"sandboxes": true, "sessions": true, "runs": true, "skills_installed": true,
	}
	rows, err := conn.Query(ctx, `
        SELECT table_name FROM information_schema.tables
        WHERE table_schema = 'public'`)
	if err != nil {
		t.Fatalf("query tables: %v", err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[n] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing core table %q after migrate", k)
		}
	}
}
