# F01 — Sandbox Orchestrator (Daytona wrapper)

**Phase:** 0 | **Wave:** 0.A | **Dependencies:** F12 schema (interfaces only)

## Goal

Wrap the Daytona SDK behind a narrow interface so the rest of the platform can ask for a per-user sandbox without depending on Daytona internals.

## Scope (in)

- One sandbox per `user_id`, persistent — `create` / `get_or_resume` / `hibernate` / `exec` / `start_daemon`.
- Persistent volumes mounted at `/home/user/.codex`, `/home/user/.gemini`, `/home/user/.claude`, `/home/user/workspace`.
- HTTP API on port 8081: `POST /sandboxes`, `GET /healthz`.
- Live mode (with `DAYTONA_API_KEY`) backed by `LiveDaytonaClient`; in-memory fake mode for unit tests.
- `start_daemon` opens a stdio JSON-RPC channel to the runtime daemon (F05) so the orchestrator can drive runs.

## Scope (out)

- Daytona-backed `DaemonSpawner` (production daemon-in-sandbox) — Phase 3.
- Autoscale, hibernate timer, warm pool — F15.
- Stream consumer that reads `agent:runs` from Redis — separate concern; the orchestrator exposes lifecycle, the consumer (a future service or the gateway) drives it.

## Deliverables

```
services/orchestrator/
├── pyproject.toml
├── README.md
├── src/orchestrator/
│   ├── __init__.py
│   ├── config.py                 # OrchestratorConfig + load_config()
│   ├── volumes.py                # frozen mount-path constants
│   ├── exec.py                   # ExecResult, DaemonHandle, DaemonTransport
│   ├── daytona_client.py         # DaytonaClient Protocol + LiveDaytonaClient
│   ├── _fake_daytona.py          # in-memory fake for unit tests
│   ├── sandbox.py                # Orchestrator class
│   ├── api.py                    # FastAPI router
│   └── main.py                   # uvicorn entry point
└── tests/
    ├── conftest.py
    ├── test_daytona_client.py
    ├── test_sandbox.py
    ├── test_exec.py
    └── fixtures/echo_daemon.py
```

## Interface contract

```python
class Orchestrator:
    async def create(self, user_id: str) -> Sandbox: ...
    async def get_or_resume(self, user_id: str) -> Sandbox: ...
    async def hibernate(self, sandbox_id: str) -> None: ...
    async def exec(self, sandbox_id, cmd, *, env=None, cwd=None,
                   timeout_s=60, stdin=None) -> ExecResult: ...
    async def start_daemon(self, sandbox_id, cmd, env=None) -> DaemonHandle: ...
```

## Acceptance criteria

1. `pytest services/orchestrator/tests/` passes (mocked Daytona).
2. `DAYTONA_API_KEY=<staging>` `pytest --integration` passes (live).
3. Hibernate then resume preserves volume content (verified by file roundtrip in volume).
4. `start_daemon` JSON-RPC roundtrip with the `echo_daemon.py` fixture returns a response within 1s.
5. `docker-compose -f infra/docker/docker-compose.dev.yml up orchestrator` → `curl :8081/healthz` returns 200.
6. F01 spike notes for cold-start, OQ-4 auth UX in `docs/spikes/F01-daytona-coldstart.md`.

## Reference implementations

- `~/Workspace/open-source/daytona/test_daytona.py` — canonical SDK usage
- `~/Workspace/open-source/daytona/estimate_daytona_cost.py` — cost model

## Out-of-band notes

- Daytona SDK ≥ 0.169 uses snake_case pydantic field names (`volume_id`, `mount_path`); previous versions used camelCase. Wrapper code uses snake_case.
- The orchestrator's daemon spawner is pluggable via the `DaemonSpawner` Protocol — production deployments swap in a Daytona-session-backed spawner; tests use `LocalSubprocessDaemonSpawner`.
