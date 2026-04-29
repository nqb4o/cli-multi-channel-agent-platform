-- 0002_indexes.sql
-- F12: Hot-path indexes that aren't covered by primary/unique keys.

CREATE INDEX IF NOT EXISTS idx_runs_user_started ON runs (user_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_last_used ON sessions (last_used_at);
CREATE INDEX IF NOT EXISTS idx_channels_user ON channels (user_id);
CREATE INDEX IF NOT EXISTS idx_runs_status ON runs (status);
