package linearizer_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/adityaraj-09/Continuum/engine/internal/linearizer"
	"github.com/adityaraj-09/Continuum/engine/internal/types"
)

func TestPostgresCASIntegration(t *testing.T) {
	dsn := os.Getenv("CONTINUUM_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CONTINUUM_POSTGRES_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pg, err := linearizer.NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pg.Close()
	if err := pg.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	repo := "repo_test_" + time.Now().Format("150405.000")
	ref := "refs/heads/main"
	oid1 := "1111111111111111111111111111111111111111"
	oid2 := "2222222222222222222222222222222222222222"

	st, err := pg.CompareAndSwap(ctx, repo, ref, types.ZeroOID, 0, oid1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if st.Version != 1 || st.OID != oid1 {
		t.Fatalf("unexpected create state: %+v", st)
	}

	_, err = pg.CompareAndSwap(ctx, repo, ref, types.ZeroOID, 0, oid2)
	if !errors.Is(err, linearizer.ErrConflict) {
		t.Fatalf("expected conflict on stale create, got %v", err)
	}

	st, err = pg.CompareAndSwap(ctx, repo, ref, oid1, 1, oid2)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if st.Version != 2 || st.OID != oid2 {
		t.Fatalf("unexpected update state: %+v", st)
	}
}
