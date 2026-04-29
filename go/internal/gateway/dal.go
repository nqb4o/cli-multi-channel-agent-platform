package gateway

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ChannelLookup is the result of a channel routing query (channel_id, user_id,
// agent_id) — same shape as persistence.ChannelLookup for cross-package
// structural compatibility.
type ChannelLookup struct {
	ChannelID string
	UserID    string
	AgentID   string
}

// User mirrors persistence.User but with string ids for gateway-internal use.
type User struct {
	UserID    string
	Email     string
	CreatedAt time.Time
}

// Agent mirrors persistence.Agent with string ids.
type Agent struct {
	AgentID    string
	UserID     string
	Name       string
	ConfigYAML string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ChannelRow is the row carried by the user-facing channels API.
type ChannelRow struct {
	ChannelID       string
	UserID          string
	AgentID         string
	ChannelType     string
	ExtID           string
	ConfigEncrypted []byte
	CreatedAt       time.Time
}

// ---------------------------------------------------------------------------
// Repo interfaces — narrow versions used by the gateway.
// ---------------------------------------------------------------------------

// HealthCheckable is anything we can ping for /readyz.
type HealthCheckable interface {
	Ping(ctx context.Context) bool
}

// UsersRepo is the gateway-facing slice of the users repo.
type UsersRepo interface {
	Create(ctx context.Context, email string) (*User, error)
	Get(ctx context.Context, userID string) (*User, error)
	GetByEmail(ctx context.Context, email string) (*User, error)
}

// AgentsRepo is the gateway-facing slice of the agents repo. Delete is
// optional (matches the Python AgentsRepo Protocol — the gateway probes for it
// at request time).
type AgentsRepo interface {
	Create(ctx context.Context, userID, name, configYAML string) (*Agent, error)
	Get(ctx context.Context, agentID string) (*Agent, error)
	ListForUser(ctx context.Context, userID string) ([]Agent, error)
	UpdateConfig(ctx context.Context, agentID, configYAML string) (*Agent, error)
}

// AgentsRepoWithDelete is the optional Delete extension.
type AgentsRepoWithDelete interface {
	AgentsRepo
	Delete(ctx context.Context, agentID string) (bool, error)
}

// ChannelsRepo is the slice the gateway uses for routing + admin/user routes.
type ChannelsRepo interface {
	LookupRouting(ctx context.Context, channelType, extID string) (*ChannelLookup, error)
	Register(
		ctx context.Context,
		userID, channelType, extID string,
		configEncrypted []byte,
		agentID string,
	) (*ChannelLookup, error)
}

// ChannelsRepoWithListGetDelete is the user-facing extension surface.
type ChannelsRepoWithListGetDelete interface {
	ChannelsRepo
	ListForUser(ctx context.Context, userID string) ([]ChannelRow, error)
	Get(ctx context.Context, channelID string) (*ChannelRow, error)
	Delete(ctx context.Context, channelID string) (bool, error)
}

// ---------------------------------------------------------------------------
// In-memory implementations (used by tests + dev mode).
// ---------------------------------------------------------------------------

// AlwaysHealthy is the default db_health stub.
type AlwaysHealthy struct{}

// Ping always returns true.
func (AlwaysHealthy) Ping(context.Context) bool { return true }

// ToggleHealth is a HealthCheckable whose ping result is mutable.
type ToggleHealth struct {
	mu sync.Mutex
	OK bool
}

// NewToggleHealth returns a healthy toggle.
func NewToggleHealth() *ToggleHealth { return &ToggleHealth{OK: true} }

// Ping returns the current toggle state.
func (h *ToggleHealth) Ping(context.Context) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.OK
}

// Set updates the toggle. Test helper.
func (h *ToggleHealth) Set(ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.OK = ok
}

// InMemoryUsersRepo is a goroutine-safe in-memory users repo.
type InMemoryUsersRepo struct {
	mu      sync.Mutex
	byID    map[string]User
	byEmail map[string]User
}

// NewInMemoryUsersRepo constructs an empty repo.
func NewInMemoryUsersRepo() *InMemoryUsersRepo {
	return &InMemoryUsersRepo{
		byID:    map[string]User{},
		byEmail: map[string]User{},
	}
}

// Create is idempotent on email — re-inserting an existing email returns the
// existing row, mirroring the Python InMemoryUsersRepo.
func (r *InMemoryUsersRepo) Create(_ context.Context, email string) (*User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := strings.ToLower(email)
	if u, ok := r.byEmail[key]; ok {
		out := u
		return &out, nil
	}
	u := User{
		UserID:    uuid.NewString(),
		Email:     email,
		CreatedAt: time.Now().UTC(),
	}
	r.byID[u.UserID] = u
	r.byEmail[key] = u
	return &u, nil
}

// Get fetches a user by id.
func (r *InMemoryUsersRepo) Get(_ context.Context, userID string) (*User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.byID[userID]
	if !ok {
		return nil, nil
	}
	return &u, nil
}

// GetByEmail fetches a user by email (case-insensitive).
func (r *InMemoryUsersRepo) GetByEmail(_ context.Context, email string) (*User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.byEmail[strings.ToLower(email)]
	if !ok {
		return nil, nil
	}
	return &u, nil
}

// InMemoryAgentsRepo is a goroutine-safe in-memory agents repo.
type InMemoryAgentsRepo struct {
	mu   sync.Mutex
	rows map[string]Agent
}

// NewInMemoryAgentsRepo constructs an empty repo.
func NewInMemoryAgentsRepo() *InMemoryAgentsRepo {
	return &InMemoryAgentsRepo{rows: map[string]Agent{}}
}

// Create inserts a fresh agent.
func (r *InMemoryAgentsRepo) Create(_ context.Context, userID, name, configYAML string) (*Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	a := Agent{
		AgentID:    uuid.NewString(),
		UserID:     userID,
		Name:       name,
		ConfigYAML: configYAML,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	r.rows[a.AgentID] = a
	return &a, nil
}

// Get returns the agent or (nil, nil) if missing.
func (r *InMemoryAgentsRepo) Get(_ context.Context, agentID string) (*Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.rows[agentID]
	if !ok {
		return nil, nil
	}
	return &a, nil
}

// ListForUser returns all agents owned by userID, sorted by created_at.
func (r *InMemoryAgentsRepo) ListForUser(_ context.Context, userID string) ([]Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Agent
	for _, a := range r.rows {
		if a.UserID == userID {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// UpdateConfig replaces config_yaml for an existing agent. Returns (nil, nil)
// if the agent doesn't exist.
func (r *InMemoryAgentsRepo) UpdateConfig(_ context.Context, agentID, configYAML string) (*Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.rows[agentID]
	if !ok {
		return nil, nil
	}
	a.ConfigYAML = configYAML
	a.UpdatedAt = time.Now().UTC()
	r.rows[agentID] = a
	return &a, nil
}

// Delete hard-deletes an agent.
func (r *InMemoryAgentsRepo) Delete(_ context.Context, agentID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.rows[agentID]; !ok {
		return false, nil
	}
	delete(r.rows, agentID)
	return true, nil
}

// InMemoryChannelsRepo is a goroutine-safe in-memory channels repo.
type InMemoryChannelsRepo struct {
	mu    sync.Mutex
	byExt map[extKey]ChannelLookup
	rows  map[string]ChannelRow
}

type extKey struct{ Type, ExtID string }

// NewInMemoryChannelsRepo constructs an empty repo.
func NewInMemoryChannelsRepo() *InMemoryChannelsRepo {
	return &InMemoryChannelsRepo{
		byExt: map[extKey]ChannelLookup{},
		rows:  map[string]ChannelRow{},
	}
}

// LookupRouting resolves (channelType, extID) → ChannelLookup, or nil.
func (r *InMemoryChannelsRepo) LookupRouting(_ context.Context, channelType, extID string) (*ChannelLookup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lk, ok := r.byExt[extKey{channelType, extID}]
	if !ok {
		return nil, nil
	}
	out := lk
	return &out, nil
}

// Register inserts (or updates) a channel registration. Idempotent on
// (channelType, extID).
func (r *InMemoryChannelsRepo) Register(
	_ context.Context,
	userID, channelType, extID string,
	configEncrypted []byte,
	agentID string,
) (*ChannelLookup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := extKey{channelType, extID}
	if existing, ok := r.byExt[key]; ok {
		// ON CONFLICT DO UPDATE — refresh config + agent on the existing row.
		row := r.rows[existing.ChannelID]
		row.ConfigEncrypted = append([]byte(nil), configEncrypted...)
		row.AgentID = agentID
		r.rows[existing.ChannelID] = row
		out := existing
		out.AgentID = agentID
		return &out, nil
	}
	lk := ChannelLookup{
		ChannelID: uuid.NewString(),
		UserID:    userID,
		AgentID:   agentID,
	}
	r.byExt[key] = lk
	r.rows[lk.ChannelID] = ChannelRow{
		ChannelID:       lk.ChannelID,
		UserID:          userID,
		AgentID:         agentID,
		ChannelType:     channelType,
		ExtID:           extID,
		ConfigEncrypted: append([]byte(nil), configEncrypted...),
		CreatedAt:       time.Now().UTC(),
	}
	out := lk
	return &out, nil
}

// ListForUser returns all rows owned by userID, sorted by created_at.
func (r *InMemoryChannelsRepo) ListForUser(_ context.Context, userID string) ([]ChannelRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ChannelRow
	for _, row := range r.rows {
		if row.UserID == userID {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Get returns a row by id.
func (r *InMemoryChannelsRepo) Get(_ context.Context, channelID string) (*ChannelRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[channelID]
	if !ok {
		return nil, nil
	}
	return &row, nil
}

// Delete removes a row by id, keeping the routing index consistent.
func (r *InMemoryChannelsRepo) Delete(_ context.Context, channelID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[channelID]
	if !ok {
		return false, nil
	}
	delete(r.rows, channelID)
	delete(r.byExt, extKey{row.ChannelType, row.ExtID})
	return true, nil
}

// SeedRouting is a test helper that pre-populates a routing entry.
func (r *InMemoryChannelsRepo) SeedRouting(channelType, extID string, lk ChannelLookup) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byExt[extKey{channelType, extID}] = lk
}

// Rows returns a snapshot of all rows. Test helper.
func (r *InMemoryChannelsRepo) Rows() []ChannelRow {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ChannelRow, 0, len(r.rows))
	for _, row := range r.rows {
		out = append(out, row)
	}
	return out
}

// ErrNotImplemented is returned by repos that lack an optional method.
var ErrNotImplemented = errors.New("operation not supported by configured repo")
