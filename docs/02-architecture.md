# 02 — System Architecture

## Component map

```
┌──────────────────────────────────────────────────────────────────────┐
│                       Channel Layer (per tenant)                      │
│   Telegram bot │ Zalo OA │ Discord │ WebChat │ … (plugin contract)   │
└──────────────────────────────┬───────────────────────────────────────┘
                               │  webhook / long-poll / WS
                               ▼
┌──────────────────────────────────────────────────────────────────────┐
│                   Gateway (stateless, scales horizontally)            │
│   - HTTP REST + WebSocket events                                      │
│   - Auth (JWT for users, signed webhooks per channel)                 │
│   - Idempotency cache (Redis)                                         │
│   - Channel router → resolve (channel_id → user_id, agent_id)         │
│   - Enqueue agent run job (Redis Streams or NATS JetStream)           │
│   - OTEL traces, structured audit log                                 │
└──────────────────────────────┬───────────────────────────────────────┘
                               │  job queue
                               ▼
┌──────────────────────────────────────────────────────────────────────┐
│                       Sandbox Orchestrator                            │
│   - Daytona-backed sandbox pool (SDK wrapper)                         │
│   - 1 long-lived sandbox per user (hibernate after ~5min idle)        │
│   - Persistent volumes: ~/.codex, ~/.gemini, ~/.claude, ~/workspace   │
│   - Read-only overlay: ~/skills (synced from registry)                │
│   - Cold start on resume: ~2-5s with hibernate                        │
│   - Health checks + zombie reaper                                     │
└──────────────────────────────┬───────────────────────────────────────┘
                               │  exec request (gRPC over Daytona API)
                               ▼
┌──────────────────────────────────────────────────────────────────────┐
│                  Per-User Sandbox (Daytona container)                 │
│ ┌──────────────────────────────────────────────────────────────────┐ │
│ │  ADK Agent Runtime (Python, long-running daemon inside sandbox)  │ │
│ │  - JSON-RPC over stdin/stdout to orchestrator                    │ │
│ │  - Loop: plan → tool/skill → CLI turn → observe → respond        │ │
│ │  - Bootstrap files: AGENTS.md, SOUL.md, IDENTITY.md, USER.md     │ │
│ │  - Skill resolver (workspace > personal > managed > bundled)     │ │
│ └────────┬──────────────────────────────────┬──────────────────────┘ │
│          │                                  │                         │
│ ┌────────▼─────────┐  ┌───────────────────▼──────────────────────┐  │
│ │  CLI Backends    │  │  Loopback MCP Bridge (HTTP, 127.0.0.1)   │  │
│ │  - codex exec    │◄►│  - exposes platform tools to CLIs         │  │
│ │  - gemini        │  │  - per-session OPENCLAW_MCP_TOKEN         │  │
│ │  - claude -p     │  │  - scope: (user, channel, session)        │  │
│ └────────┬─────────┘  └────────────────────┬──────────────────────┘  │
│          │                                  │                         │
│ ┌────────▼──────────────────────────────────▼──────────────────────┐ │
│ │  Skills filesystem (~/skills, RO overlay from registry)           │ │
│ │  trend-analysis/  regime-detection/  web-search/  summarize-url/ │ │
│ │  Each skill: SKILL.md + scripts/ + (optional) mcp_server.py      │ │
│ └───────────────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────────┘
                ▲                                    ▲
                │                                    │
   ┌────────────┴──────────┐         ┌──────────────┴────────────────┐
   │  Skill Registry       │         │  Telemetry Backend             │
   │  (REST + CLI client)  │         │  OTEL Collector → Tempo/Loki/  │
   │  - search/install     │         │            Prometheus           │
   │  - signing/version    │         │  Dashboards: latency, error    │
   │  - revenue tracking   │         │  rate, runs/min, sandbox-hours │
   └───────────────────────┘         └────────────────────────────────┘

   ┌───────────────────────────────────────────────────────────────────┐
   │  Control plane DB (Postgres)                                       │
   │  - users, agents, channels, sessions, skill installs, audit log   │
   └───────────────────────────────────────────────────────────────────┘
```

## Component boundaries

### Gateway (stateless service)
- **Owns:** HTTP/WS protocol, auth, channel routing, job enqueue, OTEL spans.
- **Does not own:** agent state, sandbox lifecycle, provider calls.
- **Scaling:** horizontal, behind LB. State in Redis + Postgres.

### Sandbox Orchestrator (stateful service)
- **Owns:** Daytona sandbox pool, lifecycle (create/hibernate/resume/destroy), volume mounts, exec API.
- **Does not own:** agent logic, channel routing.
- **Scaling:** horizontal with sticky routing per user (consistent hash on user_id).

### ADK Agent Runtime (per-sandbox daemon)
- **Owns:** agent loop, skill resolution, CLI subprocess management, MCP bridge lifecycle.
- **Does not own:** auth, sandbox provisioning, channel I/O.
- **Lifetime:** starts when sandbox resumes, exits when sandbox hibernates.

### CLI Backend Adapter (Python module inside ADK runtime)
- **Owns:** subprocess spawn, JSON/JSONL parse, session ID tracking, image arg handling.
- **One adapter per provider** (codex, gemini, claude). Common interface, provider-specific shims.
- **Pattern:** ports the design from `~/Workspace/open-source/openclaw/docs/gateway/cli-backends.md`.

### MCP Loopback Bridge
- **Owns:** HTTP server bound to `127.0.0.1:<random>` inside sandbox, per-session bearer token, tool dispatch.
- **Does not own:** which tools exist (gets a filtered list at session start).

### Channel Adapters (plugins)
- **Owns:** webhook/long-poll loop, signature verification, message normalization (incoming + outgoing).
- **One adapter per channel** (telegram, zalo). Common interface.

### Skill Registry (service)
- **Owns:** skill catalog, search, install, signing, version pinning.
- **Storage:** S3-compatible blob for skill tarballs + Postgres metadata.

## Data flow: incoming message → reply

```
1. Telegram → webhook → POST /channels/telegram/webhook  (Gateway)
2. Gateway verifies signature, looks up (channel_id → user_id, agent_id)
3. Gateway enqueues AgentRunJob{user_id, agent_id, message, thread_id}
4. Orchestrator dequeues, resolves user's sandbox
   - If hibernated: resume (2-5s)
   - If not yet provisioned: create + bootstrap volumes
5. Orchestrator exec's `agent_runtime serve` daemon if not already running
6. Orchestrator sends RPC: {method: "run", params: {message, thread_id, ...}}
7. ADK runtime:
   a. Loads agent config from /home/user/agent.yaml
   b. Looks up session IDs from RPC params (DB-backed)
   c. Resolves skill set
   d. Starts MCP loopback bridge (if any skill exposes MCP)
   e. Spawns CLI subprocess (e.g., codex exec --json --session <sid>)
   f. Streams JSONL, surfaces tool calls back to MCP bridge as needed
   g. On final reply: returns {text, new_session_id, telemetry}
8. Orchestrator → Gateway → Channel adapter sends reply to Telegram
9. Gateway updates DB with new session ID per provider
10. After idle timer: orchestrator hibernates sandbox
```

## Key non-functional requirements

| NFR | Target |
|---|---|
| Cold-start latency (resume from hibernate) | < 5s p95 |
| Reply latency (warm sandbox, no skill calls) | < 3s p95 (CLI-bound) |
| Concurrent active sandboxes per orchestrator node | 50–100 |
| Channel webhook ingest throughput | 1k msg/s per gateway pod |
| Sandbox cost at idle (hibernated) | < $0.001 / hour |
| OTEL trace coverage | 100% of agent runs, 100% of CLI calls |

## Failure modes & mitigations

| Failure | Mitigation |
|---|---|
| CLI auth expired mid-session | Detect from CLI error; notify user via channel + drop cached session ID |
| Provider primary fails | Fallback chain in agent config (Phase 2 — F16) |
| Sandbox stuck/zombie | Liveness probe → force-recreate; drop session cache |
| Skill MCP server crash | Bridge isolates errors to single tool call; agent gets explicit error |
| Webhook duplicate (Telegram retry) | Idempotency key in Redis (10 min TTL) |
| Provider rate limit / token exhausted | Surface to user via channel; do not retry blindly |
