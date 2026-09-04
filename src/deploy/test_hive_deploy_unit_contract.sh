#!/usr/bin/env bash
# hive-deploy.service must not trust a source checkout a local user could have
# planted (#5936, same class as #5435/#5483 — with a larger payoff).
# Run: bash src/deploy/test_hive_deploy_unit_contract.sh
#
# WHAT THE BUG WAS. Unlike the discord (#5435) and snapshot (#5483) units, this
# unit's ExecStart is a root-owned file in /usr/local/bin — the exposure is not
# the entrypoint but the SOURCE TREE. bin/hive-deploy.sh cds into /tmp/hive
# checking only that .git exists, runs git inside it (executing the checkout's
# own .git/hooks as `dev`), and then for every /tmp/hive/bin/*.sh that differs
# from an installed counterpart does `sudo cp` + `sudo chmod +x` into
# /usr/local/bin — including bin/hive.sh → /usr/local/bin/hive and
# bin/gh-wrapper.sh → /usr/local/bin/gh, which fronts /usr/bin/gh in PATH for
# every user on the host.
#
# /tmp's sticky bit only stops a user deleting or renaming entries owned by
# someone else; it does not stop CREATING /tmp/hive in the window between the
# boot that wipes /tmp and the checkout being restored. hive-deploy.timer fires
# OnBootSec=60, so the winner of that race had their scripts ROOT-INSTALLED
# host-wide within a minute of boot — strictly worse than the `dev`-execution
# payoff of #5483, yet this was the one unit in the class left unguarded.
#
# WHY THIS TEST RUNS THE GUARD INSTEAD OF GREPPING THE UNIT. A grep for
# "ExecStartPre" would pass against a guard that never rejects anything (an
# inline `find` exits 0 whether or not it matched). The assertions below
# EXECUTE bin/hive-checkout-guard.sh against real directory trees, once per
# attack shape, and assert on its EXIT STATUS — including the pre-fix
# arrangement, which must be rejected.
set -uo pipefail

PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; [ $# -gt 1 ] && echo "        $2"; FAIL=$((FAIL + 1)); }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
UNIT="${ROOT}/systemd/hive-deploy.service"
GUARD="${ROOT}/bin/hive-checkout-guard.sh"
DEPLOY="${ROOT}/bin/hive-deploy.sh"

echo "=== hive-deploy.service does not root-install planted code (#5936) ==="

for f in "$UNIT" "$GUARD" "$DEPLOY"; do
  if [ ! -f "$f" ]; then
    fail "locate $f" "the layout moved — this test cannot verify anything"
    echo ""
    echo "=== Results: $PASS passed, $FAIL failed ==="
    exit 1
  fi
done

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# --- the unit wires the guard in ---------------------------------------------
echo ""
echo "--- the unit file ---"

GUARD_LINE="$(grep -E '^ExecStartPre=' "$UNIT" || true)"
if [ -z "$GUARD_LINE" ]; then
  fail "hive-deploy.service has an ExecStartPre guard" \
       "without one, the sudo drift-sync trusts whatever /tmp/hive holds when the timer fires"
else
  pass "hive-deploy.service has an ExecStartPre guard"
  if printf '%s' "$GUARD_LINE" | grep -q 'hive-checkout-guard.sh'; then
    pass "the guard is hive-checkout-guard.sh (exit status is the assertion)"
  else
    fail "the guard is hive-checkout-guard.sh" "got: ${GUARD_LINE}"
  fi

  # The guard must validate the SOURCE tree the sudo sync copies from — the
  # entrypoint itself is root-owned in /usr/local/bin and is not the exposure.
  if printf '%s' "$GUARD_LINE" | grep -qF -- "/tmp/hive/bin"; then
    pass "the guard checks /tmp/hive/bin (the directory the sudo sync copies FROM)"
  else
    fail "the guard checks /tmp/hive/bin" "got: ${GUARD_LINE}"
  fi

  # The PATH-fronting installs are the payoff; pin that they are named.
  for f in hive.sh gh-wrapper.sh; do
    if printf '%s' "$GUARD_LINE" | grep -qF -- "$f"; then
      pass "the guard pins ${f} (synced over a PATH-fronting install)"
    else
      fail "the guard pins ${f}" "got: ${GUARD_LINE}"
    fi
  done
fi

# PrivateTmp would give the unit a private /tmp, hiding the very checkout the
# script exists to sync from. Asserting its ABSENCE keeps a future "more
# hardening is better" edit from silently turning every run into a no-op error.
if grep -qE '^PrivateTmp=(yes|true|1)' "$UNIT"; then
  fail "PrivateTmp is not set on this unit" \
       "the checkout is under /tmp; a private /tmp hides it and the unit cannot work"
else
  pass "PrivateTmp is not set (it would hide the /tmp/hive checkout)"
fi

# NoNewPrivileges is correct for hive-discord/hive-snapshot but WRONG here, and
# this assertion records why: hive-deploy.sh performs its sync with `sudo cp` /
# `sudo install` / `sudo systemctl`, all of which NoNewPrivileges forbids.
if grep -qE '^NoNewPrivileges=(yes|true|1)' "$UNIT"; then
  fail "NoNewPrivileges is not set on this unit" \
       "hive-deploy.sh syncs via sudo; NoNewPrivileges breaks every sudo call it makes"
else
  pass "NoNewPrivileges is not set (the script's sync steps require sudo)"
fi

# ProtectSystem (any level) makes /usr read-only for the unit — and /usr/local/bin
# is exactly where this unit installs to.
if grep -qE '^ProtectSystem=' "$UNIT"; then
  fail "ProtectSystem is not set on this unit" \
       "every level mounts /usr read-only; this unit installs into /usr/local/bin"
else
  pass "ProtectSystem is not set (the unit installs into /usr/local/bin)"
fi

# --- the guard actually rejects things ---------------------------------------
echo ""
echo "--- the guard, run against real trees ---"

# Build a tree shaped like the real one: a sticky world-writable ancestor
# standing in for /tmp, with the checkout's bin/ underneath it. The synced
# scripts ship 755 in git, so 755 entrypoints must be accepted — the guard's
# group/other-WRITABLE check is what matters, not the exec bit.
ENTRYPOINTS=(hive.sh gh-wrapper.sh hive-deploy.sh hive-checkout-guard.sh)
mk_tree() {
  rm -rf "${WORK}/tmp"
  mkdir -p "${WORK}/tmp/hive/bin"
  chmod 1777 "${WORK}/tmp"
  chmod 755 "${WORK}/tmp/hive" "${WORK}/tmp/hive/bin"
  local f
  for f in "${ENTRYPOINTS[@]}"; do
    : > "${WORK}/tmp/hive/bin/$f"
    chmod 755 "${WORK}/tmp/hive/bin/$f"
  done
}

# expect <rc> <label> -- runs the guard exactly as the unit does.
expect() {
  local want="$1" label="$2"
  local out rc
  out="$(bash "$GUARD" "${WORK}/tmp/hive/bin" "${ENTRYPOINTS[@]}" 2>&1)"
  rc=$?
  if [ "$rc" -eq "$want" ]; then
    if [ "$want" -eq 0 ]; then
      pass "${label}: guard allows startup (rc=0)"
    else
      pass "${label}: guard REFUSES startup (rc=${rc})"
    fi
  else
    fail "${label}: expected rc=${want}, got rc=${rc}" "${out}"
  fi
}

# The good state. If this ever fails the guard is too strict and would stop
# every deploy on a healthy host — a worse outcome than the bug being fixed.
mk_tree
expect 0 "healthy checkout under a sticky /tmp, scripts 755 as shipped"

# THE VULNERABILITY, reproduced: the post-reboot window. /tmp/hive absent means
# any local user can create it; the pre-fix unit would then git-pull inside the
# planted tree and sudo-sync its bin/. A missing directory is a refusal, not a
# skip — and it matches hive-deploy.sh's own "not a git repo" bail-out, so the
# guard never blocks a state the script could have serviced.
mk_tree
rm -rf "${WORK}/tmp/hive"
expect 1 "post-reboot window, /tmp/hive absent (the #5936 race)"

# The attacker's own tree: they got there first and it is theirs to rewrite.
mk_tree
chmod 777 "${WORK}/tmp/hive/bin"
expect 1 "bin/ is world-writable"

# Sticky on the LEAF must not buy an exemption: new files there are the attack.
mk_tree
chmod 1777 "${WORK}/tmp/hive/bin"
expect 1 "bin/ is world-writable even with the sticky bit"

# An intermediate the attacker controls lets them swap the whole subtree.
mk_tree
chmod 777 "${WORK}/tmp/hive"
expect 1 "an ancestor is world-writable without the sticky bit"

mk_tree
chmod 775 "${WORK}/tmp/hive"
expect 1 "an ancestor is group-writable"

# A writable synced script can be rewritten in place before the next timer
# firing, then sudo-copied over its /usr/local/bin counterpart. 775/757 rather
# than 664 because these files legitimately carry the exec bit — the guard must
# key on the WRITE bits, not on mode equality.
mk_tree
chmod 775 "${WORK}/tmp/hive/bin/gh-wrapper.sh"
expect 1 "gh-wrapper.sh is group-writable (and executable, as it ships)"

mk_tree
chmod 757 "${WORK}/tmp/hive/bin/hive.sh"
expect 1 "hive.sh is world-writable (and executable, as it ships)"

# Symlinks are how an attacker redirects an otherwise-clean path at their own
# code, so neither the directory nor a pinned file may be one.
mk_tree
rm -f "${WORK}/tmp/hive/bin/hive.sh"
printf '#!/bin/sh\n' > "${WORK}/evil.sh"
chmod 755 "${WORK}/evil.sh"
ln -s "${WORK}/evil.sh" "${WORK}/tmp/hive/bin/hive.sh"
expect 1 "hive.sh is a symlink"

mk_tree
mv "${WORK}/tmp/hive/bin" "${WORK}/elsewhere"
ln -s "${WORK}/elsewhere" "${WORK}/tmp/hive/bin"
expect 1 "bin/ is a symlink"

# --- the guard is installed before anything references it --------------------
echo ""
echo "--- deploy bootstraps the guard ---"

# hive-deploy.sh's sync loops both skip files that are not already installed,
# so a NEW helper is never bootstrapped by them. #5481 added the explicit
# install; three units' ExecStartPre now depend on exactly the same file, so
# the assertion is repeated here rather than assumed.
if grep -q 'hive-checkout-guard.sh' "$DEPLOY"; then
  pass "hive-deploy.sh installs hive-checkout-guard.sh explicitly"
  if grep -A5 'CHECKOUT_GUARD_SRC=' "$DEPLOY" | grep -q 'install -m 0755'; then
    pass "and installs it executable (0755)"
  else
    fail "the guard is installed executable" "ExecStartPre needs the exec bit"
  fi
else
  fail "hive-deploy.sh installs hive-checkout-guard.sh" \
       "the drift loops skip files that are not already installed, so an upgraded host would never receive it and this unit would fail to start"
fi

if [ -x "$GUARD" ]; then
  pass "bin/hive-checkout-guard.sh is executable in git"
else
  fail "bin/hive-checkout-guard.sh is executable in git" "mode is $(ls -l "$GUARD" | cut -d' ' -f1)"
fi

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
