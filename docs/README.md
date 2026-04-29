# Agent Platform

A CLI-first, multi-provider agent platform built in Go. Users compose conversational agents from a persona, a skill set, and one or more chat channels. Each agent runs inside an isolated [Daytona](https://www.daytona.io/) sandbox and talks to an LLM through the user's own CLI subscription — the platform never holds provider tokens.

---

## How it works

```
User message (Telegram / Zalo)
  └─▶ Gateway (HTTP)          verify, deduplicate, enqueue
        └─▶ Orchestrator      resume or create Daytona sandbox (~2–5 s)
              └─▶ Runtime     load agent.yaml, build prompt, run CLI turn
                    └─▶ claude / codex / gemini  (CLI subprocess in sandbox)
                          └─▶ reply delivered back to the channel
```

- **No tokens stored server-side.** The LLM API key lives in the user's CLI session inside their sandbox.
- **Provider-agnostic.** Swap between Claude, Codex, and Gemini in `agent.yaml`, or chain them as a fallback.
- **Skill system.** Attach reusable skills (MCP tools) to an agent via a versioned registry.
- **Sandboxed per user.** Each user gets a dedicated Daytona sandbox with persistent home directories (`~/.claude`, `~/.codex`, `~/.gemini`, `~/workspace`).

---

## Features

| # | Feature | What it does |
|---|---|---|
| F01 | Sandbox orchestrator | Daytona sandbox lifecycle — create, resume, hibernate, warm-pool |
| F02–F04 | CLI backends | Codex, Gemini, Claude adapters (subprocess + JSON-RPC) |
| F05 | Runtime daemon | Agent loop, prompt assembly, session continuity |
| F06 | Gateway | HTTP API — auth, webhooks, idempotency, Redis job queue |
| F07 | Telegram channel | Bot API webhook adapter |
| F08 | Zalo channel | Zalo OA webhook adapter |
| F09 | Skill loader | Schema v1 skill loading and MCP config generation |
| F10 | Seed skill set | Built-in starter skills |
| F11 | MCP loopback bridge | Per-session HTTP server injecting skill tools into the CLI |
| F12 | Session persistence | Postgres-backed sessions with AES-GCM encrypted secrets |
| F13 | Skill registry | Publish, search, and install skills; `platform skills` CLI |
| F14 | Observability | OpenTelemetry traces + metrics; Grafana/Tempo/Loki stack |
| F15 | Sandbox pool autoscale | Warm-pool pre-provisioning and auto-stop |
| F16 | Provider fallback chain | Ordered fallback across providers in `agent.yaml` |

---

## Quick start

### Prerequisites

- Go 1.22+ (a repo-local install is at `.tools/go-install/`)
- Docker (for Postgres, Redis, and optional observability stack)
- Node.js 18+ (for `claude` / `codex` / `gemini` provider CLIs)

### Build

```bash
export PATH="$PATH:$(pwd)/.tools/go-install/go/bin"
cd go && go build ./...
```

This produces 7 binaries in `go/`: `gateway`, `orchestrator`, `runtime-daemon`, `registry`, `platform`, `migrate`, `demo-pipeline`.

### Run infra

```bash
docker run -d --name pg   -p 5432:5432 -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=agent_platform postgres:16
docker run -d --name redis -p 6379:6379 redis:7-alpine
```

### Apply schema

```bash
DB_DSN="postgresql://postgres:postgres@localhost:5432/agent_platform" ./go/migrate up
```

### Start services

```bash
# Orchestrator (fake-Daytona mode when DAYTONA_API_KEY is unset)
ORCHESTRATOR_PORT=8081 ./go/orchestrator &

# Gateway
REDIS_URL=redis://localhost:6379/0 \
  ADMIN_TOKEN=devtoken \
  USER_JWT_SECRET=dev-secret \
  BYPASS_LOGIN=1 \
  POSTGRES_DSN="postgresql://postgres:postgres@localhost:5432/agent_platform" \
  DB_ENCRYPTION_KEY=$(openssl rand -hex 32) \
  ORCHESTRATOR_URL=http://localhost:8081 \
  ./go/gateway &
```

### Run the end-to-end demo

```bash
# (optional) set TELEGRAM_BOT_TOKEN + USER_CHAT_ID in .env first
set -a; source .env; set +a
cd go && go run ./cmd/demo-pipeline
```

See [`docs/USAGE.md`](docs/USAGE.md) for the full setup guide including real LLM auth, Telegram webhook, skill publishing, and Docker Compose.

---

## Docker Compose

All services in one command:

```bash
export DAYTONA_API_KEY=<your-key>          # omit for fake/local mode
export DB_ENCRYPTION_KEY=$(openssl rand -hex 32)
docker compose -f infra/docker/docker-compose.dev.yml up --build
```

Services exposed:

| Service | Port | Notes |
|---|---|---|
| Gateway | 8080 | Main HTTP API |
| Orchestrator | 8081 | Internal — sandbox lifecycle |
| Registry | 8090 | Skill registry |
| Postgres | 5432 | |
| Redis | 6379 | |
| MinIO | 9000 / 9001 | Object storage for registry blobs |

---

## Adding a Telegram bot

1. Create a bot via [@BotFather](https://t.me/BotFather) and get the token.
2. Set env vars before starting the gateway:
   ```bash
   TELEGRAM_BOT_TOKEN=<token>
   TELEGRAM_WEBHOOK_SECRET=<random-string>
   ```
3. Register the webhook URL with Telegram:
   ```bash
   curl "https://api.telegram.org/bot<token>/setWebhook" \
     -d url=https://<your-domain>/channels/telegram/webhook \
     -d secret_token=<random-string>
   ```

---

## Using real LLM providers

```bash
# Claude
.tools/node_modules/.bin/claude auth login --claudeai

# Codex
.tools/node_modules/.bin/codex login

# Gemini
.tools/node_modules/.bin/gemini auth login
```

Set the provider in `agent.yaml`:

```yaml
providers:
  - id: claude-cli
    model: claude-haiku-4-5
  - id: gemini-cli
    model: gemini-2.0-flash
    fallback_only: true   # only used if claude-cli fails
```

---

## Observability

```bash
docker compose -f infra/observability/docker-compose.observability.yml up -d
```

Opens Grafana at `http://localhost:3000` with pre-built dashboards for traces (Tempo), logs (Loki), and metrics (Prometheus). Enable export from services via:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317
```

---

## Running tests

```bash
cd go && go test ./... -count=1 -timeout 120s
```

~815 tests across 25 packages, no external services required (orchestrator uses an in-memory fake).

---

## Project layout

```
go/                  Go module — all platform code
  cmd/               Binary entry points (7 binaries)
  internal/          Private packages per feature
  pkg/               Shared crypto + JSON-RPC utilities
  adapters/          Channel adapters (Telegram, Zalo)
  tests/e2e/         End-to-end smoke tests
infra/
  docker/            Dockerfiles + docker-compose.dev.yml
  observability/     LGTM stack (Grafana, Tempo, Loki, Prometheus)
docs/
  USAGE.md           Full setup guide
  features/F*.md     One design brief per feature
demo/                Demo instructions
scripts/             Ops tools (sandbox-shell.sh)
```

---

## License

See [LICENSE](LICENSE).
