// Package repository manages disposable bare Git repositories on local disk.
package repository

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/adityaraj-09/Continuum/engine/internal/storage"
	"github.com/adityaraj-09/Continuum/engine/internal/types"
	"github.com/adityaraj-09/Continuum/engine/internal/wal"
)

var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type Manager struct {
	DataDir    string
	HookBinary string
	Store      storage.Storage
}

func ValidateID(repoID string) error {
	if !validID.MatchString(repoID) {
		return fmt.Errorf("invalid repository id %q", repoID)
	}
	return nil
}

func (m *Manager) ReposDir() string { return filepath.Join(m.DataDir, "repos") }

func (m *Manager) Path(repoID string) (string, error) {
	if err := ValidateID(repoID); err != nil {
		return "", err
	}
	return filepath.Join(m.ReposDir(), repoID+".git"), nil
}

func (m *Manager) Create(ctx context.Context, repoID string) error {
	path, err := m.Path(repoID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.ReposDir(), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return m.installHook(path, repoID)
	} else if !os.IsNotExist(err) {
		return err
	}
	if out, err := exec.CommandContext(ctx, "git", "init", "--bare", "--initial-branch=main", path).CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %w: %s", err, out)
	}
	for key, value := range map[string]string{
		"http.receivepack":      "true",
		"receive.unpackLimit":   "0", // keep incoming objects as a validated, fixed pack
		"core.logAllRefUpdates": "true",
	} {
		if out, err := exec.CommandContext(ctx, "git", "--git-dir", path, "config", key, value).CombinedOutput(); err != nil {
			return fmt.Errorf("git config %s: %w: %s", key, err, out)
		}
	}
	return m.installHook(path, repoID)
}

func (m *Manager) installHook(path, repoID string) error {
	if m.HookBinary == "" {
		return fmt.Errorf("hook binary is required")
	}
	hooks := filepath.Join(path, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		return err
	}
	script := fmt.Sprintf("#!/bin/sh\nexec %s reference-transaction %s \"$1\"\n",
		shellQuote(m.HookBinary), shellQuote(repoID))
	return os.WriteFile(filepath.Join(hooks, "reference-transaction"), []byte(script), 0o755)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func (m *Manager) DeleteLocal(repoID string) error {
	path, err := m.Path(repoID)
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}

// Materialize reconstructs a bare repository from WAL 1..targetSeq and checks
// that the replay reaches the linearizer's expected chain head.
func (m *Manager) Materialize(ctx context.Context, repoID string, targetSeq uint64, targetHash string) error {
	path, err := m.Path(repoID)
	if err != nil {
		return err
	}
	tmp := path + ".materializing"
	_ = os.RemoveAll(tmp)
	if out, err := exec.CommandContext(ctx, "git", "init", "--bare", "--initial-branch=main", tmp).CombinedOutput(); err != nil {
		return fmt.Errorf("git init materialized repo: %w: %s", err, out)
	}
	defer os.RemoveAll(tmp)

	previousHash := "genesis"
	for seq := uint64(1); seq <= targetSeq; seq++ {
		entry, err := wal.Get(ctx, m.Store, repoID, seq)
		if err != nil {
			return fmt.Errorf("read WAL %d: %w", seq, err)
		}
		if entry.RepoID != repoID || entry.Seq != seq || entry.ParentSeq != seq-1 || entry.PrevHash != previousHash {
			return fmt.Errorf("WAL %d breaks repository sequence/hash chain", seq)
		}
		for _, pack := range entry.Packs {
			if err := m.installPack(ctx, tmp, pack); err != nil {
				return fmt.Errorf("install pack %s: %w", pack.Key, err)
			}
		}
		if err := applyRefs(ctx, tmp, entry.RefUpdates); err != nil {
			return fmt.Errorf("apply WAL %d refs: %w", seq, err)
		}
		previousHash = entry.EntryHash
	}
	if targetSeq > 0 && previousHash != targetHash {
		return fmt.Errorf("materialized WAL head %s does not match expected %s", previousHash, targetHash)
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return m.installHook(path, repoID)
}

func (m *Manager) installPack(ctx context.Context, gitDir string, pack types.PackRef) error {
	rc, _, err := m.Store.Get(ctx, pack.Key)
	if err != nil {
		return err
	}
	defer rc.Close()
	cmd := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "index-pack", "--stdin")
	hash := sha256.New()
	cmd.Stdin = io.TeeReader(rc, hash)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git index-pack: %w: %s", err, out)
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); actual != pack.SHA256 {
		return fmt.Errorf("SHA-256 mismatch: got %s want %s", actual, pack.SHA256)
	}
	return nil
}

func applyRefs(ctx context.Context, gitDir string, updates []types.RefUpdate) error {
	cmd := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "update-ref", "--stdin")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return err
	}
	w := bufio.NewWriter(stdin)
	fmt.Fprintln(w, "start")
	for _, update := range updates {
		if update.New == types.ZeroOID {
			fmt.Fprintf(w, "delete %s %s\n", update.Ref, update.Old)
		} else {
			fmt.Fprintf(w, "update %s %s %s\n", update.Ref, update.New, update.Old)
		}
	}
	fmt.Fprintln(w, "prepare")
	fmt.Fprintln(w, "commit")
	w.Flush()
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git update-ref: %w: %s", err, output.String())
	}
	return nil
}

// StreamFile is test support for reading a local ref without exposing paths.
func (m *Manager) StreamFile(repoID, relative string, dst io.Writer) error {
	path, err := m.Path(repoID)
	if err != nil {
		return err
	}
	f, err := os.Open(filepath.Join(path, filepath.Clean(relative)))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(dst, f)
	return err
}
