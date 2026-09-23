#!/usr/bin/env bash
# Runnable example for the #8361 report-only Flue binding pilot.
#
# 1. Starts the deterministic Flue fixture as a second local process.
# 2. Walks its native surface with curl: info, keyed dispatch (twice, to show
#    deduplication), a conflicting payload, three stage ticks, status, and the
#    receipt artifact.
# 3. Runs the conformance suite, which drives the same fixture through the
#    real adapter from a fresh process.
#
# Needs only Go and curl. No network, no model, no GitHub token.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SRC="$ROOT/src"
WORKFLOW="$SRC/pkg/extwork/flue/testdata/flue-fixture"
STATE="$(mktemp -d)"
trap 'kill "${FIXTURE_PID:-}" 2>/dev/null || true; rm -rf "$STATE"' EXIT

cd "$SRC"
go run ./cmd/flue-fixture -workflow "$WORKFLOW" -state "$STATE" -incarnation example > "$STATE/fixture.out" &
FIXTURE_PID=$!
for _ in $(seq 1 50); do
  ADDR="$(sed -n 's/^FLUE_FIXTURE_ADDR //p' "$STATE/fixture.out" 2>/dev/null || true)"
  [ -n "$ADDR" ] && break
  sleep 0.1
done
[ -n "${ADDR:-}" ] || { echo "fixture did not start" >&2; exit 1; }
echo "fixture at $ADDR"

BUNDLE='{"admission":{"work_key":"github:hivecommons/hive#8361","assignment_id":"task-example","generation":1,"stage":"implement","contract_revision":"contract-1","input_revision":"9f1c2d3e4a5b6c7d8e9f0a1b2c3d4e5f60718293"},"summary":"example bundle","repo":"hivecommons/flue-fixture"}'
PAYLOAD="$(printf '%s' "$BUNDLE" | base64 | tr -d '\n')"
OTHER="$(printf '%s' "${BUNDLE/example bundle/changed bundle}" | base64 | tr -d '\n')"

echo "--- runtime info"; curl -s "$ADDR/"; echo
echo "--- keyed dispatch"; curl -s -X POST "$ADDR/dispatch" -d "{\"idempotency_key\":\"example-key\",\"payload\":\"$PAYLOAD\"}"; echo
echo "--- same key, same payload (deduplicated)"; curl -s -X POST "$ADDR/dispatch" -d "{\"idempotency_key\":\"example-key\",\"payload\":\"$PAYLOAD\"}"; echo
echo "--- same key, changed payload (409 submission_conflict)"; curl -s -w ' %{http_code}' -X POST "$ADDR/dispatch" -d "{\"idempotency_key\":\"example-key\",\"payload\":\"$OTHER\"}"; echo
for i in 1 2 3; do
  echo "--- tick $i"; curl -s -X POST "$ADDR/control/tick"; echo
  curl -s "$ADDR/submissions?key=example-key"; echo
done
ID="$(curl -s "$ADDR/submissions?key=example-key" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
echo "--- receipt artifact"; curl -s "$ADDR/submissions/$ID/artifacts/receipt.json"; echo
echo "--- stats (effects attempted without a route are no_route; the tests prove refusal through an egress proxy)"; curl -s "$ADDR/control/stats"; echo

echo "--- conformance suite against a fresh fixture process"
go test ./pkg/extwork/flue -run 'TestConformance' -count=1 -v
