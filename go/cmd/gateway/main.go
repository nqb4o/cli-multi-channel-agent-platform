// Command gateway is the F06 HTTP front door (Go port).
//
// Configuration is fully env-driven. With REDIS_URL unset the binary refuses
// to start with a clear error so a broken deployment is loud, not silent.
//
// When TELEGRAM_BOT_TOKEN and TELEGRAM_WEBHOOK_SECRET are set the Telegram
// channel adapter is auto-registered at startup.
//
// When RUNTIME_DAEMON_BIN is set (or runtime-daemon is on PATH) the built-in
// AgentRunConsumer is started: it reads from agent:runs, dispatches each job
// to runtime-daemon over JSON-RPC stdio, and delivers the reply via the
// matching channel adapter.
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
	"github.com/openclaw/agent-platform/internal/persistence"
	"github.com/openclaw/agent-platform/internal/persistence/repos"
)

// Ensure pg_repos.go types are used (avoids unused import errors in main.go
// when all adapter calls live in that file).
var _ = (*pgChannelsRepo)(nil)

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

	// Wire Postgres repos when DB_ENCRYPTION_KEY + POSTGRES_DSN are set (F12).
	var channelsRepo gateway.ChannelsRepo
	var agentsRepo gateway.AgentsRepo
	var usersRepo gateway.UsersRepo
	if pCfg, pErr := persistence.NewFromEnv(); pErr == nil {
		poolCtx, poolCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer poolCancel()
		pool, poolErr := persistence.NewPool(poolCtx, pCfg.DSN, persistence.DefaultPoolOptions())
		if poolErr != nil {
			log.Printf("WARNING: postgres pool failed (%v) — using in-memory repos", poolErr)
		} else {
			cr, crErr := repos.NewChannelsRepo(pool, &pCfg)
			if crErr != nil {
				log.Printf("WARNING: channels repo init failed (%v) — using in-memory", crErr)
			} else {
				channelsRepo = &pgChannelsRepo{r: cr}
				agentsRepo = &pgAgentsRepo{r: repos.NewAgentsRepo(pool)}
				usersRepo = &pgUsersRepo{r: repos.NewUsersRepo(pool)}
				dsn := pCfg.DSN
				if len(dsn) > 30 {
					dsn = dsn[:30]
				}
				log.Printf("postgres repos wired (dsn prefix: %s...)", dsn)
			}
		}
	} else {
		log.Printf("WARNING: DB_ENCRYPTION_KEY not set — using in-memory repos (%v)", pErr)
	}

	app, err := gateway.NewApp(
		cfg, rdb,
		channelsRepo, // nil → in-memory fallback
		usersRepo,
		agentsRepo,
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Start the agent-run consumer if a concrete *redis.Client is available.
	// The consumer reads from agent:runs, dispatches each job to runtime-daemon
	// over JSON-RPC stdio, and delivers the reply via the channel adapter.
	if chanDB, ok := app.ChannelsRepo.(gateway.ChannelsRepoWithListGetDelete); ok {
		consumer := gateway.NewAgentRunConsumer(
			rdb,
			cfg.StreamName,
			os.Getenv("RUNTIME_DAEMON_BIN"),
			app.Channels,
			chanDB,
			app.AgentsRepo,
		)
		go func() {
			if err := consumer.Run(ctx); err != nil {
				log.Printf("consumer exited: %v", err)
			}
		}()
	} else {
		log.Printf("WARNING: ChannelsRepo does not implement ChannelsRepoWithListGetDelete — consumer not started")
	}

	go func() {
		log.Printf("gateway listening on %s (stream=%s, group=%s)",
			cfg.HTTPAddr, cfg.StreamName, cfg.ConsumerGroup)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("gateway: server error: %v", err)
		}
	}()

	<-stop
	log.Printf("gateway: shutting down")
	cancel() // signal consumer to stop
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	_ = server.Shutdown(shutCtx)
	return nil
}
