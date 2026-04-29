# 03 — Phase 0: Foundation (MVP-0)

**Duration:** 4 weeks | **Goal:** smallest end-to-end slice that proves the architecture.

## Definition of done

A user can:

1. Be provisioned a Daytona sandbox via the orchestrator (real Daytona staging).
2. Send a message to a Telegram bot whose webhook hits the gateway.
3. The gateway routes the run via Redis Streams to the orchestrator.
4. The runtime daemon starts inside the sandbox, dispatches the turn through the **Codex CLI** backend.
5. The reply text is posted back to Telegram.
6. Session state (`cli_session_id`) is persisted in Postgres so a follow-up message resumes the same Codex session.

No skills, no MCP, no Gemini/Claude, no Zalo. The single "happy path" is the deliverable.

## Features in scope

| Feature | Owner | Notes |
|---|---|---|
| F01 — Sandbox Orchestrator | Wave 0.A | Daytona SDK wrapper, lifecycle, exec, daemon spawn |
| F06 — Gateway HTTP | Wave 0.A | FastAPI, channel webhook, Redis Streams producer |
| F12 — Persistence | Wave 0.A | Postgres schema, DAL Protocols, AES-GCM crypto |
| F02 — Codex CLI backend | Wave 0.B | Defines `CliBackend` ABC + Codex impl |
| F05 — ADK runtime daemon | Wave 0.B | JSON-RPC daemon, agent loop, bootstrap files |
| F07 — Telegram channel | Wave 0.B | Webhook + outbound, F06 plugin |

## Repository layout (after Phase 0)

```
.
├── services/
│   ├── gateway/                            # F06
│   │   └── src/gateway/{app,queue,idempotency,channel_registry,…}
│   ├── orchestrator/                       # F01
│   │   └── src/orchestrator/{sandbox,daytona_client,exec,…}
│   └── runtime/                            # F05 + F02
│       └── src/runtime/{daemon,agent_loop,bootstrap,cli_backends/{base,codex}}
├── adapters/channels/telegram/             # F07
├── packages/persistence/                   # F12
├── infra/docker/                           # docker-compose, Dockerfiles
└── tests/e2e/                              # Wave 0.C smoke
```

## Wave 0.C — End-to-end smoke

After F02/F05/F07 land, an E2E suite under `tests/e2e/` verifies the full chain (faked Daytona, fake codex shell script, fakeredis, in-memory repos):

- S1: happy path → reply delivered
- S2: idempotency (same `update_id` twice → 1 run)
- S3: bad signature → 401
- S4: daemon `health` RPC < 100 ms
- S5: cold session resume — second turn uses session id from first

## Out of scope (deferred to later phases)

- Gemini, Claude (Phase 1)
- Zalo channel (Phase 1)
- Skills / MCP loopback (Phase 1)
- Skill registry, OTEL pipeline, autoscale, fallback chain (Phase 2)
- Real Daytona-backed daemon spawner (Phase 3)
- Multi-tenant signup UI / auth (Phase 3)

## Risks

| Risk | Mitigation |
|---|---|
| Daytona pricing or quota surprises | F01 spike measures cold-start + cost early |
| Codex CLI auth UX in a fresh sandbox | OQ-4 — recommended device-code flow, fallback to noVNC |
| Redis Streams + idempotency edge cases | F06 unit tests cover the dedup + payload contract |
| ADK fits the CLI subprocess model | ADR-003 spike inside F05 — wrapper is a no-op if ADK refuses |
