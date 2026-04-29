// Command runtime-daemon is the Go port of the F05 ADK Agent Runtime
// daemon (services/runtime/src/runtime/daemon.py).
//
// Lifecycle:
//
//   - Process is launched by F01 (orchestrator) inside a Daytona sandbox.
//   - Reads JSON-RPC 2.0 envelopes from stdin (one per line).
//   - Replies on stdout (line-delimited JSON).
//   - Logs to stderr.
//   - SIGTERM / SIGINT → drain in-flight runs, exit 0 (bounded by the
//     jsonrpc package's drain timeout).
//
// Usage:
//
//	agent-runtime --config /home/user/agent.yaml \
//	              --workspace /home/user/workspace \
//	              [--register-stub] [--log-level INFO]
//
// In the production deployment the orchestrator pre-writes agent.yaml +
// the workspace mount before launching this binary; backends are
// registered in init() by F02/F03/F04 (we only ship the stub here so
// smoke-test harnesses have a known-good path).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/openclaw/agent-platform/internal/clibackend"
	"github.com/openclaw/agent-platform/internal/runtime"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is the testable main entrypoint. Returns the process exit code.
func run(args []string, stdin *os.File, stdout *os.File, stderr *os.File) int {
	fs := flag.NewFlagSet("agent-runtime", flag.ContinueOnError)
	fs.SetOutput(stderr)

	configPath := fs.String("config", runtime.DefaultAgentYAMLPath, "path to agent.yaml")
	workspaceDir := fs.String("workspace", runtime.DefaultWorkspaceDir, "workspace directory")
	registerStub := fs.Bool("register-stub", false, "register a no-op 'stub' backend for smoke testing")
	logLevel := fs.String("log-level", "INFO", "log level (DEBUG, INFO, WARN, ERROR)")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{
		Level: parseLogLevel(*logLevel),
	}))
	slog.SetDefault(logger)

	registry := clibackend.NewBackendRegistry()
	// Register real CLI backends unconditionally — each will surface an auth
	// error at run time if the underlying CLI is absent or unauthenticated.
	for _, b := range []clibackend.CliBackend{
		clibackend.NewClaudeBackend(),
		clibackend.NewCodexBackend(),
		clibackend.NewGeminiBackend(clibackend.GeminiConfig{}),
	} {
		if err := registry.Register(b); err != nil {
			// Non-fatal: continue without this backend.
			fmt.Fprintf(stderr, "WARNING: register %T: %v\n", b, err)
		}
	}
	if *registerStub {
		if err := registry.Register(runtime.NewStubBackend()); err != nil {
			fmt.Fprintf(stderr, "ERROR: register stub backend: %v\n", err)
			return 1
		}
	}

	dal := runtime.NewInMemorySessionDal()

	daemon, _, err := runtime.BuildDaemon(runtime.BuildDaemonOptions{
		AgentYAML:    *configPath,
		Registry:     registry,
		SessionDal:   dal,
		WorkspaceDir: *workspaceDir,
		Logger:       logger,
	})
	if err != nil {
		fmt.Fprintf(stderr, "ERROR: build daemon: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// SIGTERM / SIGINT → graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case <-sigCh:
			logger.Info("received shutdown signal — draining")
			daemon.Shutdown()
		case <-ctx.Done():
		}
	}()

	if err := daemon.Serve(ctx, stdin, stdout); err != nil {
		fmt.Fprintf(stderr, "ERROR: serve: %v\n", err)
		return 1
	}
	return 0
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
