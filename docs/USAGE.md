# USAGE — CLI-First Multi-Provider Agent Platform

Everything you need to go from a fresh clone to a working Telegram bot driven by a real LLM.

---

## Prerequisites

| Tool | Version | Notes |
|---|---|---|
| Go | 1.22+ | Installed at `.tools/go-install/go/bin/go` in this repo |
| Docker | 20.10+ | For Postgres / Redis / MinIO / observability stack |
| Node.js + npm | 18+ | For the `claude` / `codex` / `gemini` provider CLIs |
| `curl` / `jq` | any | Optional — only for manual API calls in the examples |

---

## 0. Quick-start (3 commands)

```bash
# 1. Add Go to PATH (repo-local install)
export PATH="$PATH:$(pwd)/.tools/go-install/go/bin"

# 2. Build everything
cd go && go build ./...

# 3. Run all 815 tests
go test ./... -count=1 -timeout 120s
```

Expected output ends with a line per package, all `ok`. No Python needed.

---

## 1. Build the binaries

`go build ./...` produces 7 binaries in `go/`:

| Binary | Feature | Purpose |
|---|---|---|
| `gateway` | F06 | HTTP front door — webhooks, auth, agent/channel API |
| `orchestrator` | F01 | Daytona sandbox lifecycle (create, resume, hibernate) |
| `runtime-daemon` | F05 | JSON-RPC agent runtime — runs inside a sandbox |
| `registry` | F13 | Skill registry HTTP server |
| `platform` | F13 | `platform skills` CLI client |
| `migrate` | F12 | Applies Postgres schema migrations |
| `demo-pipeline` | demo | End-to-end pipeline demo |

```bash
export PATH="$PATH:$(pwd)/.tools/go-install/go/bin"
cd go
go build ./...
ls -lh gateway orchestrator runtime-daemon registry platform migrate demo-pipeline
```

---

## 2. Infra: Postgres + Redis

Both services must be running before you start the gateway or run migrations.

```bash
# Start Postgres on port 5433 (avoids conflict with any local postgres)
docker run -d --name pipe-postgres -p 5433:5432 \
  -e POSTGRES_USER=postgres \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=agent_platform \
  postgres:16

# Start Redis on port 6380
docker run -d --name pipe-redis -p 6380:6379 redis:7-alpine

# Wait for Postgres to be ready
until docker exec pipe-postgres pg_isready -U postgres >/dev/null 2>&1; do sleep 0.5; done
echo "Postgres ready"
```

### Apply DB migrations

```bash
DB_DSN="postgresql://postgres:postgres@localhost:5433/agent_platform" \
  ./migrate up
# Prints: migration 0001_init applied, 0002_sessions applied, 0003_skill_registry applied
```

---

## 3. Run the services

### Orchestrator

Manages Daytona sandboxes. Without `DAYTONA_API_KEY` it starts in **fake mode** — sandboxes are in-memory and all exec calls succeed instantly. This is the default for local dev.

```bash
ORCHESTRATOR_PORT=8081 go/orchestrator &
curl -s http://localhost:8081/healthz
# → {"status":"ok"}
```

With a real Daytona account:

```bash
DAYTONA_API_KEY=dtn_... ORCHESTRATOR_PORT=8081 go/orchestrator &
curl -s http://localhost:8081/healthz
# → {"status":"ok","daytona":"ok"}
```

### Gateway

The HTTP front door. Reads `REDIS_URL` (required), all other settings have defaults.

```bash
REDIS_URL=redis://localhost:6380/0 \
  ADMIN_TOKEN=devtoken \
  USER_JWT_SECRET=dev-secret \
  BYPASS_LOGIN=1 \
  ORCHESTRATOR_URL=http://localhost:8081 \
  DB_ENCRYPTION_KEY=$(go run ./cmd/gateway/main.go --print-key 2>/dev/null || head -c 32 /dev/urandom | xxd -p | tr -d '\n') \
  go/gateway &
curl -s http://localhost:8080/healthz
# → {"status":"ok"}
curl -s http://localhost:8080/readyz
# → {"status":"ok","redis":true,"db":true}
```

> **Tip:** `DB_ENCRYPTION_KEY` must be 64 hex characters (32 bytes). Generate one with:
> `python3 -c 'import secrets; print(secrets.token_hex(32))'`

---

## 4. API walkthrough

The gateway exposes a REST API. All routes below assume the gateway is on `localhost:8080`.

### 4.1 Create a user + get a JWT

```bash
curl -s -X POST http://localhost:8080/auth/signup \
  -H "Content-Type: application/json" \
  -d '{"email":"alice@example.com"}' | jq .
# Returns: {"user_id":"<uuid>","email":"alice@example.com","token":"eyJ...","created":true}
```

Save the token:

```bash
TOKEN=$(curl -s -X POST http://localhost:8080/auth/signup \
  -H "Content-Type: application/json" \
  -d '{"email":"alice@example.com"}' | jq -r .token)
```

Login with bypass (dev mode, requires `BYPASS_LOGIN=1`):

```bash
TOKEN=$(curl -s -X POST http://localhost:8080/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email":"alice@example.com","magic_code":"BYPASS"}' | jq -r .token)
```

### 4.2 Create an agent

```bash
AGENT_ID=$(curl -s -X POST http://localhost:8080/agents \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-bot",
    "config_yaml": "identity:\n  name: MyBot\n  persona_file: SOUL.md\nproviders:\n  - id: stub\n    model: stub-model\nskills: []\n"
  }' | jq -r .agent_id)
echo "Agent: $AGENT_ID"
```

### 4.3 Register a Telegram channel (admin route)

```bash
# Get bot_id from token: the prefix before ":"
BOT_ID=$(echo $TELEGRAM_BOT_TOKEN | cut -d: -f1)

curl -s -X POST http://localhost:8080/admin/channels \
  -H "Authorization: Bearer devtoken" \
  -H "Content-Type: application/json" \
  -d "{
    \"user_id\": \"<user_uuid>\",
    \"agent_id\": \"$AGENT_ID\",
    \"channel_type\": \"telegram\",
    \"ext_id\": \"tg:${BOT_ID}:${USER_CHAT_ID}\",
    \"config\": {\"webhook_secret\": \"my-webhook-secret\"}
  }" | jq .
```

### 4.4 List agents / channels

```bash
# List agents
curl -s http://localhost:8080/agents \
  -H "Authorization: Bearer $TOKEN" | jq .

# List channels
curl -s http://localhost:8080/channels \
  -H "Authorization: Bearer $TOKEN" | jq .
```

---

## 5. End-to-end demo pipeline

The `demo-pipeline` binary does all of steps 3–4 automatically, then sends a real message to Telegram.

### 5.1 Set up `.env`

```bash
cat > .env <<'EOF'
TELEGRAM_BOT_TOKEN=123456:ABCdef...  # from @BotFather
USER_CHAT_ID=7741148933              # your Telegram chat_id
DAYTONA_API_KEY=                     # optional — leave blank for fake-Daytona
EOF
```

To find your `USER_CHAT_ID`:
1. Open Telegram, send any message to your bot
2. Run:
   ```bash
   set -a; source .env; set +a
   curl -s "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/getUpdates" \
     | python3 -c 'import sys,json; print(json.load(sys.stdin)["result"][-1]["message"]["chat"]["id"])'
   ```

### 5.2 Start infra + run demo

```bash
set -a; source .env; set +a

# Start Postgres + Redis (if not running)
docker run -d --name pipe-postgres -p 5433:5432 \
  -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=agent_platform postgres:16
docker run -d --name pipe-redis -p 6380:6379 redis:7-alpine
until docker exec pipe-postgres pg_isready -U postgres >/dev/null 2>&1; do sleep 0.5; done
DB_DSN="postgresql://postgres:postgres@localhost:5433/agent_platform" go/migrate up

# Run the demo
REDIS_URL=redis://localhost:6380/0 \
  go run ./go/cmd/demo-pipeline
```

**What happens:**
1. Builds `gateway`, `orchestrator`, `runtime-daemon` if not already compiled
2. Starts them as child processes
3. Creates a demo user + agent + Telegram channel via the gateway API
4. POSTs a synthetic Telegram webhook to the gateway
5. Reads the `agent:runs` Redis stream entry
6. Spawns `runtime-daemon --register-stub`, sends a JSON-RPC `run` call
7. Posts the reply to your Telegram chat

**Expected Telegram message:** `[pipeline] [stub echo] hello from full-pipeline demo`

---

## 6. Real LLM providers

### 6.1 Claude (Anthropic)

```bash
# Install CLI (one-time)
mkdir -p .tools && cd .tools && npm init -y >/dev/null
npm install @anthropic-ai/claude-code
cd ..

# Login (opens browser)
.tools/node_modules/.bin/claude auth login --claudeai

# Verify
.tools/node_modules/.bin/claude auth status
# Should print: Logged in as ...
```

Use in `agent.yaml`:

```yaml
providers:
  - id: claude-cli
    model: claude-haiku-4-5   # cheapest; change to claude-opus-4-5 for best
```

### 6.2 Codex (OpenAI)

```bash
npm install @openai/codex          # in .tools/
.tools/node_modules/.bin/codex login

# agent.yaml
providers:
  - id: codex-cli
    model: gpt-4o
```

### 6.3 Gemini (Google)

```bash
npm install @google/gemini-cli     # in .tools/
.tools/node_modules/.bin/gemini    # first launch triggers OAuth → /quit when done

# agent.yaml
providers:
  - id: google-gemini-cli
    model: gemini-2.5-pro
```

### 6.4 Multi-provider fallback (F16)

```yaml
# agent.yaml
providers:
  - id: claude-cli
    model: claude-haiku-4-5
  - id: codex-cli
    model: gpt-4o
    fallback_only: true
  - id: google-gemini-cli
    model: gemini-2.5-pro
    fallback_only: true
```

When the primary returns `auth_expired`, `rate_limit`, or `transient` the daemon automatically tries the next provider.

---

## 7. runtime-daemon directly

You can talk to the daemon without the full stack — useful for testing agent behavior.

```bash
cd go

# Create a workspace
mkdir -p /tmp/my-workspace
cat > /tmp/my-workspace/SOUL.md <<'EOF'
You are a helpful assistant.
EOF
cat > /tmp/agent.yaml <<'EOF'
identity:
  name: "MyAgent"
  persona_file: SOUL.md
providers:
  - id: stub
    model: stub-model
skills: []
EOF

# Start the daemon (reads JSON-RPC from stdin, writes to stdout)
./runtime-daemon --config /tmp/agent.yaml --workspace /tmp/my-workspace --register-stub &
DAEMON_PID=$!

# Send a health check
echo '{"jsonrpc":"2.0","id":"1","method":"health","params":{}}' | nc -q1 localhost 0
# Or pipe directly:
echo '{"jsonrpc":"2.0","id":"1","method":"health","params":{}}' \
  | ./runtime-daemon --config /tmp/agent.yaml --workspace /tmp/my-workspace --register-stub

# Send a run request (stdin → stdout)
echo '{"jsonrpc":"2.0","id":"2","method":"run","params":{"user_id":"u1","agent_id":"a1","channel_id":"c1","thread_id":"123","run_id":"r1","message":{"text":"hello","images":[]}}}' \
  | ./runtime-daemon --config /tmp/agent.yaml --workspace /tmp/my-workspace --register-stub
# → {"jsonrpc":"2.0","id":"2","result":{"ok":true,"result":{"text":"[stub echo] hello"}}}
```

With a real Claude backend:

```bash
cat > /tmp/agent.yaml <<'EOF'
identity:
  name: "MyAgent"
  persona_file: SOUL.md
providers:
  - id: claude-cli
    model: claude-haiku-4-5
skills: []
EOF

echo '{"jsonrpc":"2.0","id":"1","method":"run","params":{"user_id":"u1","agent_id":"a1","channel_id":"c1","thread_id":"123","run_id":"r1","message":{"text":"What is 2+2?","images":[]}}}' \
  | ./runtime-daemon --config /tmp/agent.yaml --workspace /tmp/my-workspace
# → {"jsonrpc":"2.0","id":"1","result":{"ok":true,"result":{"text":"4"}}}
```

---

## 8. Skill registry

### 8.1 Start the registry

```bash
cd go
REGISTRY_PORT=8090 \
  REGISTRY_DSN=postgres://postgres:postgres@localhost:5433/agent_platform \
  REGISTRY_BLOB_DIR=/tmp/registry-blobs \
  ./registry &
curl -s http://localhost:8090/healthz
# → {"status":"ok"}
```

Without a DSN it uses an in-memory store (no persistence across restarts):

```bash
REGISTRY_PORT=8090 ./registry &
```

### 8.2 Search + install skills

```bash
# Search the registry
./platform skills search "web-search"

# Get skill info
./platform skills info web-search

# Install a skill (downloads + verifies + extracts)
./platform skills install web-search

# List installed skills
./platform skills list
```

Default install directory: `~/.platform/skills/`

### 8.3 Publish a skill (developer)

```bash
# Publish a tarball with a bearer token
curl -s -X POST http://localhost:8090/skills/my-skill/versions \
  -H "Authorization: Bearer test-publisher-bearer-token" \
  -F "tarball=@my-skill.tar.gz" \
  -F "signature=<ed25519-sig-hex>" | jq .
```

---

## 9. Daytona sandboxes

The orchestrator talks to Daytona Cloud to create per-user sandboxes. Each sandbox has persistent volumes at:

- `/home/user/.claude` — Claude OAuth state
- `/home/user/.codex` — Codex OAuth state
- `/home/user/.gemini` — Gemini OAuth state
- `/home/user/workspace` — agent workspace

### 9.1 Provision a sandbox via HTTP

```bash
# Provision or resume sandbox for a user
curl -s -X POST http://localhost:8081/sandboxes \
  -H "Content-Type: application/json" \
  -d '{"user_id":"alice"}' | jq .
# → {"id":"<sandbox-id>","user_id":"alice","state":"running"}

# Hibernate it
curl -s -X POST http://localhost:8081/sandboxes/<id>/hibernate | jq .

# Resume it
curl -s -X POST http://localhost:8081/sandboxes/<id>/resume | jq .

# Run a command inside
curl -s -X POST http://localhost:8081/sandboxes/<id>/exec \
  -H "Content-Type: application/json" \
  -d '{"command":["echo","hello from sandbox"]}' | jq .
```

### 9.2 OPS: open a shell in a sandbox

```bash
ORCHESTRATOR_URL=http://localhost:8081 USER_ID=alice bash scripts/sandbox-shell.sh
```

---

## 10. Observability (OTEL / Grafana)

```bash
# Start the LGTM stack (Loki + Grafana + Tempo + Prometheus + OTEL collector)
docker compose -f infra/observability/docker-compose.observability.yml up -d

# Open Grafana at http://localhost:3000 (admin / admin)
# Three dashboards are pre-loaded: Gateway, Runtime, Orchestrator

# Point Go services at the collector
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317

# Re-run any binary — spans + metrics appear in Grafana
REDIS_URL=redis://localhost:6380/0 ADMIN_TOKEN=devtoken ... go/gateway
```

---

## 11. Docker Compose (all services)

```bash
# Required env
export DAYTONA_API_KEY=dtn_...
export TELEGRAM_BOT_TOKEN=123456:ABC...
export TELEGRAM_WEBHOOK_SECRET=my-webhook-secret
export ADMIN_TOKEN=devtoken
export USER_JWT_SECRET=dev-secret
export DB_ENCRYPTION_KEY=$(python3 -c 'import secrets; print(secrets.token_hex(32))')

# Build + start everything (Postgres, Redis, MinIO, orchestrator, gateway, registry)
docker compose -f infra/docker/docker-compose.dev.yml up --build

# Check health
curl -s http://localhost:8080/healthz  # gateway
curl -s http://localhost:8081/healthz  # orchestrator
curl -s http://localhost:8090/healthz  # registry
```

Port map:

| Service | Host port |
|---|---|
| gateway | 8080 |
| orchestrator | 8081 |
| registry | 8090 |
| Postgres | 5432 |
| Redis | 6379 |
| MinIO S3 API | 9000 |
| MinIO console | 9001 |

---

## 12. Environment variable reference

| Variable | Default | Used by | Notes |
|---|---|---|---|
| `DAYTONA_API_KEY` | _(empty)_ | orchestrator | Empty → fake in-memory mode |
| `DAYTONA_API_URL` | Daytona default | orchestrator | Override for self-hosted |
| `DAYTONA_TARGET` | _(empty)_ | orchestrator | Region / target |
| `SANDBOX_IMAGE` | `ubuntu:24.04` | orchestrator | Container image for new sandboxes |
| `ORCHESTRATOR_PORT` | `8081` | orchestrator | HTTP bind port |
| `ORCHESTRATOR_HOST` | `0.0.0.0` | orchestrator | HTTP bind host |
| `REDIS_URL` | `redis://localhost:6379/0` | gateway | Required |
| `POSTGRES_DSN` | `postgresql://postgres:postgres@localhost:5432/...` | gateway | DB connection |
| `DB_DSN` | _(none)_ | migrate | Required for `migrate up` |
| `DB_ENCRYPTION_KEY` | _(none)_ | gateway | 32-byte hex AES-GCM key |
| `ADMIN_TOKEN` | _(none)_ | gateway | Bearer for `/admin/*` |
| `USER_JWT_SECRET` | _(none)_ | gateway | HS256 JWT signing secret |
| `BYPASS_LOGIN` | `0` | gateway | `1` → `/auth/login` accepts `magic_code: "BYPASS"` |
| `TELEGRAM_BOT_TOKEN` | _(none)_ | gateway | Auto-registers Telegram adapter |
| `TELEGRAM_WEBHOOK_SECRET` | _(none)_ | gateway | Required when bot token is set |
| `GATEWAY_HTTP_ADDR` | `:8080` | gateway | HTTP bind addr |
| `ORCHESTRATOR_URL` | `http://localhost:8081` | gateway | Orchestrator base URL |
| `REGISTRY_DSN` | _(none)_ | registry | Empty → in-memory store |
| `REGISTRY_BLOB_DIR` | _(none)_ | registry | Empty → in-memory blobs |
| `REGISTRY_HOST` | `127.0.0.1` | registry | HTTP bind host |
| `REGISTRY_PORT` | `8090` | registry | HTTP bind port |
| `REGISTRY_VERIFIER` | `inprocess` | registry | `inprocess` \| `cosign` \| `always-accept` |
| `PLATFORM_REGISTRY_URL` | `http://127.0.0.1:8090` | platform CLI | Registry base URL |
| `PLATFORM_SKILLS_DIR` | `~/.platform/skills` | platform CLI | Skill install root |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | _(none)_ | all | Opt-in OTEL export |

---

## 13. Cleanup

```bash
# Stop background services
pkill -f "go/gateway" 2>/dev/null
pkill -f "go/orchestrator" 2>/dev/null
pkill -f "go/runtime-daemon" 2>/dev/null
pkill -f "go/registry" 2>/dev/null

# Stop Docker containers
docker rm -f pipe-postgres pipe-redis 2>/dev/null

# Stop observability stack
docker compose -f infra/observability/docker-compose.observability.yml down 2>/dev/null

# Stop full compose
docker compose -f infra/docker/docker-compose.dev.yml down 2>/dev/null
```

---

## 14. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `go: command not found` | Go not on PATH | `export PATH=$PATH:$(pwd)/.tools/go-install/go/bin` |
| `REDIS_URL required` | Gateway started without Redis URL | Set `REDIS_URL` env var |
| `redis ping failed` | Redis not running | Start Redis container (§2) |
| `./migrate up: dial error` | Postgres not healthy yet | `docker exec pipe-postgres pg_isready -U postgres` |
| `no agent:runs entry within 15s` | Gateway didn't route the webhook | Check the channel registration — `ext_id` must match `tg:<bot_id>:<chat_id>` |
| `webhook returned 401` | Webhook secret mismatch | `X-Telegram-Bot-Api-Secret-Token` header must match `TELEGRAM_WEBHOOK_SECRET` |
| `webhook returned 404` | Telegram adapter not registered | Set `TELEGRAM_BOT_TOKEN` + `TELEGRAM_WEBHOOK_SECRET` before starting gateway |
| `daemon reply: (empty)` | Stub backend active; no real LLM | Login Claude (§6.1) and change `agent.yaml` to `id: claude-cli` |
| `auth expired` from Claude | OAuth token stale | `.tools/node_modules/.bin/claude auth login --claudeai` |
| `DB_ENCRYPTION_KEY` invalid | Wrong length | Must be exactly 64 hex chars (32 bytes) |
| `gateway readyz db=false` | Postgres unreachable | Set `POSTGRES_DSN` and check the DSN is correct |
