#!/usr/bin/env bash
# Exhaustive Spin verification of contribute-lease.pml — every property, every run.
#
# Each row of the matrix below carries an EXPECTED verdict:
#   pass — the property must hold (0 errors); a violation fails this script.
#   fail — a KNOWN counterexample exists (documented pre-fix witness; see
#          README.md). The run must still find it: if a "fail" row stops failing,
#          either the model no longer encodes the historical bug or the intended
#          design changed. Either way, a human must look, so this exits nonzero.
#
# Requirements: spin (>= 6.x, the Bell Labs model checker) and a C compiler.
set -u

cd "$(dirname "$0")" || exit 2

SPIN=${SPIN:-spin}
if ! command -v "$SPIN" >/dev/null 2>&1; then
    echo "error: spin not found (install: apt-get install spin / brew install spin)" >&2
    exit 2
fi
if ! "$SPIN" -V 2>/dev/null | grep -q "Spin Version"; then
    echo "error: '$SPIN -V' does not look like the Spin model checker" >&2
    exit 2
fi
CC=${CC:-}
if [ -z "$CC" ]; then
    CC=$(command -v gcc || command -v cc) || { echo "error: no C compiler" >&2; exit 2; }
fi
PAN_DEPTH=${PAN_DEPTH:-500000}

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

failures=0
printf '%-28s %-10s %-10s %12s\n' "RUN" "EXPECT" "RESULT" "STATES"
printf '%-28s %-10s %-10s %12s\n' "---" "------" "------" "------"

# run <name> <expect:pass|fail> <spin-defines> <cc-extra> <pan-args>
run() {
    local name=$1 expect=$2 defines=$3 ccextra=$4 panargs=$5
    local dir="$workdir/$name"
    mkdir -p "$dir"
    # shellcheck disable=SC2086
    if ! (cd "$dir" && "$SPIN" -a $defines "$OLDPWD/contribute-lease.pml" >/dev/null 2>&1 &&
          "$CC" -O2 -DCOLLAPSE $ccextra -o pan pan.c >/dev/null 2>&1); then
        printf '%-28s %-10s %-10s %12s\n' "$name" "$expect" "BUILD-ERR" "-"
        failures=$((failures + 1))
        return
    fi
    local out
    # shellcheck disable=SC2086
    out=$(cd "$dir" && ./pan -m"$PAN_DEPTH" $panargs 2>&1)
    local states result verdict
    states=$(printf '%s\n' "$out" | sed -n 's/^ *\([0-9.e+]*\) states, stored.*/\1/p' | head -1)
    if printf '%s\n' "$out" | grep -q "errors: 0"; then
        result=pass
    elif printf '%s\n' "$out" | grep -Eq "errors: [1-9]"; then
        result=fail
    else
        result=inconclusive
    fi
    if [ "$result" = "$expect" ]; then
        verdict=$result
    else
        verdict="UNEXPECTED-$result"
        failures=$((failures + 1))
    fi
    printf '%-28s %-10s %-10s %12s\n' "$name" "$expect" "$verdict" "${states:-?}"
}

SAFETY="-DSAFETY -DNOCLAIM"

# --- properties expected to HOLD -------------------------------------------
run p1_no_double_lease     pass ""                    "$SAFETY" ""
run p2_backoff_no_abandon  pass "-DMON_RECONNECT"     "$SAFETY" ""

# --- pre-fix witnesses expected to FAIL ------------------------------------
# #7773: release cooldown (10m) lapses while the resume lease (30m) is still
# live, so another contributor can receive the same issue.
run w_7773_cooldown_lt_lease fail "-DBUG_COOLDOWN_LEASE" "$SAFETY" ""
# #7838: instant read-fail release books abandoned_disconnect before the relay's
# first reconnect attempt at BASE_RECONNECT_DELAY_MS=1000ms.
run w_7838_instant_release fail "-DMON_RECONNECT -DBUG_INSTANT_RELEASE" "$SAFETY" ""

echo
if [ "$failures" -ne 0 ]; then
    echo "FORMAL VERIFICATION: $failures run(s) diverged from the expected verdicts."
    exit 1
fi
echo "FORMAL VERIFICATION: all runs matched their expected verdicts."
