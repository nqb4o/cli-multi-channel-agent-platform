// Package repos contains the pgx-backed implementations of the repo
// interfaces declared in package persistence.
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

// UsersRepo implements persistence.UsersRepo via pgx.
type UsersRepo struct {
	pool *pgxpool.Pool
}

// NewUsersRepo wires a UsersRepo onto an existing pgx pool.
func NewUsersRepo(pool *pgxpool.Pool) *UsersRepo { return &UsersRepo{pool: pool} }

// Create inserts a new user and returns the populated row.
func (r *UsersRepo) Create(ctx context.Context, email string) (*persistence.User, error) {
	row := r.pool.QueryRow(ctx,
		"INSERT INTO users (email) VALUES ($1) RETURNING id, email, created_at",
		email,
	)
	u := &persistence.User{}
	if err := row.Scan(&u.ID, &u.Email, &u.CreatedAt); err != nil {
		return nil, fmt.Errorf("UsersRepo.Create: %w", err)
	}
	return u, nil
}

// Get fetches a user by id, returning (nil, nil) if missing.
func (r *UsersRepo) Get(ctx context.Context, userID uuid.UUID) (*persistence.User, error) {
	row := r.pool.QueryRow(ctx,
		"SELECT id, email, created_at FROM users WHERE id = $1",
		userID,
	)
	u := &persistence.User{}
	if err := row.Scan(&u.ID, &u.Email, &u.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("UsersRepo.Get: %w", err)
	}
	return u, nil
}

// GetByEmail looks up a user by email, returning (nil, nil) if missing.
func (r *UsersRepo) GetByEmail(ctx context.Context, email string) (*persistence.User, error) {
	row := r.pool.QueryRow(ctx,
		"SELECT id, email, created_at FROM users WHERE email = $1",
		email,
	)
	u := &persistence.User{}
	if err := row.Scan(&u.ID, &u.Email, &u.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("UsersRepo.GetByEmail: %w", err)
	}
	return u, nil
}
