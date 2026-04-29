# F16 — Provider Fallback Chain

**Phase:** 2 | **Wave:** 2.B | **Dependencies:** F02, F03, F04, F14

## Goal

When the primary provider fails (auth_expired, rate_limit, transient), automatically retry with the next provider in the agent's configured fallback chain — mirroring OpenClaw `agents.defaults.model.fallbacks`.

## Scope (in)

- Extend `agent.yaml` schema to allow ordered provider list with optional fallback flag:
  ```yaml
  providers:
    - id: anthropic-claude-cli
      model: claude-opus-4-6
    - id: codex-cli
      model: gpt-5
      fallback_only: true   # only used when others fail
    - id: google-gemini-cli
      model: gemini-2.5-pro
      fallback_only: true
  ```
- Failover logic in F05's `agent_loop.py`:
  - Try primary.
  - On `CliTurnError` with class in `{auth_expired, rate_limit, transient}`, advance to next provider.
  - On final provider failure, surface the error to the user (no infinite retries).
  - Never fall back on `unknown` — surface immediately (might indicate a bug).
- Per-provider session state remains isolated — falling back from Claude to Codex starts a fresh Codex session for that thread.
- Telemetry: every fallback emits a structured log + a metric `agent.fallback.total{from, to, reason}`. Original failure is **always** logged even when fallback succeeds.
- Per-provider circuit breaker: if a provider returns `auth_expired` or hits rate limit > N times in M minutes, mark it unhealthy for K minutes and skip it preemptively. Configurable thresholds.

## Scope (out)

- Cost-based routing (we do not pay tokens; not applicable).
- Latency-based routing (Phase 3 concern).
- Auto-retry within the same provider on transient errors — that is a provider-CLI concern, not failover.

## Deliverables

```
services/runtime/src/runtime/
├── fallback.py                # ProviderChain class + circuit breaker
└── agent_loop.py              # extended to use ProviderChain

services/runtime/tests/
├── test_fallback.py
└── fixtures/
    └── flaky_backends.py      # CliBackend implementations that fail on cue
```

## Interface contract

```python
# services/runtime/src/runtime/fallback.py

@dataclass
class ProviderEntry:
    id: str
    model: str | None
    fallback_only: bool = False

class ProviderChain:
    def __init__(self, entries: list[ProviderEntry], breaker: CircuitBreaker): ...

    async def turn(self, inp: CliTurnInput) -> CliTurnOutput | CliTurnError:
        """Walks the chain; returns first success or final aggregated error."""

class CircuitBreaker:
    def is_open(self, provider_id: str) -> bool: ...
    def record_success(self, provider_id: str) -> None: ...
    def record_failure(self, provider_id: str, error_class: ErrorClass) -> None: ...
```

## Acceptance criteria

1. `pytest services/runtime/tests/test_fallback.py` passes.
2. Two-provider chain: primary returns `rate_limit` → fallback returns success → final result is the fallback's, latency includes both attempts (logged), telemetry records both.
3. Circuit breaker opens after configurable failure count; subsequent requests skip the broken provider until the breaker half-opens.
4. `unknown` errors do **not** trigger fallback — they propagate immediately.
5. Live test (Phase 2 chaos suite): kill primary provider auth deliberately → agent run still succeeds via fallback within the SLO budget.
6. Original failure is always logged with `parent_error=true` attribute even when fallback succeeds.

## Reference implementations

- **OpenClaw fallback config** — `~/Workspace/open-source/openclaw/docs/gateway/cli-backends.md` lines 65–80 and `~/Workspace/open-source/openclaw/docs/concepts/model-failover.md`.

## Out-of-band notes

- Fallback chain is an **agent-level** config, not platform-level. Each agent decides its preferred order.
- Session state is not migrated across providers — that is by design (each provider has its own `cli_session_id`). On fallback, downstream provider starts fresh; UX may notice a slight loss of in-thread context. Document this clearly in user docs.
- Circuit breaker state is per-orchestrator-node in MVP; cross-node sharing is a Phase 3 concern (Redis-backed breaker).
