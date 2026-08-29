#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BASE_URL="${CONTINUUM_URL:-http://127.0.0.1:8080}"
REPO="phase1-e2e-$(date +%s%N)"
CLIENT="$(mktemp -d)"
CLONE="$(mktemp -d)"
trap 'rm -rf "$CLIENT" "$CLONE"' EXIT

for _ in $(seq 1 30); do
  if curl -fsS "$BASE_URL/readyz" >/dev/null; then
    break
  fi
  sleep 1
done
curl -fsS "$BASE_URL/readyz" >/dev/null

curl -fsS -X POST "$BASE_URL/api/repos/$REPO"
git init -q -b main "$CLIENT"
git -C "$CLIENT" config user.name "Continuum E2E"
git -C "$CLIENT" config user.email "continuum-e2e@example.invalid"
printf 'phase one survives local loss\n' >"$CLIENT/README.md"
git -C "$CLIENT" add README.md
git -C "$CLIENT" commit -q -m "initial"
expected="$(git -C "$CLIENT" rev-parse HEAD)"
git -C "$CLIENT" remote add origin "$BASE_URL/git/$REPO.git"

# Real Git smart-HTTP receive-pack.
git -C "$CLIENT" push -q -u origin main

# A second push proves ordered WAL replay rather than only genesis recovery.
printf 'second durable push\n' >>"$CLIENT/README.md"
git -C "$CLIENT" add README.md
git -C "$CLIENT" commit -q -m "second"
expected="$(git -C "$CLIENT" rev-parse HEAD)"
git -C "$CLIENT" push -q origin main

# Destroy the materialized source of the successful push, rebuild exclusively
# from S3 WAL + packs, then prove upload-pack returns the same history.
curl -fsS -X DELETE "$BASE_URL/api/repos/$REPO/local"
curl -fsS -X POST "$BASE_URL/api/repos/$REPO/materialize"
git clone -q "$BASE_URL/git/$REPO.git" "$CLONE"
actual="$(git -C "$CLONE" rev-parse HEAD)"

test "$actual" = "$expected"
test "$(cat "$CLONE/README.md")" = $'phase one survives local loss\nsecond durable push'

printf 'Phase 1 E2E OK\nrepo=%s\ncommit=%s\n' "$REPO" "$actual"
