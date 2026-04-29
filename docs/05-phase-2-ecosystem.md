# 05 — Phase 2: Ecosystem (MVP-2)

**Duration:** 4 weeks | **Goal:** make the platform operable at scale and ship the skill marketplace.

## Definition of done

1. A skill author can `platform skills publish ./trend-analysis` against F13's registry. Another user runs `platform skills install trend-analysis` inside their sandbox and F09 picks it up via the `managed` precedence tier.
2. Every component (gateway, orchestrator, runtime) emits OTEL spans + metrics that land in the LGTM stack at `infra/observability/`. Three Grafana dashboards are pre-loaded: `agent-runs`, `sandbox-pool`, `provider-health`.
3. F15's idle hibernate, top-N warm pool, presence prewarm, and force-recreate health probes are wired into the orchestrator's HTTP API.
4. F16's provider fallback chain triggers automatically when the primary provider returns `auth_expired` / `rate_limit` / `transient`.

## Features in scope

| Feature | Owner | Notes |
|---|---|---|
| F13 — Skill registry + CLI | Wave 2.A | FastAPI service + content-addressed blob store + sigstore-compatible signing |
| F14 — OTEL telemetry | Wave 2.A | shared `packages/telemetry` + LGTM compose stack |
| F15 — Sandbox autoscale | Wave 2.A | extends F01 |
| F16 — Provider fallback chain | Wave 2.B | extends F05's agent loop, uses F14's `HealthRegistry` |
| Wave 2.C | load + chaos tests | requires real Postgres + Redis + Daytona |

## Out of scope

- Cross-region routing (Phase 3)
- Multi-orchestrator-node sticky routing (Phase 3)
- Skill marketplace billing / revenue share (post-MVP)
- Daytona-backed `DaemonSpawner` (Phase 3 — daemon currently runs as local subprocess in tests)

## Risks

| Risk | Mitigation |
|---|---|
| OTEL global state issues across services | F14 enforces "first-call wins" `setup.init` per process; tests use `force_reset=True` |
| Skill registry blob storage bloat | Content-addressed deduplication; lifecycle policy on S3 (Phase 3) |
| Warm pool cost | Conservative N=100 default; admin endpoint to manually flush |
| Fallback chain confuses the user when a turn switches mid-thread to a different provider with no shared session | UX copy in user docs; telemetry surfaces `fallback_used: true` so dashboards can show it |
