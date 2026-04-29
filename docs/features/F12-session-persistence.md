# F12 — Session Persistence (Postgres schema + DAL)

**Phase:** 0 | **Wave:** 0.A | **Dependencies:** none

## Goal

Postgres schema + typed DAL Protocols. Owns the migration policy (forward-only, append). Provides AES-GCM helpers for encrypted channel config.

## Scope (in)

- 3 migrations under `packages/persistence/src/persistence/migrations/`:
  - `0001_init.sql` — users, agents, channels, sandboxes, sessions, runs, skills_installed.
  - `0002_indexes.sql` — supporting indexes.
  - `0003_skill_registry.sql` (Phase 2 — added by F13) — skill_publishers, skill_packages, skill_releases, skill_signatures.
- Frozen dataclasses in `models.py`: `User`, `Agent`, `Channel`, `Sandbox`, `Session`, `Run`, `SkillInstalled`, `ChannelLookup`.
- Typed Protocol surface (`*RepoP`) in `protocols.py` so other features depend on contracts, not concrete impls.
- Concrete asyncpg-backed repos in `repos/`.
- AES-GCM-256 helpers (`encrypt(key, plaintext) → bytes`, `decrypt(key, blob) → bytes`) with versioned wire format.
- `migrate.py` CLI: `python -m persistence.migrate up|status`. Forward-only, idempotent.
- Sessions table PK: `(user_id, channel_id, thread_id, provider)` — explicitly NOT a single primary key; mirrors per-provider isolation.
- Channel config blob stored as `bytea` (encrypted).

## Scope (out)

- Replication, partitioning (Phase 3).
- Read replicas (Phase 3).

## Deliverables

```
packages/persistence/
├── pyproject.toml
├── README.md
└── src/persistence/
    ├── __init__.py                # public exports
    ├── config.py                  # PersistenceConfig + from_env()
    ├── pool.py                    # asyncpg pool + transaction + ping
    ├── crypto.py                  # AES-GCM helpers
    ├── models.py                  # frozen dataclasses
    ├── protocols.py               # Protocol surface
    ├── migrate.py                 # CLI
    ├── migrations/
    │   ├── 0001_init.sql
    │   ├── 0002_indexes.sql
    │   ├── 0003_skill_registry.sql   # F13 adds this
    │   └── README.md
    └── repos/
        ├── __init__.py
        ├── users.py
        ├── agents.py
        ├── channels.py
        ├── sandboxes.py
        ├── sessions.py
        ├── runs.py
        └── skills_installed.py

packages/persistence/tests/
├── conftest.py                    # testcontainer Postgres
├── test_crypto.py
├── test_migrations.py
└── test_repos.py
```

## Published Protocols

```python
class UsersRepoP(Protocol):
    async def create(self, email: str) -> User: ...
    async def get(self, user_id: UUID) -> User | None: ...
    async def get_by_email(self, email: str) -> User | None: ...

class AgentsRepoP(Protocol):
    async def create(self, user_id, name, config_yaml) -> Agent: ...
    async def get(self, agent_id) -> Agent | None: ...
    async def list_for_user(self, user_id) -> list[Agent]: ...
    async def update_config(self, agent_id, config_yaml) -> Agent | None: ...

class ChannelsRepoP(Protocol):
    async def register(self, user_id, channel_type, ext_id,
                       config_encrypted, agent_id) -> ChannelLookup: ...
    async def lookup_routing(self, channel_type, ext_id) -> ChannelLookup | None: ...
    async def get(self, channel_id) -> Channel | None: ...
    async def get_decrypted_config(self, channel_id) -> bytes | None: ...
    async def list_for_user(self, user_id) -> list[Channel]: ...

class SandboxesRepoP(Protocol):
    async def upsert(self, user_id, daytona_id, state) -> Sandbox: ...
    async def get_for_user(self, user_id) -> Sandbox | None: ...
    async def update_state(self, user_id, state, *, last_active_at=None) -> Sandbox | None: ...

class SessionsRepoP(Protocol):
    async def get(self, user_id, channel_id, thread_id, provider) -> Session | None: ...
    async def upsert_after_turn(self, user_id, channel_id, thread_id,
                                provider, cli_session_id) -> None: ...
    async def drop_for_provider(self, user_id, provider) -> int: ...

class RunsRepoP(Protocol):
    async def start(self, user_id, agent_id, channel_id, thread_id, provider) -> Run: ...
    async def finish_ok(self, run_id, latency_ms, *, ended_at=None) -> Run | None: ...
    async def finish_error(self, run_id, latency_ms, error_class, error_msg,
                           *, ended_at=None) -> Run | None: ...
    async def get(self, run_id) -> Run | None: ...
    async def list_recent_for_user(self, user_id, *, limit=50) -> list[Run]: ...

class SkillsInstalledRepoP(Protocol):
    async def install(self, user_id, slug, version, source) -> SkillInstalled: ...
    async def uninstall(self, user_id, slug) -> bool: ...
    async def list_for_user(self, user_id) -> list[SkillInstalled]: ...
```

## Acceptance criteria

1. `python -m persistence.migrate up` brings up a clean DB.
2. `pytest packages/persistence/tests/` passes against testcontainer Postgres.
3. Channel `config_encrypted` round-trip: encrypt → INSERT → SELECT → decrypt yields plaintext.
4. `SessionsRepo.get` cold returns None; `upsert_after_turn` fixes `initialized_at` to first turn while updating `cli_session_id`.
5. `drop_for_provider` clears that provider only (cross-provider + cross-user isolation).
6. Migrations forward-only and idempotent.
