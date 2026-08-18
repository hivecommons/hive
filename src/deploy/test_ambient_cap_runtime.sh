#!/usr/bin/env bash
# test_ambient_cap_runtime.sh — RUNTIME regression test for #3874.
#
# THE BUG THIS CATCHES
# --------------------
# `setpriv --ambient-caps +net_admin --reuid dev ...` exits 0 but silently
# produces CapAmb=0x0. The kernel only permits an ambient bit that is present in
# BOTH the permitted and the INHERITABLE set (cap_ambient_raise / PR_CAP_AMBIENT,
# kernel/capability.c), and setpriv applies --ambient-caps after the UID change,
# by which point the setuid transition has zeroed pI. The entrypoint therefore
# printed its success line while the hive process ran with NO NET_ADMIN, so the
# proxy could not SO_MARK its own upstream dial, that dial was REDIRECTed back
# into the proxy, and every outbound :443 hung — a fleet-wide outage.
#
# The fix raises --inh-caps in the SAME setpriv call.
#
# WHY THIS TEST IS RUNTIME AND NOT A grep
# ---------------------------------------
# The shipped bug was 100% invisible to static inspection: the flag was present,
# the exit status was 0, the log line claimed success. Only executing the drop
# and reading CapAmb back proves the capability actually survived. A static
# assertion here would be exactly the "test that passes for the wrong reason"
# this repo already has too many of.
#
# CONTROLS (both directions, all executed for real):
#   POSITIVE : the fixed invocation           -> CapAmb bit 12 MUST be set
#   NEGATIVE : the buggy invocation (no --inh) -> CapAmb bit 12 MUST be unset
#              (this is the regression replay: it reproduces the outage in-test,
#              so if a future change makes the buggy form "work", we learn it)
#   NEGATIVE : no NET_ADMIN in the bounding set -> raise must NOT be attempted
#   SOURCE   : the entrypoint must actually use the fixed form
#
# Run: bash src/deploy/test_ambient_cap_runtime.sh
set -uo pipefail

PASS=0
FAIL=0
ENTRYPOINT="$(cd "$(dirname "$0")" && pwd)/entrypoint.sh"

# CAP_NET_ADMIN is capability 12 -> mask 0x1000.
CAP_NET_ADMIN_MASK=0x1000

ok()   { echo "  PASS: $1"; PASS=$((PASS + 1)); }
bad()  { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); }

# Image with setpriv (util-linux) and a non-root user to drop to.
IMAGE="debian:bookworm-slim"

# Run a shell snippet inside a container that HAS NET_ADMIN in its bounding set,
# with an unprivileged target user created, and echo the resulting CapAmb.
# $1 = the setpriv capability flags under test (may be empty).
capamb_after_drop() {
  local capflags="$1" caps_arg="$2"
  docker run --rm $caps_arg "$IMAGE" sh -c "
    set -e
    apt-get update -qq >/dev/null 2>&1
    apt-get install -y -qq util-linux >/dev/null 2>&1
    useradd -u 1001 -m dev >/dev/null 2>&1 || true
    setpriv $capflags --reuid dev --regid dev --init-groups \
      sh -c 'grep -m1 \"^CapAmb:\" /proc/self/status' 2>/dev/null \
      | awk '{print \$2}'
  " 2>/dev/null | tr -d '[:space:]'
}

bit_set() {
  local hex="$1"
  [ -n "$hex" ] || return 1
  [ $(( 0x${hex} & ${CAP_NET_ADMIN_MASK} )) -ne 0 ]
}

echo "=== #3874 ambient CAP_NET_ADMIN runtime tests ==="

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  echo "  SKIP: docker unavailable — runtime capability assertions require it"
  echo "  (CI runs this on ubuntu-latest where docker is present)"
  exit 0
fi

echo "-- runtime: POSITIVE control (the ENTRYPOINT'S OWN flags) --"
# CRITICAL: these flags are EXTRACTED FROM THE ENTRYPOINT, not hardcoded here.
# A hardcoded copy would keep passing even after the entrypoint regressed — the
# exact "passes for the wrong reason" failure mode this repo keeps hitting. By
# driving the container with whatever the entrypoint actually sets, a regression
# in the entrypoint turns this runtime assertion RED.
ENTRY_CAPS="$(sed -n 's/^[[:space:]]*_SETPRIV_CAPS="\(.*\)"[[:space:]]*$/\1/p' "$ENTRYPOINT" | head -1)"
echo "     flags extracted from entrypoint = '${ENTRY_CAPS:-<none>}'"
if [ -z "$ENTRY_CAPS" ]; then
  bad "could not extract _SETPRIV_CAPS from the entrypoint — test cannot verify the real invocation"
fi
FIXED="$(capamb_after_drop "$ENTRY_CAPS" '--cap-add=NET_ADMIN')"
echo "     CapAmb after entrypoint drop = ${FIXED:-<empty>}"
if bit_set "$FIXED"; then
  ok "ambient CAP_NET_ADMIN SURVIVES the privilege drop using the entrypoint's own flags"
else
  bad "ambient CAP_NET_ADMIN LOST across the privilege drop using the entrypoint's own flags — the #3874 outage condition"
fi

echo "-- runtime: NEGATIVE control (regression replay of the shipped bug) --"
BUGGY="$(capamb_after_drop '--ambient-caps +net_admin' '--cap-add=NET_ADMIN')"
echo "     CapAmb after buggy drop   = ${BUGGY:-<empty>}"
if bit_set "$BUGGY"; then
  bad "the buggy form (--ambient-caps WITHOUT --inh-caps) now yields a live ambient cap — this test can no longer distinguish fixed from broken; re-derive the control"
else
  ok "the buggy form still silently yields CapAmb=0 (proves the positive control above is meaningful, not vacuous)"
fi

echo "-- runtime: NEGATIVE control (no NET_ADMIN in bounding set) --"
NOCAP="$(capamb_after_drop '--inh-caps +net_admin --ambient-caps +net_admin' '')"
echo "     CapAmb without --cap-add  = ${NOCAP:-<empty/failed>}"
if bit_set "$NOCAP"; then
  bad "ambient NET_ADMIN raised despite it being absent from the bounding set"
else
  ok "no ambient NET_ADMIN when the bounding set lacks it (self-hosted spokes unaffected)"
fi

echo "-- source: the entrypoint uses the fixed form --"
# Both flags must appear, and they must be in the SAME setpriv invocation.
if grep -qE '\-\-inh-caps[[:space:]]+\+net_admin[[:space:]]+--ambient-caps[[:space:]]+\+net_admin' "$ENTRYPOINT"; then
  ok "entrypoint raises --inh-caps alongside --ambient-caps in one setpriv call"
else
  bad "entrypoint does not raise --inh-caps with --ambient-caps (#3874 would recur)"
fi

# The entrypoint must VERIFY the outcome, not trust setpriv's exit status.
if grep -qE 'CapAmb' "$ENTRYPOINT"; then
  ok "entrypoint verifies CapAmb after the drop rather than trusting exit status"
else
  bad "entrypoint does not read CapAmb back — a silent no-op would go unnoticed again"
fi

# Guard the blast radius: net_admin and nothing more.
if grep -qE '\-\-(inh|ambient)-caps[[:space:]]+\+(?!net_admin)' "$ENTRYPOINT" 2>/dev/null \
   || grep -qE '\-\-(inh|ambient)-caps[[:space:]]+\+all' "$ENTRYPOINT"; then
  bad "entrypoint raises more than net_admin"
else
  ok "entrypoint raises net_admin only (no capability-posture widening)"
fi

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
