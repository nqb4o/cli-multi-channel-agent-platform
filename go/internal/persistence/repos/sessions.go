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

// SessionsRepo is the pgx-backed implementation of persistence.SessionsRepo.
//
// Sessions are identified by (user, channel, thread, provider) and only ever
// dropped on auth rotation via DropForProvider.
type SessionsRepo struct {
	pool *pgxpool.Pool
}

// NewSessionsRepo wires a SessionsRepo onto a pool.
func NewSessionsRepo(pool *pgxpool.Pool) *SessionsRepo { return &SessionsRepo{pool: pool} }

// Get fetches the session row for the composite key.
func (r *SessionsRepo) Get(ctx context.Context, userID, channelID uuid.UUID, threadID, provider string) (*persistence.Session, error) {
	row := r.pool.QueryRow(ctx, `
        SELECT user_id, channel_id, thread_id, provider,
               cli_session_id, initialized_at, last_used_at
        FROM sessions
        WHERE user_id = $1 AND channel_id = $2
          AND thread_id = $3 AND provider = $4`,
		userID, channelID, threadID, provider)
	s := &persistence.Session{}
	var (
		cliSID  *string
		initAt  *time.Time
	)
	if err := row.Scan(&s.UserID, &s.ChannelID, &s.ThreadID, &s.Provider, &cliSID, &initAt, &s.LastUsedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("SessionsRepo.Get: %w", err)
	}
	s.CLISessionID = cliSID
	s.InitializedAt = initAt
	return s, nil
}

// UpsertAfterTurn records a CLI session id after a successful turn.
//
// The first turn populates initialized_at; subsequent turns preserve it and
// only refresh last_used_at + cli_session_id.
func (r *SessionsRepo) UpsertAfterTurn(ctx context.Context, userID, channelID uuid.UUID, threadID, provider, cliSessionID string) error {
	_, err := r.pool.Exec(ctx, `
        INSERT INTO sessions (
            user_id, channel_id, thread_id, provider,
            cli_session_id, initialized_at, last_used_at
        )
        VALUES ($1, $2, $3, $4, $5, now(), now())
        ON CONFLICT (user_id, channel_id, thread_id, provider) DO UPDATE
            SET cli_session_id = EXCLUDED.cli_session_id,
                initialized_at = COALESCE(sessions.initialized_at, now()),
                last_used_at = now()`,
		userID, channelID, threadID, provider, cliSessionID,
	)
	if err != nil {
		return fmt.Errorf("SessionsRepo.UpsertAfterTurn: %w", err)
	}
	return nil
}

// DropForProvider deletes every session for the given (user, provider) pair.
// Returns the number of rows removed.
func (r *SessionsRepo) DropForProvider(ctx context.Context, userID uuid.UUID, provider string) (int64, error) {
	tag, err := r.pool.Exec(ctx,
		"DELETE FROM sessions WHERE user_id = $1 AND provider = $2", userID, provider)
	if err != nil {
		return 0, fmt.Errorf("SessionsRepo.DropForProvider: %w", err)
	}
	return tag.RowsAffected(), nil
}
