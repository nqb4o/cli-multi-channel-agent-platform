# 04 — Phase 1: Multi-Provider + Skills (MVP-1)

**Duration:** 4 weeks | **Goal:** broaden providers, channels, and unlock the skill ecosystem.

## Definition of done

On top of the Phase 0 happy path:

1. Agents can be configured with `provider: anthropic-claude-cli` or `google-gemini-cli` and the daemon dispatches accordingly.
2. A Zalo Official Account bot can serve the same agent via `adapters/channels/zalo`.
3. A user-supplied skill folder is discoverable by F09's loader, validated against the FROZEN frontmatter schema, and rendered into the system prompt by `runtime.skills.render_catalog`.
4. F11's MCP loopback bridge spins up per-turn, exposes the skill set's MCP tools to the active provider, and tears down at turn end.
5. All five seed skills (`web-search`, `summarize-url`, `trend-analysis`, `regime-detection`, `image-describe`) ship under `skills/bundled/`.

## Features in scope

| Feature | Owner | Notes |
|---|---|---|
| F03 — Gemini CLI backend | Wave 1.A | inherits frozen `CliBackend` ABC from F02 |
| F04 — Claude CLI backend | Wave 1.A | inherits frozen `CliBackend` ABC from F02 |
| F08 — Zalo channel | Wave 1.A | OA token refresher, 24h messaging window |
| F09 — Skill loader | Wave 1.A | FROZEN `SKILL.md` schema, resolver, plugin_dir generator |
| F10 — Seed skill set | Wave 1.A | 5 bundled skills + tests |
| F11 — MCP loopback bridge | Wave 1.B | Per-session HTTP MCP, per-provider config builders |
| Wave 1.C | integration tests | multi-provider + skills + MCP composition |

## Schema lock

F09 publishes `SKILL_SCHEMA_VERSION = 1` with frozen `SkillManifest` dataclasses. Any future change requires a version bump + a migration. F09 ships a `test_schema_locked.py` tripwire.

## Out of scope

- Skill registry (Phase 2 — F13)
- OTEL telemetry (Phase 2 — F14)
- Sandbox autoscale (Phase 2 — F15)
- Provider fallback chain (Phase 2 — F16)
- Discord/Slack/WhatsApp channels (Phase 3)

## Risks

| Risk | Mitigation |
|---|---|
| Provider CLIs drift between versions | Pinning each CLI version in `infra/docker/sandbox.Dockerfile`; version-check probes |
| F09 schema lock too restrictive | One bump path documented; F10's seed skills validate against it |
| F11 MCP tool injection differs across providers | Three separate config builders (Codex inline TOML, Claude plugin-dir entry, Gemini settings overlay), all share the same scope token |
| Zalo OA 24h window | F08 surfaces a structured error so the agent can choose to fallback to email or skip |
