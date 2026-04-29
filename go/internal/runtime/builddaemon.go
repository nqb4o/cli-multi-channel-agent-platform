package runtime

import (
	"fmt"
	"log/slog"

	"github.com/openclaw/agent-platform/internal/clibackend"
)

// BuildDaemonOptions bundles the wiring for [BuildDaemon].
//
// Required fields:
//
//   - AgentYAML — path to the agent.yaml on disk. Pass "" to use
//     [DefaultAgentYAMLPath].
//   - Registry — pre-populated [clibackend.BackendRegistry]. F02/F03/F04
//     register their backends before BuildDaemon runs; BuildDaemon never
//     hardcodes any.
//   - SessionDal — a SessionDal (DB-backed via [NewDBSessionDal] or the
//     in-memory fallback via [NewInMemorySessionDal]).
//
// Optional:
//
//   - WorkspaceDir — default workspace dir for bootstrap-file lookup.
//     Falls back to [DefaultWorkspaceDir].
//   - Logger — slog.Logger for both the loop and the daemon. Defaults to
//     slog.Default().
type BuildDaemonOptions struct {
	AgentYAML    string
	Registry     *clibackend.BackendRegistry
	SessionDal   SessionDal
	WorkspaceDir string
	Logger       *slog.Logger
}

// BuildDaemon parses agent.yaml, builds the AgentLoop, and returns a
// configured Daemon.
//
// Production code calls this once at startup. Tests construct Daemon
// directly with their own AgentLoop.
//
// Mirrors build_daemon in services/runtime/src/runtime/daemon.py minus
// the F09/F11 wiring (skill_roots + mcp_manager) — those are deferred
// to Wave G-D.
func BuildDaemon(opts BuildDaemonOptions) (*Daemon, []ConfigWarning, error) {
	if opts.Registry == nil {
		return nil, nil, fmt.Errorf("BuildDaemon: Registry is required")
	}
	if opts.SessionDal == nil {
		return nil, nil, fmt.Errorf("BuildDaemon: SessionDal is required")
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	cfg, warnings, err := LoadAgentConfig(opts.AgentYAML)
	if err != nil {
		return nil, warnings, fmt.Errorf("BuildDaemon: load agent.yaml: %w", err)
	}
	for _, w := range warnings {
		logger.Warn("agent.yaml warning", "code", w.Code, "message", w.Message)
	}

	loop := NewAgentLoop(
		cfg,
		opts.Registry,
		opts.SessionDal,
		WithWorkspaceDir(opts.WorkspaceDir),
		WithLogger(logger),
	)

	d := NewDaemon(loop, WithDaemonLogger(logger))
	return d, warnings, nil
}
