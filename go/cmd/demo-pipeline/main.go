// Command demo-pipeline exercises the full Go pipeline end-to-end.
//
// What it does:
//  1. Finds (or builds) the gateway, orchestrator, and runtime-daemon binaries.
//  2. Starts Go orchestrator + gateway as child processes.
//  3. Uses the gateway HTTP API to create a demo user, agent, and channel.
//  4. POSTs a synthetic Telegram webhook to the gateway.
//  5. Reads the resulting agent:runs entry from Redis.
//  6. Spawns runtime-daemon --register-stub and sends a JSON-RPC run request.
//  7. Replies to Telegram with the daemon's response text.
//
// Required env vars:
//
//	TELEGRAM_BOT_TOKEN   — BotFather token
//	USER_CHAT_ID         — Telegram chat_id to send the reply to
//
// Optional env vars (with defaults for local dev):
//
//	REDIS_URL            — default: redis://localhost:6380/0
//	GATEWAY_ADDR         — default: 127.0.0.1:18080
//	ORCHESTRATOR_ADDR    — default: 127.0.0.1:18081
//
// Run from repo root:
//
//	set -a; source .env; set +a
//	go run ./go/cmd/demo-pipeline
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

const (
	adminToken     = "demo-admin-token"
	jwtSecret      = "demo-pipeline-jwt-secret"
	webhookSecret  = "demo-pipeline-secret-token"
	streamName     = "agent:runs"
	demoEmail      = "demo@platform.local"
	agentConfigYAML = `identity:
  name: "Demo"
  persona_file: SOUL.md
providers:
  - id: stub
    model: stub-model
skills: []
`
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// Binary helpers
// ---------------------------------------------------------------------------

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	// go/cmd/demo-pipeline/ → go/cmd/ → go/ → repo root (3 levels)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func goBin() string {
	candidates := []string{
		"go",
		filepath.Join(repoRoot(), ".tools", "go-install", "go", "bin", "go"),
	}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

// ensureBinary builds name if the binary is missing and Go is available.
func ensureBinary(name string) (string, error) {
	goDir := filepath.Join(repoRoot(), "go")
	bin := filepath.Join(goDir, name)
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}
	gb := goBin()
	if gb == "" {
		return "", fmt.Errorf("binary %s not found and no go compiler available", name)
	}
	log.Printf("building %s …", name)
	cmd := exec.Command(gb, "build", "-o", bin, "./cmd/"+name)
	cmd.Dir = goDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go build %s: %w", name, err)
	}
	return bin, nil
}

// ---------------------------------------------------------------------------
// Subprocess management
// ---------------------------------------------------------------------------

type proc struct {
	cmd  *exec.Cmd
	name string
}

func startService(name, bin string, env []string) (*proc, error) {
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	log.Printf("%s started (pid=%d)", name, cmd.Process.Pid)
	return &proc{cmd: cmd, name: name}, nil
}

func (p *proc) stop() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = p.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = p.cmd.Process.Kill()
		}
		log.Printf("%s stopped", p.name)
	}
}

func waitHealthy(ctx context.Context, url string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		resp, err := client.Get(url) //nolint:noctx
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %s", url)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------------------
// Gateway HTTP helpers
// ---------------------------------------------------------------------------

func doJSON(method, url string, headers map[string]string, body any) (map[string]any, int, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, resp.StatusCode, nil
}

// ---------------------------------------------------------------------------
// Tenant bootstrap
// ---------------------------------------------------------------------------

func bootstrapTenant(gwBase string) (userID, agentID, channelID, token, botID string, err error) {
	// 1. Signup → get user + JWT
	body, status, err := doJSON("POST", gwBase+"/auth/signup", nil, map[string]string{
		"email": demoEmail,
	})
	if err != nil {
		return "", "", "", "", "", fmt.Errorf("signup: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", "", "", "", "", fmt.Errorf("signup status %d: %v", status, body)
	}
	userID, _ = body["user_id"].(string)
	token, _ = body["token"].(string)
	log.Printf("user: id=%s email=%s", userID, demoEmail)

	// 2. Create agent
	body, status, err = doJSON("POST", gwBase+"/agents",
		map[string]string{"Authorization": "Bearer " + token},
		map[string]string{"name": "demo-stub-agent", "config_yaml": agentConfigYAML},
	)
	if err != nil {
		return "", "", "", "", "", fmt.Errorf("create agent: %w", err)
	}
	if status != http.StatusCreated {
		return "", "", "", "", "", fmt.Errorf("create agent status %d: %v", status, body)
	}
	agentID, _ = body["agent_id"].(string)
	log.Printf("agent: id=%s", agentID)

	// 3. Fetch bot id from Telegram
	tgToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if tgToken != "" {
		resp, err2 := http.Get("https://api.telegram.org/bot" + tgToken + "/getMe")
		if err2 == nil {
			defer resp.Body.Close()
			var me struct {
				Result struct {
					ID       int64  `json:"id"`
					Username string `json:"username"`
				} `json:"result"`
			}
			if jerr := json.NewDecoder(resp.Body).Decode(&me); jerr == nil {
				botID = strconv.FormatInt(me.Result.ID, 10)
				log.Printf("telegram bot: @%s id=%s", me.Result.Username, botID)
			}
		}
	}
	if botID == "" {
		botID = "0"
	}

	// 4. Register channel
	chatID := envOr("USER_CHAT_ID", "0")
	extID := "tg:" + botID + ":" + chatID
	body, status, err = doJSON("POST", gwBase+"/admin/channels",
		map[string]string{"Authorization": "Bearer " + adminToken},
		map[string]any{
			"user_id":      userID,
			"agent_id":     agentID,
			"channel_type": "telegram",
			"ext_id":       extID,
			"config":       map[string]string{"webhook_secret": webhookSecret},
		},
	)
	if err != nil {
		return "", "", "", "", "", fmt.Errorf("register channel: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", "", "", "", "", fmt.Errorf("register channel status %d: %v", status, body)
	}
	channelID, _ = body["channel_id"].(string)
	log.Printf("channel: id=%s ext=%s", channelID, extID)
	return userID, agentID, channelID, token, botID, nil
}

// ---------------------------------------------------------------------------
// Runtime daemon
// ---------------------------------------------------------------------------

type daemonResp struct {
	ID     any    `json:"id"`
	Result any    `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func dispatchJob(ctx context.Context, daemonBin string, fields map[string]string) (string, error) {
	workdir, err := os.MkdirTemp("", "demo_workspace_")
	if err != nil {
		return "", err
	}

	os.WriteFile(filepath.Join(workdir, "AGENTS.md"), []byte("# AGENTS\nDemo pipeline.\n"), 0o644)
	os.WriteFile(filepath.Join(workdir, "SOUL.md"), []byte("# SOUL\nFriendly demo bot.\n"), 0o644)
	agentYAML := filepath.Join(workdir, "agent.yaml")
	os.WriteFile(agentYAML, []byte(agentConfigYAML), 0o644)

	cmd := exec.CommandContext(ctx, daemonBin,
		"--config", agentYAML,
		"--workspace", workdir,
		"--register-stub",
		"--log-level", "ERROR",
	)
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("daemon stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("daemon stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("daemon start: %w", err)
	}
	defer func() {
		stdin.Close()
		cmd.Wait() //nolint:errcheck
	}()

	userText := fields["message"]
	if userText != "" {
		var msg map[string]any
		if json.Unmarshal([]byte(userText), &msg) == nil {
			if t, ok := msg["text"].(string); ok {
				userText = t
			}
		}
	}

	rpc := map[string]any{
		"jsonrpc": "2.0",
		"id":      "demo-1",
		"method":  "run",
		"params": map[string]any{
			"user_id":    fields["user_id"],
			"agent_id":   fields["agent_id"],
			"channel_id": fields["channel_id"],
			"thread_id":  fields["thread_id"],
			"run_id":     fields["run_id"],
			"message":    map[string]any{"text": userText, "images": []any{}},
		},
	}
	b, _ := json.Marshal(rpc)
	_, _ = fmt.Fprintf(stdin, "%s\n", b)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	if !scanner.Scan() {
		return "", errors.New("daemon closed stdout without a response")
	}
	var resp daemonResp
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return "", fmt.Errorf("parse daemon response: %w", err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("daemon error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	// Unwrap nested result envelope: {ok, result:{text,...}}
	text := extractText(resp.Result)
	return text, nil
}

func extractText(v any) string {
	if v == nil {
		return ""
	}
	if m, ok := v.(map[string]any); ok {
		if t, ok := m["text"].(string); ok {
			return t
		}
		if inner, ok := m["result"]; ok {
			return extractText(inner)
		}
	}
	return fmt.Sprintf("%v", v)
}

// ---------------------------------------------------------------------------
// Telegram send
// ---------------------------------------------------------------------------

func telegramSend(ctx context.Context, chatID, text string) error {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Printf("TELEGRAM_BOT_TOKEN unset — skipping real Telegram send")
		log.Printf("reply text: %s", text)
		return nil
	}
	body := map[string]string{
		"chat_id": chatID,
		"text":    "[pipeline] " + text,
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST",
		"https://api.telegram.org/bot"+token+"/sendMessage",
		bytes.NewReader(b),
	)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram sendMessage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram sendMessage %d: %s", resp.StatusCode, raw)
	}
	log.Printf("reply sent to Telegram chat %s", chatID)
	return nil
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	log.SetFlags(log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[demo] ")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("pipeline failed: %v", err)
	}
}

func run(ctx context.Context) error {
	redisURL := envOr("REDIS_URL", "redis://localhost:6380/0")
	gwAddr := envOr("GATEWAY_ADDR", "127.0.0.1:18080")
	orchAddr := envOr("ORCHESTRATOR_ADDR", "127.0.0.1:18081")
	gwBase := "http://" + gwAddr
	orchBase := "http://" + orchAddr
	chatID := envOr("USER_CHAT_ID", "0")
	tgToken := os.Getenv("TELEGRAM_BOT_TOKEN")

	// 1. Find / build binaries
	gwBin, err := ensureBinary("gateway")
	if err != nil {
		return fmt.Errorf("gateway binary: %w", err)
	}
	orchBin, err := ensureBinary("orchestrator")
	if err != nil {
		return fmt.Errorf("orchestrator binary: %w", err)
	}
	daemonBin, err := ensureBinary("runtime-daemon")
	if err != nil {
		return fmt.Errorf("runtime-daemon binary: %w", err)
	}

	// 2. Start orchestrator
	orchProc, err := startService("orchestrator", orchBin, []string{
		"ORCHESTRATOR_PORT=18081",
		"ORCHESTRATOR_HOST=127.0.0.1",
	})
	if err != nil {
		return err
	}
	defer orchProc.stop()

	// 3. Start gateway
	gwEnv := []string{
		"GATEWAY_HTTP_ADDR=" + gwAddr,
		"REDIS_URL=" + redisURL,
		"ADMIN_TOKEN=" + adminToken,
		"USER_JWT_SECRET=" + jwtSecret,
		"BYPASS_LOGIN=1",
		"ORCHESTRATOR_URL=" + orchBase,
		"DB_ENCRYPTION_KEY=" + strings.Repeat("0", 64),
		"AGENT_RUNS_STREAM=" + streamName,
	}
	if tgToken != "" {
		gwEnv = append(gwEnv,
			"TELEGRAM_BOT_TOKEN="+tgToken,
			"TELEGRAM_WEBHOOK_SECRET="+webhookSecret,
		)
	}
	gwProc, err := startService("gateway", gwBin, gwEnv)
	if err != nil {
		return err
	}
	defer gwProc.stop()

	// 4. Wait for both to be healthy
	healthCtx, hCancel := context.WithTimeout(ctx, 30*time.Second)
	defer hCancel()
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for _, pair := range [][2]string{{"orchestrator", orchBase + "/healthz"}, {"gateway", gwBase + "/healthz"}} {
		name, url := pair[0], pair[1]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := waitHealthy(healthCtx, url); err != nil {
				errCh <- fmt.Errorf("%s healthcheck: %w", name, err)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		return e
	}
	log.Printf("gateway + orchestrator healthy")

	// 5. Bootstrap tenant via gateway API
	_, _, channelID, _, botID, err := bootstrapTenant(gwBase)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	// 6. Capture Redis stream cursor before posting webhook
	rdbOpts, err := redis.ParseURL(redisURL)
	if err != nil {
		return fmt.Errorf("parse redis url: %w", err)
	}
	rdb := redis.NewClient(rdbOpts)
	defer rdb.Close()

	cursorBefore := "0-0"
	if latest, err := rdb.XRevRange(ctx, streamName, "+", "-").Result(); err == nil && len(latest) > 0 {
		cursorBefore = latest[0].ID
	}

	// 7. POST synthetic Telegram webhook
	ts := time.Now().Unix()
	chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
	webhookBody := map[string]any{
		"update_id": ts % 1_000_000_000,
		"message": map[string]any{
			"message_id": ts%1_000_000_000 + 1,
			"from": map[string]any{
				"id":         chatIDInt,
				"is_bot":     false,
				"first_name": "DemoUser",
			},
			"chat": map[string]any{
				"id":         chatIDInt,
				"type":       "private",
				"first_name": "DemoUser",
			},
			"date": ts,
			"text": "hello from full-pipeline demo",
		},
	}
	_, whStatus, err := doJSON("POST", gwBase+"/channels/telegram/webhook",
		map[string]string{
			"X-Telegram-Bot-Api-Secret-Token": webhookSecret,
		},
		webhookBody,
	)
	if err != nil {
		return fmt.Errorf("webhook POST: %w", err)
	}
	if whStatus != http.StatusOK {
		return fmt.Errorf("webhook returned %d", whStatus)
	}
	log.Printf("webhook accepted by gateway (status=%d)", whStatus)

	// 8. Wait for agent:runs entry
	log.Printf("waiting for agent:runs entry …")
	var fields map[string]string //nolint:prealloc
	pollCtx, pollCancel := context.WithTimeout(ctx, 15*time.Second)
	defer pollCancel()
	for {
		entries, err := rdb.XRange(ctx, streamName, "("+cursorBefore, "+").Result()
		if err != nil {
			return fmt.Errorf("redis xrange: %w", err)
		}
		if len(entries) > 0 {
			fields = make(map[string]string, len(entries[0].Values))
			for k, v := range entries[0].Values {
				fields[k] = fmt.Sprintf("%v", v)
			}
			log.Printf("stream entry %s (%d fields)", entries[0].ID, len(fields))
			break
		}
		select {
		case <-pollCtx.Done():
			return errors.New("no agent:runs entry within 15s")
		case <-time.After(100 * time.Millisecond):
		}
	}

	if fields["thread_id"] == "" {
		fields["thread_id"] = chatID
	}
	if fields["channel_id"] == "" {
		fields["channel_id"] = channelID
	}
	_ = botID

	// 9. Dispatch to runtime-daemon
	log.Printf("dispatching to runtime-daemon …")
	replyText, err := dispatchJob(ctx, daemonBin, fields)
	if err != nil {
		return fmt.Errorf("dispatch: %w", err)
	}
	log.Printf("daemon reply: %s", replyText)

	// 10. Send reply to Telegram
	if err := telegramSend(ctx, chatID, replyText); err != nil {
		return fmt.Errorf("telegram send: %w", err)
	}

	log.Printf("=== PIPELINE COMPLETE ===")
	log.Printf("Check your Telegram for the '[pipeline] ...' reply")
	return nil
}
