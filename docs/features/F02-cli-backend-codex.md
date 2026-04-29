# F02 — Codex CLI Backend

**Phase:** 0 | **Wave:** 0.B | **Dependencies:** F01

## Goal

Define the `CliBackend` ABC + dataclasses (the contract F03/F04 will inherit from) and ship the Codex implementation.

## Scope (in)

- FROZEN `CliBackend` ABC + `CliTurnInput` / `CliTurnOutput` / `CliTurnError` dataclasses (`frozen=True`).
- `CodexBackend(CliBackend)` implementation:
  - argv: `codex exec --json <prompt>` (initial), `codex exec resume <sid>` (resume; text only).
  - System prompt via `-c model_instructions_file=<tempfile>` (Codex has no `--append-system-prompt`).
  - User prompt on stdin when > 8000 chars; otherwise on argv.
  - Image arg: `--image <path>` per image.
  - MCP config: inline TOML via `-c mcp_servers='[…]'`.
  - JSONL parser → text + `new_session_id` + usage.
  - Error classifier: `auth_expired` / `rate_limit` / `transient` / `unknown`.
  - Cancellation: SIGTERM with 1s grace, then SIGKILL.

## Scope (out)

- Provider auth bootstrap (OQ-4 — orchestrator/admin concern, not the backend).
- ADK harness wrapping (F05's concern).

## Deliverables

```
services/runtime/
├── pyproject.toml
├── README.md
└── src/runtime/cli_backends/
    ├── __init__.py             # BackendRegistry + factory hooks
    ├── base.py                 # FROZEN ABC + dataclasses
    └── codex.py                # CodexBackend

services/runtime/tests/cli_backends/
├── test_base.py
├── test_codex.py
├── conftest.py
└── fixtures/
    ├── fake_codex.sh
    └── jsonl/
        ├── happy_path.jsonl
        ├── auth_expired.jsonl
        └── rate_limit.jsonl
```

## Interface contract (FROZEN)

```python
class ErrorClass(str, Enum):
    AUTH_EXPIRED = "auth_expired"
    RATE_LIMIT = "rate_limit"
    TRANSIENT = "transient"
    UNKNOWN = "unknown"

@dataclass(frozen=True)
class CliTurnInput:
    system_prompt: str
    user_prompt: str
    images: tuple[str, ...] = ()
    session_id: Optional[str] = None
    model: Optional[str] = None
    extra_env: dict[str, str] = field(default_factory=dict)
    mcp_config: Optional[dict[str, Any]] = None
    run_id: Optional[str] = None
    timeout_s: Optional[float] = None

@dataclass(frozen=True)
class CliTurnOutput:
    text: str
    new_session_id: Optional[str]
    usage: dict[str, Any] = field(default_factory=dict)
    raw_events: list[dict[str, Any]] = field(default_factory=list)

@dataclass(frozen=True)
class CliTurnError:
    error_class: ErrorClass
    message: str
    exit_code: int
    stderr_tail: str
    raw_events: list[dict[str, Any]] = field(default_factory=list)

class CliBackend(ABC):
    id: str                       # "codex-cli", "google-gemini-cli", "claude-cli"
    default_command: list[str]
    @abstractmethod
    async def turn(self, inp: CliTurnInput) -> CliTurnOutput | CliTurnError: ...
    @abstractmethod
    def supports_resume_in_stream(self) -> bool: ...
```

## Acceptance criteria

1. `pytest services/runtime/tests/cli_backends/` passes.
2. `happy_path.jsonl` → text="hello world", session id extracted, usage dict populated (incl. nested `cached_tokens`).
3. `auth_expired.jsonl` → `CliTurnError(AUTH_EXPIRED)`. Same for inline JSONL error events and non-zero exit + stderr.
4. Live integration gated on `CODEX_LIVE_TEST=1` + real `codex login`.
5. Resume preserves prior session id, swaps to text output, drops `--json`.
6. Cancellation kills subprocess in < 1 s.

## Reference implementations

- `~/Workspace/open-source/openclaw/src/agents/cli-runner/helpers.ts`
- `~/Workspace/open-source/openclaw/src/agents/cli-output.ts`
- `~/Workspace/open-source/openclaw/docs/gateway/cli-backends.md`
