package linearizer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/adityaraj-09/Continuum/engine/internal/types"
	"github.com/adityaraj-09/Continuum/engine/internal/wal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres implements Linearizer with per-ref optimistic concurrency.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres opens a pool and returns a Linearizer.
func NewPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

// Close releases the pool.
func (p *Postgres) Close() { p.pool.Close() }

// Migrate applies the Phase-0 schema (idempotent).
func (p *Postgres) Migrate(ctx context.Context) error {
	_, err := p.pool.Exec(ctx, schemaSQL)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS repos (
    repo_id       TEXT PRIMARY KEY,
    current_seq   BIGINT NOT NULL DEFAULT 0,
    snapshot_seq  BIGINT NOT NULL DEFAULT 0,
    sealed_seq    BIGINT NOT NULL DEFAULT 0,
    last_wal_hash TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE repos ADD COLUMN IF NOT EXISTS current_seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE repos ADD COLUMN IF NOT EXISTS last_wal_hash TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS refs (
    repo_id    TEXT NOT NULL REFERENCES repos(repo_id) ON DELETE CASCADE,
    ref        TEXT NOT NULL,
    oid        TEXT NOT NULL,
    version    BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, ref)
);

CREATE INDEX IF NOT EXISTS refs_repo_idx ON refs(repo_id);

CREATE TABLE IF NOT EXISTS pushes (
    push_id     TEXT PRIMARY KEY,
    repo_id     TEXT NOT NULL REFERENCES repos(repo_id) ON DELETE CASCADE,
    seq         BIGINT NOT NULL,
    entry_hash  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (repo_id, seq)
);
`

func (p *Postgres) EnsureRepo(ctx context.Context, repoID string) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO repos (repo_id) VALUES ($1)
		ON CONFLICT (repo_id) DO NOTHING
	`, repoID)
	if err != nil {
		return fmt.Errorf("ensure repo: %w", err)
	}
	return nil
}

func (p *Postgres) GetRef(ctx context.Context, repoID, ref string) (types.RefState, error) {
	var st types.RefState
	err := p.pool.QueryRow(ctx, `
		SELECT repo_id, ref, oid, version FROM refs WHERE repo_id=$1 AND ref=$2
	`, repoID, ref).Scan(&st.RepoID, &st.Ref, &st.OID, &st.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RefState{}, ErrNotFound
	}
	if err != nil {
		return types.RefState{}, fmt.Errorf("get ref: %w", err)
	}
	return st, nil
}

func (p *Postgres) ListRefs(ctx context.Context, repoID string) ([]types.RefState, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT repo_id, ref, oid, version FROM refs WHERE repo_id=$1 ORDER BY ref
	`, repoID)
	if err != nil {
		return nil, fmt.Errorf("list refs: %w", err)
	}
	defer rows.Close()
	var out []types.RefState
	for rows.Next() {
		var st types.RefState
		if err := rows.Scan(&st.RepoID, &st.Ref, &st.OID, &st.Version); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (p *Postgres) CompareAndSwap(
	ctx context.Context,
	repoID, ref, expectedOID string,
	expectedVersion uint64,
	newOID string,
) (types.RefState, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return types.RefState{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := p.ensureRepoTx(ctx, tx, repoID); err != nil {
		return types.RefState{}, err
	}

	var curOID string
	var curVersion uint64
	err = tx.QueryRow(ctx, `
		SELECT oid, version FROM refs WHERE repo_id=$1 AND ref=$2 FOR UPDATE
	`, repoID, ref).Scan(&curOID, &curVersion)

	if errors.Is(err, pgx.ErrNoRows) {
		if expectedOID != types.ZeroOID || expectedVersion != 0 {
			return types.RefState{}, ErrConflict
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO refs (repo_id, ref, oid, version) VALUES ($1, $2, $3, 1)
		`, repoID, ref, newOID)
		if err != nil {
			return types.RefState{}, fmt.Errorf("insert ref: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return types.RefState{}, err
		}
		return types.RefState{RepoID: repoID, Ref: ref, OID: newOID, Version: 1}, nil
	}
	if err != nil {
		return types.RefState{}, fmt.Errorf("lock ref: %w", err)
	}

	if curOID != expectedOID || curVersion != expectedVersion {
		return types.RefState{}, ErrConflict
	}

	if newOID == types.ZeroOID {
		_, err = tx.Exec(ctx, `DELETE FROM refs WHERE repo_id=$1 AND ref=$2`, repoID, ref)
		if err != nil {
			return types.RefState{}, fmt.Errorf("delete ref: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return types.RefState{}, err
		}
		return types.RefState{RepoID: repoID, Ref: ref, OID: types.ZeroOID, Version: curVersion + 1}, nil
	}

	newVersion := curVersion + 1
	_, err = tx.Exec(ctx, `
		UPDATE refs SET oid=$1, version=$2, updated_at=now()
		WHERE repo_id=$3 AND ref=$4
	`, newOID, newVersion, repoID, ref)
	if err != nil {
		return types.RefState{}, fmt.Errorf("update ref: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return types.RefState{}, err
	}
	return types.RefState{RepoID: repoID, Ref: ref, OID: newOID, Version: newVersion}, nil
}

func (p *Postgres) ensureRepoTx(ctx context.Context, tx pgx.Tx, repoID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO repos (repo_id) VALUES ($1) ON CONFLICT (repo_id) DO NOTHING
	`, repoID)
	return err
}

func (p *Postgres) GetRepoHead(ctx context.Context, repoID string) (types.RepoHead, error) {
	var h types.RepoHead
	err := p.pool.QueryRow(ctx, `
		SELECT repo_id, current_seq, snapshot_seq, sealed_seq, last_wal_hash
		FROM repos WHERE repo_id=$1
	`, repoID).Scan(&h.RepoID, &h.CurrentSeq, &h.SnapshotSeq, &h.SealedSeq, &h.LastWALHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RepoHead{}, ErrNotFound
	}
	if err != nil {
		return types.RepoHead{}, fmt.Errorf("get repo head: %w", err)
	}
	return h, nil
}

func (p *Postgres) SetSnapshotSeq(ctx context.Context, repoID string, snapshotSeq uint64) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE repos SET snapshot_seq=$1 WHERE repo_id=$2
	`, snapshotSeq, repoID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) SetSealedSeq(ctx context.Context, repoID string, sealedSeq uint64) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE repos SET sealed_seq=$1 WHERE repo_id=$2
	`, sealedSeq, repoID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CommitPush implements the Phase-1 commit protocol. The repo row lock gives
// the hash-chained WAL a total order. Individual refs still carry independent
// versions and old-OID checks.
func (p *Postgres) CommitPush(
	ctx context.Context,
	entry *types.WALEntry,
	seal func(*types.WALEntry) error,
) (types.PushResult, error) {
	if entry.RepoID == "" || entry.PushID == "" || len(entry.RefUpdates) == 0 {
		return types.PushResult{}, fmt.Errorf("invalid push: repo, push id, and ref updates are required")
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return types.PushResult{}, fmt.Errorf("begin push: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := p.ensureRepoTx(ctx, tx, entry.RepoID); err != nil {
		return types.PushResult{}, err
	}

	var previous types.PushResult
	err = tx.QueryRow(ctx, `
		SELECT push_id, seq, entry_hash FROM pushes WHERE push_id=$1
	`, entry.PushID).Scan(&previous.PushID, &previous.Seq, &previous.EntryHash)
	if err == nil {
		previous.Existing = true
		return previous, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return types.PushResult{}, fmt.Errorf("check push id: %w", err)
	}

	var currentSeq uint64
	var previousHash string
	if err := tx.QueryRow(ctx, `
		SELECT current_seq, last_wal_hash FROM repos WHERE repo_id=$1 FOR UPDATE
	`, entry.RepoID).Scan(&currentSeq, &previousHash); err != nil {
		return types.PushResult{}, fmt.Errorf("lock repo: %w", err)
	}

	seen := make(map[string]struct{}, len(entry.RefUpdates))
	for _, update := range entry.RefUpdates {
		if _, duplicate := seen[update.Ref]; duplicate {
			return types.PushResult{}, fmt.Errorf("duplicate ref update %q", update.Ref)
		}
		seen[update.Ref] = struct{}{}

		var currentOID string
		err := tx.QueryRow(ctx, `
			SELECT oid FROM refs WHERE repo_id=$1 AND ref=$2 FOR UPDATE
		`, entry.RepoID, update.Ref).Scan(&currentOID)
		if errors.Is(err, pgx.ErrNoRows) {
			if update.Old != types.ZeroOID {
				return types.PushResult{}, ErrConflict
			}
			continue
		}
		if err != nil {
			return types.PushResult{}, fmt.Errorf("lock ref %q: %w", update.Ref, err)
		}
		if currentOID != update.Old {
			return types.PushResult{}, ErrConflict
		}
	}

	entry.Schema = 1
	entry.Seq = currentSeq + 1
	entry.ParentSeq = currentSeq
	entry.PrevHash = previousHash
	if currentSeq == 0 {
		entry.PrevHash = "genesis"
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	if _, err := wal.Finalize(entry); err != nil {
		return types.PushResult{}, err
	}

	// External I/O deliberately occurs while the repo row is locked. This keeps
	// WAL order simple and guarantees bytes exist before the DB commit point.
	if err := seal(entry); err != nil {
		return types.PushResult{}, fmt.Errorf("seal WAL: %w", err)
	}

	for _, update := range entry.RefUpdates {
		if update.New == types.ZeroOID {
			if _, err := tx.Exec(ctx, `
				DELETE FROM refs WHERE repo_id=$1 AND ref=$2
			`, entry.RepoID, update.Ref); err != nil {
				return types.PushResult{}, fmt.Errorf("delete ref %q: %w", update.Ref, err)
			}
			continue
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO refs (repo_id, ref, oid, version)
			VALUES ($1, $2, $3, 1)
			ON CONFLICT (repo_id, ref) DO UPDATE
			SET oid=EXCLUDED.oid, version=refs.version+1, updated_at=now()
		`, entry.RepoID, update.Ref, update.New)
		if err != nil {
			return types.PushResult{}, fmt.Errorf("publish ref %q: %w", update.Ref, err)
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE repos
		SET current_seq=$1, sealed_seq=$1, last_wal_hash=$2
		WHERE repo_id=$3
	`, entry.Seq, entry.EntryHash, entry.RepoID); err != nil {
		return types.PushResult{}, fmt.Errorf("advance repo head: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pushes (push_id, repo_id, seq, entry_hash)
		VALUES ($1, $2, $3, $4)
	`, entry.PushID, entry.RepoID, entry.Seq, entry.EntryHash); err != nil {
		return types.PushResult{}, fmt.Errorf("record push: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return types.PushResult{}, fmt.Errorf("commit push: %w", err)
	}
	return types.PushResult{
		PushID: entry.PushID, Seq: entry.Seq, EntryHash: entry.EntryHash,
	}, nil
}
