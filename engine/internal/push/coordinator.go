// Package push turns Git's prepared reference transaction into durable packs,
// an immutable WAL entry, and an atomic Postgres ref commit.
package push

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/adityaraj-09/Continuum/engine/internal/linearizer"
	"github.com/adityaraj-09/Continuum/engine/internal/storage"
	"github.com/adityaraj-09/Continuum/engine/internal/types"
	"github.com/adityaraj-09/Continuum/engine/internal/wal"
)

type Coordinator struct {
	Store      storage.Storage
	Linearizer linearizer.Linearizer
}

// Prepare is called by Git's reference-transaction hook in the "prepared"
// state. GIT_QUARANTINE_PATH contains packs Git already validated and fixed.
func (c *Coordinator) Prepare(
	ctx context.Context,
	repoID string,
	updates []types.RefUpdate,
	quarantinePath string,
) (types.PushResult, error) {
	if len(updates) == 0 {
		return types.PushResult{}, fmt.Errorf("empty reference transaction")
	}
	sort.Slice(updates, func(i, j int) bool { return updates[i].Ref < updates[j].Ref })

	packs, err := c.persistPacks(ctx, repoID, quarantinePath)
	if err != nil {
		return types.PushResult{}, err
	}
	if len(packs) == 0 {
		for _, update := range updates {
			if update.New != types.ZeroOID {
				return types.PushResult{}, fmt.Errorf("no validated pack available for non-delete ref update")
			}
		}
	}
	pushID := deterministicPushID(repoID, updates, packs)
	entry := &types.WALEntry{
		RepoID: repoID, PushID: pushID, CreatedAt: time.Now().UTC(),
		Actor: types.Actor{Via: "git"}, RefUpdates: updates, Packs: packs,
	}
	return c.Linearizer.CommitPush(ctx, entry, func(final *types.WALEntry) error {
		return wal.Put(ctx, c.Store, final)
	})
}

func (c *Coordinator) persistPacks(ctx context.Context, repoID, quarantinePath string) ([]types.PackRef, error) {
	if quarantinePath == "" {
		return nil, nil // ref-only push (delete or move to existing object)
	}
	var paths []string
	err := filepath.WalkDir(quarantinePath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".pack") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk quarantine: %w", err)
	}
	sort.Strings(paths)
	result := make([]types.PackRef, 0, len(paths))
	for _, path := range paths {
		ref, err := c.persistPack(ctx, repoID, path)
		if err != nil {
			return nil, err
		}
		result = append(result, ref)
	}
	return result, nil
}

func (c *Coordinator) persistPack(ctx context.Context, repoID, path string) (types.PackRef, error) {
	f, err := os.Open(path)
	if err != nil {
		return types.PackRef{}, err
	}
	h := sha256.New()
	size, err := io.Copy(h, f)
	f.Close()
	if err != nil {
		return types.PackRef{}, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	key := fmt.Sprintf("repos/%s/packs/%s.pack", repoID, sum)
	f, err = os.Open(path)
	if err != nil {
		return types.PackRef{}, err
	}
	defer f.Close()
	_, err = c.Store.Put(ctx, key, f, size, storage.PutOptions{
		ContentType: "application/x-git-packed-objects", IfNoneMatchStar: true,
	})
	if err != nil && !errors.Is(err, storage.ErrPreconditionFailed) {
		return types.PackRef{}, fmt.Errorf("upload pack: %w", err)
	}
	return types.PackRef{Key: key, Size: size, SHA256: sum, ThinFixed: true}, nil
}

func deterministicPushID(repoID string, updates []types.RefUpdate, packs []types.PackRef) string {
	h := sha256.New()
	fmt.Fprintln(h, repoID)
	for _, update := range updates {
		fmt.Fprintf(h, "%s %s %s\n", update.Old, update.New, update.Ref)
	}
	for _, pack := range packs {
		fmt.Fprintln(h, pack.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}
