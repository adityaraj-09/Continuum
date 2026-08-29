package wal

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/adityaraj-09/Continuum/engine/internal/types"
)

func TestFinalizeIsDeterministicAndVerifiable(t *testing.T) {
	entry := types.WALEntry{
		Schema: 1, RepoID: "repo", Seq: 1, PushID: "push", ParentSeq: 0,
		CreatedAt: time.Unix(1, 0).UTC(), PrevHash: "genesis",
		RefUpdates: []types.RefUpdate{{
			Ref: "refs/heads/main", Old: types.ZeroOID,
			New: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}},
	}
	first, err := Finalize(&entry)
	if err != nil {
		t.Fatal(err)
	}
	hash := entry.EntryHash
	second, err := Finalize(&entry)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) || entry.EntryHash != hash {
		t.Fatalf("Finalize is not deterministic")
	}

	var decoded types.WALEntry
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatal(err)
	}
	stored := decoded.EntryHash
	if _, err := Finalize(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.EntryHash != stored {
		t.Fatalf("stored hash does not verify")
	}
}
