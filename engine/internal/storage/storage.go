// Package storage defines the object-store abstraction used by Continuum.
// Implementations: MinIO (dev) / AWS S3 (prod).
package storage

import (
	"context"
	"errors"
	"io"
)

var (
	// ErrNotFound means the key does not exist.
	ErrNotFound = errors.New("storage: not found")
	// ErrPreconditionFailed means a conditional write lost the race.
	ErrPreconditionFailed = errors.New("storage: precondition failed")
)

// ObjectMeta describes a stored object.
type ObjectMeta struct {
	Key         string
	Size        int64
	ETag        string
	ContentType string
}

// PutOptions controls a Put.
type PutOptions struct {
	ContentType string
	// IfNoneMatchStar: only create if the key does not exist (If-None-Match: *).
	IfNoneMatchStar bool
	// IfMatchETag: only overwrite when current ETag matches.
	IfMatchETag string
}

// Storage is the cold durability layer (packs, WAL objects, snapshots).
type Storage interface {
	Put(ctx context.Context, key string, body io.Reader, size int64, opts PutOptions) (etag string, err error)
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectMeta, error)
	Head(ctx context.Context, key string) (ObjectMeta, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string, limit int) ([]ObjectMeta, error)
	EnsureBucket(ctx context.Context) error
}
