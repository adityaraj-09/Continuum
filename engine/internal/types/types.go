// Package types holds shared Layer-1 domain types for Continuum.
package types

import "time"

// ZeroOID is Git's null object id (create/delete ref sentinel).
const ZeroOID = "0000000000000000000000000000000000000000"

// RefUpdate is a single Git ref mutation (old → new).
type RefUpdate struct {
	Ref string `json:"ref"`
	Old string `json:"old"`
	New string `json:"new"`
}

// PackRef points at a content-addressed pack in object storage.
type PackRef struct {
	Key       string `json:"key"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	ThinFixed bool   `json:"thin_fixed"`
}

// Actor is provenance for a push.
type Actor struct {
	UserID string `json:"user_id,omitempty"`
	Via    string `json:"via,omitempty"` // ssh | https | internal
}

// WALEntry is one durable push record (written to object storage).
// Phase 0 only defines the shape; Phase 1 writes these on push.
type WALEntry struct {
	Schema     int         `json:"schema"`
	RepoID     string      `json:"repo_id"`
	Seq        uint64      `json:"seq"`
	PushID     string      `json:"push_id"`
	ParentSeq  uint64      `json:"parent_seq"`
	CreatedAt  time.Time   `json:"created_at"`
	Actor      Actor       `json:"actor"`
	RefUpdates []RefUpdate `json:"ref_updates"`
	Packs      []PackRef   `json:"packs"`
	PrevHash   string      `json:"prev_hash,omitempty"`
	EntryHash  string      `json:"entry_hash,omitempty"`
}

// PushResult is the durable result returned when a push is committed.
type PushResult struct {
	PushID    string
	Seq       uint64
	EntryHash string
	Existing  bool
}

// RefState is the linearized current tip of one ref.
type RefState struct {
	RepoID  string
	Ref     string
	OID     string
	Version uint64 // monotonic CAS token per (repo, ref)
}

// RepoHead is optional repo-level metadata (snapshot pointer, etc.).
type RepoHead struct {
	RepoID      string
	CurrentSeq  uint64
	SnapshotSeq uint64
	SealedSeq   uint64
	LastWALHash string
}
