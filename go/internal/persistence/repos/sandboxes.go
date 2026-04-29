package repos

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openclaw/agent-platform/internal/persistence"
)

// SandboxesRepo is the pgx-backed implementation of persistence.SandboxesRepo.
type SandboxesRepo struct {
	pool *pgxpool.Pool
}

// NewSandboxesRepo wires a SandboxesRepo onto a pool.
func NewSandboxesRepo(pool *pgxpool.Pool) *SandboxesRepo { return &SandboxesRepo{pool: pool} }

const sandboxSelectCols = `id, user_id, daytona_id, state, created_at, last_active_at`

func scanSandbox(row pgx.Row) (*persistence.Sandbox, error) {
	s := &persistence.Sandbox{}
	var lastActive *time.Time
	if err := row.Scan(&s.ID, &s.UserID, &s.DaytonaID, &s.State, &s.CreatedAt, &lastActive); err != nil {
		return nil, err
	}
	s.LastActiveAt = lastActive
	return s, nil
}

// Upsert inserts or, on user_id conflict, refreshes the daytona_id+state.
func (r *SandboxesRepo) Upsert(ctx context.Context, userID uuid.UUID, daytonaID, state string) (*persistence.Sandbox, error) {
	row := r.pool.QueryRow(ctx, `
        INSERT INTO sandboxes (user_id, daytona_id, state)
        VALUES ($1, $2, $3)
        ON CONFLICT (user_id) DO UPDATE
            SET daytona_id = EXCLUDED.daytona_id,
                state = EXCLUDED.state
        RETURNING `+sandboxSelectCols, userID, daytonaID, state)
	s, err := scanSandbox(row)
	if err != nil {
		return nil, fmt.Errorf("SandboxesRepo.Upsert: %w", err)
	}
	return s, nil
}

// GetForUser returns the user's sandbox row, or nil if there is none.
func (r *SandboxesRepo) GetForUser(ctx context.Context, userID uuid.UUID) (*persistence.Sandbox, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+sandboxSelectCols+` FROM sandboxes WHERE user_id = $1`, userID)
	s, err := scanSandbox(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("SandboxesRepo.GetForUser: %w", err)
	}
	return s, nil
}

// UpdateState sets a new state and (optionally) bumps last_active_at.
func (r *SandboxesRepo) UpdateState(ctx context.Context, userID uuid.UUID, state string, lastActiveAt *time.Time) (*persistence.Sandbox, error) {
	row := r.pool.QueryRow(ctx, `
        UPDATE sandboxes
        SET state = $2,
            last_active_at = COALESCE($3, last_active_at)
        WHERE user_id = $1
        RETURNING `+sandboxSelectCols, userID, state, lastActiveAt)
	s, err := scanSandbox(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("SandboxesRepo.UpdateState: %w", err)
	}
	return s, nil
}
