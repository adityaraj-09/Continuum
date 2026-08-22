# Continuum

A GitHub-style code hosting platform built on a Cursor "Origin/Continuity"-style storage engine:
an **S3 write-ahead log (WAL) is the source of truth**, and normal Git repositories on local NVMe
are treated as fast, disposable, **materialized caches**.

See the technical plan and architecture discussion: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).
