# Continuum — Architecture & Technical Plan

A GitHub-style code hosting platform built on a **Cursor "Origin/Continuity"-style storage engine**:
an S3 write-ahead log (WAL) is the source of truth, and normal Git repositories on local
NVMe are treated as fast, disposable, materialized caches.

This document does two things:

1. **Reviews and corrects** the proposed architecture (the transcript) — what is right, what is
   under-specified, and what I would change.
2. Lays out a **concrete, phased technical plan** to build it.

---

## 0. TL;DR

The transcript's core model is correct and faithful to Cursor's published design:

- **S3 WAL = source of truth.** Each push is a durable WAL entry: `{repo, seq, ref updates (old→new), pack pointer, checksum, metadata}`.
- **Local Git on NVMe = warm cache**, materialized from `snapshot + WAL tail`.
- **Publication (visibility) is decoupled from durability**: a push is durable when its pack + WAL entry are in S3, but only *visible* once the authoritative index advances (atomic CAS) and the local ref transaction is applied.
- **Compaction/snapshots** keep replay bounded; **gossip + S3 index ETag** give replication and read freshness; **rendezvous hashing** routes without a routing DB.

The things I would **change or make explicit** for our build (details in §3):

1. **Make the CAS commit point unambiguous and put it *before* the local ref update.** The linearization point is the successful compare-and-swap on the index HEAD; local ref application is materialization, not commitment.
2. **Keep the "index" object tiny** (a HEAD pointer: `{committed_seq, etag, snapshot ptr}`), not a growing list of all entries. Derive the entry list by listing the `wal/` prefix.
3. **Handle thin packs explicitly** (`git index-pack --fix-thin`). Pushes send delta-against-assumed-base "thin" packs; a stored WAL pack must be self-contained (or provably applicable against prior state) or replay/materialization breaks. The transcript omits this and it is a real correctness bug if ignored.
4. **Separate "durability store" (S3) from "linearizer".** Two valid options: (a) *pure S3 conditional writes* (faithful, fewer moving parts, but conditional-write semantics vary by provider); (b) *external strongly-consistent CAS* (DynamoDB / Postgres / FoundationDB / etcd). I recommend building against a small `Linearizer` interface so we can start with S3 conditional writes and swap in a metadata store if needed.
5. **Single-writer-per-repo via routing is the common path; CAS is the safety net** for the failover race. Don't rely on CAS for throughput.
6. **Treat the "GitHub app" and the "Git storage engine" as two clean layers.** The engine only knows `repo_id`, packs, refs, WAL. Users/PRs/issues/permissions live in a separate relational metadata service.

---

## 1. What we are actually building (two layers)

```
┌──────────────────────────────────────────────────────────────┐
│  Layer 2: "GitHub" application                                 │
│  web UI · REST/GraphQL API · auth · repos/PRs/issues/reviews   │
│  webhooks · CI hooks · code browsing · search                  │
│         (state in a relational DB: Postgres)                   │
└───────────────┬──────────────────────────────────────────────┘
                │  repo_id, permissions, ref reads
                ▼
┌──────────────────────────────────────────────────────────────┐
│  Layer 1: Continuum storage engine ("Origin"-like)             │
│  git gateway (SSH/HTTPS) · receive-pack/upload-pack            │
│  WAL writer · materializer · compaction · replication          │
│      source of truth = S3 (WAL + packs + snapshots + index)    │
│      warm cache        = normal bare Git repos on NVMe         │
└──────────────────────────────────────────────────────────────┘
```

Most of the novelty and risk is in **Layer 1**. Layer 2 is a "normal" web app that must be careful to
treat Git ref reads as *possibly-stale-unless-verified* (see §9). We build Layer 1 first and prove it,
then wrap Layer 2 around it.

---

## 2. Core concepts (refined schemas)

### 2.1 WAL entry (one per push)

Stored as a small JSON/protobuf object at `repos/<repo_id>/wal/<seq>`.

```jsonc
{
  "schema": 1,
  "repo_id": "repo_123",
  "seq": 1842,                    // monotonic, gap-free per repo
  "push_id": "01J...ULID",        // idempotency key
  "parent_seq": 1841,             // must equal committed_seq at commit time (CAS base)
  "created_at": "2026-08-22T16:00:00Z",
  "actor": { "user_id": "u_9", "via": "ssh" },   // provenance
  "ref_updates": [
    { "ref": "refs/heads/main", "old": "8f31…", "new": "a92b…" }
    // old = 40 zeros  => create ; new = 40 zeros => delete
  ],
  "packs": [
    { "key": "repos/repo_123/packs/push_01J….pack",
      "size": 918273, "sha256": "…", "thin_fixed": true }
    // may be empty for delete-only or ref-only updates
  ]
}
```

Notes / corrections vs. transcript:
- `parent_seq` makes the CAS base explicit and gives us idempotency + gap-free ordering.
- `packs` is a **list** and may be **empty** (ref deletions, or updates that need no new objects).
- `thin_fixed: true` records that we already ran `index-pack --fix-thin` so the pack is standalone.
- `actor` gives the provenance the transcript sells in §32.

### 2.2 Index / HEAD (the linearization object) — keep it tiny

Stored at `repos/<repo_id>/index` (or in the linearizer store). **Do not** store the full list of entries here.

```jsonc
{
  "repo_id": "repo_123",
  "committed_seq": 1842,          // authoritative latest committed push
  "snapshot_seq": 1800,           // nearest snapshot for fast materialization
  "refs": {                       // authoritative ref map at committed_seq (optional cache)
    "refs/heads/main": "a92b…"
  }
}
```

The full history is `list(repos/<repo_id>/wal/)`; the HEAD object only needs the current sequence,
the snapshot pointer, and (optionally, as an optimization) the current ref map so cold readers don't
have to replay the tail just to answer "what is main?".

CAS rule: advance `index` from `committed_seq = N` to `N+1` **iff** the current ETag matches the one
we read. That conditional PUT is **the commit point**.

### 2.3 Snapshot manifest

Stored at `repos/<repo_id>/snapshots/<seq>/manifest.json`, alongside the packs/idx it references.

```jsonc
{
  "repo_id": "repo_123",
  "snapshot_seq": 1800,
  "refs": { "refs/heads/main": "…", "refs/tags/v1": "…" },
  "packs": [ { "pack": "pack-A.pack", "idx": "pack-A.idx", "sha256": "…" } ],
  "created_by": "node_A",
  "created_at": "…"
}
```

---

## 3. Key design decisions (the "discussion")

### 3.1 The commit point and correct ordering

The single most important thing to get right. The pipeline must be:

```
1. receive pack into quarantine (temp dir, objects NOT reachable)
2. validate + de-thin: git index-pack --fix-thin  → self-contained pack, connectivity check
3. upload pack(s) to S3            (durable data)
4. upload WAL entry object to S3   (durable record, at seq = committed_seq+1)
5. CAS index: committed_seq N → N+1 (If-Match ETag)   ◄── COMMIT POINT
        └─ fail? someone else committed. Reject (non-ff) or rebase our seq and retry from 4.
6. apply local ref transaction (git update-ref) to materialized repo
7. gossip "repo now at N+1"
8. ACK the client
```

Corrections vs. transcript:
- The transcript's "two-stage: durable → publish ref → then CAS" is ambiguous about which write is
  authoritative. **The CAS is authoritative.** Local `update-ref` happens *after* CAS success, so a node
  that loses the race never exposes a ref that isn't committed.
- Steps 3+4 can run **in parallel**, but **5 must not start until both succeed** (the §30 invariant:
  "if the index says committed, everything to reconstruct it is already durable").
- ACK only after CAS success (Cursor: don't ack until fully persisted).

### 3.2 Thin packs (a real correctness issue the transcript misses)

`git push` sends a **thin pack**: it omits base objects it assumes the server already has and encodes
new objects as deltas against them. If we naively store that pack and later try to apply it to a
freshly materialized repo that doesn't yet contain those bases, it will fail. Fix: run
`git index-pack --fix-thin --stdin` on receipt, which appends the needed base objects and produces a
**self-contained** pack we can safely store and replay. We record `thin_fixed: true`.

### 3.3 Where does linearization live? S3 CAS vs. external store

The transcript hand-waves S3 conditional writes. Reality:
- **Pure S3 conditional writes** are the most faithful ("S3 is the *only* source of truth"). AWS S3
  supports `If-None-Match: *` (create-if-absent) and `If-Match: <etag>` conditional PUTs; MinIO and
  other providers vary. Pro: no extra system. Con: per-object write contention, provider-dependent.
- **External linearizer** (DynamoDB conditional update, Postgres row w/ `version` column, FoundationDB,
  etcd): trivially strong CAS, easy transactions, easy to reason about. Con: introduces a second
  "source of truth" for *ordering* (S3 still owns *data* durability).

**Decision:** define a narrow interface and start with S3 conditional writes to stay faithful; keep a
Postgres-backed implementation ready as a fallback / for local dev.

```
type Linearizer interface {
    Read(repo) (committedSeq, token)          // token = ETag or version
    CompareAndSwap(repo, expectToken, newHead) (ok, newToken)
}
```

Because routing gives us **single-writer-per-repo** in the common case, CAS contention is rare (only
during failover / rebalancing), so either backend is fine for the MVP.

### 3.4 Single-writer routing + CAS as safety net

Rendezvous hashing (`argmax_node hash(repo_id, node_id)`) picks the owner node for a repo. That node
serializes pushes with a local per-repo lock — cheap and fast. CAS only matters when two nodes briefly
believe they own the same repo (deploys, network partitions, node death). So: **routing for throughput,
CAS for safety.** Don't design the hot path around cross-node CAS.

### 3.5 Reads must be freshness-checked, not trusted blindly

A materialized node may lag. Before serving a ref-dependent read (clone/fetch tip, "what is main?",
web code browsing), the node compares its local applied seq to the authoritative `committed_seq`
(cheap: a `HEAD` object GET / ETag check, or trust a recent gossip). If behind, catch up (apply WAL
tail) then serve. To avoid an S3 round-trip on every read we use: gossip push notifications + a short
freshness TTL + on-demand verification for strong reads.

### 3.6 Garbage collection & retention (transcript under-specifies)

WAL entries and push packs before the latest snapshot are redundant for *materialization*, but valuable
for *provenance/time-travel*. Policy: after a snapshot at seq S is durable and verified, WAL entries and
push packs with `seq < S` become eligible for GC **after a retention window** (e.g. keep N days / M
snapshots for audit + rewind). Also handle objects orphaned by force-push. GC must be safe under
concurrent materialization (never delete something a lagging replica still needs — gate on min replica
seq or retention window).

### 3.7 Two clean layers, one boundary

The engine's public contract to Layer 2 is small: create repo, authorize + stream `receive-pack` /
`upload-pack`, read ref map at committed seq, read blobs/trees/commits (for browsing), stream WAL for
audit. Everything user-facing (PRs, issues, permissions, orgs) lives in Layer 2's Postgres and never
touches the WAL semantics directly.

---

## 4. Tech stack (recommended)

| Concern | Choice | Why |
|---|---|---|
| Storage engine + gateway | **Go** | first-class S3 SDK, easy `os/exec` to git, great concurrency, static binaries |
| Git | **system `git`** invoked via CLI (`receive-pack`, `upload-pack`, `index-pack`, `update-ref`, `repack`) | never reimplement pack parsing/validation |
| Object storage | **MinIO** locally, **S3** in prod | S3-compatible, supports conditional writes |
| Linearizer | S3 conditional writes → interface → **Postgres** fallback | faithful default, pragmatic fallback |
| App metadata | **Postgres** | users/repos/PRs/issues/permissions |
| Gossip | **UDP** (or SWIM lib) for notifications; **not** source of truth | matches design |
| API backend | **Go** (or Node/TS) | shares types with engine |
| Web UI | **Next.js + React + Tailwind** | GitHub-like UI quickly |
| Git transport | **HTTPS smart protocol** first, then **SSH** | HTTPS is easiest to auth/proxy |
| Container/dev | **Docker Compose** (MinIO + Postgres + N nodes) | reproducible multi-node local |
| Tests | Go test + shell integration harness driving real `git push/clone` | prove end-to-end |

---

## 5. S3 layout

```
continuum/
└── repos/<repo_id>/
    ├── index                       # tiny HEAD object (CAS target) — see §2.2
    ├── wal/<seq>                    # WAL entry objects (zero-padded seq)
    ├── packs/push_<id>.pack        # per-push packfiles (fix-thin'd)
    └── snapshots/<seq>/
        ├── manifest.json
        ├── pack-*.pack
        └── pack-*.idx
```

Local NVMe per node:

```
/var/lib/continuum/repos/<repo_id>.git/   # normal bare repo
/var/lib/continuum/state/<repo_id>.json   # {applied_seq}
/tmp/continuum/quarantine/<push_id>/      # incoming, pre-commit
```

---

## 6. Pipelines

### 6.1 Push (authoritative) — see §3.1 for the ordered steps and the commit point.

### 6.2 Fetch / clone
`upload-pack` served from the local materialized repo, **after** a freshness check (§3.5). Cold node with
no local copy → materialize first (§6.3), then serve.

### 6.3 Materialize (given repo + target seq)
```
read HEAD → committed_seq, snapshot_seq
download snapshot/<snapshot_seq> → git init + index-pack the snapshot packs + set refs from manifest
for seq in (snapshot_seq+1 .. committed_seq):
    download WAL entry; download its pack(s); git index-pack (install objects)
    apply ref_updates via git update-ref (respecting old→new)
write state.applied_seq = committed_seq
```
This is the transcript's §17/§22, made precise.

### 6.4 Compaction (owner/primary only)
`git repack -adf` (or `git gc`) the local repo → upload resulting packs+idx + manifest to
`snapshots/<committed_seq>/` → advance `snapshot_seq` in HEAD (CAS). Replicas then consume the snapshot
instead of independently repacking. Old WAL/packs become GC-eligible per retention (§3.6).

---

## 7. Replication, consistency, routing (summary of §24–27, made concrete)

- **Replication:** owner writes WAL + CAS + gossips `repo@seq`. Replicas apply the tail lazily. S3 is
  the truth; gossip is a hint. Lost packet → replica reconciles from S3 on next read/verify.
- **Consistency:** strong reads verify against HEAD ETag; eventual reads trust recent gossip within TTL.
- **Routing:** rendezvous hashing chooses owner; on owner death the next node materializes and takes over.
  No routing DB needed; membership via gossip/registry.

---

## 8. Layer 2 (the "GitHub") — scope

Postgres-backed services, built after the engine is proven:

- **Auth/identity:** users, orgs, teams, SSH keys, personal access tokens, OAuth login.
- **Repos:** metadata, visibility, permissions (map to engine `repo_id`); repo create/delete/rename.
- **Git transport auth:** gateway authenticates HTTPS/SSH, checks per-repo permission, then proxies to
  `receive-pack`/`upload-pack`.
- **Code browsing:** read trees/blobs/commits from the materialized repo via the engine read API.
- **Collaboration:** pull requests (diff = merge-base..head using Git), reviews/comments, issues, labels.
- **Automation:** webhooks + pre-/post-receive hooks (can inspect the WAL actor/refs), CI trigger stubs.
- **Search:** commit/code search (start naive; later a real index).

PRs reuse the engine cleanly: a PR is just two refs (base, head); merge produces a new push (new WAL entry).

---

## 9. Correctness invariants (must always hold)

1. **Durability-before-visibility:** if HEAD says `committed_seq = N`, then WAL entry N and all packs it
   references are already durable in S3. (Enforced by ordering: data → WAL → CAS.)
2. **Gap-free monotonic seq per repo:** `parent_seq == committed_seq` required at CAS.
3. **Self-contained packs:** every stored pack is fix-thin'd (or provably applicable against `< seq` state).
4. **Ref CAS:** apply `old→new` only if current matches `old` (Git's own check + our WAL record).
5. **Idempotency:** replay of a `push_id` (client retry, crash after CAS but before ACK) must not double-commit.
6. **GC safety:** never delete data still required by the retention window or any lagging replica.

---

## 10. Phased roadmap (with acceptance criteria)

### Phase 0 — Foundations
- Docker Compose: MinIO + Postgres + 1 node. `Storage` (S3) and `Linearizer` interfaces defined.
- **Accept:** `git init --bare` repo on disk; can put/get/conditional-put to MinIO.

### Phase 1 — Single-node core (the transcript's "prove it")
Git receive layer + WAL writer + materializer, one repo, one node, one bucket.
- **Accept (the money test):**
  `git push` → pack+WAL in S3 → CAS advances HEAD → delete local repo → **materialize from S3** →
  `git log` / `git clone` return the pushed history. (Transcript §33.)
- Includes: quarantine, `index-pack --fix-thin`, connectivity validation, WAL entry write, HEAD CAS,
  local ref transaction, ACK ordering, idempotency by `push_id`.

### Phase 2 — Fetch/clone + freshness
Serve `upload-pack`; cold materialize on demand; strong-read HEAD verification.
- **Accept:** clone from a node that never had the repo; force-push then re-clone shows correct tip.

### Phase 3 — Multi-node + CAS races + gossip replication
2–3 nodes, rendezvous routing, UDP gossip, replica catch-up, concurrent-push CAS conflict test.
- **Accept:** two nodes racing the same base → exactly one wins, loser gets clean non-ff rejection;
  replica serves correct data after gossip and after simulated lost gossip (S3 reconcile).

### Phase 4 — Snapshots, compaction, GC
Owner repack→snapshot upload→HEAD snapshot_seq CAS; replicas consume snapshot; retention-based GC.
- **Accept:** repo with many pushes materializes from `snapshot + short tail`, not full replay; GC frees
  pre-snapshot data without breaking materialization or lagging replicas.

### Phase 5 — Git transport auth + minimal web
HTTPS smart protocol with token auth; permission checks; minimal web UI (repo list, file/commit browse).
- **Accept:** authenticated push/clone over HTTPS honoring permissions; browse code in the UI.

### Phase 6 — Collaboration (GitHub features)
Repos/orgs/teams, SSH keys, PRs (+diff/merge → new push), issues, comments, webhooks, basic search.
- **Accept:** open PR from branch, review, merge → merge commit appears as a new WAL entry; issues/webhooks work.

### Phase 7 — Hardening
Provenance/time-travel (rewind a replica to seq N), audit views, chaos tests (kill owner mid-push),
metrics/tracing, backpressure/quotas, large-repo handling.
- **Accept:** kill owner between S3-durable and ACK → client retry is idempotent, no corruption; can
  rewind/inspect a repo to any historical seq.

---

## 11. Top risks & mitigations

| Risk | Mitigation |
|---|---|
| Thin packs break replay | `index-pack --fix-thin` on receipt; assert self-contained |
| S3 conditional-write semantics vary by provider | `Linearizer` interface; Postgres fallback; test on target provider |
| Torn state on crash between S3-durable and CAS | CAS is the only commit; uncommitted WAL/packs are inert & GC'd |
| Crash between CAS and ACK | idempotent `push_id`; client retry re-observes committed state |
| Read serves stale ref | freshness check vs HEAD ETag for strong reads |
| Unbounded replay | snapshots + compaction (Phase 4) |
| GC deletes needed data | retention window + min-replica-seq gate |
| Reinventing Git | always delegate to the `git` binary for pack/ref/connectivity |

---

## 12. Summary

The transcript's model is sound and worth building. Our refinements are: make the **CAS the explicit
commit point (before local ref apply)**, keep the **index object tiny**, **de-thin packs**, **abstract the
linearizer** (S3 conditional writes with a Postgres fallback), lean on **routing for throughput and CAS
for safety**, verify **read freshness**, and add a real **GC/retention** story. Build Layer 1 to the
"push → S3 → wipe → materialize → clone" acceptance test first (Phase 1); everything else — multi-node,
snapshots, and the GitHub app — layers cleanly on top.
