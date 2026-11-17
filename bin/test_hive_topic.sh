#!/usr/bin/env bash
# Tests for bin/hive-topic.sh — topic claims from the hive pod or a sandbox
# (hivecommons/hive#18572).
#
# Run: bash bin/test_hive_topic.sh
set -euo pipefail

PASS=0
FAIL=0

SCRIPT="$(cd "$(dirname "$0")" && pwd)/hive-topic.sh"

check() {
  local label="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then
    echo "  PASS: $label"; PASS=$((PASS + 1))
  else
    echo "  FAIL: $label"; echo "        want: '$want'"; echo "        got:  '$got'"; FAIL=$((FAIL + 1))
  fi
}

echo "=== hive-topic.sh tests ==="

TMPDIR_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_ROOT"' EXIT
KEY="0123456789abcdef0123456789abcdef"
SPOOL="$TMPDIR_ROOT/workspace/.hive/review-requests"

# Run the relay as a sandbox would, with the spool and key in its env.
in_sandbox() { env HIVE_AGENT=deps HIVE_SANDBOX_REQUEST_DIR="$SPOOL" HIVE_SANDBOX_SPOOL_KEY="$KEY" bash "$SCRIPT" "$@"; }
newest() { { ls -t "$SPOOL"/deps-topic-*.json 2>/dev/null || true; } | { grep -v '\.result\.json$' || true; } | head -1; }
reqfield() { python3 -c 'import json,sys; print(json.loads(json.load(open(sys.argv[1]))["request"]).get(sys.argv[2], ""))' "$1" "$2"; }

echo ""
echo "--- in-pod: runs bd topic ---"
FAKEBIN="$TMPDIR_ROOT/bin"; mkdir -p "$FAKEBIN"
printf '#!/bin/sh\necho "bd $*"\n' > "$FAKEBIN/bd"; chmod +x "$FAKEBIN/bd"
check "no spool env execs bd topic with the same args" "bd topic get deps-bump --json" \
  "$(env -u HIVE_SANDBOX_REQUEST_DIR -u HIVE_REVIEW_REQUEST_DIR PATH="$FAKEBIN:$PATH" bash "$SCRIPT" get deps-bump --json)"
check "a spool dir without a key is not a spool" "bd topic list" \
  "$(env HIVE_SANDBOX_REQUEST_DIR="$SPOOL" PATH="$FAKEBIN:$PATH" bash "$SCRIPT" list)"

echo ""
echo "--- sandbox: claim without --pr is written signed, kind topic ---"
in_sandbox claim deps-bump --no-wait >/dev/null
REQ="$(newest)"
if [ -n "$REQ" ]; then
  check "envelope kind is topic" "topic" "$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["kind"])' "$REQ")"
  check "envelope MAC binds the topic kind" "ok" "$(python3 -c '
import hashlib, hmac, json, sys
env = json.load(open(sys.argv[1]))
want = hmac.new(sys.argv[2].encode(), ("topic\n" + env["request"]).encode(), hashlib.sha256).hexdigest()
print("ok" if hmac.compare_digest(env["mac"], want) else "bad")' "$REQ" "$KEY")"
  check "op is claim" "claim" "$(reqfield "$REQ" op)"
  check "key is carried" "deps-bump" "$(reqfield "$REQ" key)"
  check "no pr: hive binds the kick's PR" "" "$(reqfield "$REQ" pr)"
  if grep -q "$KEY" "$REQ"; then
    echo "  FAIL: the spool key was written into the spool"; FAIL=$((FAIL + 1))
  else
    echo "  PASS: the spool key is not in the spool file"; PASS=$((PASS + 1))
  fi
else
  echo "  FAIL: no topic request in the spool"; FAIL=$((FAIL + 6))
fi
rm -f "$SPOOL"/*.json

in_sandbox claim deps-bump --repo acme/widget --pr 42 --branch bump --no-wait >/dev/null
REQ="$(newest)"
check "explicit pr, repo and branch" "42 acme/widget bump" "$(reqfield "$REQ" pr) $(reqfield "$REQ" repo) $(reqfield "$REQ" branch)"
rm -f "$SPOOL"/*.json

echo ""
echo "--- sandbox: usage errors write nothing ---"
rc=0; in_sandbox claim >/dev/null 2>&1 || rc=$?
check "claim without a key exits 1" "1" "$rc"
rc=0; in_sandbox claim k --pr nope >/dev/null 2>&1 || rc=$?
check "non-numeric --pr exits 1" "1" "$rc"
rc=0; in_sandbox frobnicate >/dev/null 2>&1 || rc=$?
check "unknown subcommand exits 1" "1" "$rc"
rc=0; in_sandbox get k --bogus >/dev/null 2>&1 || rc=$?
check "unknown flag exits 1" "1" "$rc"
check "nothing was written" "0" "$(find "$SPOOL" -name '*.json' | wc -l | tr -d ' ')"

# reply answers the newest request with $1 once it appears, as the relay does.
reply() {
  ( for _ in $(seq 1 50); do
      r="$(newest)"
      if [ -n "$r" ]; then printf '%s' "$1" > "${r%.json}.result.json"; exit 0; fi
      sleep 0.1
    done ) &
}

echo ""
echo "--- sandbox: waits for hive's reply ---"
reply '{"ok":true,"pending":true,"claim":{"key":"deps-bump","repo":"acme/widget","pr":0,"branch":"hive/deps-1"}}'
OUT="$(in_sandbox claim deps-bump --timeout 10)"; wait
check "pending claim is reported" "claimed deps-bump -> acme/widget (PR not opened yet) branch hive/deps-1" "$OUT"
rm -f "$SPOOL"/*.json

reply '{"ok":false,"refused":true,"error":"topic already claimed","claim":{"key":"deps-bump","repo":"acme/widget","pr":7}}'
rc=0; in_sandbox claim deps-bump --pr 9 --timeout 10 >/dev/null 2>&1 || rc=$?; wait
check "a refused claim exits 2" "2" "$rc"
rm -f "$SPOOL"/*.json

reply '{"ok":false,"error":"authorization denied: no topic store configured (fail closed)"}'
rc=0; in_sandbox release deps-bump --timeout 10 >/dev/null 2>&1 || rc=$?; wait
check "an error exits 1" "1" "$rc"
rm -f "$SPOOL"/*.json

reply '{"ok":true,"claims":[{"key":"a","repo":"acme/widget","pr":3},{"key":"b","repo":"acme/widget","pr":4}]}'
check "list prints every claim" "2" "$(in_sandbox list --json --timeout 10 | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')"; wait
rm -f "$SPOOL"/*.json

rc=0; in_sandbox get deps-bump --timeout 1 >/dev/null 2>&1 || rc=$?
check "no reply in time exits 1" "1" "$rc"

echo ""
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
