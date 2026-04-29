# F09 — Skill Loader / Resolver / Catalog

**Phase:** 1 | **Wave:** 1.A | **Dependencies:** F12 (`SkillsInstalledRepoP`, optional)

## Goal

Define the FROZEN `SKILL.md` frontmatter schema. Load skill folders from a list of roots, resolve precedence + version + eligibility, render the system-prompt catalog, and produce the Claude `--plugin-dir` layout that F11 builds on top of.

## Scope (in)

- FROZEN `SKILL_SCHEMA_VERSION = 1` with `SkillManifest`, `SkillMcpConfig`, `SkillSigning` (`frozen=True`).
- `parse_skill_md(text, fallback_slug)` → `SkillManifest`.
- `load_all(roots)` → walk dirs, parse SKILL.md, return manifests + warnings.
- `resolve(available, requested, env)` → ADR-007 precedence (workspace > project > personal > managed > bundled), version pin, `required_env` eligibility.
- `render_catalog(manifests)` → markdown bullet list for the system prompt.
- `generate_plugin_dir(manifests, run_id)` → Claude-Code-compatible plugin layout under `/tmp/claude_plugins_<run_id>/`.
- Per-provider MCP config builders (in `mcp_config_gen.py`):
  - `codex_inline_config(bridge)` → dict for Codex's inline TOML
  - `claude_mcp_config(bridge)` → dict for Claude's `.mcp.json` plugin entry
  - `gemini_settings_payload(bridge)` → dict for Gemini's settings overlay
  - `layer_loopback_into_plugin_dir(bridge, plugin_dir)` → fold F11 into Claude's plugin dir

## Schema (FROZEN)

```yaml
---
name: <slug>                       # REQUIRED, str
version: <semver-ish>              # REQUIRED, str
description: <one-liner>           # REQUIRED, str
when_to_use: <one-liner>           # REQUIRED, str
allowed_tools: [bash, python]      # OPTIONAL, list[str]
required_env: [TIINGO_API_KEY]     # OPTIONAL, list[str]
mcp:                               # OPTIONAL
  enabled: <bool>
  command: [<argv>...]
  transport: stdio | http
signing:                           # OPTIONAL
  publisher: <str|null>
  sig: <str|null>
---
# Markdown body (when_to_use detail, examples, etc.)
```

## Scope (out)

- Skill registry (F13).
- Skill MCP execution (F11).
- Modifying the schema after this PR ships (`test_schema_locked.py` enforces).

## Deliverables

```
services/runtime/src/runtime/skills/
├── __init__.py                   # public API + legacy resolve_skills shim
├── schema.py                     # FROZEN dataclasses + parser
├── loader.py                     # walk + parse
├── resolver.py                   # precedence + version + eligibility
├── catalog.py                    # markdown renderer
├── plugin_dir.py                 # Claude plugin-dir generator
├── mcp_config_gen.py             # per-provider MCP config builders (used by F11)
└── README.md                     # FROZEN schema reference

services/runtime/tests/skills/
├── test_schema.py
├── test_schema_locked.py         # tripwire on field set
├── test_loader.py
├── test_resolver.py
├── test_catalog.py
├── test_plugin_dir.py
└── fixtures/
    └── skills_{workspace,project,personal,managed,bundled}/...
```

## Acceptance criteria

1. `pytest services/runtime/tests/skills/` passes.
2. Loader correctly tags each manifest with its source tier.
3. Workspace skill wins over managed skill of the same slug.
4. `required_env` filters skills out when env is missing.
5. Catalog format matches the brief's markdown spec.
6. Plugin dir lays out `<plugin-name>/skills/<slug>/SKILL.md` per Claude Code conventions.

## Reference implementations

- `~/Workspace/open-source/openclaw/src/agents/skills/{frontmatter,local-loader,agent-filter,filter,skill-contract}.ts`
- `~/.claude/plugins/marketplaces/claude-plugins-official/plugins/playground/` — real Claude Code plugin layout
