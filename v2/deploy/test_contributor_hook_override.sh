#!/usr/bin/env bash
# Tests that entrypoint.d hooks can override the backend tables in
# bin/contributor-agent.sh (kubestellar/hive#2833).
#
# config/backends.conf defines backend_binary() / backend_perm_flag() as bash
# FUNCTIONS. Sourcing a function definition silently replaces an earlier one,
# so whichever of {backends.conf, entrypoint.d hooks} is sourced LAST wins.
# The entrypoint used to source backends.conf after the hooks, which discarded
# a downstream image's override with no warning — making the documented seam
# useless for retargeting a backend's binary, its most obvious use.
#
# Rather than grepping for the order, this extracts the two real blocks from
# the shipped entrypoint and executes them, so it fails if the order regresses.
#
# Run: bash v2/deploy/test_contributor_hook_override.sh
set -euo pipefail

PASS=0
FAIL=0

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ENTRYPOINT="${REPO_ROOT}/bin/contributor-agent.sh"
BACKENDS_CONF="${REPO_ROOT}/config/backends.conf"

check() {
  local label="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label"
    echo "        want: '$want'"
    echo "        got:  '$got'"
    FAIL=$((FAIL + 1))
  fi
}

echo "=== contributor entrypoint.d hook override tests ==="

[ -f "$ENTRYPOINT" ]     || { echo "  FAIL: missing $ENTRYPOINT"; exit 1; }
[ -f "$BACKENDS_CONF" ]  || { echo "  FAIL: missing $BACKENDS_CONF"; exit 1; }

# Extract the two ordered blocks verbatim from the shipped entrypoint, so this
# tests the real code rather than a copy that can drift away from it.
# 1. the backends.conf sourcing block
BACKENDS_BLOCK="$(sed -n '/^BACKENDS_CONF=/,/^fi$/p' "$ENTRYPOINT")"
# 2. the entrypoint.d hook loop + HIVE_PRE_AGENT_HOOK
HOOK_BLOCK="$(sed -n '/^if \[\[ -d \/etc\/hive\/entrypoint.d \]\]; then$/,/^fi$/p' "$ENTRYPOINT")"
PRE_HOOK_BLOCK="$(sed -n '/^if \[\[ -n "${HIVE_PRE_AGENT_HOOK:-}" \]\]; then$/,/^fi$/p' "$ENTRYPOINT")"

[ -n "$BACKENDS_BLOCK" ] || { echo "  FAIL: could not extract backends.conf block"; exit 1; }
[ -n "$HOOK_BLOCK" ]     || { echo "  FAIL: could not extract entrypoint.d block"; exit 1; }
[ -n "$PRE_HOOK_BLOCK" ] || { echo "  FAIL: could not extract HIVE_PRE_AGENT_HOOK block"; exit 1; }

# The ordering IS the fix: assert backends.conf is sourced above the hook seam.
backends_line=$(grep -n '^BACKENDS_CONF=' "$ENTRYPOINT" | head -1 | cut -d: -f1)
hooks_line=$(grep -n '^if \[\[ -d /etc/hive/entrypoint.d \]\]; then$' "$ENTRYPOINT" | head -1 | cut -d: -f1)
if [ "$backends_line" -lt "$hooks_line" ]; then
  check "backends.conf sourced before entrypoint.d hooks" "yes" "yes"
else
  check "backends.conf sourced before entrypoint.d hooks" \
        "backends.conf(<$hooks_line)" "backends.conf(line $backends_line) after hooks(line $hooks_line)"
fi

# run_entrypoint <hook-body> — replays the real ordered blocks in a sandbox
# with a fake /etc/hive/entrypoint.d, and prints "<binary>|<permflag>".
# An empty hook body means "no hook installed".
run_entrypoint() {
  local hook_body="$1"
  local dir
  dir="$(mktemp -d)"

  mkdir -p "$dir/entrypoint.d"
  if [ -n "$hook_body" ]; then
    printf '%s\n' "$hook_body" > "$dir/entrypoint.d/10-override.sh"
  fi

  # Assemble the blocks in the order they actually appear in the entrypoint --
  # NOT the order they happen to be listed in this file. The ordering is the
  # thing under test, so hardcoding it here would make every behavioural case
  # pass against the buggy entrypoint too.
  local first second
  if [ "$backends_line" -lt "$hooks_line" ]; then
    first="$BACKENDS_BLOCK"
    second="$(printf '%s\n%s' "$HOOK_BLOCK" "$PRE_HOOK_BLOCK")"
  else
    first="$(printf '%s\n%s' "$HOOK_BLOCK" "$PRE_HOOK_BLOCK")"
    second="$BACKENDS_BLOCK"
  fi

  # Run in a subshell so redefined functions never leak between cases.
  (
    set -euo pipefail
    # Consumed by the eval'd BACKENDS_BLOCK below, not by this script directly.
    # shellcheck disable=SC2034
    SCRIPT_DIR="${REPO_ROOT}/bin"
    # Point the hook loop at the sandbox instead of the real /etc/hive.
    local_blocks="$(printf '%s\n%s' "$first" "$second" \
      | sed "s#/etc/hive/entrypoint.d#${dir}/entrypoint.d#g")"
    # The blocks echo progress ("Running entrypoint hook: ...") on stdout;
    # send that to stderr so only the resolved values are captured.
    eval "$local_blocks" >&2
    printf '%s|%s' "$(backend_binary goose)" "$(backend_perm_flag goose)"
  )

  rm -rf "$dir"
}

# 1. No hook: the stock backends.conf mapping applies.
got="$(run_entrypoint "")"
check "default backend_binary goose (no hook)" "goose|" "$got"

# 2. A hook that retargets a backend's binary wins — the bug in #2833.
got="$(run_entrypoint 'backend_binary() { case "$1" in goose) echo "my-custom-goose-wrapper" ;; *) echo "$1" ;; esac; }')"
check "hook overrides backend_binary" "my-custom-goose-wrapper|" "$got"

# 3. A hook may also override the permission flags.
got="$(run_entrypoint 'backend_perm_flag() { echo "--hook-flag"; }')"
check "hook overrides backend_perm_flag" "goose|--hook-flag" "$got"

# 4. A hook that wraps rather than replaces still sees the upstream table,
#    which only works if backends.conf was sourced FIRST.
got="$(run_entrypoint '
_upstream_backend_binary() { :; }
eval "_upstream_backend_binary() $(declare -f backend_binary | tail -n +2)"
backend_binary() { echo "wrapped-$(_upstream_backend_binary "$1")"; }')"
check "hook can wrap the upstream backend_binary" "wrapped-goose|" "$got"

# 5. HIVE_PRE_AGENT_HOOK gets the same override power as a dropped-in file.
got="$(HIVE_PRE_AGENT_HOOK='backend_binary() { echo "env-hook-binary"; }' run_entrypoint "")"
check "HIVE_PRE_AGENT_HOOK overrides backend_binary" "env-hook-binary|" "$got"

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
