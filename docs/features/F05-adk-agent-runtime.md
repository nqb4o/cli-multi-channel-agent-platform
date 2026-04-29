# F05 — ADK Agent Runtime daemon

**Phase:** 0 | **Wave:** 0.B | **Dependencies:** F01, F12 (Protocols only)

## Goal

A long-running daemon spawned per-sandbox that:
- speaks JSON-RPC 2.0 over stdio,
- parses `agent.yaml`,
- injects bootstrap files (AGENTS.md, SOUL.md, IDENTITY.md, USER.md) on first turn,
- looks up cached `cli_session_id` from F12,
- dispatches one CLI turn per `run` request,
- returns `{text, telemetry}`.

## Scope (in)

- JSON-RPC dispatch: `run` / `health` / `shutdown` methods.
- Stdio framing — line-delimited JSON-RPC.
- Concurrent in-flight runs per daemon (one task per request).
- Drain-on-shutdown semantics (5 s grace).
- Agent loop integration with the `BackendRegistry` from F02.
- Bootstrap-file loading + system-prompt construction.
- ADR-003 ADK spike: harness toggle (`RUNTIME_USE_ADK=1`), fall back to bare CLI turn if ADK is unavailable.
- Per-process `_InMemorySessionsRepo` fallback so the daemon runs without F12 wired up.

## Scope (out)

- Real backend registration (F02/F03/F04 inject their own).
- Skill resolution beyond a stub (F09/F10).
- MCP loopback bridge (F11).
- Multi-provider fallback chain (F16).

## Deliverables

```
services/runtime/
├── pyproject.toml
├── README.md
└── src/runtime/
    ├── __init__.py                       # public API surface
    ├── daemon.py                         # JSON-RPC loop + main()
    ├── agent_loop.py                     # AgentLoop + RunRequest/Result
    ├── bootstrap.py                      # bootstrap file loader + system-prompt builder
    ├── config.py                         # AgentConfig + load_agent_config()
    ├── session_dal.py                    # SessionDal Protocol + in-memory impl
    ├── cli_backends/__init__.py          # registry (F02 owns base.py + codex.py)
    └── skills/__init__.py                # F09 stub (returns [])

services/runtime/tests/
├── conftest.py
├── test_daemon_rpc.py
├── test_agent_loop.py
├── test_bootstrap.py
└── fixtures/
    ├── agent.yaml
    └── workspace/
        ├── AGENTS.md
        ├── SOUL.md
        ├── IDENTITY.md
        └── USER.md
```

## Acceptance criteria

1. `pytest services/runtime/tests/` (excluding `cli_backends/`) passes.
2. `health` RPC returns version + ok within 100 ms.
3. `run` RPC with the stub backend returns expected text + telemetry.
4. Bootstrap files injected on first turn only; `initialized_at` recorded in DAL.
5. Live sandbox test (Wave 0.C) exercises the daemon end-to-end via F01's `start_daemon`.
6. `shutdown` RPC drains in-flight runs within 5 s.

## Reference implementations

- `~/Workspace/open-source/openclaw/src/agents/workspace.ts` — bootstrap-file loader
- `~/Workspace/open-source/openclaw/docs/concepts/agent-loop.md`
