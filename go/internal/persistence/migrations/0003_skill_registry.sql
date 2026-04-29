-- 0003_skill_registry.sql
-- F13: Registry-side tables for the Skill Registry service.
--
-- Forward-only, append-only — see migrations/README.md.
--
-- The MVP registry uses these tables to catalog skill packages, the
-- versions ("releases") published under each, and the cosign-style
-- detached signatures attached to each release. Sigstore/cosign is the
-- preferred signer per docs/07-decisions.md, but the registry stores the
-- signature as opaque bytes plus a key reference so the verifier impl can
-- be swapped.
--
-- These tables live in the same Postgres instance as the F12 control
-- plane for MVP simplicity; the F13 brief leaves room to split them out
-- to their own schema/database in production.

CREATE TABLE IF NOT EXISTS skill_publishers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    handle TEXT UNIQUE NOT NULL,            -- e.g., "platform", "acme-corp"
    display_name TEXT,
    public_key_pem TEXT NOT NULL,           -- PEM-encoded verification key
    -- Hashed bearer token, scoped to publish-only. Hashed via sha256
    -- (no plaintext at rest). Lookup happens by sha256 over the presented token.
    publish_token_sha256 TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS skill_packages (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug TEXT UNIQUE NOT NULL,              -- e.g., "trend-analysis"
    publisher_id UUID NOT NULL REFERENCES skill_publishers(id) ON DELETE RESTRICT,
    description TEXT NOT NULL DEFAULT '',
    summary TEXT NOT NULL DEFAULT '',       -- short search blurb
    tags TEXT[] NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS skill_releases (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    package_id UUID NOT NULL REFERENCES skill_packages(id) ON DELETE CASCADE,
    version TEXT NOT NULL,                  -- e.g., "0.1.0"
    -- Manifest digest is the SHA-256 of the canonical tarball.  This is the
    -- content-addressing identifier: blob storage is keyed by it.
    manifest_digest TEXT NOT NULL,
    blob_size_bytes BIGINT NOT NULL,
    -- Sub-set of the SKILL.md frontmatter we surface for search/info without
    -- needing to download the tarball. Stored as JSON so we don't duplicate
    -- the F09 schema in SQL columns.
    manifest_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    changelog TEXT NOT NULL DEFAULT '',
    yanked BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (package_id, version)
);

CREATE TABLE IF NOT EXISTS skill_signatures (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    release_id UUID NOT NULL REFERENCES skill_releases(id) ON DELETE CASCADE,
    -- Detached signature bytes, base64-encoded. Cosign-style: this is the
    -- raw signature blob a `cosign verify-blob` would read.
    signature_b64 TEXT NOT NULL,
    -- Identifier for the verifying key (publisher handle by default; allows
    -- multiple keys per publisher in future).
    key_id TEXT NOT NULL,
    algorithm TEXT NOT NULL DEFAULT 'sigstore-cosign',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (release_id, key_id)
);

CREATE INDEX IF NOT EXISTS idx_skill_packages_publisher ON skill_packages (publisher_id);
CREATE INDEX IF NOT EXISTS idx_skill_releases_package ON skill_releases (package_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_skill_releases_digest ON skill_releases (manifest_digest);
CREATE INDEX IF NOT EXISTS idx_skill_signatures_release ON skill_signatures (release_id);
