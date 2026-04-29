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

// RunsRepo is the pgx-backed implementation of persistence.RunsRepo.
type RunsRepo struct {
	pool *pgxpool.Pool
}

// NewRunsRepo wires a RunsRepo onto a pool.
func NewRunsRepo(pool *pgxpool.Pool) *RunsRepo { return &RunsRepo{pool: pool} }

const runSelectCols = `id, user_id, agent_id, channel_id, thread_id, provider,
       status, started_at, ended_at, latency_ms, error_class, error_msg`

func scanRun(row pgx.Row) (*persistence.Run, error) {
	r := &persistence.Run{}
	var (
		provider   *string
		ended      *time.Time
		latency    *int
		errorClass *string
		errorMsg   *string
	)
	if err := row.Scan(
		&r.ID, &r.UserID, &r.AgentID, &r.ChannelID, &r.ThreadID, &provider,
		&r.Status, &r.StartedAt, &ended, &latency, &errorClass, &errorMsg,
	); err != nil {
		return nil, err
	}
	r.Provider = provider
	r.EndedAt = ended
	r.LatencyMS = latency
	r.ErrorClass = errorClass
	r.ErrorMsg = errorMsg
	return r, nil
}

// Start inserts a new run row in 'accepted' status.
func (r *RunsRepo) Start(
	ctx context.Context,
	userID, agentID, channelID uuid.UUID,
	threadID string,
	provider *string,
) (*persistence.Run, error) {
	row := r.pool.QueryRow(ctx, `
        INSERT INTO runs (user_id, agent_id, channel_id, thread_id, provider, status)
        VALUES ($1, $2, $3, $4, $5, 'accepted')
        RETURNING `+runSelectCols, userID, agentID, channelID, threadID, provider)
	out, err := scanRun(row)
	if err != nil {
		return nil, fmt.Errorf("RunsRepo.Start: %w", err)
	}
	return out, nil
}

// FinishOK closes a run as 'ok' with a recorded latency.
func (r *RunsRepo) FinishOK(ctx context.Context, runID uuid.UUID, latencyMS int, endedAt *time.Time) (*persistence.Run, error) {
	row := r.pool.QueryRow(ctx, `
        UPDATE runs
        SET status = 'ok',
            ended_at = COALESCE($2, now()),
            latency_ms = $3
        WHERE id = $1
        RETURNING `+runSelectCols, runID, endedAt, latencyMS)
	out, err := scanRun(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("RunsRepo.FinishOK: %w", err)
	}
	return out, nil
}

// FinishError closes a run as 'error' with the supplied error class + message.
func (r *RunsRepo) FinishError(
	ctx context.Context,
	runID uuid.UUID,
	latencyMS int,
	errorClass, errorMsg string,
	endedAt *time.Time,
) (*persistence.Run, error) {
	row := r.pool.QueryRow(ctx, `
        UPDATE runs
        SET status = 'error',
            ended_at = COALESCE($2, now()),
            latency_ms = $3,
            error_class = $4,
            error_msg = $5
        WHERE id = $1
        RETURNING `+runSelectCols, runID, endedAt, latencyMS, errorClass, errorMsg)
	out, err := scanRun(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("RunsRepo.FinishError: %w", err)
	}
	return out, nil
}

// Get returns a run by id.
func (r *RunsRepo) Get(ctx context.Context, runID uuid.UUID) (*persistence.Run, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+runSelectCols+` FROM runs WHERE id = $1`, runID)
	out, err := scanRun(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("RunsRepo.Get: %w", err)
	}
	return out, nil
}

// ListRecentForUser returns up to `limit` runs for the user, newest first.
func (r *RunsRepo) ListRecentForUser(ctx context.Context, userID uuid.UUID, limit int) ([]persistence.Run, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `
        SELECT `+runSelectCols+`
        FROM runs WHERE user_id = $1
        ORDER BY started_at DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("RunsRepo.ListRecentForUser: %w", err)
	}
	defer rows.Close()
	var out []persistence.Run
	for rows.Next() {
		one, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("RunsRepo.ListRecentForUser scan: %w", err)
		}
		out = append(out, *one)
	}
	return out, rows.Err()
}
