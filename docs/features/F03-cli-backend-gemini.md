# F03 — Gemini CLI Backend

**Phase:** 1 | **Wave:** 1.A | **Dependencies:** F02 (frozen `CliBackend` ABC)

## Goal

Inherit F02's frozen `CliBackend` and ship a `GeminiBackend` that talks to the `gemini` CLI.

## Scope (in)

- `GeminiBackend(CliBackend)` with `id = "google-gemini-cli"`.
- Single-object JSON output (`--output-format json`), not JSONL.
- `@<path>` image-arg syntax (different from Codex's `--image`).
- Settings file overlay for MCP config (atomic swap of `~/.gemini/settings.json`).
- `gemini --resume <sid>` for session resume.
- `supports_resume_in_stream()` → True.
- Workspace-scoped image paths — reject `@<path>` outside `workspace_root` before spawn.
- `--version` prereq probe at `__init__` (skippable for tests).

## Scope (out)

- Modify F02's `cli_backends/base.py` (FROZEN).
- Modify `services/runtime/pyproject.toml` destructively — append non-destructively only.

## Deliverables

```
services/runtime/src/runtime/cli_backends/gemini.py

services/runtime/tests/cli_backends/
├── test_gemini.py
└── fixtures/
    ├── fake_gemini.sh
    └── json/
        ├── gemini_happy.json
        ├── gemini_resume.json
        └── gemini_auth_err.json
```

## Acceptance criteria

1. `pytest services/runtime/tests/cli_backends/test_gemini.py` passes.
2. `gemini_happy.json` → text="hello world", session=`sess-gem-happy-001`, usage with `cache_read_input_tokens` derived from `stats.cached`.
3. `gemini_auth_err.json` → `CliTurnError(AUTH_EXPIRED)`.
4. Live test gated on `GEMINI_LIVE_TEST=1`.
5. Resume: `prev-sess-42` placed on argv after `--resume`, prompt re-sent.
6. `@<workspace-path>` works; `@<outside-workspace>` returns `CliTurnError` before spawn.

## Reference implementations

- `~/Workspace/open-source/openclaw/docs/gateway/cli-backends.md` lines 232-253 — Gemini defaults
- `~/Workspace/open-source/openclaw/src/agents/cli-output.ts` — usage normalisation
