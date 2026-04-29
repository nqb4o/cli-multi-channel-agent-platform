# Implementation Plan — CLI-First Multi-Provider Agent Platform

This folder contains the full implementation plan, designed so that **multiple subagents can work in parallel**.

## Goal

Build an MVP platform where end-users create **skills + agents**, connect them to chat **channels** (Telegram, Zalo, …), and run them through **provider CLIs** (OpenAI Codex CLI, Google Gemini CLI, Anthropic Claude CLI) hosted in **per-user isolated sandboxes** (Daytona-style). The platform never proxies provider tokens — token cost lives in the user's own CLI subscription.

## How to read this folder

Read top-to-bottom for full context, or jump to a feature brief if you are a subagent assigned a single feature.

| File | Purpose | Audience |
|---|---|---|
| `01-overview.md` | Vision, principles, storage strategy per provider | Everyone |
| `02-architecture.md` | System architecture, data flow, component boundaries | Architects, leads |
| `03-phase-0-foundation.md` | MVP-0 release scope (4 weeks) | Phase 0 team |
| `04-phase-1-multi-provider.md` | MVP-1 release scope (4 weeks) | Phase 1 team |
| `05-phase-2-ecosystem.md` | MVP-2 release scope (4 weeks) | Phase 2 team |
| `06-parallelization.md` | Feature dependency graph + parallel waves | Orchestrator / human PM |
| `07-decisions.md` | ADRs and open decisions | Everyone |
| `features/F01..F16.md` | Self-contained feature briefs (one per parallel work unit) | Subagent assigned to that feature |

## How to dispatch parallel subagents

1. Read `06-parallelization.md` to identify the next wave of features that have no unmet dependencies.
2. For each feature in that wave, spawn a subagent with the prompt: *"Read `docs/features/<id>.md` and implement it. The brief is self-contained — do not assume prior conversation context. Report back with diff summary and acceptance test results."*
3. Wait for all subagents in the wave to finish before starting the next wave.
4. After each wave, run integration smoke tests defined in the matching `phase-*.md`.

## Conventions

- All paths in feature briefs are repo-relative from the project root.
- Every feature has explicit **dependencies**, **out-of-scope**, and **acceptance criteria** — subagents must not exceed scope.
- Reference implementations to study live in `~/Workspace/open-source/openclaw` (gateway/cli-backends, channels, skills) and `~/Workspace/open-source/daytona` (sandbox SDK).

## Code reuse policy

**Copy-paste from the reference repos is allowed and encouraged when existing code meets the requirements.** Do not reimplement from scratch what already works in OpenClaw or Daytona.

Rules:
1. **Search first.** Before writing new code for any feature, grep `~/Workspace/open-source/openclaw` and `~/Workspace/open-source/daytona` for similar functionality. The most reusable areas:
   - OpenClaw `cliBackends/*` adapters (Codex, Gemini, Claude) → maps directly to F02/F03/F04
   - OpenClaw channel adapters (`telegram`, `zalo`) → maps to F07/F08
   - OpenClaw skill loader/resolver → maps to F09
   - OpenClaw bundle-MCP loopback bridge → maps to F11
   - OpenClaw plugin SDK + manifest → maps to plugin contracts in F06/F09
   - Daytona SDK usage → F01
2. **Adapt to our interfaces.** Copied code must be reshaped to match the interface contracts defined in our feature briefs (e.g., `CliBackend` base in F02). Do not import OpenClaw types into our codebase.
3. **Preserve license + attribution.** Both repos have OSS licenses (check `LICENSE` files). When you copy non-trivial code, add a header comment: `// Adapted from openclaw/<path> @ <commit>, <license>`. Verify license compatibility with our project license before vendoring.
4. **Prefer porting over forking.** Translate the algorithm + structure into our codebase rather than dropping in entire OpenClaw/Daytona modules. We want to own and evolve the result, not maintain a fork.
5. **No copy-paste of secrets / config / vendor identifiers.** Copy logic, not provider tokens, default URLs pointing at OpenClaw services, or branding.
6. **Tests too.** If a reference repo has tests for the logic you copied, port them too — adjusted to our fixtures.

When in doubt, document the source in the PR description: "Ported `services/runtime/src/runtime/cli_backends/codex.py` from `openclaw/<path>` with adaptations: <list>."
