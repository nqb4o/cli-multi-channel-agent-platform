package repos

import (
	"github.com/openclaw/agent-platform/internal/persistence"
)

// Compile-time guards: every concrete repo satisfies its interface.
var (
	_ persistence.UsersRepo           = (*UsersRepo)(nil)
	_ persistence.AgentsRepo          = (*AgentsRepo)(nil)
	_ persistence.ChannelsRepo        = (*ChannelsRepo)(nil)
	_ persistence.SandboxesRepo       = (*SandboxesRepo)(nil)
	_ persistence.SessionsRepo        = (*SessionsRepo)(nil)
	_ persistence.RunsRepo            = (*RunsRepo)(nil)
	_ persistence.SkillsInstalledRepo = (*SkillsInstalledRepo)(nil)
)
