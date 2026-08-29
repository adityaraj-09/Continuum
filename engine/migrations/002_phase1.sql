-- Phase 1 adds the ordered, hash-chained WAL head and push idempotency.

ALTER TABLE repos ADD COLUMN IF NOT EXISTS current_seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE repos ADD COLUMN IF NOT EXISTS last_wal_hash TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS pushes (
    push_id     TEXT PRIMARY KEY,
    repo_id     TEXT NOT NULL REFERENCES repos(repo_id) ON DELETE CASCADE,
    seq         BIGINT NOT NULL,
    entry_hash  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (repo_id, seq)
);
