# 06 — Parallelization Plan

This document is the **dispatch guide** for orchestrating subagents.

## Feature dependency graph

```
                                   ┌─────────────────────┐
                                   │   F12 Persistence   │
                                   │   (DB schema, DAL)  │
                                   └──────────┬──────────┘
                                              │
   ┌──────────────────────┐                   │
   │  F01 Sandbox         │◄──────────────────┘
   │  Orchestrator        │
   │  (Daytona wrapper)   │
   └─────┬──────────┬─────┘
         │          │
         │          └──────────────────────┐
         │                                 │
         ▼                                 ▼
   ┌──────────────────┐              ┌──────────────────┐
   │  F02 Codex CLI   │              │  F05 ADK Runtime │
   │  backend         │              │  daemon          │
   └────────┬─────────┘              └────────┬─────────┘
            │                                 │
            │ (interface)                     │
            ▼                                 │
   ┌──────────────────┐  ┌──────────────────┐ │
   │  F03 Gemini CLI  │  │  F04 Claude CLI  │ │
   │  backend         │  │  backend         │ │
   └──────────────────┘  └──────────────────┘ │
                                              │
   ┌──────────────────┐  ┌──────────────────┐ │
   │  F09 Skill       │  │  F10 Seed skills │ │
   │  loader          │──│  (5 skills)      │─┼──► F05 consumes
   └────────┬─────────┘  └──────────────────┘ │
            │                                 │
            └─────────────┐                   │
                          ▼                   │
                    ┌──────────────────┐      │
                    │  F11 MCP         │──────┘
                    │  loopback bridge │
                    └──────────────────┘

   ┌──────────────────┐
   │  F06 Gateway HTTP│
   └────────┬─────────┘
            │
            ├──► F07 Telegram channel
            └──► F08 Zalo channel

   ┌──────────────────┐
   │  F13 Skill       │── independent service, integrated in Phase 2
   │  registry        │
   └──────────────────┘

   ┌──────────────────┐
   │  F14 OTEL        │── cross-cutting, instruments F01/F05/F06 in Phase 2
   │  telemetry       │
   └──────────────────┘

   ┌──────────────────┐
   │  F15 Sandbox     │── extends F01 with hibernate/warm-pool
   │  autoscale       │
   └──────────────────┘

   ┌──────────────────┐
   │  F16 Fallback    │── extends F05 with provider failover, uses F14 health
   │  chain           │
   └──────────────────┘
```

## Wave dispatch table

Each wave is a set of features with no unmet dependencies. Spawn one subagent per feature in the wave **in parallel**.

### Phase 0 dispatch

| Wave | Features (parallel) | Blockers |
|---|---|---|
| 0.A | F01, F06, F12 | none |
| 0.B | F02, F05, F07 | 0.A complete |
| 0.C | E2E smoke test | 0.B complete |

### Phase 1 dispatch

| Wave | Features (parallel) | Blockers |
|---|---|---|
| 1.A | F03, F04, F08, F09, F10 | Phase 0 complete |
| 1.B | F11 | 1.A complete (needs F03, F04, F09) |
| 1.C | Phase 1 integration tests | 1.B complete |

### Phase 2 dispatch

| Wave | Features (parallel) | Blockers |
|---|---|---|
| 2.A | F13, F14, F15 | Phase 1 complete |
| 2.B | F16 | 2.A complete (needs F14) |
| 2.C | Load + chaos tests | 2.B complete |

## Subagent dispatch prompt template

When spawning a subagent for a feature, use this prompt:

> You are a subagent assigned to implement **<feature_id>: <feature_title>**.
>
> Read **`docs/features/<feature_id>.md`** for the complete brief — it is self-contained, do not assume any prior conversation context.
>
> Also read **`docs/01-overview.md`** and **`docs/02-architecture.md`** for shared context.
>
> Constraints:
> - Stay strictly within the brief's **Scope** and **Out of scope** sections.
> - Match the file paths under **Deliverables** exactly so other subagents' work composes cleanly.
> - Do not modify files outside your feature's deliverables list. If you discover a blocker requiring cross-feature changes, stop and report.
> - When done, run the brief's **Acceptance tests** and report pass/fail with logs.
>
> Reference implementations: `~/Workspace/open-source/openclaw` and `~/Workspace/open-source/daytona` — paths are linked from each brief.
>
> **Code reuse is allowed and encouraged.** If existing code in those repos meets the requirements, port (adapt + copy) it rather than reimplementing. Read `docs/README.md` § "Code reuse policy" for the full rules: search before writing, reshape to our interfaces, preserve attribution + license headers, copy tests too, document the source in your PR. Do not import OpenClaw types into our codebase — translate.

## Coordination rules

1. **Shared interfaces freeze before forks.** F02 defines `CliBackend` base class first. F03/F04 inherit it without modification — if they need changes, stop and propose a base class update.
2. **DB migrations are append-only.** Any feature touching schema (F12 owns the schema) must add a new migration file, never edit existing ones.
3. **No shared mutable config.** Each feature owns its own config namespace under `config/<feature_id>/`. Cross-feature config goes through F12 DAL.
4. **Tests run in isolation.** Each feature ships unit tests that can run without other features running. Integration tests live in `tests/e2e/` and are owned by the phase, not individual features.
5. **One PR per feature.** No bundled multi-feature PRs — keeps the parallel review tractable.

## Conflict resolution

If two parallel features both need to touch a shared file (e.g., `services/runtime/src/runtime/agent_loop.py`):

- The brief that lists the file in **Deliverables** owns the write.
- The other brief documents what hook it needs and **stops**, escalating to a human or a coordinator subagent.
- Coordinator opens an interface change request, both subagents resume after the interface is updated.

## Estimated parallelism

| Phase | Wave A peak parallel agents | Wave B peak | Total wall-clock weeks |
|---|---|---|---|
| 0 | 3 (F01/F06/F12) | 3 (F02/F05/F07) | ~4 |
| 1 | 5 (F03/F04/F08/F09/F10) | 1 (F11) | ~4 |
| 2 | 3 (F13/F14/F15) | 1 (F16) | ~4 |

With 5 parallel subagents at peak and ~3 weeks of focused work per wave, the ~12-week roadmap can compress meaningfully if subagents execute cleanly within their briefs.
