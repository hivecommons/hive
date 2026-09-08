#!/usr/bin/env bash
# #6287: the token-access audit log must never be agent-writable.
#
# The entrypoint's root phase pre-creates /var/run/hive-metrics/token-access.jsonl.
# v4 made it dev:node 0664, and every agent UID's primary group is node, so every
# agent could truncate or forge the operator-only trail behind GET /api/token-access.
# The log is now dev-owned 0600, the agent-side wrappers drop events into a
# drop-box spool instead, and the Go process ingests them. The permissions
# watcher never touches /var/run/hive-metrics, so nothing after boot loosens it.
#
# The entrypoint needs root to execute, so this pins the invariant STATICALLY on
# the shipped source: the log gets exactly mode 600 and no other chmod in the
# file ever hands it a group or other bit; the spool is a drop-box (no group
# read, sticky, setgid); and neither wrapper opens the log path at all.
#
# Run: bash src/deploy/test_entrypoint_token_access_audit.sh
set -uo pipefail

PASS=0
FAIL=0
HERE="$(cd "$(dirname "$0")" && pwd)"
ENTRYPOINT="${HERE}/entrypoint.sh"
REPO_ROOT="$(cd "${HERE}/../.." && pwd)"
LOG_PATH="/var/run/hive-metrics/token-access.jsonl"
SPOOL_PATH="/var/run/hive-metrics/token-access-events"

ok() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "        $2"; FAIL=$((FAIL + 1)); }

echo "=== #6287: token-access audit log is not agent-writable ==="

# 1. The log is created with mode 600, and 600 is the ONLY mode it ever gets.
log_chmods="$(grep -E "^[[:space:]]*chmod[[:space:]]+[0-7]+[[:space:]]+${LOG_PATH}" "$ENTRYPOINT" | awk '{print $2}' | sort -u)"
if [ "$log_chmods" = "600" ]; then
  ok "entrypoint gives ${LOG_PATH} mode 600 and nothing else"
else
  bad "entrypoint chmods ${LOG_PATH} to: '${log_chmods:-<none>}' (want exactly 600)" \
      "every agent UID is in group node; any group bit on this file is an agent bit"
fi

# 2. No chmod anywhere in the entrypoint grants group/other write to the log,
#    including a chmod of the parent that could be recursive.
if grep -nE "chmod[[:space:]]+(-[a-zA-Z]*R[a-zA-Z]*[[:space:]]+)?[0-7]*[2367][0-7]?[[:space:]]+${LOG_PATH}" "$ENTRYPOINT" >/dev/null \
   || grep -nE "chmod[[:space:]]+-[a-zA-Z]*R[a-zA-Z]*[[:space:]]+[0-7]+[[:space:]]+/var/run/hive-metrics([[:space:]]|/?$|/?[[:space:]])" "$ENTRYPOINT" >/dev/null; then
  bad "a chmod in the entrypoint can hand agents write access to the audit log"
else
  ok "no chmod in the entrypoint grants group/other write on the audit log"
fi

# 3. The log is owned by dev (the hive UID), never an agent.
if grep -qE "^[[:space:]]*chown[[:space:]]+dev:node[[:space:]]+${LOG_PATH}" "$ENTRYPOINT"; then
  ok "audit log is chowned to dev (the hive UID)"
else
  bad "audit log is not chowned to dev:node"
fi

# 4. The spool is a drop-box: 3730 = setgid + sticky + owner rwx + group wx.
spool_chmod="$(grep -E "^[[:space:]]*chmod[[:space:]]+[0-7]+[[:space:]]+${SPOOL_PATH}" "$ENTRYPOINT" | awk '{print $2}' | sort -u)"
if [ "$spool_chmod" = "3730" ]; then
  ok "event spool is a 3730 drop-box (agents create, cannot list; sticky; setgid)"
else
  bad "event spool chmod is '${spool_chmod:-<none>}' (want 3730)"
fi

# 5. Neither agent-side wrapper references the log path: the only writer is the
#    hive process. Both must point at the spool the Go ingester watches.
for wrapper in bin/gh-wrapper.sh bin/git-credential-hive.sh; do
  f="${REPO_ROOT}/${wrapper}"
  if grep -q "token-access.jsonl" "$f"; then
    bad "${wrapper} still references the audit log path; agents must not write it"
  elif grep -q "TOKEN_ACCESS_SPOOL=\"${SPOOL_PATH}\"" "$f"; then
    ok "${wrapper} writes only to the event spool"
  else
    bad "${wrapper} does not point at ${SPOOL_PATH}"
  fi
  if grep -qE "TOKEN_ACCESS_SPOOL=\"\\\$\{" "$f"; then
    bad "${wrapper} lets the environment override the spool path" \
        "an override would let an agent redirect its own audit events into the void"
  fi
done

# 6. The Go side agrees on both paths and on the 0600 mode.
go_src="${REPO_ROOT}/src/pkg/github/token_access_audit.go"
if grep -q "TokenAccessSpoolDir = \"${SPOOL_PATH}\"" "$go_src" \
   && grep -q "TokenAccessLogPath = \"${LOG_PATH}\"" "$go_src" \
   && grep -qE "tokenAccessLogMode[[:space:]]+os.FileMode[[:space:]]*=[[:space:]]*0o600" "$go_src"; then
  ok "Go ingester uses the same spool, the same log, and mode 0600"
else
  bad "Go ingester constants disagree with the entrypoint/wrappers"
fi

# 7. The Go permissions watcher never has /var/run/hive-metrics in its roots,
#    so nothing running as dev after boot can re-open the hole (#6238 / #6277
#    moved umask handling around; the watcher's roots are the only mode-fixer).
if grep -q '"/var/run/hive-metrics' "${REPO_ROOT}/src/pkg/agent/permissions_watcher.go"; then
  bad "the permissions watcher walks /var/run/hive-metrics and could loosen the audit log"
else
  ok "the permissions watcher does not walk /var/run/hive-metrics"
fi

echo
echo "=== ${PASS} passed, ${FAIL} failed ==="
[ "$FAIL" -eq 0 ] || exit 1
