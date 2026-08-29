// Package linearizer defines the per-ref compare-and-swap commit point.
package linearizer

import (
	"context"
	"errors"

	"github.com/adityaraj-09/Continuum/engine/internal/types"
)

var (
	// ErrConflict means the CAS lost (expected version / oid mismatch).
	ErrConflict = errors.New("linearizer: conflict")
	// ErrNotFound means the ref (or repo) does not exist.
	ErrNotFound = errors.New("linearizer: not found")
)

// Linearizer is the authoritative ordering layer for Git refs.
// The successful CompareAndSwap is the commit point of a push.
type Linearizer interface {
	EnsureRepo(ctx context.Context, repoID string) error
	GetRef(ctx context.Context, repoID, ref string) (types.RefState, error)
	ListRefs(ctx context.Context, repoID string) ([]types.RefState, error)
	CompareAndSwap(ctx context.Context, repoID, ref, expectedOID string, expectedVersion uint64, newOID string) (types.RefState, error)
	GetRepoHead(ctx context.Context, repoID string) (types.RepoHead, error)
	SetSnapshotSeq(ctx context.Context, repoID string, snapshotSeq uint64) error
	SetSealedSeq(ctx context.Context, repoID string, sealedSeq uint64) error

	// CommitPush serializes a repository push, validates every old OID, assigns
	// its WAL sequence/hash, durably seals the WAL through seal, and atomically
	// publishes all authoritative refs. The database commit is the commit point.
	CommitPush(ctx context.Context, entry *types.WALEntry, seal func(*types.WALEntry) error) (types.PushResult, error)
}
