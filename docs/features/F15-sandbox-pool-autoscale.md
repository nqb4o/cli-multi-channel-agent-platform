# F15 — Sandbox Pool Autoscale + Hibernate

**Phase:** 2 | **Wave:** 2.A | **Dependencies:** F01

## Goal

Extend the orchestrator (F01) with smart lifecycle management: idle hibernation, warm-pool prewarming for top users, and graceful eviction.

## Scope (in)

- **Idle hibernate policy:** sandbox without activity for ≥ 5 minutes → hibernate. Configurable per-tier (free: 2 min, pro: 15 min).
- **Warm pool:** keep the top-N most-active users' sandboxes resumed at all times to avoid cold-start. N defaults to 100. List computed from runs in last 7 days.
- **Pre-resume on presence signal:** when a channel adapter receives a typing indicator or "user opened chat" event, trigger a prewarm if the sandbox is hibernated. Reduces user-perceived latency.
- **Eviction policy:** if the orchestrator node hits its concurrent-active-sandbox limit (default 100), evict the least-recently-used non-warm-pool sandbox to hibernate.
- **Single-flight resume:** if multiple messages arrive for a hibernated sandbox simultaneously, only one resume operation runs; others wait.
- **Health probes:** every 30s, ping the runtime daemon's `health` RPC for active sandboxes; flag stuck ones for force-recreate.
- **Metrics:** sandbox-hours active, sandbox-hours hibernated, cold-start histogram (already in F14, F15 ensures the data is correct).

## Scope (out)

- Cross-region routing (Phase 3).
- Multi-orchestrator-node sticky routing (separate concern; F15 implements at single-node level, multi-node uses consistent hashing in a future story).
- Billing integration (separate post-MVP track).

## Deliverables

```
services/orchestrator/src/orchestrator/
├── pool.py                       # active-set tracking, eviction
├── hibernate.py                  # idle timer, hibernate scheduler
├── warm_pool.py                  # top-N tracker, prewarm scheduler
├── presence.py                   # presence-signal endpoint + prewarm trigger
├── single_flight.py              # async single-flight helper
└── health_probe.py               # periodic health check loop

services/orchestrator/tests/
├── test_pool.py
├── test_hibernate.py
├── test_warm_pool.py
├── test_presence.py
└── test_health_probe.py
```

## Interface contract

```python
# services/orchestrator/src/orchestrator/pool.py

class SandboxPool:
    async def get_or_resume(self, user_id: str) -> Sandbox:
        """Single-flight; resumes from hibernate if needed; updates LRU."""

    async def maybe_evict(self) -> None:
        """If at capacity, hibernate LRU non-warm-pool sandbox."""

# services/orchestrator/src/orchestrator/warm_pool.py

class WarmPoolManager:
    def __init__(self, top_n: int, refresh_interval: timedelta): ...

    async def refresh(self) -> None:
        """Recompute top-N from runs table; prewarm new entries, hibernate evictees."""
```

## Acceptance criteria

1. `pytest services/orchestrator/tests/` passes.
2. A sandbox idle for ≥ 5 min is hibernated within 30s of crossing the threshold.
3. Top-N user list correctly reflects last-7-day activity from `runs` table.
4. Warm-pool sandboxes survive idle hibernation; non-warm sandboxes hibernate normally.
5. Single-flight: 10 concurrent `get_or_resume(user_id)` calls for a hibernated sandbox issue exactly 1 resume operation.
6. Presence prewarm: a `POST /sandboxes/{user_id}/prewarm` call resumes a hibernated sandbox.
7. Health probe detects a stuck daemon (test fixture: daemon not responding to RPC) and force-recreates within the configured grace period.
8. Load test in Phase 2: cold-start p95 with warm pool < 1.5s for top-100 users; without warm pool < 5s.

## Reference implementations

- F01 (`services/orchestrator/src/orchestrator/sandbox.py`) is the foundation; F15 extends, does not replace.
- OpenClaw's queue + retry concepts: `~/Workspace/open-source/openclaw/docs/concepts/queue.md` and `retry.md`.

## Out-of-band notes

- Hibernate timer per sandbox lives in process memory plus a Postgres `last_active_at` column. On orchestrator restart, replay state from DB.
- Warm-pool refresh runs every 1 hour. Manual refresh trigger via admin endpoint for debugging.
- F16 (fallback chain) does not affect this feature directly, but a fallback-triggering provider error should NOT count as user activity for hibernate-timer purposes — only successful turns reset the timer.
