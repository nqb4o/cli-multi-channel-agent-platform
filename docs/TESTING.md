# Feature Testing Guide

Step-by-step verification of all 16 features (F01–F16) plus the end-to-end pipeline.

## Prerequisites

```bash
cd /path/to/demo-app-for-testing

# Add local Go to PATH if needed
export PATH="$PATH:$(pwd)/.tools/go-install/go/bin"

# Build all binaries
cd go && go build ./...

# Load environment (copy .env.example to .env and fill in secrets)
set -a; source .env; set +a
```

Required secrets in `.env`:
| Variable | Required for |
|---|---|
| `DAYTONA_API_KEY` | Live sandbox ops (omit → fake mode) |
| `POSTGRES_DSN` | All DB features |
| `REDIS_URL` | Gateway queue, idempotency |
| `ADMIN_TOKEN` | Admin endpoints |
| `USER_JWT_SECRET` | User auth |
| `BYPASS_LOGIN=1` | Dev login (skip email verification) |
| `TELEGRAM_BOT_TOKEN` | F07 end-to-end |
| `TELEGRAM_WEBHOOK_SECRET` | F07 end-to-end |

---

## Infrastructure

Start all backing services via Docker Compose:

```bash
docker compose -f infra/docker/docker-compose.dev.yml up -d
# starts: postgres (5433), redis (6380), minio (9000/9001), orchestrator (8081), gateway (8080), registry (8090)
```

Or run services individually (for development):

```bash
# Terminal 1 — Orchestrator
set -a; source .env; set +a
ORCHESTRATOR_PORT=8081 go/orchestrator

# Terminal 2 — Gateway
set -a; source .env; set +a
RUNTIME_DAEMON_BIN="$(pwd)/go/runtime-daemon" go/gateway

# Terminal 3 — Skill Registry (optional, only needed for F13)
REGISTRY_PORT=8090 go/registry
```

---

## F01 — Sandbox Orchestrator

The orchestrator provisions per-user Daytona sandboxes. With no `DAYTONA_API_KEY` it runs in fake mode (in-memory).

```bash
# Health check
curl http://localhost:8081/healthz
# → {"status":"ok"}

# Provision a sandbox for a user
curl -s -X POST http://localhost:8081/sandboxes \
  -H 'Content-Type: application/json' \
  -d '{"user_id":"alice"}' | python3 -m json.tool
# → {"id":"<sandbox-id>","state":"running","user_id":"alice"}

# Get sandbox
SBID=<sandbox-id>
curl -s http://localhost:8081/sandboxes/$SBID | python3 -m json.tool

# Exec inside sandbox
curl -s -X POST http://localhost:8081/sandboxes/$SBID/exec \
  -H 'Content-Type: application/json' \
  -d '{"cmd":["echo","hello world"],"timeout_s":5}' | python3 -m json.tool
# → {"stdout":"hello world\n","stderr":"","exit_code":0,"timed_out":false}

# Hibernate
curl -s -X POST http://localhost:8081/sandboxes/$SBID/hibernate
# → 204 No Content

# Resume
curl -s -X POST http://localhost:8081/sandboxes/$SBID/resume | python3 -m json.tool
# → {"state":"running",...}

# Destroy
curl -s -X DELETE http://localhost:8081/sandboxes/$SBID
# → 204 No Content
```

**Unit tests:**
```bash
cd go && go test ./internal/orchestrator/... -v -run TestOrchestrator
```

---

## F02–F04 — CLI Backends (Codex / Gemini / Claude)

Each backend wraps a local CLI subprocess. Test them via the runtime-daemon.

```bash
# Write a minimal agent.yaml
cat > /tmp/test-agent.yaml <<'EOF'
identity:
  name: TestBot
providers:
  - id: claude-cli
    model: claude-haiku-4-5-20251001
    system_prompt: "You are a helpful assistant."
skills: []
EOF

# Run runtime-daemon — it speaks line-delimited JSON-RPC on stdin/stdout
(echo '{"jsonrpc":"2.0","id":1,"method":"run","params":{"message":"Say hello in one word"}}'; sleep 5) \
  | go/runtime-daemon \
      --config /tmp/test-agent.yaml \
      --workspace /tmp/test-ws

# Expected: {"jsonrpc":"2.0","id":1,"result":{"text":"Hello!","..."},...}
```

For other providers swap `id: claude-cli` → `id: codex-cli` or `id: google-gemini-cli`.

**Unit tests:**
```bash
cd go && go test ./internal/clibackend/... -v
```

---

## F05 — Agent Runtime Daemon

Tests the full agent loop (session hydration → prompt building → backend call → session save).

```bash
# Using the demo-pipeline binary (sets everything up automatically)
set -a; source .env; set +a
cd go && go run ./cmd/demo-pipeline
# Runs a Claude turn end-to-end and prints the reply
```

**Unit tests:**
```bash
cd go && go test ./internal/runtime/... -v
```

---

## F06 — Gateway HTTP

The gateway provides auth, agent/channel management, the Telegram webhook, and the Redis queue consumer.

### Auth

```bash
GW=http://localhost:8080

# Signup (creates a new user in DB)
curl -s -X POST $GW/auth/signup \
  -H 'Content-Type: application/json' \
  -d '{"email":"alice@example.com","password":"secret123"}' | python3 -m json.tool

# Login with bypass (BYPASS_LOGIN=1 must be set)
TOKEN=$(curl -s -X POST $GW/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"alice@example.com","magic_code":"BYPASS"}' \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')
echo "Token: ${TOKEN:0:40}..."

# Who am I
curl -s $GW/auth/me -H "Authorization: Bearer $TOKEN" | python3 -m json.tool
```

### Agents

```bash
# Create an agent
AGENT=$(curl -s -X POST $GW/agents \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "MyBot",
    "config_yaml": "identity:\n  name: MyBot\nproviders:\n  - id: claude-cli\n    model: claude-haiku-4-5-20251001\n    system_prompt: \"You are helpful.\"\nskills: []\n"
  }')
echo $AGENT | python3 -m json.tool
AGENT_ID=$(echo $AGENT | python3 -c 'import sys,json; print(json.load(sys.stdin)["agent_id"])')

# List agents
curl -s $GW/agents -H "Authorization: Bearer $TOKEN" | python3 -m json.tool

# Get agent
curl -s $GW/agents/$AGENT_ID -H "Authorization: Bearer $TOKEN" | python3 -m json.tool

# Update agent
curl -s -X PATCH $GW/agents/$AGENT_ID \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"MyBotV2"}' | python3 -m json.tool

# Delete agent
# curl -s -X DELETE $GW/agents/$AGENT_ID -H "Authorization: Bearer $TOKEN"
```

### Channels

```bash
# Register a Telegram channel for a user (admin endpoint)
ADMIN_TOKEN=$(grep ADMIN_TOKEN .env | cut -d= -f2)
USER_ID=$(curl -s $GW/auth/me -H "Authorization: Bearer $TOKEN" | python3 -c 'import sys,json; print(json.load(sys.stdin)["user_id"])')

curl -s -X POST $GW/admin/channels \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{
    \"user_id\": \"$USER_ID\",
    \"agent_id\": \"$AGENT_ID\",
    \"channel_type\": \"telegram\",
    \"ext_id\": \"<telegram-chat-id>\"
  }" | python3 -m json.tool

# List channels
curl -s $GW/channels -H "Authorization: Bearer $TOKEN" | python3 -m json.tool
```

### Admin sandbox provisioning

```bash
curl -s -X POST $GW/admin/sandboxes \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"user_id\":\"$USER_ID\"}" | python3 -m json.tool
# → {"id":"...","state":"running","user_id":"..."}
```

**Unit tests:**
```bash
cd go && go test ./internal/gateway/... -v
```

---

## F07 — Telegram Channel

Requires a real Telegram bot (`TELEGRAM_BOT_TOKEN`, `TELEGRAM_WEBHOOK_SECRET`). Gateway auto-registers the webhook at startup.

```bash
# Verify the webhook is registered with Telegram
curl -s "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/getWebhookInfo" \
  | python3 -m json.tool

# Simulate an incoming Telegram message (signed webhook)
# Replace <secret> with TELEGRAM_WEBHOOK_SECRET value
curl -s -X POST $GW/channels/telegram/webhook \
  -H 'Content-Type: application/json' \
  -H "X-Telegram-Bot-Api-Secret-Token: <secret>" \
  -d '{
    "update_id":100,
    "message":{
      "message_id":1,
      "from":{"id":123,"first_name":"Test","username":"testuser"},
      "chat":{"id":"<telegram-chat-id>","type":"private"},
      "text":"Hello bot",
      "date":1700000000
    }
  }'
# → 200 OK (job enqueued; reply arrives in Telegram)
```

For the real end-to-end test: open Telegram, send a message to the bot directly, and watch for a Claude reply.

**Unit tests:**
```bash
cd go && go test ./adapters/telegram/... -v
```

---

## F08 — Zalo Channel

Requires Zalo OA credentials (`ZALO_APP_ID`, `ZALO_APP_SECRET`, `ZALO_OA_ACCESS_TOKEN`, `ZALO_OA_REFRESH_TOKEN`).

```bash
# Register Zalo channel via admin
curl -s -X POST $GW/admin/channels \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{
    \"user_id\": \"$USER_ID\",
    \"agent_id\": \"$AGENT_ID\",
    \"channel_type\": \"zalo\",
    \"ext_id\": \"<zalo-user-id>\"
  }" | python3 -m json.tool

# Simulate a Zalo webhook event (HMAC-signed)
curl -s -X POST $GW/channels/zalo/webhook \
  -H 'Content-Type: application/json' \
  -H "X-Zevent-Mac: <hmac-signature>" \
  -d '{"event_name":"user_send_text","message":{"text":"Hi bot","msg_id":"1"},"sender":{"id":"<zalo-user-id>"},"recipient":{"id":"<oa-id>"}}'
```

**Unit tests:**
```bash
cd go && go test ./adapters/zalo/... -v
```

---

## F09 — Skill Schema & Loader

Skills follow a frozen schema (version 1). Tested via unit tests — no service needed.

```bash
cd go && go test ./internal/skills/... -v
# Includes: TestSkillSchemaVersionTripwire (guards schema contract)
```

To load a skill manually:

```bash
# Check a skill file parses correctly
cat > /tmp/my-skill.yaml <<'EOF'
schema_version: 1
name: "example/hello"
version: "0.1.0"
description: "Says hello"
entrypoint:
  type: mcp_server
  command: ["node", "index.js"]
tools:
  - name: say_hello
    description: "Returns a greeting"
    input_schema:
      type: object
      properties:
        name: {type: string}
      required: [name]
EOF

# The loader validates the schema
cd go && go run -v ./internal/skills/ 2>&1 | head -5  # won't run, but import check works
```

---

## F10 — Warm Pool (Sandbox Pre-provisioning)

The orchestrator keeps a warm pool of pre-provisioned sandboxes for the most-active users.

```bash
# Check warm pool status
curl -s http://localhost:8081/admin/warm-pool/status | python3 -m json.tool
# → {"active":0,"capacity":3,"users":[]}

# Manually trigger a pool refresh
curl -s -X POST http://localhost:8081/admin/warm-pool/refresh | python3 -m json.tool

# Pre-warm a specific user
curl -s -X POST http://localhost:8081/sandboxes/alice/prewarm | python3 -m json.tool
```

Configure pool size via env: `ORCHESTRATOR_POOL_CAPACITY=5`.

**Unit tests:**
```bash
cd go && go test ./internal/orchestrator/... -v -run TestWarmPool
```

---

## F11 — MCP Loopback Bridge

The MCP bridge runs an in-process HTTP server that the CLI backend connects to, making platform skills available as MCP tools.

**Unit tests (no external service needed):**
```bash
cd go && go test ./internal/mcpbridge/... -v
```

In a real agent run the bridge starts automatically when the agent has skills with `type: mcp_server` entrypoints.

---

## F12 — Session Persistence

Agents store conversation session IDs in Postgres so multi-turn context is preserved.

```bash
# Run migrations (idempotent)
DB_DSN="postgresql://postgres:postgres@localhost:5433/agent_platform" \
  go/migrate up

# Check migration status
DB_DSN="postgresql://postgres:postgres@localhost:5433/agent_platform" \
  go/migrate status
# → [x] 0001_init
# → [x] 0002_indexes
# → [x] 0003_skill_registry

# Verify Postgres has the expected tables
psql "postgresql://postgres:postgres@localhost:5433/agent_platform" \
  -c '\dt'
# → users, agents, channels, sessions, skills, ...
```

**Unit tests (requires running Postgres on 5433):**
```bash
cd go && go test ./internal/persistence/... -v
```

---

## F13 — Skill Registry

A blob-store-backed skill catalog with search and versioning.

```bash
# Registry health
curl -s http://localhost:8090/healthz
# → {"status":"ok"}

# List all skills
curl -s http://localhost:8090/skills | python3 -m json.tool

# Search skills
curl -s "http://localhost:8090/skills?q=hello" | python3 -m json.tool

# Platform CLI: list skills
PLATFORM_REGISTRY_URL=http://localhost:8090 go/platform skills list

# Platform CLI: search
PLATFORM_REGISTRY_URL=http://localhost:8090 go/platform skills search hello

# Publish a skill (requires publisher auth token)
curl -s -X POST http://localhost:8090/skills \
  -H "Authorization: Bearer <publisher-token>" \
  -H 'Content-Type: application/yaml' \
  --data-binary @/tmp/my-skill.yaml | python3 -m json.tool

# Install a skill from registry into agent (platform CLI)
PLATFORM_REGISTRY_URL=http://localhost:8090 go/platform skills install example/hello
```

**Unit tests:**
```bash
cd go && go test ./internal/registry/... -v
```

---

## F14 — Telemetry (OTEL)

Traces, metrics, and logs export to a local OTEL collector when configured.

```bash
# Start the LGTM observability stack
docker compose -f infra/observability/docker-compose.observability.yml up -d
# starts: otel-collector (4317/4318), tempo, loki, prometheus, grafana (3000)

# Configure gateway to export
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_SERVICE_NAME=gateway
# Restart gateway — it will emit spans

# Open Grafana at http://localhost:3000 (admin/admin)
# → Explore → Tempo: find traces from service "gateway"
# → Explore → Loki: query {service="gateway"}
# → Dashboards: platform metrics (agent.run.duration_ms, etc.)
```

**Unit tests (no collector needed):**
```bash
cd go && go test ./internal/telemetry/... -v
```

---

## F15 — Sandbox Hibernate / Auto-stop

Sandboxes auto-hibernate after the configured idle window.

```bash
# Default auto-stop is 5 minutes. Override for faster testing:
ORCHESTRATOR_AUTO_STOP_M=1 go/orchestrator &

# Provision a sandbox
SBID=$(curl -s -X POST http://localhost:8081/sandboxes \
  -H 'Content-Type: application/json' \
  -d '{"user_id":"bob"}' | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')

# Wait 1+ minutes, then check state
sleep 70
curl -s http://localhost:8081/sandboxes/$SBID | python3 -m json.tool
# → {"state":"hibernated",...}

# Resume is transparent — next API call auto-restarts it
curl -s -X POST http://localhost:8081/sandboxes/$SBID/resume | python3 -m json.tool
# → {"state":"running",...}
```

Disable auto-hibernate: `ORCHESTRATOR_DISABLE_HIBERNATE=1`.

**Unit tests:**
```bash
cd go && go test ./internal/orchestrator/... -v -run TestHibernate
```

---

## F16 — Fallback Chain

When multiple providers are configured, a failed primary provider automatically falls back to the next.

```bash
# Write an agent.yaml with two providers; the second is the fallback
cat > /tmp/fallback-agent.yaml <<'EOF'
identity:
  name: FallbackBot
providers:
  - id: claude-cli
    model: claude-haiku-4-5-20251001
    system_prompt: "You are helpful."
  - id: codex-cli
    model: o4-mini
    system_prompt: "You are helpful."
    fallback_only: true
skills: []
EOF

# Run a turn — if claude-cli fails (no auth, network error) codex-cli takes over
(echo '{"jsonrpc":"2.0","id":1,"method":"run","params":{"message":"hi"}}'; sleep 10) \
  | go/runtime-daemon --config /tmp/fallback-agent.yaml --workspace /tmp/fb-ws
```

**Unit tests:**
```bash
cd go && go test ./internal/runtime/... -v -run TestFallback
```

---

## Interactive OAuth (Provider Auth)

Seeds CLI credentials into a user's Daytona sandbox without browser interaction.

```bash
TOKEN=$(curl -s -X POST $GW/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"alice@example.com","magic_code":"BYPASS"}' \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')

# 1. Export your local Claude credentials JSON
CREDS=$(cat ~/.claude/.credentials.json)

# 2. Seed them into the sandbox
curl -s -X POST "$GW/users/me/provider-auth/claude/credentials" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "$CREDS" | python3 -m json.tool
# → {"provider":"claude","status":"ok","written_to":"/home/user/.claude/.credentials.json",...}

# 3. Verify authentication
curl -s "$GW/users/me/provider-auth/claude/status" \
  -H "Authorization: Bearer $TOKEN" | python3 -m json.tool
# → {"authenticated":true,"provider":"claude",...}

# The same flow works for codex and gemini:
# POST /users/me/provider-auth/codex/credentials  (body: ~/.codex/auth.json content)
# POST /users/me/provider-auth/gemini/credentials (body: ~/.gemini/credentials.json content)
```

---

## End-to-End: Real Telegram → Claude Reply

This is the golden path through the entire system.

### Setup (one-time)

```bash
# 1. Start services
set -a; source .env; set +a
RUNTIME_DAEMON_BIN="$(pwd)/go/runtime-daemon" go/gateway &
go/orchestrator &

# 2. Get a JWT
TOKEN=$(curl -s -X POST http://localhost:8080/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"me@example.com","magic_code":"BYPASS"}' \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')
USER_ID=$(curl -s http://localhost:8080/auth/me -H "Authorization: Bearer $TOKEN" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["user_id"])')

# 3. Seed Claude credentials into sandbox
curl -s -X POST http://localhost:8080/users/me/provider-auth/claude/credentials \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "$(cat ~/.claude/.credentials.json)"

# 4. Create an agent
AGENT_ID=$(curl -s -X POST http://localhost:8080/agents \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "TelegramBot",
    "config_yaml": "identity:\n  name: TelegramBot\nproviders:\n  - id: claude-cli\n    model: claude-haiku-4-5-20251001\n    system_prompt: \"You are a friendly Telegram assistant.\"\nskills: []\n"
  }' | python3 -c 'import sys,json; print(json.load(sys.stdin)["agent_id"])')

# 5. Wire the agent to your Telegram chat ID
ADMIN_TOKEN=$(grep ADMIN_TOKEN .env | cut -d= -f2)
MY_CHAT_ID=$(grep USER_CHAT_ID .env | cut -d= -f2)
curl -s -X POST http://localhost:8080/admin/channels \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{
    \"user_id\": \"$USER_ID\",
    \"agent_id\": \"$AGENT_ID\",
    \"channel_type\": \"telegram\",
    \"ext_id\": \"$MY_CHAT_ID\"
  }"
```

### Test

Open Telegram, find the bot, send any message. Claude's reply should arrive within ~5–15 seconds (cold sandbox) or ~2–3 seconds (warm sandbox).

---

## Run All Tests

```bash
cd go && go test ./... -count=1 -timeout 120s
# ~815 tests, all should pass
```

Tests that require external services (Postgres, Redis, Daytona) are skipped when the service is unreachable. Run with the Docker Compose stack for full coverage.
