# Continuum

A GitHub-style code hosting platform built on a Cursor "Origin/Continuity"-style storage engine:
an **S3 write-ahead log (WAL) is the source of truth**, and normal Git repositories on local NVMe
are treated as fast, disposable, **materialized caches**.

## Docs

- Architecture & technical plan: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)

## Layer 1 — Phase 1 (current)

Phase 1 accepts real Git smart-HTTP pushes and makes each push recoverable:

1. Git validates/de-thins incoming objects with `receive-pack`.
2. Content-addressed packs and a hash-chained WAL entry are written to MinIO/S3.
3. Postgres atomically validates old OIDs, assigns a sequence, and publishes refs.
4. A deleted bare repository can be materialized from S3 WAL + packs.

### Layout

```
engine/                 Go module (Layer 1)
  cmd/continuum/        CLI and reference-transaction hook entrypoint
  internal/gitserver/   system Git smart-HTTP gateway
  internal/push/        pack persistence + push coordinator
  internal/repository/  bare repository lifecycle + materializer
  internal/wal/         immutable hash-chained WAL
  internal/storage/     Storage interface + S3/MinIO
  internal/linearizer/  Linearizer interface + Postgres CAS
  internal/types/       WAL / ref domain types
  migrations/           SQL schema
deploy/docker-compose.yml
```

### Run locally

```bash
# start MinIO + Postgres + Valkey + engine
docker compose -f deploy/docker-compose.yml up -d --build

# health
curl -s localhost:8080/healthz
curl -s localhost:8080/readyz

# prove Phase 0 backend acceptance (MinIO put/get + Postgres per-ref CAS)
export CONTINUUM_POSTGRES_DSN='postgres://continuum:continuum@127.0.0.1:5432/continuum?sslmode=disable'
export CONTINUUM_S3_ENDPOINT='http://127.0.0.1:9000'
export CONTINUUM_S3_BUCKET=continuum
export CONTINUUM_S3_ACCESS_KEY=minioadmin
export CONTINUUM_S3_SECRET_KEY=minioadmin
export CONTINUUM_S3_PATH_STYLE=true
go run ./engine/cmd/continuum smoke

# prove Phase 1: push twice → wipe local repo → rebuild → clone same commit
./engine/scripts/phase1-e2e.sh
```

> Note: the engine service uses `network_mode: host` in Compose so it can reach
> published Postgres/MinIO ports reliably in constrained environments.
