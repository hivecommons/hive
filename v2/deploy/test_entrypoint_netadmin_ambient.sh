#!/usr/bin/env bash
# test_entrypoint_netadmin_ambient.sh — regression tests for the #3760 fix:
# the hive binary ships with NO file capability, and the entrypoint grants
# CAP_NET_ADMIN as a BOUNDING-SET-GATED AMBIENT capability at the privilege drop.
#
# Two things are proven here:
#  1. STRUCTURE: the entrypoint contains the bounding-set gate (CapBnd + the
#     CAP_NET_ADMIN 0x1000 bit test), the setpriv ambient-cap raise, and the
#     gosu fallback — with a clear notice on the no-cap path.
#  2. LOGIC: the exact CapBnd bit-test used in the entrypoint decides correctly.
#     Because the arithmetic gate is pure shell (`$(( 0xHEX & 0x1000 ))`), we can
#     execute it directly against synthetic bounding-set masks and assert the
#     verdict — with POSITIVE controls (masks that DO contain NET_ADMIN, incl.
#     the real OKE/OpenShift full-cap set) and NEGATIVE controls (the default
#     docker/containerd bounding set, which lacks it → the #3760 EPERM case).
#
# Run: bash v2/deploy/test_entrypoint_netadmin_ambient.sh
set -euo pipefail

PASS=0
FAIL=0

ENTRYPOINT="$(cd "$(dirname "$0")" && pwd)/entrypoint.sh"

assert_contains() {
  local pattern="$1" label="$2"
  # `-e -- ` so a pattern beginning with `--` is not parsed as a grep option.
  if grep -qE -e "$pattern" -- "$ENTRYPOINT"; then
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label"
    echo "        pattern '$pattern' not found in $ENTRYPOINT"
    FAIL=$((FAIL + 1))
  fi
}

# The SAME gate the entrypoint runs. Given a CapBnd hex mask (as printed in
# /proc/self/status, no 0x prefix), return 0 iff CAP_NET_ADMIN (bit 12 → 0x1000)
# is set. Kept byte-for-byte equivalent to the entrypoint's arithmetic test.
# NB: 0x2000 is bit 13 / CAP_NET_RAW (which IS in the docker default set), so the
# mask MUST be 0x1000 — the wrong mask would fire on self-hosted spokes.
cap_net_admin_in_bset() {
  local capbnd_hex="$1"
  [ -n "$capbnd_hex" ] || return 1
  [ $(( 0x${capbnd_hex} & 0x1000 )) -ne 0 ]
}

assert_gate() {
  local mask="$1" want="$2" label="$3"  # want: yes|no
  local got="no"
  if cap_net_admin_in_bset "$mask"; then got="yes"; fi
  if [ "$got" = "$want" ]; then
    echo "  PASS: $label (mask=$mask → $got)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label (mask=$mask → $got, expected $want)"
    FAIL=$((FAIL + 1))
  fi
}

echo "=== #3760 entrypoint NET_ADMIN ambient-cap tests ==="

echo "-- structure --"
assert_contains 'CapBnd' \
  "entrypoint reads the bounding set (CapBnd) from /proc/self/status"
assert_contains '0x1000' \
  "entrypoint tests the CAP_NET_ADMIN bit (bit 12 / 0x1000)"
# Guard against the classic off-by-one-bit bug: the mask must NOT be 0x2000
# (that is CAP_NET_RAW / bit 13, which IS in the docker default bounding set).
if grep -qE '&[[:space:]]*0x2000' "$ENTRYPOINT"; then
  echo "  FAIL: entrypoint masks 0x2000 (CAP_NET_RAW / bit 13) — CAP_NET_ADMIN is bit 12 / 0x1000"
  FAIL=$((FAIL + 1))
else
  echo "  PASS: entrypoint does not mask the wrong bit (0x2000 / NET_RAW)"
  PASS=$((PASS + 1))
fi
assert_contains 'setpriv[[:space:]]+--ambient-caps[[:space:]]+\+net_admin' \
  "entrypoint raises an ambient NET_ADMIN cap via setpriv"
assert_contains '--reuid[[:space:]]+dev' \
  "entrypoint drops the ambient-cap path to the dev user (--reuid dev)"
assert_contains 'exec[[:space:]]+gosu[[:space:]]+dev' \
  "entrypoint keeps a gosu dev fallback when the cap is unavailable"
assert_contains 'NOTICE:.*NET_ADMIN.*bounding set' \
  "entrypoint prints a clear notice on the no-NET_ADMIN (self-hosted) path"
# The binary must NOT be given a file capability anywhere in the entrypoint.
if grep -qE 'setcap' "$ENTRYPOINT"; then
  echo "  FAIL: entrypoint must not setcap a file capability (#3760)"
  FAIL=$((FAIL + 1))
else
  echo "  PASS: entrypoint grants no file capability (ambient only)"
  PASS=$((PASS + 1))
fi

echo "-- bounding-set gate logic --"
# NEGATIVE controls — bounding sets WITHOUT CAP_NET_ADMIN (self-hosted/rootless,
# the #3760 EPERM case). Ambient raise must be SKIPPED → falls back to gosu.
# 00000000a80425fb is the REAL docker/containerd default bounding set: it
# contains CAP_NET_RAW (0x2000 / bit 13) but NOT CAP_NET_ADMIN (0x1000 / bit 12).
# This is the exact mask that would false-trigger under the wrong 0x2000 mask.
assert_gate "00000000a80425fb" "no" \
  "REAL docker default bounding set (has NET_RAW, lacks NET_ADMIN) → skip ambient, use gosu"
assert_gate "0000000000000000" "no" \
  "empty bounding set → skip ambient"
assert_gate "0000000000000400" "no" \
  "only CAP_NET_BIND_SERVICE (bit 10 / 0x400) set → skip ambient"
assert_gate "0000000000002000" "no" \
  "only CAP_NET_RAW (bit 13 / 0x2000) set — adjacent bit must not false-trigger"

# POSITIVE controls — bounding sets WITH CAP_NET_ADMIN (managed OKE/OpenShift
# via SCC/PSA). Ambient raise must be ATTEMPTED.
assert_gate "0000000000001000" "yes" \
  "only CAP_NET_ADMIN (bit 12 / 0x1000) set → attempt ambient raise"
assert_gate "0000003fffffffff" "yes" \
  "full cap set (privileged / all-caps) → attempt ambient raise"
# Real docker default OR'd with NET_ADMIN — what --cap-add NET_ADMIN produces.
assert_gate "00000000a80435fb" "yes" \
  "docker default + NET_ADMIN (--cap-add NET_ADMIN) → attempt ambient raise"

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
