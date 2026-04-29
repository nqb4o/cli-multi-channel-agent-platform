package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// UsersRepo is the canonical contract for the users table. (Maps to Python UsersRepoP.)
type UsersRepo interface {
	Create(ctx context.Context, email string) (*User, error)
	Get(ctx context.Context, userID uuid.UUID) (*User, error)
	GetByEmail(ctx context.Context, email string) (*User, error)
}

// AgentsRepo — canonical agents repo contract.
type AgentsRepo interface {
	Create(ctx context.Context, userID uuid.UUID, name, configYAML string) (*Agent, error)
	Get(ctx context.Context, agentID uuid.UUID) (*Agent, error)
	ListForUser(ctx context.Context, userID uuid.UUID) ([]Agent, error)
	UpdateConfig(ctx context.Context, agentID uuid.UUID, configYAML string) (*Agent, error)
}

// ChannelsRepo — canonical channels repo contract.
//
// register/lookup_routing accept string-typed UUIDs to match the F06 gateway's
// narrow Protocol; Get/list-style methods take uuid.UUID for type safety.
type ChannelsRepo interface {
	Register(
		ctx context.Context,
		userID, channelType, extID string,
		configEncrypted []byte,
		agentID string,
	) (*ChannelLookup, error)
	LookupRouting(ctx context.Context, channelType, extID string) (*ChannelLookup, error)
	Get(ctx context.Context, channelID uuid.UUID) (*Channel, error)
	GetDecryptedConfig(ctx context.Context, channelID uuid.UUID) ([]byte, error)
	ListForUser(ctx context.Context, userID uuid.UUID) ([]Channel, error)
}

// SandboxesRepo — canonical sandbox repo contract (one row per user).
type SandboxesRepo interface {
	Upsert(ctx context.Context, userID uuid.UUID, daytonaID, state string) (*Sandbox, error)
	GetForUser(ctx context.Context, userID uuid.UUID) (*Sandbox, error)
	UpdateState(ctx context.Context, userID uuid.UUID, state string, lastActiveAt *time.Time) (*Sandbox, error)
}

// SessionsRepo — composite-PK session-id cache contract.
type SessionsRepo interface {
	Get(ctx context.Context, userID, channelID uuid.UUID, threadID, provider string) (*Session, error)
	UpsertAfterTurn(ctx context.Context, userID, channelID uuid.UUID, threadID, provider, cliSessionID string) error
	DropForProvider(ctx context.Context, userID uuid.UUID, provider string) (int64, error)
}

// RunsRepo — audit log contract: one row per agent run.
type RunsRepo interface {
	Start(
		ctx context.Context,
		userID, agentID, channelID uuid.UUID,
		threadID string,
		provider *string,
	) (*Run, error)
	FinishOK(ctx context.Context, runID uuid.UUID, latencyMS int, endedAt *time.Time) (*Run, error)
	FinishError(
		ctx context.Context,
		runID uuid.UUID,
		latencyMS int,
		errorClass, errorMsg string,
		endedAt *time.Time,
	) (*Run, error)
	Get(ctx context.Context, runID uuid.UUID) (*Run, error)
	ListRecentForUser(ctx context.Context, userID uuid.UUID, limit int) ([]Run, error)
}

// SkillsInstalledRepo — per-user installed skill record contract.
type SkillsInstalledRepo interface {
	Install(ctx context.Context, userID uuid.UUID, slug, version, source string) (*SkillInstalled, error)
	Uninstall(ctx context.Context, userID uuid.UUID, slug string) (bool, error)
	ListForUser(ctx context.Context, userID uuid.UUID) ([]SkillInstalled, error)
}
