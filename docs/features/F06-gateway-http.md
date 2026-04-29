# F06 — Gateway HTTP Service

**Phase:** 0 | **Wave:** 0.A | **Dependencies:** F12 (Protocols)

## Goal

The single ingress for the platform: receives channel webhooks, verifies auth, deduplicates, routes to the right user/agent, enqueues an `agent:runs` job, and returns 200.

## Scope (in)

- FastAPI app with routes:
  - `POST /channels/{type}/webhook` — channel-agnostic webhook endpoint
  - `GET /healthz`, `GET /readyz` (Redis + DB ping)
  - `POST /admin/sandboxes` (admin-token gated)
- `ChannelAdapter` Protocol (verify_signature, parse_incoming, send_outgoing).
- `channel_registry` module — channel adapters self-register.
- Idempotency cache via Redis SET NX with configurable TTL.
- Redis Streams producer for `agent:runs` (frozen `AgentRunJob` schema).
- Routing via `ChannelsRepo.lookup_routing(type, ext_id)` Protocol.
- DAL injection via `create_app(channels_repo=...)`.

## Scope (out)

- Channel-specific code (F07/F08 own that).
- DB writes (only reads channel routing — F12 / consumer handles the rest).
- Authentication for end-users (Phase 3).

## Deliverables

```
services/gateway/
├── pyproject.toml
├── README.md
└── src/gateway/
    ├── __init__.py
    ├── app.py                  # create_app()
    ├── auth.py                 # admin-token guard
    ├── config.py               # GatewayConfig
    ├── idempotency.py
    ├── queue.py                # AgentRunJob (FROZEN), AgentRunQueue
    ├── channel_registry.py
    ├── orchestrator_client.py  # OrchestratorClient Protocol + HTTP impl
    ├── dal.py                  # ChannelsRepo Protocol + InMemoryChannelsRepo
    ├── channels/
    │   ├── __init__.py
    │   └── base.py             # ChannelAdapter Protocol + NormalizedMessage
    └── routes/
        ├── __init__.py
        ├── health.py
        ├── webhooks.py
        └── admin.py

services/gateway/tests/
├── conftest.py                 # fakeredis, in-memory repos
├── test_idempotency.py
├── test_queue.py
├── test_webhooks.py
└── test_admin.py
```

## Frozen contract

```python
@dataclass(frozen=True)
class AgentRunJob:
    run_id: str
    user_id: str
    agent_id: str
    channel_id: str
    thread_id: str
    message: str       # JSON-encoded NormalizedMessage payload
    received_at: str   # ISO-8601 UTC
```

Stream: `agent:runs`, group: `orchestrator`.

## Acceptance criteria

1. `pytest services/gateway/tests/` passes (no live Redis/Postgres).
2. Duplicate webhook within TTL → 200 OK both times, exactly 1 stream entry.
3. Bad signature → 401 (verified for telegram + zalo).
4. Admin endpoints: missing token → 401, bad token → 403, fail-closed when admin token unconfigured (503).
5. `/healthz` always 200; `/readyz` 200 only when Redis + DB reachable.
6. Webhook stream entry contains the full `AgentRunJob` payload.

## Reference implementations

- `~/Workspace/open-source/openclaw/src/gateway/hooks.ts` — idempotency
- `~/Workspace/open-source/openclaw/docs/concepts/architecture.md` — gateway shape
