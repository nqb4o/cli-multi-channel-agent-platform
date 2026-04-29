package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/openclaw/agent-platform/internal/gateway"
	"github.com/openclaw/agent-platform/internal/gateway/channels"
)

// ---------------------------------------------------------------------------
// Test fixtures: a fake Telegram-like adapter, a fake orchestrator client,
// and a builder that wires up a fully-isolated App + chi.Mux for each test.
// ---------------------------------------------------------------------------

const (
	testAdminToken     = "admin-tok-test"
	testJWTSecret      = "user-jwt-secret-test"
	testEncryptionKey  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // 64 hex / 32 bytes
	testWebhookSecret  = "secret-shhh"
	testTGType         = "telegram"
	testTelegramChanID = "tg:bot-x:chat-1"
)

// fakeTelegramAdapter mimics the production adapter just enough for the
// webhook tests: header secret check + Telegram-shape parser.
type fakeTelegramAdapter struct {
	secret    string
	chanID    string
	parseErr  error
	verifyErr bool
}

func (f *fakeTelegramAdapter) Type() string { return testTGType }

func (f *fakeTelegramAdapter) VerifySignature(h http.Header, _ []byte) bool {
	if f.verifyErr {
		panic("simulated verify panic")
	}
	return h.Get("X-Telegram-Bot-Api-Secret-Token") == f.secret
}

func (f *fakeTelegramAdapter) ParseIncoming(body []byte) (*channels.NormalizedMessage, error) {
	if f.parseErr != nil {
		return nil, f.parseErr
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, errors.New("invalid JSON")
	}
	updateID, ok := raw["update_id"]
	if !ok {
		return nil, errors.New("missing update_id")
	}
	msgRaw, ok := raw["message"].(map[string]any)
	if !ok {
		return nil, errors.New("missing message")
	}
	chat, _ := msgRaw["chat"].(map[string]any)
	chatID, _ := chat["id"].(float64)
	text, _ := msgRaw["text"].(string)
	from, _ := msgRaw["from"].(map[string]any)
	fromID, _ := from["id"].(float64)

	chanID := f.chanID
	if chanID == "" {
		chanID = "tg:bot:" + strconv.Itoa(int(chatID))
	}
	return &channels.NormalizedMessage{
		MessageID:  strconv.Itoa(int(updateID.(float64))),
		ChannelID:  chanID,
		ThreadID:   strconv.Itoa(int(chatID)),
		Text:       text,
		Payload:    map[string]any{"raw_update_id": updateID},
		SenderID:   strconv.Itoa(int(fromID)),
		Attachments: nil,
		ReceivedAt: "2026-04-28T00:00:00Z",
	}, nil
}

func (f *fakeTelegramAdapter) SendOutgoing(_ context.Context, _, _, _ string, _ map[string]any) error {
	return nil
}

// fakeOrchestrator implements gateway.OrchestratorClient.
type fakeOrchestrator struct {
	calls    []string
	raiseErr error
	nextID   int
}

func (f *fakeOrchestrator) ProvisionSandbox(_ context.Context, userID string) (*gateway.SandboxView, error) {
	f.calls = append(f.calls, userID)
	if f.raiseErr != nil {
		return nil, f.raiseErr
	}
	f.nextID++
	return &gateway.SandboxView{
		ID:     "sb-" + strconv.Itoa(f.nextID),
		UserID: userID,
		State:  "running",
	}, nil
}

func (f *fakeOrchestrator) ExecInSandbox(_ context.Context, _ string, _ []string, _ int) (*gateway.ExecResult, error) {
	return &gateway.ExecResult{Stdout: "ok", ExitCode: new(int)}, nil
}

func (f *fakeOrchestrator) Healthz(_ context.Context) bool { return true }

// testEnv bundles the per-test wiring so individual tests can poke at any
// dependency without rebuilding from scratch.
type testEnv struct {
	t            *testing.T
	app          *gateway.App
	router       http.Handler
	mux          *miniredis.Miniredis
	redis        *redis.Client
	channelsRepo *gateway.InMemoryChannelsRepo
	usersRepo    *gateway.InMemoryUsersRepo
	agentsRepo   *gateway.InMemoryAgentsRepo
	orch         *fakeOrchestrator
	registry     *gateway.ChannelRegistry
	dbHealth     *gateway.ToggleHealth
}

type envOpts struct {
	adminToken    string
	jwtSecret     string
	bypassLogin   bool
	encryptionKey string
}

func defaultEnvOpts() envOpts {
	return envOpts{
		adminToken:    testAdminToken,
		jwtSecret:     testJWTSecret,
		bypassLogin:   true,
		encryptionKey: testEncryptionKey,
	}
}

func newTestEnv(t *testing.T, opts envOpts) *testEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	cfg := gateway.Config{
		AdminToken:            opts.adminToken,
		RedisURL:              "redis://" + mr.Addr(),
		StreamName:            "agent:runs",
		ConsumerGroup:         "orchestrator",
		IdempotencyTTLSeconds: 600,
		IdempotencyNamespace:  "gw:idem",
		UserJWTSecret:         opts.jwtSecret,
		UserJWTTTLSeconds:     3600,
		BypassLogin:           opts.bypassLogin,
		DBEncryptionKeyHex:    opts.encryptionKey,
		OrchestratorURL:       "http://stub.invalid",
	}

	channelsRepo := gateway.NewInMemoryChannelsRepo()
	usersRepo := gateway.NewInMemoryUsersRepo()
	agentsRepo := gateway.NewInMemoryAgentsRepo()
	orch := &fakeOrchestrator{}
	dbHealth := gateway.NewToggleHealth()
	// Each test gets its own registry — DON'T touch the process-wide one.
	registry := gateway.NewChannelRegistry()

	app, err := gateway.NewApp(
		cfg, rdb, channelsRepo, usersRepo, agentsRepo, orch, dbHealth, registry,
	)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	return &testEnv{
		t:            t,
		app:          app,
		router:       NewRouter(app),
		mux:          mr,
		redis:        rdb,
		channelsRepo: channelsRepo,
		usersRepo:    usersRepo,
		agentsRepo:   agentsRepo,
		orch:         orch,
		registry:     registry,
		dbHealth:     dbHealth,
	}
}

// authedToken creates a user in the in-memory repo and returns the bearer JWT.
func (e *testEnv) authedToken(email string) (*gateway.User, string) {
	u, err := e.usersRepo.Create(context.Background(), email)
	if err != nil {
		e.t.Fatalf("create user: %v", err)
	}
	tok, err := gateway.IssueUserToken(u.UserID, u.Email, e.app.Config.UserJWTSecret, e.app.UserJWTTTL())
	if err != nil {
		e.t.Fatalf("issue token: %v", err)
	}
	return u, tok
}

// registerTelegram wires a fake Telegram adapter into this env's registry.
func (e *testEnv) registerTelegram() *fakeTelegramAdapter {
	a := &fakeTelegramAdapter{
		secret: testWebhookSecret,
		chanID: testTelegramChanID,
	}
	if err := e.registry.Register(testTGType, a); err != nil {
		e.t.Fatalf("register adapter: %v", err)
	}
	return a
}
