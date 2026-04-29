package repos

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openclaw/agent-platform/internal/persistence"
)

// SkillsInstalledRepo is the pgx-backed implementation of
// persistence.SkillsInstalledRepo.
type SkillsInstalledRepo struct {
	pool *pgxpool.Pool
}

// NewSkillsInstalledRepo wires a SkillsInstalledRepo onto a pool.
func NewSkillsInstalledRepo(pool *pgxpool.Pool) *SkillsInstalledRepo {
	return &SkillsInstalledRepo{pool: pool}
}

const skillSelectCols = `user_id, slug, version, source, installed_at`

func scanSkill(row pgx.Row) (*persistence.SkillInstalled, error) {
	s := &persistence.SkillInstalled{}
	if err := row.Scan(&s.UserID, &s.Slug, &s.Version, &s.Source, &s.InstalledAt); err != nil {
		return nil, err
	}
	return s, nil
}

// Install upserts a skill install row and bumps installed_at on conflict.
func (r *SkillsInstalledRepo) Install(ctx context.Context, userID uuid.UUID, slug, version, source string) (*persistence.SkillInstalled, error) {
	row := r.pool.QueryRow(ctx, `
        INSERT INTO skills_installed (user_id, slug, version, source)
        VALUES ($1, $2, $3, $4)
        ON CONFLICT (user_id, slug) DO UPDATE
            SET version = EXCLUDED.version,
                source = EXCLUDED.source,
                installed_at = now()
        RETURNING `+skillSelectCols, userID, slug, version, source)
	s, err := scanSkill(row)
	if err != nil {
		return nil, fmt.Errorf("SkillsInstalledRepo.Install: %w", err)
	}
	return s, nil
}

// Uninstall removes the row for (user, slug). Returns true iff a row was deleted.
func (r *SkillsInstalledRepo) Uninstall(ctx context.Context, userID uuid.UUID, slug string) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		"DELETE FROM skills_installed WHERE user_id = $1 AND slug = $2", userID, slug)
	if err != nil {
		return false, fmt.Errorf("SkillsInstalledRepo.Uninstall: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListForUser returns every installed skill for the user, ordered by slug.
func (r *SkillsInstalledRepo) ListForUser(ctx context.Context, userID uuid.UUID) ([]persistence.SkillInstalled, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+skillSelectCols+` FROM skills_installed WHERE user_id = $1 ORDER BY slug`, userID)
	if err != nil {
		return nil, fmt.Errorf("SkillsInstalledRepo.ListForUser: %w", err)
	}
	defer rows.Close()
	var out []persistence.SkillInstalled
	for rows.Next() {
		s, err := scanSkill(rows)
		if err != nil {
			return nil, fmt.Errorf("SkillsInstalledRepo.ListForUser scan: %w", err)
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}
