package persistence

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// applyMigrations is a tiny test helper: open a single connection, run Up,
// close. Idempotent.
func applyMigrations(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)
	_, err = Up(ctx, conn, nil)
	return err
}
