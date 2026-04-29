package repos

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openclaw/agent-platform/internal/persistence"
)

// AgentsRepo is the pgx-backed implementation of persistence.AgentsRepo.
type AgentsRepo struct {
	pool *pgxpool.Pool
}

// NewAgentsRepo wires an AgentsRepo onto a pool.
func NewAgentsRepo(pool *pgxpool.Pool) *AgentsRepo { return &AgentsRepo{pool: pool} }

const agentSelectCols = `id, user_id, name, config_yaml, created_at, updated_at`

func scanAgent(row pgx.Row) (*persistence.Agent, error) {
	a := &persistence.Agent{}
	if err := row.Scan(&a.ID, &a.UserID, &a.Name, &a.ConfigYAML, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	return a, nil
}

// Create inserts a new agent.
func (r *AgentsRepo) Create(ctx context.Context, userID uuid.UUID, name, configYAML string) (*persistence.Agent, error) {
	row := r.pool.QueryRow(ctx, `
        INSERT INTO agents (user_id, name, config_yaml)
        VALUES ($1, $2, $3)
        RETURNING `+agentSelectCols, userID, name, configYAML)
	a, err := scanAgent(row)
	if err != nil {
		return nil, fmt.Errorf("AgentsRepo.Create: %w", err)
	}
	return a, nil
}

// Get fetches one agent by id.
func (r *AgentsRepo) Get(ctx context.Context, agentID uuid.UUID) (*persistence.Agent, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+agentSelectCols+` FROM agents WHERE id = $1`, agentID)
	a, err := scanAgent(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("AgentsRepo.Get: %w", err)
	}
	return a, nil
}

// ListForUser returns all agents owned by the user, ordered by created_at.
func (r *AgentsRepo) ListForUser(ctx context.Context, userID uuid.UUID) ([]persistence.Agent, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+agentSelectCols+` FROM agents WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("AgentsRepo.ListForUser: %w", err)
	}
	defer rows.Close()

	var out []persistence.Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, fmt.Errorf("AgentsRepo.ListForUser scan: %w", err)
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// UpdateConfig replaces config_yaml for an agent and bumps updated_at.
func (r *AgentsRepo) UpdateConfig(ctx context.Context, agentID uuid.UUID, configYAML string) (*persistence.Agent, error) {
	row := r.pool.QueryRow(ctx, `
        UPDATE agents SET config_yaml = $2, updated_at = now()
        WHERE id = $1
        RETURNING `+agentSelectCols, agentID, configYAML)
	a, err := scanAgent(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("AgentsRepo.UpdateConfig: %w", err)
	}
	return a, nil
}
