package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/adityaraj-09/Continuum/engine/internal/config"
	"github.com/adityaraj-09/Continuum/engine/internal/linearizer"
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
		fmt.Fprintf(w, `{"ok":true,"node":%q,"phase":0}`, cfg.NodeID)
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

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: mux}
	go func() {
		log.Printf("continuum phase0 listening on %s (node=%s)", cfg.ListenAddr, cfg.NodeID)
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
