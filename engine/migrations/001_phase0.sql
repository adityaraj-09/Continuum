-- Phase 0 schema (also applied via linearizer.Postgres.Migrate).

CREATE TABLE IF NOT EXISTS repos (
    repo_id       TEXT PRIMARY KEY,
    snapshot_seq  BIGINT NOT NULL DEFAULT 0,
    sealed_seq    BIGINT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS refs (
    repo_id    TEXT NOT NULL REFERENCES repos(repo_id) ON DELETE CASCADE,
    ref        TEXT NOT NULL,
    oid        TEXT NOT NULL,
    version    BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, ref)
);

CREATE INDEX IF NOT EXISTS refs_repo_idx ON refs(repo_id);
