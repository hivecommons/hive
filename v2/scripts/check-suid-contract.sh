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
# NET_ADMIN invariant (#3760 — asserted against Dockerfile + entrypoint):
#   - NO `setcap` file capability on the hive binary (a file cap EPERMs on exec
#     wherever the runtime bounding set lacks the cap — the #3760 crash-loop).
#   - the entrypoint instead raises NET_ADMIN as an AMBIENT capability, gated on
#     the CAP_NET_ADMIN bounding-set bit, via
#     `setpriv --inh-caps +net_admin --ambient-caps +net_admin`,
#     with a `gosu dev` fallback when the cap is unavailable.
#     BOTH flags are required (#3874): --ambient-caps alone exits 0 but yields
#     CapAmb=0x0, because the kernel only allows an ambient bit that is also in
#     the inheritable set, which the setuid transition has already zeroed.
#
# Usage: v2/scripts/check-suid-contract.sh [path-to-Dockerfile] [path-to-entrypoint]
set -euo pipefail

DOCKERFILE="${1:-v2/Dockerfile}"
ENTRYPOINT="${2:-v2/deploy/entrypoint.sh}"

if [[ ! -f "$DOCKERFILE" ]]; then
  echo "ERROR: Dockerfile not found at ${DOCKERFILE}" >&2
  exit 1
fi
if [[ ! -f "$ENTRYPOINT" ]]; then
  echo "ERROR: entrypoint not found at ${ENTRYPOINT}" >&2
  exit 1
fi

fail=0

# CODE_FILE holds the Dockerfile with comment lines stripped, written to a temp
# file. The forbidden-pattern checks grep this FILE directly (never a
# `printf | grep -q` pipe — grep -q closes the pipe on first match and the
# writing printf takes SIGPIPE, which under `set -o pipefail` made the check
# spuriously fail; see #3774). An explanatory comment mentioning e.g. `setcap`
# (describing the OLD file-cap build) is thus not itself flagged — only a real
# build instruction would be. Positive checks grep the raw file.
CODE_FILE="$(mktemp)"
trap 'rm -f "$CODE_FILE"' EXIT
grep -vE '^[[:space:]]*#' "$DOCKERFILE" > "$CODE_FILE" || true

check() {
  local desc="$1" pattern="$2" file="${3:-$DOCKERFILE}"
  # `-e -- ` so a pattern that begins with `--` (e.g. --reuid) is not parsed as
  # a grep option.
  if grep -qE -e "$pattern" -- "$file"; then
    echo "  ok: ${desc}"
  else
    echo "  FAIL: ${desc} (expected to find pattern: ${pattern})"
    fail=1
  fi
}

check_absent() {
  local desc="$1" pattern="$2" file="${3:-$CODE_FILE}"
  if grep -qE -e "$pattern" -- "$file"; then
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
# The group must still be a --system group named hive-launch, but the flags
# between them are not fixed: N5 added `--gid` to pin the GID so the deployment
# manifests can name it in fsGroup (Secret defaultMode 0440 is only a boundary
# if the group is one agents are NOT in). Match --system and the group name
# without assuming they are adjacent.
check "hive-launch launcher group is created" \
  'groupadd[[:space:]]+([^[:space:]]+[[:space:]]+)*--system([[:space:]]+[^[:space:]]+)*[[:space:]]+hive-launch'
check "dev is a member of hive-launch" \
  'useradd.*-G[[:space:]]+hive-launch'

# ── NET_ADMIN contract: NO file capability, ambient grant instead (#3760) ─────
#
# The hive process needs CAP_NET_ADMIN in its EFFECTIVE set so the MITM proxy can
# stamp SO_MARK on its own upstream dials and be exempted from the forced-egress
# iptables REDIRECT (on OpenShift/OVN, where xt_owner is absent, SO_MARK is the
# ONLY exemption that works — refs #2674/#2678, observed live on vllm-d).
#
# It USED to carry this as a FILE capability (setcap cap_net_admin+ep). That is
# now FORBIDDEN: the kernel refuses execve of a file-capped binary whenever the
# runtime BOUNDING set lacks the cap (default docker/podman/containerd — every
# self-hosted k3s/rootless spoke), producing the #3760 exec-EPERM crash-loop. A
# file capability and "execs everywhere" are incompatible. Instead the entrypoint
# raises NET_ADMIN as an AMBIENT capability, gated on the bounding set having it.
#
# (a) FORBID any file capability on the binary — no setcap on it, and the binary
#     must ship with no security.capability xattr. A getcap-based build-time
#     verification (which only makes sense alongside a setcap) is also forbidden.
check_absent "NO setcap file capability on the hive binary (#3760: EPERMs on exec where bounding set lacks the cap)" \
  'setcap[[:space:]]+[^#]*cap_net_admin[^#]*/usr/local/bin/hive'
check_absent "NO getcap build-time verification of a hive file capability" \
  'getcap[[:space:]]+/usr/local/bin/hive'

# No binary at all may be given a file capability — agents run their own
# binaries, and a file cap on any of them would (a) hand NET_ADMIN straight to an
# agent process and (b) reintroduce the exec-EPERM class of bug. Any `setcap` in
# a real build instruction is a failure.
if grep -qE '^[[:space:]]*(RUN[[:space:]].*)?setcap[[:space:]]' "$CODE_FILE"; then
  echo "  FAIL: a setcap file capability is present in a build instruction (forbidden — grant NET_ADMIN at runtime via the entrypoint's ambient cap, not a file cap)"
  fail=1
else
  echo "  ok: no setcap file capability is granted to any binary"
fi

# (b) REQUIRE the entrypoint's bounding-set-gated ambient-cap grant. The
#     entrypoint must: read the bounding set (CapBnd), test the CAP_NET_ADMIN bit
#     (bit 12 → mask 0x1000), and — only when present — drop to dev via setpriv
#     +net_admin. Assert each element so the mechanism can't be silently dropped.
check "entrypoint reads the bounding set (CapBnd)" \
  'CapBnd' "$ENTRYPOINT"
check "entrypoint tests the CAP_NET_ADMIN bounding-set bit (bit 12 / 0x1000)" \
  '0x1000' "$ENTRYPOINT"
# INVERTED (not relaxed) for #3874. This assertion previously required the
# literal `setpriv --ambient-caps +net_admin` — i.e. it demanded EXACTLY the
# form that is silently broken, and would have gone RED on the correct fix. It
# ENCODED the bug: an ambient bit cannot be raised unless the same setpriv call
# also raises the INHERITABLE set (the kernel requires the cap in pP *and* pI;
# setpriv applies --ambient-caps after the UID change, when pI is already zero).
# The contract is now the stronger one: --inh-caps must accompany --ambient-caps.
check "entrypoint raises NET_ADMIN into the INHERITABLE set (required for the ambient raise to stick — #3874)" \
  '--inh-caps[[:space:]]+\+net_admin' "$ENTRYPOINT"
check "entrypoint raises NET_ADMIN as an ambient capability" \
  '--ambient-caps[[:space:]]+\+net_admin' "$ENTRYPOINT"
# The setpriv identity (--reuid dev) may live in a shell variable reused by both
# the probe and the exec, so assert it appears in the file rather than adjacent
# to --ambient-caps. A missing --reuid dev would leave the drop-to-dev broken.
check "entrypoint drops the ambient-cap path to the dev user (--reuid dev)" \
  '--reuid[[:space:]]+dev' "$ENTRYPOINT"
check "entrypoint keeps a gosu fallback when the cap is unavailable" \
  'exec[[:space:]]+gosu[[:space:]]+dev' "$ENTRYPOINT"

if [[ "$fail" -ne 0 ]]; then
  echo ""
  echo "SUID/NET_ADMIN contract check FAILED. Either a world-exec or unpinned"
  echo "setuid-root binary would ship (security finding C6, CWE-250/269), OR the"
  echo "NET_ADMIN contract regressed: a file capability was re-added to the hive"
  echo "binary (EPERMs on exec where the bounding set lacks NET_ADMIN — #3760), or"
  echo "the entrypoint's bounding-set-gated ambient-cap grant was removed."
  exit 1
fi

echo ""
echo "SUID/NET_ADMIN contract check passed."
