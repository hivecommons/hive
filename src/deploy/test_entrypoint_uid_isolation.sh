#!/usr/bin/env bash
# #5525: a version flip must not park PID 1 on an O(size of PVC) ownership walk.
# The root repair runs in a background worker, while protected markers keep the
# Go manager from launching an agent into a partially migrated tree.
# Run: bash src/deploy/test_entrypoint_uid_isolation.sh
set -uo pipefail

PASS=0
FAIL=0
ENTRYPOINT="$(cd "$(dirname "$0")" && pwd)/entrypoint.sh"

ok() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "        $2"; FAIL=$((FAIL + 1)); }

echo "=== #5525: non-blocking UID-isolation migration ==="

# The provisioning loop is on PID 1's root startup path. It may invalidate a
# marker and touch the agent root, but it must never recurse into the PVC.
provisioning="$(sed -n '/Creating per-agent users for UID isolation/,/Write uid-map.json/p' "$ENTRYPOINT")"
if grep -qE '^[[:space:]]*(chown|chmod)[[:space:]]+-[^[:space:]]*R.*[" ]/data/agents|^[[:space:]]*find[[:space:]]+/data/agents' <<<"$provisioning"; then
  bad "the synchronous per-agent provisioning loop contains a recursive walk" \
      "all size-dependent ownership work must stay in hive_run_uid_isolation_migration"
else
  ok "the synchronous per-agent provisioning loop is O(number of agents)"
fi

worker="$(sed -n '/^hive_run_uid_isolation_migration() {/,/^}/p' "$ENTRYPOINT")"
starter="$(sed -n '/^hive_start_uid_isolation_migration() {/,/^}/p' "$ENTRYPOINT")"
if grep -q 'chown -R' <<<"$worker" && grep -q 'hive_run_uid_isolation_migration &' <<<"$starter"; then
  ok "recursive ownership repair is confined to the background worker"
else
  bad "background migration wiring is missing" \
      "expected recursive repair in the worker and an '&' at its call site"
fi

# The shared-home preparation (/data/home/.codex, .gemini, .bob, ...) is also
# on PID 1's startup path and runs on EVERY boot, not just a version flip. A
# recursive chown there over a mature spoke's tens of thousands of session
# files on shared NFS pushed boot past the startup budget and crash-looped 40
# spokes in lock-step (2026-10-26). Only the directories themselves may be
# re-owned synchronously; the tree belongs to the background worker.
shared_home="$(sed -n '/Shared CLI auth\/cache lives in \/data\/home/,/ownership marker and perms OK/p' "$ENTRYPOINT")"
if [ -z "$shared_home" ]; then
  bad "could not extract the shared-home preparation block from entrypoint.sh"
elif grep -qE '^[[:space:]]*(chown|chmod|chgrp)[[:space:]]+-[^[:space:]]*R[^[:space:]]*[[:space:]].*/data/home' <<<"$shared_home"; then
  bad "the synchronous shared-home preparation contains a recursive walk under /data/home" \
      "$(grep -nE '^[[:space:]]*(chown|chmod|chgrp)[[:space:]]+-[^[:space:]]*R' <<<"$shared_home")"
else
  ok "the synchronous shared-home preparation is O(number of directories)"
fi

if grep -qE 'chown -R dev:node .*/data/home/\.codex' <<<"$worker" \
   && grep -qE 'chown -R dev:node .*/data/home/\.gemini' <<<"$worker"; then
  ok "the background worker re-owns the .codex and .gemini trees"
else
  bad "the background worker does not cover both /data/home/.codex and /data/home/.gemini"
fi

if grep -q "isolation_marker_dir" "$ENTRYPOINT" \
   && grep -q "isolation_revision" "$ENTRYPOINT"; then
  ok "uid-map.json carries the marker contract to the Go manager"
else
  bad "uid-map.json does not carry both isolation marker fields"
fi

# Execute the real marker helpers in a temporary directory. The marker must be
# exact (revision + target UID), and publishing must leave no temporary file.
helpers="$(sed -n '/^hive_uid_isolation_marker_path() {/,/^hive_run_uid_isolation_migration() {/p' "$ENTRYPOINT" | sed '$d')"
if [ -z "$helpers" ]; then
  bad "could not extract the marker helpers from entrypoint.sh"
else
  marker_tmp="$(mktemp -d)"
  # shellcheck disable=SC1090,SC2086
  # Intentionally execute extracted shipped helpers.
  eval "$helpers"
  # shellcheck disable=SC2034
  HIVE_UID_ISOLATION_MARKER_DIR="$marker_tmp"
  marker="$(hive_uid_isolation_marker_path 2001)"
  if hive_publish_uid_isolation_marker "$marker" "1:2001" \
     && hive_uid_isolation_marker_matches "$marker" "1:2001" \
     && ! hive_uid_isolation_marker_matches "$marker" "1:2002" \
     && ! compgen -G "${marker}.tmp.*" >/dev/null; then
    ok "completion markers are atomically published and UID-bound"
  else
    bad "marker helper behaviour is not atomic and UID-bound"
  fi
  rm -rf "$marker_tmp"
fi

echo
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1
