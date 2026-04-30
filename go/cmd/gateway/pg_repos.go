package main

// Adapter wrappers that bridge persistence/repos (uuid-typed IDs) to the
// gateway DAL interfaces (string-typed IDs).

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/openclaw/agent-platform/internal/gateway"
	"github.com/openclaw/agent-platform/internal/persistence"
	"github.com/openclaw/agent-platform/internal/persistence/repos"
)

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

type pgUsersRepo struct{ r *repos.UsersRepo }

func (p *pgUsersRepo) Create(ctx context.Context, email string) (*gateway.User, error) {
	u, err := p.r.Create(ctx, email)
	if err != nil {
		return nil, err
	}
	return pgUser(u), nil
}

func (p *pgUsersRepo) Get(ctx context.Context, userID string) (*gateway.User, error) {
	id, err := uuid.Parse(userID)
	if err != nil {
		return nil, fmt.Errorf("pgUsersRepo.Get: bad user_id: %w", err)
	}
	u, err := p.r.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return pgUser(u), nil
}

func (p *pgUsersRepo) GetByEmail(ctx context.Context, email string) (*gateway.User, error) {
	u, err := p.r.GetByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	return pgUser(u), nil
}

func pgUser(u *persistence.User) *gateway.User {
	if u == nil {
		return nil
	}
	return &gateway.User{
		UserID:    u.ID.String(),
		Email:     u.Email,
		CreatedAt: u.CreatedAt,
	}
}

// ---------------------------------------------------------------------------
// Agents
// ---------------------------------------------------------------------------

type pgAgentsRepo struct{ r *repos.AgentsRepo }

func (p *pgAgentsRepo) Create(ctx context.Context, userID, name, configYAML string) (*gateway.Agent, error) {
	id, err := uuid.Parse(userID)
	if err != nil {
		return nil, fmt.Errorf("pgAgentsRepo.Create: bad user_id: %w", err)
	}
	a, err := p.r.Create(ctx, id, name, configYAML)
	if err != nil {
		return nil, err
	}
	return pgAgent(a), nil
}

func (p *pgAgentsRepo) Get(ctx context.Context, agentID string) (*gateway.Agent, error) {
	id, err := uuid.Parse(agentID)
	if err != nil {
		return nil, fmt.Errorf("pgAgentsRepo.Get: bad agent_id: %w", err)
	}
	a, err := p.r.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return pgAgent(a), nil
}

func (p *pgAgentsRepo) ListForUser(ctx context.Context, userID string) ([]gateway.Agent, error) {
	id, err := uuid.Parse(userID)
	if err != nil {
		return nil, fmt.Errorf("pgAgentsRepo.ListForUser: bad user_id: %w", err)
	}
	rows, err := p.r.ListForUser(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.Agent, len(rows))
	for i := range rows {
		if a := pgAgent(&rows[i]); a != nil {
			out[i] = *a
		}
	}
	return out, nil
}

func (p *pgAgentsRepo) UpdateConfig(ctx context.Context, agentID, configYAML string) (*gateway.Agent, error) {
	id, err := uuid.Parse(agentID)
	if err != nil {
		return nil, fmt.Errorf("pgAgentsRepo.UpdateConfig: bad agent_id: %w", err)
	}
	a, err := p.r.UpdateConfig(ctx, id, configYAML)
	if err != nil {
		return nil, err
	}
	return pgAgent(a), nil
}

func pgAgent(a *persistence.Agent) *gateway.Agent {
	if a == nil {
		return nil
	}
	return &gateway.Agent{
		AgentID:    a.ID.String(),
		UserID:     a.UserID.String(),
		Name:       a.Name,
		ConfigYAML: a.ConfigYAML,
		CreatedAt:  a.CreatedAt,
		UpdatedAt:  a.UpdatedAt,
	}
}

// ---------------------------------------------------------------------------
// Channels
// ---------------------------------------------------------------------------

type pgChannelsRepo struct{ r *repos.ChannelsRepo }

func (p *pgChannelsRepo) LookupRouting(ctx context.Context, channelType, extID string) (*gateway.ChannelLookup, error) {
	lk, err := p.r.LookupRouting(ctx, channelType, extID)
	if err != nil {
		return nil, err
	}
	if lk == nil {
		return nil, nil
	}
	return &gateway.ChannelLookup{ChannelID: lk.ChannelID, UserID: lk.UserID, AgentID: lk.AgentID}, nil
}

func (p *pgChannelsRepo) Register(
	ctx context.Context,
	userID, channelType, extID string,
	configEncrypted []byte,
	agentID string,
) (*gateway.ChannelLookup, error) {
	lk, err := p.r.Register(ctx, userID, channelType, extID, configEncrypted, agentID)
	if err != nil {
		return nil, err
	}
	return &gateway.ChannelLookup{ChannelID: lk.ChannelID, UserID: lk.UserID, AgentID: lk.AgentID}, nil
}

func (p *pgChannelsRepo) ListForUser(ctx context.Context, userIDStr string) ([]gateway.ChannelRow, error) {
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		return nil, fmt.Errorf("pgChannelsRepo.ListForUser: bad user_id: %w", err)
	}
	rows, err := p.r.ListForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.ChannelRow, len(rows))
	for i, ch := range rows {
		agentID := ""
		if ch.AgentID != nil {
			agentID = ch.AgentID.String()
		}
		out[i] = gateway.ChannelRow{
			ChannelID:       ch.ID.String(),
			UserID:          ch.UserID.String(),
			AgentID:         agentID,
			ChannelType:     ch.Type,
			ExtID:           ch.ExtID,
			ConfigEncrypted: ch.ConfigEncrypted,
			CreatedAt:       ch.CreatedAt,
		}
	}
	return out, nil
}

func (p *pgChannelsRepo) Get(ctx context.Context, channelID string) (*gateway.ChannelRow, error) {
	id, err := uuid.Parse(channelID)
	if err != nil {
		return nil, fmt.Errorf("pgChannelsRepo.Get: bad channel_id: %w", err)
	}
	ch, err := p.r.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if ch == nil {
		return nil, nil
	}
	agentID := ""
	if ch.AgentID != nil {
		agentID = ch.AgentID.String()
	}
	return &gateway.ChannelRow{
		ChannelID:       ch.ID.String(),
		UserID:          ch.UserID.String(),
		AgentID:         agentID,
		ChannelType:     ch.Type,
		ExtID:           ch.ExtID,
		ConfigEncrypted: ch.ConfigEncrypted,
		CreatedAt:       ch.CreatedAt,
	}, nil
}

// Delete is not yet implemented in the persistence layer; returns (false, nil).
func (p *pgChannelsRepo) Delete(ctx context.Context, channelID string) (bool, error) {
	return false, fmt.Errorf("channel delete not yet implemented")
}

// ensure compile-time interface satisfaction
var _ gateway.ChannelsRepoWithListGetDelete = (*pgChannelsRepo)(nil)
var _ gateway.AgentsRepo = (*pgAgentsRepo)(nil)
var _ gateway.UsersRepo = (*pgUsersRepo)(nil)

// min is available in Go 1.21+; provide a local copy for older builds.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ensure time import is used
var _ = time.Now
