# 07 — Decisions & Open Questions

## Architectural Decision Records (ADRs)

### ADR-001: CLI-first, no SDK proxy in MVP
**Status:** Accepted
**Context:** Multiple LLM providers (OpenAI, Google, Anthropic) and a strong constraint that the platform must not pay token costs.
**Decision:** All LLM turns go through user-installed CLIs (`codex`, `gemini`, `claude`) inside the user's sandbox. The user authenticates each CLI once with their own subscription. The platform never holds provider API keys.
**Consequences (+):** Zero token cost. Less compliance burden around provider keys. Reuses OpenClaw's well-tested CLI backend pattern.
**Consequences (−):** Slightly higher per-turn latency (subprocess overhead). Some providers' T&S around CLI-as-a-service is unclear (re-validate before GA — see open question OQ-1).

### ADR-002: One sandbox per user, persistent
**Status:** Accepted
**Context:** Each provider CLI keeps state in its own home dir; that state is per-identity and cannot be safely shared across users. A pooled sandbox would force re-login on every turn.
**Decision:** Provision one Daytona sandbox per user. Hibernate after 5min idle. Persistent volumes for `~/.codex`, `~/.gemini`, `~/.claude`, `~/workspace`.
**Consequences (+):** Auth is paid once per user. Resume is fast (~2-5s). Good isolation.
**Consequences (−):** Cost scales linearly with users. Cold start on resume adds latency. Need autoscale + warm pool for top users (Phase 2 — F15).

### ADR-003: Google ADK as agent runtime
**Status:** Accepted (revisit if ADK has gaps)
**Context:** Need a Python agent loop with tool calling, observability, multi-step reasoning support.
**Decision:** Use Google ADK as the agent runtime daemon inside each sandbox. ADK calls CLI subprocess for LLM turns rather than its native model SDKs.
**Consequences (+):** Mature loop. OTEL integration. Tool-calling abstractions.
**Consequences (−):** ADK is opinionated about model SDKs — we will be using it in an unusual configuration (CLI subprocess as model). Need to validate ADK's tool-calling integrates with MCP loopback bridge cleanly. Spike in F05.

### ADR-004: MCP loopback bridge for tool injection
**Status:** Accepted
**Context:** CLI backends do not natively expose platform tools. OpenClaw solved this with `bundleMcp: true` — a per-session HTTP MCP server bound to localhost.
**Decision:** Mirror that design. Per-session token in `OPENCLAW_MCP_TOKEN` env var. Tool scope limited to `(user, channel, session)`. Each provider injects via its native config (Codex inline `mcp_servers`, Claude `--plugin-dir`, Gemini settings file).
**Consequences (+):** Provider-agnostic tool exposure. Tight scope = good security default.
**Consequences (−):** Adds complexity to F11. Per-provider config differences are real engineering work.

### ADR-005: Postgres for control plane, Redis for queues
**Status:** Accepted
**Context:** Need durable transactional store + a fast queue with idempotency.
**Decision:** Postgres 16 for users/agents/channels/sessions/audit. Redis 7 Streams for the agent run job queue and idempotency cache.
**Consequences (+):** Standard, well-understood stack.
**Consequences (−):** Two stateful services to operate. Consider NATS JetStream as a single combined option in Phase 2 if ops cost is high.

### ADR-006: Daytona managed for sandbox
**Status:** Accepted (MVP); revisit at GA
**Context:** Need fast, cheap, isolated containers with persistent volumes and hibernate.
**Decision:** Daytona Cloud (managed) for MVP. Self-host k8s + Firecracker is the GA fallback if economics or compliance demand it.
**Consequences (+):** Time-to-market. Daytona's hibernate/resume is good (~2-5s). Per-hour cost is acceptable.
**Consequences (−):** Vendor dependency. If Daytona pricing or policy changes, we have a hard migration. Mitigate by keeping orchestrator wrapper interface narrow (F01) so swap is feasible.

### ADR-007: Skills are folder-based, registry is OCI-style
**Status:** Accepted
**Context:** Need a skill format that works across providers and ecosystems.
**Decision:** Skill = folder with `SKILL.md` (YAML frontmatter + markdown), `scripts/`, and optional `mcp_server.py`. Registry stores skills as content-addressed blobs (similar to OCI). Resolution order: workspace > project > personal > managed > bundled (mirrors OpenClaw).
**Consequences (+):** Compatible with Anthropic Skills, OpenClaw skills, agentskills.io conventions. Easy to publish + version + sign.
**Consequences (−):** Schema must be locked early (F09). Skill SDK to help authors comes later.

### ADR-008: Python everywhere for MVP-0
**Status:** Accepted (revisit at MVP-2 if Gateway throughput becomes a bottleneck)
**Context:** Team velocity matters more than per-pod throughput at MVP scale.
**Decision:** Gateway, Orchestrator, Runtime all in Python 3.11. FastAPI for HTTP services.
**Consequences (+):** One language, shared data models, faster iteration.
**Consequences (−):** Gateway throughput per pod is lower than a Go equivalent. Acceptable until > 10k concurrent users.

### ADR-009: Channel adapters as plugins, not built-in
**Status:** Accepted
**Context:** Channel set will grow (Discord, Slack, WhatsApp, …). Want clean extension points.
**Decision:** Each channel is a plugin implementing the `ChannelAdapter` interface. Telegram + Zalo ship in MVP. Plugin contract mirrors OpenClaw's plugin SDK.
**Consequences (+):** Adding channels does not touch the gateway core.
**Consequences (−):** Need a plugin SDK and good docs for third-party authors (Phase 3).

### ADR-010: Port (copy + adapt) from OpenClaw / Daytona where applicable
**Status:** Accepted
**Context:** OpenClaw already implements working code for the patterns we need (CLI backends, channel adapters, skill loader, bundle-MCP loopback, gateway architecture). Daytona's SDK is the canonical example for sandbox lifecycle. Reimplementing from scratch wastes time and risks subtle bugs.
**Decision:** Subagents are allowed and encouraged to port code from `~/Workspace/open-source/openclaw` and `~/Workspace/open-source/daytona` when it satisfies a feature's requirements. Porting means: search → adapt to our interfaces → translate (do not import OpenClaw types) → preserve attribution and license headers → copy the corresponding tests too. Full policy in `docs/README.md` § "Code reuse policy".
**Consequences (+):** Faster delivery. Battle-tested patterns. Less interface drift from the reference implementations we already chose to mirror.
**Consequences (−):** License compliance must be verified per copy. Risk of dragging in OpenClaw assumptions that do not fit our model — mitigated by the "translate, do not fork" rule.

---

## Open questions (need resolution before / during MVP)

### OQ-1: Provider T&S for CLI-as-service
**Why it matters:** Core business model assumes user CLI subscriptions can power platform-mediated chats.
**Status:** Partially resolved (OpenClaw note: Anthropic explicitly allows OpenClaw-style CLI usage). Need fresh confirmation from OpenAI and Google before GA.
**Action:** Legal review week 1 of Phase 0. Document position per provider in `docs/legal/provider-tos.md`.

### OQ-2: Sandbox cold-start UX
**Why it matters:** First message after hibernate has 2–5s extra latency. Chat UX expectations are < 3s.
**Options:**
1. Show "thinking" indicator in channel during resume.
2. Warm pool for top-N active users (Phase 2 — F15).
3. Pre-resume on user-presence signals (typing indicator, channel-open event).
**Action:** Validate p95 in F01 spike. Decide warm-pool size in Phase 2.

### OQ-3: Skill marketplace economics
**Status:** Deferred to post-MVP.
**Action:** Keep registry API capable of recording author + revenue-share metadata even though billing is not yet built.

### OQ-4: User onboarding for CLI auth
**Why it matters:** First-run UX requires logging into 1–3 CLIs inside a sandbox they cannot see.
**Options:**
1. **noVNC browser inside sandbox** — user does the OAuth dance via remote desktop. Good UX, heavier infra.
2. **Device-code flow** — user gets a code, types it on provider's site. Lighter, but not all CLIs support it.
3. **Token paste** — user runs `codex login` on their own machine, then we `scp` the auth dir into the sandbox. Brittle.
**Action:** Decide in F01 spike. Recommendation: device-code where supported (Codex, Gemini), noVNC fallback for Claude.

### OQ-5: Skill MCP isolation
**Why it matters:** A malicious or buggy skill MCP server runs inside the same sandbox and could exfiltrate data.
**Options:**
1. Run each skill MCP in a nested container (firejail or rootless docker).
2. Run skill MCP as an unprivileged user with seccomp.
3. Trust signed skills, audit unsigned.
**Action:** Decide in F11. Recommendation: signed skills get full sandbox access, unsigned/dev skills run in nested firejail.

### OQ-6: Cross-region sandbox routing
**Why it matters:** A user in Vietnam should not have their sandbox in us-east-1 (latency).
**Status:** Out of scope for MVP. Daytona regional placement is a Phase 3 concern.
**Action:** Document as known limitation.

### OQ-7: Multi-thread vs multi-channel session IDs
**Why it matters:** Session DB key is `(user, channel, thread, provider)`. Should one Telegram bot's sessions be shared across channels? Probably not, but worth confirming.
**Action:** Resolved in F12 — one session per `(user, channel, thread, provider)` quadruple. No sharing.

### OQ-8: Failed-CLI-auth recovery flow
**Why it matters:** When a user's `codex` token expires, current design tells them via the channel. Is that the right surface?
**Options:**
1. Reply in channel: "Your Codex login expired. Re-login at [link]."
2. Email notification.
3. Push notification (if mobile app exists — out of MVP).
**Action:** F02/F03/F04 should detect auth errors and emit a structured error code. Channel layer formats the user-facing message. Email later.

---

## Decisions still to be made (assigned to specific features)

| Feature | Decision needed |
|---|---|
| F01 | Daytona vs self-host (re-confirm) |
| F01 | CLI auth bootstrap UX (OQ-4) |
| F09 | Skill schema lock — must be set in stone before F10 starts |
| F11 | Skill MCP isolation policy (OQ-5) |
| F13 | Sign with Sigstore/cosign vs PGP |
| F15 | Warm pool size + eviction policy |
