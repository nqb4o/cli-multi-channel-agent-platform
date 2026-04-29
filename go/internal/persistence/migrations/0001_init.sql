-- 0001_init.sql
-- F12: Initial control-plane schema for the CLI-First Multi-Provider Agent Platform.
--
-- Conventions:
--   * Forward-only, append-only migrations. NEVER edit a file after it ships.
--   * Idempotent — every CREATE uses IF NOT EXISTS.
--   * UUID primary keys via gen_random_uuid() (pgcrypto).
--   * All timestamps are TIMESTAMPTZ.
--
-- See packages/persistence/src/persistence/migrations/README.md for the full
-- migration policy.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email TEXT UNIQUE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS agents (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    config_yaml TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS channels (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    type TEXT NOT NULL,
    ext_id TEXT NOT NULL,
    config_encrypted BYTEA NOT NULL,
    agent_id UUID REFERENCES agents(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (type, ext_id)
);

CREATE TABLE IF NOT EXISTS sandboxes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID UNIQUE NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    daytona_id TEXT NOT NULL,
    state TEXT NOT NULL,                    -- provisioning|running|hibernated|destroyed
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_active_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS sessions (
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel_id UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    thread_id TEXT NOT NULL,
    provider TEXT NOT NULL,                 -- codex-cli | google-gemini-cli | claude-cli
    cli_session_id TEXT,                    -- nullable for first turn
    initialized_at TIMESTAMPTZ,             -- non-null after first turn
    last_used_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, channel_id, thread_id, provider)
);

CREATE TABLE IF NOT EXISTS runs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id),
    agent_id UUID NOT NULL REFERENCES agents(id),
    channel_id UUID NOT NULL REFERENCES channels(id),
    thread_id TEXT NOT NULL,
    provider TEXT,
    status TEXT NOT NULL,                   -- accepted|running|ok|error
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at TIMESTAMPTZ,
    latency_ms INTEGER,
    error_class TEXT,
    error_msg TEXT
);

CREATE TABLE IF NOT EXISTS skills_installed (
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    slug TEXT NOT NULL,
    version TEXT NOT NULL,
    source TEXT NOT NULL,                   -- registry|local
    installed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, slug)
);
