# F04 — Claude CLI Backend

**Phase:** 1 | **Wave:** 1.A | **Dependencies:** F02 (frozen `CliBackend` ABC)

## Goal

Inherit F02's frozen `CliBackend` and ship a `ClaudeBackend` that talks to the `claude` CLI (Claude Code).

## Scope (in)

- `ClaudeBackend(CliBackend)` with `id = "claude-cli"`.
- Default argv: `claude -p --output-format stream-json --include-partial-messages --verbose --setting-sources user --permission-mode bypassPermissions`.
- Stream-json JSONL output parser → `text` + `new_session_id` + `usage`.
- System prompt via `--append-system-prompt <text>`.
- User prompt on stdin (paired with `-p`).
- Resume via `--resume <sid>`.
- `--plugin-dir <dir>` for skills (Claude-only feature). The plugin dir path is resolved via priority: `extra_env["CLAUDE_SKILL_PLUGIN_DIR"]` → constructor `plugin_dir` arg → `getattr(inp, "skill_plugin_dir", None)` (future-proofing for a possible base ABC extension).
- `supports_resume_in_stream()` → True.
- Cancellation: SIGTERM with 1s grace, then SIGKILL (parity with Codex).
- Strip `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST` from child env (avoids Anthropic billing-tier confusion).

## Scope (out)

- Modify F02's `cli_backends/base.py`. The brief originally proposed adding `skill_plugin_dir` to `CliTurnInput`; the FROZEN ABC stays unchanged. Use the env-var workaround instead.

## Deliverables

```
services/runtime/src/runtime/cli_backends/claude.py

services/runtime/tests/cli_backends/
├── test_claude.py
└── fixtures/
    ├── fake_claude.sh
    └── jsonl/
        ├── claude_happy.jsonl
        ├── claude_with_tools.jsonl
        └── claude_auth_err.jsonl
```

## Acceptance criteria

1. `pytest services/runtime/tests/cli_backends/test_claude.py` passes.
2. `claude_happy.jsonl` → non-empty text, session id present in telemetry.
3. `claude_auth_err.jsonl` → `CliTurnError(AUTH_EXPIRED)`.
4. `--plugin-dir` flag emitted when plugin dir is supplied via either env or constructor arg.
5. Tool-call interleaving (`claude_with_tools.jsonl`) is parsed without losing the final assistant message.

## Reference implementations

- `~/Workspace/open-source/openclaw/src/agents/cli-output.ts` — stream-json parser
- `~/Workspace/open-source/openclaw/src/agents/cli-backends.test.ts` lines 183-234 — Claude argv shape
- `~/Workspace/open-source/openclaw/src/agents/cli-runner/claude-skills-plugin.ts` — `--plugin-dir` semantics
