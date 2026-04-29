// Command gateway is the F06 HTTP front door (Go port).
//
// Configuration is fully env-driven. With REDIS_URL unset the binary refuses
// to start with a clear error so a broken deployment is loud, not silent.
//
// When TELEGRAM_BOT_TOKEN and TELEGRAM_WEBHOOK_SECRET are set the Telegram
// channel adapter is auto-registered at startup.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/openclaw/agent-platform/adapters/telegram"
	"github.com/openclaw/agent-platform/internal/gateway"
	"github.com/openclaw/agent-platform/internal/gateway/routes"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gateway: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := gateway.LoadConfig()

	if cfg.RedisURL == "" {
		return errors.New("REDIS_URL required (no fallback in single-process mode)")
	}

	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(opts)

	// Probe redis at startup so misconfig fails fast (5s budget).
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		return fmt.Errorf("redis ping failed: %w", err)
	}

	// Auto-register the Telegram adapter when credentials are present.
	if os.Getenv("TELEGRAM_BOT_TOKEN") != "" {
		tgCfg, err := telegram.FromEnv(nil)
		if err != nil {
			log.Printf("WARNING: TELEGRAM_BOT_TOKEN set but adapter failed to load: %v", err)
		} else {
			if _, err := telegram.Register(gateway.DefaultChannelRegistry(), tgCfg); err != nil {
				return fmt.Errorf("register telegram adapter: %w", err)
			}
			log.Printf("telegram adapter registered (bot_id=%s)", tgCfg.BotID)
		}
	}

	app, err := gateway.NewApp(
		cfg, rdb,
		nil, // channels repo (in-memory default; F12 swap point)
		nil, // users repo
		nil, // agents repo
		nil, // orchestrator (HTTP client at cfg.OrchestratorURL)
		nil, // db health (always-healthy default)
		nil, // channel registry (process-wide default)
	)
	if err != nil {
		return fmt.Errorf("NewApp: %w", err)
	}

	router := routes.NewRouter(app)
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Trap SIGINT/SIGTERM for graceful shutdown.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("gateway listening on %s (stream=%s, group=%s)",
			cfg.HTTPAddr, cfg.StreamName, cfg.ConsumerGroup)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("gateway: server error: %v", err)
		}
	}()

	<-stop
	log.Printf("gateway: shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutCtx)
	return nil
}
