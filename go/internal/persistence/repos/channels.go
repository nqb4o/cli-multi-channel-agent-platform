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

// ChannelsRepo is the pgx-backed implementation of persistence.ChannelsRepo.
//
// If a Config is provided at construction, GetDecryptedConfig can transparently
// AES-GCM decrypt the row's config_encrypted bytes. Without a Config it errors.
type ChannelsRepo struct {
	pool   *pgxpool.Pool
	cfg    *persistence.Config
	keyRaw []byte
}

// NewChannelsRepo wires a ChannelsRepo onto a pool. config may be nil; if
// supplied, the repo caches the decoded encryption key for fast decrypt.
func NewChannelsRepo(pool *pgxpool.Pool, config *persistence.Config) (*ChannelsRepo, error) {
	r := &ChannelsRepo{pool: pool, cfg: config}
	if config != nil {
		key, err := config.EncryptionKey()
		if err != nil {
			return nil, fmt.Errorf("NewChannelsRepo: %w", err)
		}
		r.keyRaw = key
	}
	return r, nil
}

const channelSelectCols = `id, user_id, type, ext_id, config_encrypted, agent_id, created_at`

func scanChannel(row pgx.Row) (*persistence.Channel, error) {
	c := &persistence.Channel{}
	var agentID *uuid.UUID
	if err := row.Scan(&c.ID, &c.UserID, &c.Type, &c.ExtID, &c.ConfigEncrypted, &agentID, &c.CreatedAt); err != nil {
		return nil, err
	}
	c.AgentID = agentID
	return c, nil
}

// Register inserts a channel or, on (type, ext_id) conflict, updates the
// encrypted config + agent_id and returns the row's routing tuple.
func (r *ChannelsRepo) Register(
	ctx context.Context,
	userID, channelType, extID string,
	configEncrypted []byte,
	agentID string,
) (*persistence.ChannelLookup, error) {
	uID, err := uuid.Parse(userID)
	if err != nil {
		return nil, fmt.Errorf("ChannelsRepo.Register: bad user_id: %w", err)
	}
	aID, err := uuid.Parse(agentID)
	if err != nil {
		return nil, fmt.Errorf("ChannelsRepo.Register: bad agent_id: %w", err)
	}
	row := r.pool.QueryRow(ctx, `
        INSERT INTO channels (user_id, type, ext_id, config_encrypted, agent_id)
        VALUES ($1, $2, $3, $4, $5)
        ON CONFLICT (type, ext_id) DO UPDATE
            SET config_encrypted = EXCLUDED.config_encrypted,
                agent_id = EXCLUDED.agent_id
        RETURNING id, user_id, agent_id`,
		uID, channelType, extID, configEncrypted, aID,
	)
	var (
		retID      uuid.UUID
		retUser    uuid.UUID
		retAgent   *uuid.UUID
	)
	if err := row.Scan(&retID, &retUser, &retAgent); err != nil {
		return nil, fmt.Errorf("ChannelsRepo.Register: %w", err)
	}
	out := &persistence.ChannelLookup{
		ChannelID: retID.String(),
		UserID:    retUser.String(),
		AgentID:   agentID,
	}
	if retAgent != nil {
		out.AgentID = retAgent.String()
	}
	return out, nil
}

// LookupRouting returns the routing tuple for (type, ext_id), or nil if
// nothing matches or the matching row has no agent_id.
func (r *ChannelsRepo) LookupRouting(ctx context.Context, channelType, extID string) (*persistence.ChannelLookup, error) {
	row := r.pool.QueryRow(ctx, `
        SELECT id, user_id, agent_id FROM channels
        WHERE type = $1 AND ext_id = $2`, channelType, extID)
	var (
		id        uuid.UUID
		userID    uuid.UUID
		agentID   *uuid.UUID
	)
	if err := row.Scan(&id, &userID, &agentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ChannelsRepo.LookupRouting: %w", err)
	}
	if agentID == nil {
		// Mirrors the Python repo: lookup contract requires a non-null agent.
		return nil, nil
	}
	return &persistence.ChannelLookup{
		ChannelID: id.String(),
		UserID:    userID.String(),
		AgentID:   agentID.String(),
	}, nil
}

// Get fetches a channel row.
func (r *ChannelsRepo) Get(ctx context.Context, channelID uuid.UUID) (*persistence.Channel, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+channelSelectCols+` FROM channels WHERE id = $1`, channelID)
	c, err := scanChannel(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ChannelsRepo.Get: %w", err)
	}
	return c, nil
}

// GetDecryptedConfig fetches the encrypted blob and AES-GCM-decrypts it.
// Returns (nil, nil) if the channel does not exist.
func (r *ChannelsRepo) GetDecryptedConfig(ctx context.Context, channelID uuid.UUID) ([]byte, error) {
	if r.cfg == nil {
		return nil, fmt.Errorf(
			"ChannelsRepo: GetDecryptedConfig called without Config — " +
				"pass a non-nil *persistence.Config to NewChannelsRepo",
		)
	}
	c, err := r.Get(ctx, channelID)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, nil
	}
	return persistence.Decrypt(r.keyRaw, c.ConfigEncrypted)
}

// ListForUser lists every channel owned by the user.
func (r *ChannelsRepo) ListForUser(ctx context.Context, userID uuid.UUID) ([]persistence.Channel, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+channelSelectCols+` FROM channels WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("ChannelsRepo.ListForUser: %w", err)
	}
	defer rows.Close()
	var out []persistence.Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, fmt.Errorf("ChannelsRepo.ListForUser scan: %w", err)
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}
