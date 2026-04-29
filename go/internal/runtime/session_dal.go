package runtime

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/openclaw/agent-platform/internal/persistence"
)

// SessionDal is the small interface the agent loop uses to track CLI
// session resume tokens + first-turn bootstrap injection state.
//
// The Python tree wires this against persistence.SessionsRepoP; the Go
// runtime wraps the same shape (persistence.SessionsRepo) with a thin
// adapter so unit tests can inject in-memory implementations without
// pulling in pgx.
type SessionDal interface {
	// LookupSessionID returns the cached cli_session_id for this thread,
	// or "" + ok=false when none exists.
	LookupSessionID(
		ctx context.Context,
		userID, channelID uuid.UUID,
		threadID, provider string,
	) (string, bool, error)

	// IsInitialized reports whether the bootstrap-file injection has ever
	// happened on this thread. True once the first successful turn writes
	// back via RecordTurn.
	IsInitialized(
		ctx context.Context,
		userID, channelID uuid.UUID,
		threadID, provider string,
	) (bool, error)

	// RecordTurn persists the new cli_session_id and marks the session
	// initialized. The underlying repo's UpsertAfterTurn sets
	// initialized_at on first write (via SQL COALESCE) — there is no
	// separate "mark as initialized" call.
	RecordTurn(
		ctx context.Context,
		userID, channelID uuid.UUID,
		threadID, provider, cliSessionID string,
	) error
}

// DBSessionDal adapts a persistence.SessionsRepo into the SessionDal
// interface used by the agent loop.
type DBSessionDal struct {
	repo persistence.SessionsRepo
}

// NewDBSessionDal wraps a persistence.SessionsRepo.
func NewDBSessionDal(repo persistence.SessionsRepo) *DBSessionDal {
	return &DBSessionDal{repo: repo}
}

// LookupSessionID implements SessionDal.
func (d *DBSessionDal) LookupSessionID(
	ctx context.Context,
	userID, channelID uuid.UUID,
	threadID, provider string,
) (string, bool, error) {
	row, err := d.repo.Get(ctx, userID, channelID, threadID, provider)
	if err != nil {
		return "", false, err
	}
	if row == nil || row.CLISessionID == nil {
		return "", false, nil
	}
	return *row.CLISessionID, true, nil
}

// IsInitialized implements SessionDal.
func (d *DBSessionDal) IsInitialized(
	ctx context.Context,
	userID, channelID uuid.UUID,
	threadID, provider string,
) (bool, error) {
	row, err := d.repo.Get(ctx, userID, channelID, threadID, provider)
	if err != nil {
		return false, err
	}
	return row != nil && row.InitializedAt != nil, nil
}

// RecordTurn implements SessionDal.
func (d *DBSessionDal) RecordTurn(
	ctx context.Context,
	userID, channelID uuid.UUID,
	threadID, provider, cliSessionID string,
) error {
	return d.repo.UpsertAfterTurn(ctx, userID, channelID, threadID, provider, cliSessionID)
}

// ---------------------------------------------------------------------------
// In-memory SessionsRepo — used as the daemon's Phase-0 fallback when F12
// is not wired in, and as the base for unit tests that need a SessionDal
// without pulling in Postgres.
// ---------------------------------------------------------------------------

type sessionRow struct {
	cliSessionID  string
	initializedAt *time.Time
	lastUsedAt    time.Time
}

// InMemorySessionsRepo is a tiny dict-backed persistence.SessionsRepo.
type InMemorySessionsRepo struct {
	mu   sync.Mutex
	rows map[sessionKey]*sessionRow
}

type sessionKey struct {
	userID    uuid.UUID
	channelID uuid.UUID
	threadID  string
	provider  string
}

// NewInMemorySessionsRepo builds an empty in-memory sessions repo.
func NewInMemorySessionsRepo() *InMemorySessionsRepo {
	return &InMemorySessionsRepo{rows: make(map[sessionKey]*sessionRow)}
}

// Get implements persistence.SessionsRepo.
func (r *InMemorySessionsRepo) Get(
	_ context.Context,
	userID, channelID uuid.UUID,
	threadID, provider string,
) (*persistence.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[sessionKey{userID, channelID, threadID, provider}]
	if !ok {
		return nil, nil
	}
	cliSessionID := row.cliSessionID
	out := &persistence.Session{
		UserID:        userID,
		ChannelID:     channelID,
		ThreadID:      threadID,
		Provider:      provider,
		CLISessionID:  &cliSessionID,
		InitializedAt: row.initializedAt,
		LastUsedAt:    row.lastUsedAt,
	}
	return out, nil
}

// UpsertAfterTurn implements persistence.SessionsRepo.
func (r *InMemorySessionsRepo) UpsertAfterTurn(
	_ context.Context,
	userID, channelID uuid.UUID,
	threadID, provider, cliSessionID string,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := sessionKey{userID, channelID, threadID, provider}
	now := time.Now().UTC()
	existing, ok := r.rows[key]
	initializedAt := &now
	if ok && existing.initializedAt != nil {
		initializedAt = existing.initializedAt
	}
	r.rows[key] = &sessionRow{
		cliSessionID:  cliSessionID,
		initializedAt: initializedAt,
		lastUsedAt:    now,
	}
	return nil
}

// DropForProvider implements persistence.SessionsRepo.
func (r *InMemorySessionsRepo) DropForProvider(
	_ context.Context,
	userID uuid.UUID,
	provider string,
) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var dropped int64
	for k := range r.rows {
		if k.userID == userID && k.provider == provider {
			delete(r.rows, k)
			dropped++
		}
	}
	return dropped, nil
}

// NewInMemorySessionDal is a one-line helper that constructs the in-memory
// repo + the DAL adapter — the Phase-0 fallback used by the daemon when no
// F12-backed SessionsRepo is wired.
func NewInMemorySessionDal() *DBSessionDal {
	return NewDBSessionDal(NewInMemorySessionsRepo())
}
