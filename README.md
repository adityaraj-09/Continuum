# Continuum

A GitHub-style code hosting platform built on a Cursor "Origin/Continuity"-style storage engine:
an **S3 write-ahead log (WAL) is the source of truth**, and normal Git repositories on local NVMe
are treated as fast, disposable, **materialized caches**.

## Docs

- Architecture & technical plan: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)

## Layer 1 — Phase 0 (current)

Phase 0 proves the two backends the engine needs before any real `git push`:

1. **MinIO / S3** — object put/get (cold pack + WAL store)
2. **Postgres per-ref CAS** — create / update / conflict (commit point)

### Layout

```
engine/                 Go module (Layer 1)
  cmd/continuum/        CLI: serve | smoke | migrate
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

# prove Phase 0 acceptance (MinIO put/get + Postgres per-ref CAS)
export CONTINUUM_POSTGRES_DSN='postgres://continuum:continuum@127.0.0.1:5432/continuum?sslmode=disable'
export CONTINUUM_S3_ENDPOINT='http://127.0.0.1:9000'
export CONTINUUM_S3_BUCKET=continuum
export CONTINUUM_S3_ACCESS_KEY=minioadmin
export CONTINUUM_S3_SECRET_KEY=minioadmin
export CONTINUUM_S3_PATH_STYLE=true
go run ./engine/cmd/continuum smoke
```

> Note: the engine service uses `network_mode: host` in Compose so it can reach
> published Postgres/MinIO ports reliably in constrained environments.
