package persistence

import (
	"time"

	"github.com/google/uuid"
)

// User mirrors the users table.
type User struct {
	ID        uuid.UUID
	Email     string
	CreatedAt time.Time
}

// Agent mirrors the agents table.
type Agent struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	Name       string
	ConfigYAML string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Channel mirrors the channels table.
type Channel struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	Type            string
	ExtID           string
	ConfigEncrypted []byte
	AgentID         *uuid.UUID
	CreatedAt       time.Time
}

// Sandbox mirrors the sandboxes table.
type Sandbox struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	DaytonaID    string
	State        string // provisioning|running|hibernated|destroyed
	CreatedAt    time.Time
	LastActiveAt *time.Time
}

// Session mirrors the sessions table (composite PK).
type Session struct {
	UserID        uuid.UUID
	ChannelID     uuid.UUID
	ThreadID      string
	Provider      string // codex-cli | google-gemini-cli | claude-cli
	CLISessionID  *string
	InitializedAt *time.Time
	LastUsedAt    time.Time
}

// Run mirrors the runs table.
type Run struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	AgentID    uuid.UUID
	ChannelID  uuid.UUID
	ThreadID   string
	Provider   *string
	Status     string // accepted|running|ok|error
	StartedAt  time.Time
	EndedAt    *time.Time
	LatencyMS  *int
	ErrorClass *string
	ErrorMsg   *string
}

// SkillInstalled mirrors the skills_installed table.
type SkillInstalled struct {
	UserID      uuid.UUID
	Slug        string
	Version     string
	Source      string // registry|local
	InstalledAt time.Time
}

// ChannelLookup is the result of a channel routing query. Field shapes (string-typed
// UUIDs) match the gateway's narrow ChannelLookup type so this repo is structurally
// compatible with gateway.dal.ChannelsRepo (Python parity).
type ChannelLookup struct {
	ChannelID string
	UserID    string
	AgentID   string
}
