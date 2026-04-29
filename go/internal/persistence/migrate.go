package persistence

// Migration runner — go run ./cmd/migrate up.
//
// Mirrors the Python persistence.migrate runner:
//   - Forward-only, append-only, idempotent.
//   - Tracks applied versions in schema_migrations (version PK + applied_at).
//   - Each NNNN_*.sql file is a single SQL "script" applied in a transaction.

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

const ledgerDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

var migrationRE = regexp.MustCompile(`^\d{4}_[a-zA-Z0-9_]+\.sql$`)

// MigrationFile is one SQL script with its derived version.
type MigrationFile struct {
	Version string // e.g. "0001_init"
	Name    string // e.g. "0001_init.sql"
	SQL     string
}

// MigrationStatus pairs a version with its applied/pending state.
type MigrationStatus struct {
	Version string
	Applied bool
}

// queryExec is a tiny adapter so loadFromFS / migration helpers can run on
// either *pgx.Conn or pgx.Tx.
type queryExec interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// LoadEmbeddedMigrations returns the migrations bundled with the binary.
// Files are sorted lexicographically by name.
func LoadEmbeddedMigrations() ([]MigrationFile, error) {
	return loadFromFS(embeddedMigrations, "migrations")
}

// LoadMigrationsFromDir reads *.sql files from a directory on disk.
func LoadMigrationsFromDir(dir string) ([]MigrationFile, error) {
	return loadFromFS(os.DirFS(dir), ".")
}

func loadFromFS(f fs.FS, dir string) ([]MigrationFile, error) {
	entries, err := fs.ReadDir(f, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir %q: %w", dir, err)
	}
	var files []MigrationFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !migrationRE.MatchString(name) {
			continue
		}
		path := name
		if dir != "." && dir != "" {
			path = dir + "/" + name
		}
		body, err := fs.ReadFile(f, path)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", name, err)
		}
		files = append(files, MigrationFile{
			Version: strings.TrimSuffix(name, ".sql"),
			Name:    name,
			SQL:     string(body),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}

// Up applies every pending migration from the given list (or the embedded
// set when nil/empty), returning the versions that ran in order.
func Up(ctx context.Context, conn *pgx.Conn, files []MigrationFile) ([]string, error) {
	if len(files) == 0 {
		var err error
		files, err = LoadEmbeddedMigrations()
		if err != nil {
			return nil, err
		}
	}
	if err := ensureLedger(ctx, conn); err != nil {
		return nil, err
	}
	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return nil, err
	}
	var ran []string
	for _, m := range files {
		if applied[m.Version] {
			continue
		}
		if err := applyOne(ctx, conn, m); err != nil {
			return ran, fmt.Errorf("apply %s: %w", m.Version, err)
		}
		ran = append(ran, m.Version)
	}
	return ran, nil
}

// Status returns each file in lexicographic order paired with its applied flag.
func Status(ctx context.Context, conn *pgx.Conn, files []MigrationFile) ([]MigrationStatus, error) {
	if len(files) == 0 {
		var err error
		files, err = LoadEmbeddedMigrations()
		if err != nil {
			return nil, err
		}
	}
	if err := ensureLedger(ctx, conn); err != nil {
		return nil, err
	}
	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return nil, err
	}
	out := make([]MigrationStatus, 0, len(files))
	for _, m := range files {
		out = append(out, MigrationStatus{Version: m.Version, Applied: applied[m.Version]})
	}
	return out, nil
}

func ensureLedger(ctx context.Context, q queryExec) error {
	_, err := q.Exec(ctx, ledgerDDL)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	return nil
}

func appliedVersions(ctx context.Context, q queryExec) (map[string]bool, error) {
	rows, err := q.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("select schema_migrations: %w", err)
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan version: %w", err)
		}
		out[v] = true
	}
	return out, rows.Err()
}

func applyOne(ctx context.Context, conn *pgx.Conn, m MigrationFile) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return fmt.Errorf("exec %s: %w", m.Name, err)
	}
	if _, err := tx.Exec(
		ctx,
		"INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT (version) DO NOTHING",
		m.Version,
	); err != nil {
		return fmt.Errorf("ledger insert %s: %w", m.Version, err)
	}
	return tx.Commit(ctx)
}
