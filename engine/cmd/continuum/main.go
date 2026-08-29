package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/adityaraj-09/Continuum/engine/internal/config"
	"github.com/adityaraj-09/Continuum/engine/internal/gitserver"
	"github.com/adityaraj-09/Continuum/engine/internal/linearizer"
	pushservice "github.com/adityaraj-09/Continuum/engine/internal/push"
	"github.com/adityaraj-09/Continuum/engine/internal/repository"
	"github.com/adityaraj-09/Continuum/engine/internal/storage"
	"github.com/adityaraj-09/Continuum/engine/internal/types"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		if err := runServe(); err != nil {
			log.Fatalf("serve: %v", err)
		}
	case "smoke":
		if err := runSmoke(); err != nil {
			log.Fatalf("smoke: %v", err)
		}
		fmt.Println("phase0 smoke: OK")
	case "migrate":
		if err := runMigrate(); err != nil {
			log.Fatalf("migrate: %v", err)
		}
		fmt.Println("migrate: OK")
	case "create":
		if len(os.Args) != 3 {
			log.Fatal("usage: continuum create <repo-id>")
		}
		if err := runCreate(os.Args[2]); err != nil {
			log.Fatalf("create: %v", err)
		}
	case "materialize":
		if len(os.Args) != 3 {
			log.Fatal("usage: continuum materialize <repo-id>")
		}
		if err := runMaterialize(os.Args[2]); err != nil {
			log.Fatalf("materialize: %v", err)
		}
	case "reference-transaction":
		if len(os.Args) != 4 {
			log.Fatal("usage: continuum reference-transaction <repo-id> <state>")
		}
		if err := runReferenceTransaction(os.Args[2], os.Args[3]); err != nil {
			log.Printf("reference transaction rejected: %v", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `continuum — Layer 1 engine (Phase 0)

Usage:
  continuum serve    Start health server + ensure backends
  continuum smoke    Prove MinIO put/get + Postgres per-ref CAS
  continuum migrate  Apply Phase 0 schema
  continuum create <repo-id>
  continuum materialize <repo-id>

`)
}

func load() (config.Config, error) {
	return config.LoadFromEnv()
}

func openBackends(ctx context.Context, cfg config.Config) (*storage.S3Storage, *linearizer.Postgres, error) {
	store := storage.NewS3Storage(storage.S3Config{
		Endpoint:     cfg.S3Endpoint,
		Region:       cfg.S3Region,
		Bucket:       cfg.S3Bucket,
		AccessKey:    cfg.S3AccessKey,
		SecretKey:    cfg.S3SecretKey,
		UsePathStyle: cfg.S3UsePathStyle,
	})
	lin, err := linearizer.NewPostgres(ctx, cfg.PostgresDSN)
	if err != nil {
		return nil, nil, err
	}
	return store, lin, nil
}

func runMigrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := load()
	if err != nil {
		return err
	}
	_, lin, err := openBackends(ctx, cfg)
	if err != nil {
		return err
	}
	defer lin.Close()
	return lin.Migrate(ctx)
}

func manager(cfg config.Config, store storage.Storage) (*repository.Manager, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &repository.Manager{DataDir: cfg.DataDir, HookBinary: executable, Store: store}, nil
}

func runCreate(repoID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := load()
	if err != nil {
		return err
	}
	store, lin, err := openBackends(ctx, cfg)
	if err != nil {
		return err
	}
	defer lin.Close()
	if err := lin.Migrate(ctx); err != nil {
		return err
	}
	if err := lin.EnsureRepo(ctx, repoID); err != nil {
		return err
	}
	m, err := manager(cfg, store)
	if err != nil {
		return err
	}
	return m.Create(ctx, repoID)
}

func runMaterialize(repoID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cfg, err := load()
	if err != nil {
		return err
	}
	store, lin, err := openBackends(ctx, cfg)
	if err != nil {
		return err
	}
	defer lin.Close()
	head, err := lin.GetRepoHead(ctx, repoID)
	if err != nil {
		return err
	}
	m, err := manager(cfg, store)
	if err != nil {
		return err
	}
	return m.Materialize(ctx, repoID, head.CurrentSeq, head.LastWALHash)
}

func runReferenceTransaction(repoID, state string) error {
	// "committed" and "aborted" are notifications. Durability and the
	// authoritative Postgres transaction happen in "prepared".
	if state != "prepared" {
		return nil
	}
	var updates []types.RefUpdate
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			return fmt.Errorf("invalid reference transaction line %q", scanner.Text())
		}
		// Git may report the symbolic pseudo-ref HEAD when the first branch is
		// created. Its target (refs/heads/main) is the durable mutation; replaying
		// both would apply the same ref twice and detach HEAD.
		if !strings.HasPrefix(fields[2], "refs/") {
			continue
		}
		updates = append(updates, types.RefUpdate{Old: fields[0], New: fields[1], Ref: fields[2]})
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cfg, err := load()
	if err != nil {
		return err
	}
	store, lin, err := openBackends(ctx, cfg)
	if err != nil {
		return err
	}
	defer lin.Close()
	if err := store.EnsureBucket(ctx); err != nil {
		return err
	}
	coordinator := pushservice.Coordinator{Store: store, Linearizer: lin}
	// On Git 2.39 the reference-transaction prepared hook runs after incoming
	// objects have moved from quarantine into the repository ODB, but before
	// refs are visible. Capture those validated packs. Existing packs may also
	// be encountered; content-addressed S3 keys make them harmless/idempotent.
	repoPath, pathErr := (&repository.Manager{DataDir: cfg.DataDir}).Path(repoID)
	if pathErr != nil {
		return pathErr
	}
	quarantinePath := filepath.Join(repoPath, "objects")
	result, err := coordinator.Prepare(ctx, repoID, updates, quarantinePath)
	if err != nil {
		return err
	}
	log.Printf("push committed repo=%s push=%s seq=%d existing=%t", repoID, result.PushID, result.Seq, result.Existing)
	return nil
}

func runServe() error {
	cfg, err := load()
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	var (
		mu    sync.Mutex
		store storage.Storage
		lin   *linearizer.Postgres
		ready error = fmt.Errorf("backends not initialized")
	)

	initBackends := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s, p, err := openBackends(ctx, cfg)
		if err != nil {
			mu.Lock()
			ready = err
			mu.Unlock()
			log.Printf("backend init failed: %v", err)
			return
		}
		if err := p.Migrate(ctx); err != nil {
			p.Close()
			mu.Lock()
			ready = err
			mu.Unlock()
			log.Printf("migrate failed: %v", err)
			return
		}
		if err := s.EnsureBucket(ctx); err != nil {
			p.Close()
			mu.Lock()
			ready = err
			mu.Unlock()
			log.Printf("ensure bucket failed: %v", err)
			return
		}
		mu.Lock()
		if lin != nil {
			lin.Close()
		}
		store, lin, ready = s, p, nil
		mu.Unlock()
		log.Printf("backends ready (postgres + s3 bucket %q)", cfg.S3Bucket)
	}
	go initBackends()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"node":%q,"phase":1}`, cfg.NodeID)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		err := ready
		l := lin
		s := store
		mu.Unlock()
		if err != nil || l == nil || s == nil {
			if err == nil {
				err = fmt.Errorf("backends not initialized")
			}
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		cctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := l.EnsureRepo(cctx, "_healthcheck"); err != nil {
			http.Error(w, "postgres not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if _, err := s.List(cctx, "", 1); err != nil {
			http.Error(w, "s3 not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ready")
	})
	mux.Handle("/git/", gitserver.Handler{ReposDir: filepath.Join(cfg.DataDir, "repos")})
	mux.HandleFunc("/api/repos/", func(w http.ResponseWriter, r *http.Request) {
		relative := strings.TrimPrefix(r.URL.Path, "/api/repos/")
		repoID, action, _ := strings.Cut(relative, "/")
		if err := repository.ValidateID(repoID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		l, s, backendErr := lin, store, ready
		mu.Unlock()
		if backendErr != nil || l == nil || s == nil {
			http.Error(w, "backends not ready", http.StatusServiceUnavailable)
			return
		}
		m, err := manager(cfg, s)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		switch {
		case r.Method == http.MethodPost && action == "":
			if err := l.EnsureRepo(r.Context(), repoID); err == nil {
				err = m.Create(r.Context(), repoID)
			}
		case r.Method == http.MethodDelete && action == "local":
			err = m.DeleteLocal(repoID)
		case r.Method == http.MethodPost && action == "materialize":
			var head types.RepoHead
			head, err = l.GetRepoHead(r.Context(), repoID)
			if err == nil {
				err = m.Materialize(r.Context(), repoID, head.CurrentSeq, head.LastWALHash)
			}
		default:
			http.Error(w, "method or action not supported", http.StatusMethodNotAllowed)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: mux}
	go func() {
		log.Printf("continuum phase1 listening on %s (node=%s)", cfg.ListenAddr, cfg.NodeID)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	mu.Lock()
	if lin != nil {
		lin.Close()
	}
	mu.Unlock()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// runSmoke proves Phase 0 acceptance:
// 1) MinIO/S3 put + get round-trip
// 2) Postgres per-ref CAS create / update / conflict
func runSmoke() error {
	cfg, err := load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, lin, err := openBackends(ctx, cfg)
	if err != nil {
		return err
	}
	defer lin.Close()

	if err := lin.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := store.EnsureBucket(ctx); err != nil {
		return fmt.Errorf("ensure bucket: %w", err)
	}

	if err := smokeStorage(ctx, store); err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	if err := smokeLinearizer(ctx, lin); err != nil {
		return fmt.Errorf("linearizer: %w", err)
	}
	return nil
}

func smokeStorage(ctx context.Context, store storage.Storage) error {
	key := fmt.Sprintf("phase0/smoke/%d.txt", time.Now().UnixNano())
	payload := []byte("continuum-phase0-hello")

	etag, err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), storage.PutOptions{
		ContentType: "text/plain",
	})
	if err != nil {
		return fmt.Errorf("put: %w", err)
	}
	log.Printf("storage put ok key=%s etag=%s", key, etag)

	rc, meta, err := store.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if !bytes.Equal(got, payload) {
		return fmt.Errorf("round-trip mismatch: got %q want %q", got, payload)
	}
	log.Printf("storage get ok key=%s size=%d", meta.Key, meta.Size)

	// Conditional create-if-absent should fail on existing key (when supported).
	_, err = store.Put(ctx, key, bytes.NewReader([]byte("nope")), 4, storage.PutOptions{
		ContentType:     "text/plain",
		IfNoneMatchStar: true,
	})
	if err == nil {
		log.Printf("storage warning: If-None-Match:* did not reject overwrite (provider may ignore)")
	} else if !errors.Is(err, storage.ErrPreconditionFailed) {
		return fmt.Errorf("expected precondition failed, got: %w", err)
	} else {
		log.Printf("storage conditional put ok (precondition failed as expected)")
	}

	if err := store.Delete(ctx, key); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}

func smokeLinearizer(ctx context.Context, lin linearizer.Linearizer) error {
	repo := fmt.Sprintf("repo_smoke_%d", time.Now().UnixNano())
	ref := "refs/heads/main"
	oid1 := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oid2 := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	oid3 := "cccccccccccccccccccccccccccccccccccccccc"

	if err := lin.EnsureRepo(ctx, repo); err != nil {
		return fmt.Errorf("ensure repo: %w", err)
	}

	// Create ref via CAS (expect zero).
	st, err := lin.CompareAndSwap(ctx, repo, ref, types.ZeroOID, 0, oid1)
	if err != nil {
		return fmt.Errorf("create cas: %w", err)
	}
	if st.OID != oid1 || st.Version != 1 {
		return fmt.Errorf("create cas unexpected state: %+v", st)
	}
	log.Printf("cas create ok repo=%s oid=%s version=%d", repo, st.OID, st.Version)

	// Happy-path update.
	st, err = lin.CompareAndSwap(ctx, repo, ref, oid1, 1, oid2)
	if err != nil {
		return fmt.Errorf("update cas: %w", err)
	}
	if st.OID != oid2 || st.Version != 2 {
		return fmt.Errorf("update cas unexpected state: %+v", st)
	}
	log.Printf("cas update ok oid=%s version=%d", st.OID, st.Version)

	// Conflict: stale expected oid/version (Alice won; Bob loses).
	_, err = lin.CompareAndSwap(ctx, repo, ref, oid1, 1, oid3)
	if !errors.Is(err, linearizer.ErrConflict) {
		return fmt.Errorf("expected conflict, got: %v", err)
	}
	log.Printf("cas conflict ok (stale base rejected)")

	got, err := lin.GetRef(ctx, repo, ref)
	if err != nil {
		return fmt.Errorf("get ref: %w", err)
	}
	if got.OID != oid2 || got.Version != 2 {
		return fmt.Errorf("get ref mismatch: %+v", got)
	}

	// Disjoint refs must not block each other (per-ref CAS).
	docs := "refs/heads/docs"
	stDocs, err := lin.CompareAndSwap(ctx, repo, docs, types.ZeroOID, 0, oid3)
	if err != nil {
		return fmt.Errorf("docs create: %w", err)
	}
	if stDocs.Version != 1 {
		return fmt.Errorf("docs version want 1 got %d", stDocs.Version)
	}
	log.Printf("cas per-ref ok (main + docs independent)")

	refs, err := lin.ListRefs(ctx, repo)
	if err != nil {
		return err
	}
	if len(refs) != 2 {
		return fmt.Errorf("want 2 refs, got %d", len(refs))
	}
	return nil
}
