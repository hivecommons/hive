#!/usr/bin/env bash
# check-suid-contract.sh — static contract check that the hive image never
# ships a WORLD-EXECUTABLE setuid-root helper (security finding C6,
# CWE-250/269).
#
# Background. The image needs a SUID `su-exec` so the non-root `dev` process can
# switch to per-agent UIDs at runtime (pkg/agent/manager.go,
# pkg/dashboard/workspace_cleanup.go, deploy/ttyd-tmux.sh — all run AFTER the
# entrypoint's `exec gosu dev`). The original build did
# `curl .../master/su-exec.c | gcc | chmod u+s`, producing a mode-4755
# setuid-root binary that ANY agent UID could exec as `su-exec root sh` → root
# in the pod holding the org GitHub App key, every agent token, and iptables
# control. That collapsed per-UID isolation and the F5 forced-egress guarantee.
#
# The fix (Option B — the SUID helper is genuinely required, so it is TIGHTENED,
# not removed): pin su-exec by commit SHA + verify SHA256, then own it
# root:hive-launch mode 4750 so only root and the dedicated launcher group (sole
# member: dev) can exec it. This check locks that invariant in so a future edit
# can't silently regress back to a world-exec SUID binary or an unpinned source.
#
# Invariant (asserted against v2/Dockerfile):
#   - NO `chmod u+s` / `chmod 4755` / world-exec SUID mode on su-exec.
#   - su-exec source is pinned by commit SHA (not the moving `master` ref).
#   - the SHA256 of the pinned source is verified before compiling.
#   - the installed helper is chmod 4750 and owned by root:hive-launch.
#
# Usage: v2/scripts/check-suid-contract.sh [path-to-Dockerfile]
set -euo pipefail

DOCKERFILE="${1:-v2/Dockerfile}"

if [[ ! -f "$DOCKERFILE" ]]; then
  echo "ERROR: Dockerfile not found at ${DOCKERFILE}" >&2
  exit 1
fi

fail=0

# CODE holds the Dockerfile with comment lines stripped. The forbidden-pattern
# checks run against CODE so that an explanatory comment mentioning e.g.
# `chmod u+s` (describing the OLD insecure build) is not itself flagged — only a
# real instruction would be. The positive checks run against the raw file.
CODE="$(grep -vE '^[[:space:]]*#' "$DOCKERFILE")"

check() {
  local desc="$1" pattern="$2"
  if grep -qE "$pattern" "$DOCKERFILE"; then
    echo "  ok: ${desc}"
  else
    echo "  FAIL: ${desc} (expected to find pattern: ${pattern})"
    fail=1
  fi
}

check_absent() {
  local desc="$1" pattern="$2"
  if printf '%s\n' "$CODE" | grep -qE "$pattern"; then
    echo "  FAIL: ${desc} (found forbidden pattern in a build instruction: ${pattern})"
    fail=1
  else
    echo "  ok: ${desc}"
  fi
}

echo "== SUID helper contract check (${DOCKERFILE}) =="

# 1. No world-executable setuid bit may be set on any binary. `chmod u+s`
# (implicitly 4755 on a 0755 file) and an explicit 4-digit mode whose group/
# other exec bits are set (47xx / 45xx / 46xx / 47x7 ...) are both forbidden.
# The permitted form is 4750 (owner+group exec only), asserted positively below.
check_absent "no 'chmod u+s' (world-exec setuid)" \
  'chmod[[:space:]]+u\+s'
check_absent "no world-exec setuid mode (4755/4775/4711/etc.)" \
  'chmod[[:space:]]+4[0-7][0-7][1-7]'

# 2. su-exec source must be pinned by commit SHA, not the moving master ref.
check_absent "su-exec source is not fetched from the moving 'master' ref" \
  'su-exec/master/su-exec\.c'
check "su-exec source pinned via SU_EXEC_COMMIT arg" \
  'ARG[[:space:]]+SU_EXEC_COMMIT='
check "su-exec fetch URL uses the pinned commit" \
  'su-exec/\$\{SU_EXEC_COMMIT\}/su-exec\.c'

# 3. The pinned source SHA256 must be verified before compiling.
check "su-exec SHA256 declared" \
  'ARG[[:space:]]+SU_EXEC_SHA256='
check "su-exec SHA256 verified with sha256sum -c" \
  'sha256sum[[:space:]]+-c'

# 4. The installed helper must be group-restricted (4750), owned root:hive-launch.
check "su-exec installed chmod 4750 (owner+group exec only)" \
  'chmod[[:space:]]+4750[[:space:]]+/usr/local/bin/su-exec'
check "su-exec owned root:hive-launch (dedicated launcher group)" \
  'chown[[:space:]]+root:hive-launch[[:space:]]+/usr/local/bin/su-exec'

# 5. The dedicated launcher group must exist and NOT include agent UIDs. dev is
# added to it; agent users are created in the entrypoint with `-g node` only, so
# a `-G hive-launch` on any agent useradd would be a regression.
check "hive-launch launcher group is created" \
  'groupadd[[:space:]]+--system[[:space:]]+hive-launch'
check "dev is a member of hive-launch" \
  'useradd.*-G[[:space:]]+hive-launch'

if [[ "$fail" -ne 0 ]]; then
  echo ""
  echo "SUID helper contract check FAILED — a world-exec or unpinned setuid-root"
  echo "binary must never ship (security finding C6, CWE-250/269)."
  exit 1
fi

echo ""
echo "SUID helper contract check passed."
