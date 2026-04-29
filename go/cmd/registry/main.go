// Command registry is the F13 Skill Registry HTTP server.
//
// Configuration is fully env-driven:
//
//	REGISTRY_DSN                    — Postgres DSN; empty → in-memory metadata
//	REGISTRY_BLOB_DIR               — filesystem blob root; empty → in-memory blobs
//	REGISTRY_VERIFIER               — "inprocess" (default) | "cosign" | "always-accept"
//	REGISTRY_ALLOW_INSECURE_VERIFIER — "1" to enable always-accept
//	REGISTRY_HOST                   — bind host (default: 127.0.0.1)
//	REGISTRY_PORT                   — bind port (default: 8090)
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/openclaw/agent-platform/internal/persistence"
	"github.com/openclaw/agent-platform/internal/registry"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "registry: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := registry.LoadConfigFromEnv()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Open Postgres pool when DSN is set.
	var deps *registry.Deps
	if cfg.DSN != "" {
		pool, err := persistence.NewPool(ctx, cfg.DSN, persistence.DefaultPoolOptions())
		if err != nil {
			return fmt.Errorf("open postgres pool: %w", err)
		}
		defer pool.Close()
		if !persistence.Ping(ctx, pool) {
			return errors.New("postgres ping failed")
		}
		deps, err = registry.NewDepsFromConfig(cfg, pool)
		if err != nil {
			return fmt.Errorf("build deps: %w", err)
		}
	} else {
		var err error
		deps, err = registry.NewDepsFromConfig(cfg, nil)
		if err != nil {
			return fmt.Errorf("build deps: %w", err)
		}
	}

	router := registry.NewRouter(deps)
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("registry: listening on %s", addr)

	go func() {
		<-ctx.Done()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		_ = srv.Shutdown(shutCtx)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen: %w", err)
	}
	return nil
}
