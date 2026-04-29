# F13 — Skill Registry

**Phase:** 2 | **Wave:** 2.A | **Dependencies:** F09 (FROZEN schema), F12

## Goal

A FastAPI service + CLI client that lets skill authors publish signed skill tarballs and lets users `install` them inside their sandbox.

## Scope (in)

- Registry service:
  - `GET /skills?q=...` (search), `GET /skills/{slug}`, `GET /skills/{slug}/versions/{v}`.
  - `GET /skills/{slug}/versions/{v}/tarball` (content-addressed, returns `Docker-Content-Digest: sha256:<hex>`).
  - `GET /skills/{slug}/versions/{v}/sig` (signature blob).
  - `POST /skills/{slug}/versions` (multipart upload, publisher bearer auth, requires signature).
  - `GET /keys/{publisher_handle}` (public key for verification).
- Content-addressed blob store (filesystem or MinIO).
- New migration `0003_skill_registry.sql` adding `skill_publishers`, `skill_packages`, `skill_releases`, `skill_signatures` tables.
- `SignatureVerifier` Protocol with three impls: `InProcessVerifier` (test), `CosignSubprocessVerifier` (production stub), `AlwaysAcceptVerifier` (fixture-only).
- CLI client `cli/`: `platform skills {search,info,list,install,update,check}`.
- Installer flow: download → verify integrity (sha256) → verify signature → safe-extract (symlink/traversal guard) → write `.platform-install.json` for F09's resolver to discover.

## Scope (out)

- Real Sigstore/cosign integration (left as a stubbable Protocol).
- Skill billing / revenue share (post-MVP).
- Skill marketplace UI (Phase 3).

## Deliverables

```
services/registry/
├── pyproject.toml
├── README.md
└── src/registry/
    ├── api.py                  # FastAPI routes
    ├── auth.py                 # bearer-token publisher auth
    ├── publish.py              # validation + ingest pipeline
    ├── signing.py              # SignatureVerifier Protocol + 3 impls
    └── storage/
        ├── blob.py             # content-addressed blob store
        └── metadata.py         # Memory + Postgres catalog

cli/
├── pyproject.toml
└── src/platform_cli/
    ├── main.py
    ├── api_client.py
    ├── installer.py
    ├── signing.py
    └── commands/skills.py

packages/persistence/src/persistence/migrations/0003_skill_registry.sql
infra/docker/registry.Dockerfile
```

## Acceptance criteria

1. `pytest services/registry/tests/` passes against testcontainer Postgres.
2. Publish without valid signature → 400.
3. Publish with valid signature → 201, tarball stored, metadata recorded.
4. `platform skills search "trend"` returns matching results.
5. `platform skills install trend-analysis` extracts to managed dir.
6. `--version 0.1.0` pins.
7. Tampered tarball → fail with clear message, nothing extracted.
8. F09 picks up newly installed skill without restart (managed precedence tier).

## Reference implementations

- `~/Workspace/open-source/openclaw/src/infra/clawhub.ts` — REST verb shape, integrity, auth resolution
- `~/Workspace/open-source/openclaw/src/agents/skills/local-loader.ts` — safe extraction
