// Package wal builds and persists immutable, hash-chained push records.
package wal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/adityaraj-09/Continuum/engine/internal/storage"
	"github.com/adityaraj-09/Continuum/engine/internal/types"
)

// Finalize computes the deterministic entry hash. EntryHash is excluded from
// the hashed payload; encoding/json is stable for this struct (no maps).
func Finalize(entry *types.WALEntry) ([]byte, error) {
	entry.EntryHash = ""
	payload, err := json.Marshal(entry)
	if err != nil {
		return nil, fmt.Errorf("marshal WAL payload: %w", err)
	}
	sum := sha256.Sum256(payload)
	entry.EntryHash = hex.EncodeToString(sum[:])
	final, err := json.Marshal(entry)
	if err != nil {
		return nil, fmt.Errorf("marshal WAL entry: %w", err)
	}
	return append(final, '\n'), nil
}

func Key(repoID string, seq uint64) string {
	return fmt.Sprintf("repos/%s/wal/%020d.json", repoID, seq)
}

// Put writes an immutable WAL entry. An existing identical entry is accepted
// to make retry after an uncertain response idempotent.
func Put(ctx context.Context, store storage.Storage, entry *types.WALEntry) error {
	body, err := Finalize(entry)
	if err != nil {
		return err
	}
	key := Key(entry.RepoID, entry.Seq)
	_, err = store.Put(ctx, key, bytes.NewReader(body), int64(len(body)), storage.PutOptions{
		ContentType:     "application/json",
		IfNoneMatchStar: true,
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrPreconditionFailed) {
		return err
	}
	rc, _, getErr := store.Get(ctx, key)
	if getErr != nil {
		return fmt.Errorf("read existing WAL: %w", getErr)
	}
	defer rc.Close()
	existing, getErr := io.ReadAll(rc)
	if getErr != nil {
		return getErr
	}
	if bytes.Equal(existing, body) {
		return nil
	}
	var prior types.WALEntry
	if err := json.Unmarshal(existing, &prior); err != nil {
		return fmt.Errorf("decode existing WAL %q: %w", key, err)
	}
	// A crash after writing S3 but before committing Postgres leaves an orphan
	// at this sequence. Retrying the same deterministic push adopts that exact
	// immutable record (including its original timestamp/hash).
	if prior.RepoID == entry.RepoID && prior.Seq == entry.Seq && prior.PushID == entry.PushID {
		storedHash := prior.EntryHash
		if _, err := Finalize(&prior); err != nil {
			return err
		}
		if prior.EntryHash != storedHash {
			return fmt.Errorf("existing WAL %q failed hash verification", key)
		}
		*entry = prior
		return nil
	}
	return fmt.Errorf("WAL key %q already contains a different push", key)
}

func Get(ctx context.Context, store storage.Storage, repoID string, seq uint64) (types.WALEntry, error) {
	rc, _, err := store.Get(ctx, Key(repoID, seq))
	if err != nil {
		return types.WALEntry{}, err
	}
	defer rc.Close()
	var entry types.WALEntry
	if err := json.NewDecoder(rc).Decode(&entry); err != nil {
		return types.WALEntry{}, fmt.Errorf("decode WAL %d: %w", seq, err)
	}
	expected := entry.EntryHash
	body, err := Finalize(&entry)
	_ = body
	if err != nil {
		return types.WALEntry{}, err
	}
	if entry.EntryHash != expected {
		return types.WALEntry{}, fmt.Errorf("WAL %d hash mismatch", seq)
	}
	return entry, nil
}
