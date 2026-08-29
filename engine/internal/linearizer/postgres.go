package linearizer

import (
	"context"
	"errors"
	"fmt"

	"github.com/adityaraj-09/Continuum/engine/internal/types"
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
		SELECT repo_id, snapshot_seq, sealed_seq FROM repos WHERE repo_id=$1
	`, repoID).Scan(&h.RepoID, &h.SnapshotSeq, &h.SealedSeq)
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
