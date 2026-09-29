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
