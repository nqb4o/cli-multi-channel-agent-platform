# Spike — F01 Daytona cold-start + auth bootstrap

**Status:** partial — completed where possible without live operator interaction.

## Spike goals (from F01 brief)

1. Measure cold-start latency for a fresh sandbox (first `create`).
2. Measure resume latency from hibernate.
3. Decide on CLI auth bootstrap UX (OQ-4) — device-code, noVNC, or token-paste.
4. Validate `start_daemon` JSON-RPC roundtrip against a real subprocess.

## Findings

### #1 + #2 — Latency

Measured against Daytona staging + the live `LiveDaytonaClient`:

| Operation | Latency |
|---|---|
| First `create` (incl. 4-volume provision) | 30–60 s |
| `get_or_resume` cache hit (running) | 0.4 s |
| `hibernate` | ~2 s |
| `resume` after hibernate | 2–5 s (within ADR-006 spec) |
| `exec` first roundtrip | ~23 s (incl. toolbox bootstrap) |
| `exec` follow-up | ~1 s |

The 30–60 s first-create cost is dominated by volume provisioning. Subsequent users on the same image hit the warm path (~10 s).

### #3 — CLI auth bootstrap (OQ-4)

**Recommendation:** device-code flow where supported (Codex `codex login --device-auth`, Gemini OAuth device, Claude `auth login`), fallback to admin-paste.

Operator runs `claude auth login --claudeai` on a host with a browser. Auth files in `~/.claude/` are then either:
- (production target) `scp`'d into the sandbox's `~/.claude` volume on first user onboarding, OR
- (current dev) reused from the operator's local machine because the daemon runs locally.

noVNC inside the sandbox is heavier than necessary for MVP and was discarded.

### #4 — Daemon RPC

Validated end-to-end via `services/orchestrator/tests/fixtures/echo_daemon.py` over a local subprocess. JSON-RPC 2.0 framing roundtrips cleanly. The orchestrator's `DaemonSpawner` Protocol is the swap point for production where the spawner pipes stdio through Daytona's session API.

## Open items

- Daytona-backed `DaemonSpawner` is real engineering work, deferred to Phase 3.
- A live re-run of #1/#2 inside CI requires Daytona credentials and was scoped out.
- OQ-4 final pick remains "device-code where supported"; needs operator UX validation.
