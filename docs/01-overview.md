# 01 — Overview & Principles

## Vision

A platform that lets end-users compose **agents** (persona + skills + channels) and run them through **provider CLIs** that the user has authenticated with their own subscriptions. The platform supplies orchestration, sandbox isolation, skill marketplace, channel adapters, telemetry, and scaling — but **never pays for provider tokens**.

## Core principles

1. **Token cost = $0 platform-side.** The platform exec's user-owned CLIs (`codex`, `gemini`, `claude`) inside the user's sandbox. Provider subscription costs stay with the user.
2. **CLI-first.** No SDK/API path in the MVP. CLI backends — modeled directly after OpenClaw's `cliBackends` (`~/Workspace/open-source/openclaw/docs/gateway/cli-backends.md`) — are the only execution path.
3. **One sandbox per user, persistent.** Each user gets a long-lived isolated container (Daytona-managed) with persistent volumes for CLI auth + workspace + skills. Hibernate when idle, resume on incoming message.
4. **Agent runtime = Google ADK.** Python ADK runs inside the sandbox and drives the loop: plan → tool/skill → observe → respond. ADK shells out to CLI subprocess for LLM turns.
5. **Skills are the unit of monetization.** Folder-based skills (Anthropic Skills schema) with optional MCP server. Marketplace + signing + versioning.
6. **MCP loopback bridge** (per OpenClaw `bundleMcp: true`) gives CLI backends access to platform tools without leaking them globally.
7. **Channels are pluggable.** Telegram + Zalo for MVP; same plugin contract supports Discord, Slack, WhatsApp later.

## What the platform owns vs. what the user owns

| Component | Owner | Notes |
|---|---|---|
| Provider tokens / subscription | **User** | Codex Plus, Gemini, Claude Pro |
| CLI auth state (`~/.codex`, `~/.gemini`, `~/.claude`) | **User**, stored inside their sandbox | Persistent volume, backed up by platform |
| Sandbox compute | **Platform** (passed through to user as billing) | Daytona ~ $0.07/hour idle-hibernated |
| Agent definitions | **User**, stored in platform DB | Persona, channel mappings, skill list |
| Skill catalog | **Platform marketplace** + user-uploaded | Signed, versioned, scoped |
| Channel credentials (Telegram bot token, Zalo OA secret) | **User**, encrypted in platform DB | Webhooks dispatched by platform |
| Telemetry, audit log, billing | **Platform** | OTEL, Prometheus, Loki |

## Storage strategy per provider

Each provider CLI keeps its own state directory inside the sandbox. Platform-level metadata (session IDs across channels, agent config) lives in the central DB.

| Provider | CLI binary | State path (sandbox) | Session resume | Stream format | Notes |
|---|---|---|---|---|---|
| OpenAI | `codex` | `~/.codex/` | `codex exec resume <session_id>` (text-only resume; JSONL only on initial run) | JSONL via `--json` | Sandbox flag: `workspace-write`. No `--append-system-prompt` — must use `-c model_instructions_file=<file>` |
| Google | `gemini` | `~/.gemini/` | `gemini --resume <session_id>` | JSON (`--output-format json`) | Reply at `response`, usage at `stats`. `imageArg: "@"`, scope: workspace |
| Anthropic | `claude` | `~/.claude/` (projects, plugins, sessions) | `claude -p` (oneshot) or `--continue` | stream-json compatible JSONL | Supports `--plugin-dir` to inject filtered skills |

### Storage rules in sandbox

1. **Three persistent volumes**, mounted independently, snapshot on hibernate:
   - `/home/user/.codex` — Codex auth + cache
   - `/home/user/.gemini` — Gemini auth + cache
   - `/home/user/.claude` — Claude auth + projects + plugins
2. **Workspace volume separate**: `/home/user/workspace` for agent file output. Wipeable without touching auth.
3. **Skills volume read-only overlay**: `/home/user/skills` is mounted from the platform skill registry via overlay-fs so updates do not require sandbox restart.
4. **Platform DB** (Postgres) stores `(user_id, channel_id, thread_id) → {codex_sid, gemini_sid, claude_sid}` so resume works across cross-device sessions and after sandbox restart.
5. **Auth rotation invalidates session cache.** When the user re-logs into a CLI (token rotates), platform drops stored session IDs for that provider — same behavior OpenClaw documents.

## Cost model

Per-user steady-state cost (target):
- Sandbox compute (hibernated 90% of day): ~$0.10–0.30 / day active, ~$0.01 / day idle
- Storage (3 × 1 GB volumes + workspace): ~$0.001 / day
- Network egress: negligible for chat workloads
- **Provider tokens: $0** (user pays via their own subscription)

Pricing model (tentative, validated in Phase 2):
- Free tier: 1 sandbox, 5 free skills, 1 channel, 1 hour active/day
- Pro: unlimited sandboxes, all marketplace skills, multi-channel, $X/month
- Skill marketplace: free + paid skills, revenue share with skill authors

## Reference implementations to study

| Repo | Path | What to learn |
|---|---|---|
| OpenClaw | `~/Workspace/open-source/openclaw/docs/gateway/cli-backends.md` | CLI backend protocol, session handling, MCP bundling |
| OpenClaw | `~/Workspace/open-source/openclaw/docs/concepts/agent.md` | Agent workspace + bootstrap files (`AGENTS.md`, `SOUL.md`) |
| OpenClaw | `~/Workspace/open-source/openclaw/docs/concepts/architecture.md` | Gateway WebSocket protocol |
| OpenClaw | `~/Workspace/open-source/openclaw/docs/channels/telegram.md` + `zalo.md` | Channel adapter pattern |
| OpenClaw | `~/Workspace/open-source/openclaw/docs/cli/skills.md` | Skill CLI + ClawHub registry pattern |
| OpenClaw | `~/Workspace/open-source/openclaw/docs/plugins/sdk-overview.md` | Plugin contract for providers + channels |
| Daytona | `~/Workspace/open-source/daytona/test_daytona.py` | Sandbox SDK basics |
| Daytona | `~/Workspace/open-source/daytona/estimate_daytona_cost.py` | Cost model |
