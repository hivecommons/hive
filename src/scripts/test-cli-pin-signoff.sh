#!/usr/bin/env bash
# Self-test for cli-pin-signoff.sh (#18697). No network. Builds a commit the way
# the bump workflow does (bot author + bot sign-off, then the extra trailers)
# and applies the probot DCO rule to the commit GitHub's squash would write:
# the author email must appear in some Signed-off-by trailer.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/cli-pin-signoff.sh"
PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "  ok: $1"; }
fail() { FAIL=$((FAIL+1)); echo "  FAIL: $1"; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# dco_ok AUTHOR_EMAIL MESSAGE: probot/dco's rule, case-insensitive.
dco_ok() {
  printf '%s\n' "$2" | grep -iE '^Signed-off-by: .*<'"$1"'>[[:space:]]*$' >/dev/null
}

build_msg() {
  local login="$1" id="$2"; shift 2
  git init -q "$TMP/r"
  (
    cd "$TMP/r"
    # Exported so the --amend below has a committer too: CI runners have no
    # global git identity.
    export GIT_AUTHOR_NAME="hive-release[bot]" GIT_AUTHOR_EMAIL="hive-release@hive.kubestellar.io"
    export GIT_COMMITTER_NAME="hive-release[bot]" GIT_COMMITTER_EMAIL="hive-release@hive.kubestellar.io"
    git commit -q --allow-empty -s -m "images: bump x 1 -> 2"
    trailers="$(bash "$SCRIPT" "$login" "$id" "$@")"
    args=()
    while IFS= read -r t; do [ -n "$t" ] && args+=(--trailer "$t"); done <<<"$trailers"
    git commit -q --amend --allow-empty --no-edit "${args[@]}"
    git log -1 --format=%B
  )
  rm -rf "$TMP/r"
}

msg="$(build_msg clubanderson 407614 andy@clubanderson.com null "" Andy@ClubAnderson.com)"
dco_ok 'andy@clubanderson\.com' "$msg" && pass "primary author email is signed off" || fail "primary author email missing"
dco_ok '407614\+clubanderson@users\.noreply\.github\.com' "$msg" && pass "noreply identity signed off" || fail "noreply missing"
dco_ok 'hive-release@hive\.kubestellar\.io' "$msg" && pass "bot sign-off kept" || fail "bot sign-off lost"
[ "$(grep -ic '^Signed-off-by: .*andy@clubanderson' <<<"$msg")" = 1 ] && pass "duplicate/null/empty emails dropped" || fail "duplicate trailer"

msg="$(build_msg someone 5)"
if dco_ok 'andy@clubanderson\.com' "$msg"; then fail "unrelated author wrongly matched"; else pass "no email supplied -> only noreply (author without sign-off is still detected)"; fi

echo "$PASS passed, $FAIL failed"
[ "$FAIL" = 0 ]
