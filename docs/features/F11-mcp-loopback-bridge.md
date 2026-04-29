# F11 — MCP Loopback Bridge

**Phase:** 1 | **Wave:** 1.B | **Dependencies:** F03, F04, F09

## Goal

Per-turn HTTP MCP server bound to `127.0.0.1` that exposes the resolved skill set's MCP tools to the active CLI provider, scoped to `(user, channel, session, agent)`.

## Scope (in)

- `McpBridgeManager.start(skills, scope, run_id)` → spawns the server, starts a per-skill stdio MCP child pool, returns an `McpBridge` handle.
- Per-session 64-char hex bearer token (32 random bytes). Token validated via `secrets.compare_digest`.
- Bind exclusively to `127.0.0.1` — `ValueError` on non-loopback host.
- Token plumbed to spawned CLIs via env var `OPENCLAW_MCP_TOKEN`. Provider config files reference `${OPENCLAW_MCP_TOKEN}` placeholder, not the literal token.
- Scope tuple: `(user_id, channel_id, session_id, agent_id)`.
- JSON-RPC dispatch: `initialize`, `tools/list`, `tools/call`. Tool name prefix convention: `<skill_slug>.<tool>`.
- Per-tool audit log: `(run_id, user_id, skill_slug, tool, args_hash, latency_ms, ok)`.
- `bridge.stop()` reaps all child processes within ~1 s.
- Per-provider config builders live in F09's `mcp_config_gen.py`; F11 owns the runtime that those configs point at.

## Scope (out)

- Skill MCP isolation (signed vs unsigned) — OQ-5, deferred.
- Cross-process audit shipping (Phase 2 — F14 wires it).

## Deliverables

```
services/runtime/src/runtime/cli_backends/
├── mcp_bridge.py               # McpBridge, McpBridgeManager, McpScope
└── README.md                   # sandbox-boot wiring guide

services/runtime/tests/cli_backends/
├── test_mcp_bridge.py
├── test_mcp_config_gen.py
└── fixtures/skills_with_mcp/
    ├── web-search/{SKILL.md, mcp_server.py}
    └── trend-analysis/{SKILL.md, mcp_server.py}
```

## Acceptance criteria

1. `pytest services/runtime/tests/cli_backends/test_mcp_bridge.py` passes.
2. Bridge starts with 2 fixture skills, exposes `web-search.search`, `trend-analysis.analyze_trend`, `trend-analysis.summary` at `http://127.0.0.1:<port>/mcp`.
3. Bearer-token mismatch + missing-bearer → 401.
4. End-to-end `tools/list` → `tools/call` over HTTP returns the skill's response.
5. `bridge.stop()` reaps child processes within 1 s.
6. Per-provider config shapes match ADR-004 / OpenClaw `createMcpLoopbackServerConfig` shape.
7. Live agent test (Phase 1 integration) — exercised with F05 daemon wiring.

## Reference implementations

- `~/Workspace/open-source/openclaw/src/gateway/mcp-http*.ts`
- `~/Workspace/open-source/openclaw/src/agents/pi-bundle-mcp-{runtime,materialize}.ts`
