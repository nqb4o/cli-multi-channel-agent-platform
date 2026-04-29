# Demo

End-to-end demo that exercises the full Go pipeline against real infrastructure.

## run_pipeline (Go)

The `demo-pipeline` binary is part of the Go module at `go/cmd/demo-pipeline`.

### Prerequisites

- Postgres running on `:5433` (or set `DB_DSN`)
- Redis running on `:6380` (or set `REDIS_URL`)
- `.env` with `TELEGRAM_BOT_TOKEN` and `USER_CHAT_ID`

### Run

```bash
set -a; source .env; set +a

# Build + run the Go demo (auto-builds gateway/orchestrator/runtime-daemon too)
cd go && go run ./cmd/demo-pipeline
```

**Expected output:**

```
[demo] gateway + orchestrator healthy
[demo] user: id=<uuid> email=demo@platform.local
[demo] agent: id=<uuid>
[demo] channel: id=<uuid>
[demo] webhook accepted by gateway (status=200)
[demo] waiting for agent:runs entry …
[demo] stream entry 1234567890-0 (8 fields)
[demo] dispatching to runtime-daemon …
[demo] daemon reply: [stub echo] hello from full-pipeline demo
[demo] reply sent to Telegram chat <chat_id>
[demo] === PIPELINE COMPLETE ===
```

The bot sends `[pipeline] [stub echo] hello from full-pipeline demo` to your Telegram.

### Real LLM

To use the real Claude backend instead of the stub, run:

```bash
# Login Claude first
.tools/node_modules/.bin/claude auth login --claudeai

# Then edit go/cmd/demo-pipeline/main.go:
# Replace "--register-stub" with the claude-cli provider wiring
# (change agentConfigYAML to use id: claude-cli, model: claude-haiku-4-5)
```

### Skill publish + install

Use the Go `registry` and `platform` binaries:

```bash
cd go

# Start registry
REGISTRY_PORT=8090 REGISTRY_DSN=postgres://postgres:postgres@localhost:5433/agent_platform \
  REGISTRY_BLOB_DIR=/tmp/registry-blobs ./registry &

# Use platform CLI to search/install skills
./platform skills search "web-search"
./platform skills install web-search
```
