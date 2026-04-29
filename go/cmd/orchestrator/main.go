// Service entry point for the F01 + F15 sandbox orchestrator.
//
// Builds the chi HTTP router and wires the Orchestrator (F01) together
// with the F15 lifecycle layers — SandboxPool, optional HibernateScheduler,
// optional WarmPoolManager, PresenceTrigger, and optional HealthProbeLoop.
//
// When DAYTONA_API_KEY is unset the orchestrator falls back to the
// in-memory FakeDaytonaClient so local dev keeps working without the SDK.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/openclaw/agent-platform/internal/orchestrator"
)

func main() {
	cfg := orchestrator.LoadConfig()

	var client orchestrator.DaytonaClient
	if cfg.UseFakeDaytona() {
		log.Printf("DAYTONA_API_KEY unset — using in-memory FakeDaytonaClient. " +
			"This is fine for tests + local dev; production must set the key.")
		client = orchestrator.NewFakeDaytonaClient()
	} else {
		client = orchestrator.NewLiveDaytonaClient(cfg.DaytonaAPIKey, cfg.DaytonaAPIURL, cfg.DaytonaTarget)
	}

	orch := orchestrator.NewOrchestrator(client, cfg.SandboxImage,
		orchestrator.WithAutoStopIntervalM(cfg.AutoStopIntervalM),
	)

	poolCapacity := orchestrator.DefaultPoolCapacity
	if v := os.Getenv("ORCHESTRATOR_POOL_CAPACITY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			poolCapacity = n
		}
	}
	pool := orchestrator.NewSandboxPool(orch, orchestrator.WithPoolCapacity(poolCapacity))
	presence := orchestrator.NewPresenceTrigger(pool)

	var hibernateSched *orchestrator.HibernateScheduler
	if os.Getenv("ORCHESTRATOR_DISABLE_HIBERNATE") == "" {
		hibernateSched = orchestrator.NewHibernateScheduler(pool)
		hibernateSched.Start()
	}

	var healthLoop *orchestrator.HealthProbeLoop
	if os.Getenv("ORCHESTRATOR_ENABLE_HEALTH_PROBE") == "1" {
		if pinger, ok := client.(orchestrator.HealthPinger); ok {
			prober := healthPingerAdapter{pinger: pinger}
			healthLoop = orchestrator.NewHealthProbeLoop(pool, prober)
			healthLoop.Start()
		}
	}

	deps := orchestrator.Deps{
		Orchestrator: orch,
		Pool:         pool,
		Presence:     presence,
		// WarmPool wired only when a TopActiveUsersSource is provided;
		// the live wiring needs F12 (persistence) — left nil here.
	}
	router := orchestrator.NewRouter(deps)

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	server := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("orchestrator listening on %s", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-stop
	log.Printf("shutdown signal received; stopping background loops")

	if healthLoop != nil {
		healthLoop.Stop()
	}
	if hibernateSched != nil {
		hibernateSched.Stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// healthPingerAdapter adapts a HealthPinger (DaytonaClient extension) into
// the HealthProber interface expected by HealthProbeLoop.
type healthPingerAdapter struct {
	pinger orchestrator.HealthPinger
}

func (a healthPingerAdapter) PingHealth(ctx context.Context, sandboxID string, timeout time.Duration) bool {
	return a.pinger.PingHealth(ctx, sandboxID, timeout)
}
