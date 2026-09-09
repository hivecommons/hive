#!/usr/bin/env bash
# test-check-release-sha-ancestry.sh — exercises
# src/scripts/check-release-sha-ancestry.sh (#6419) against a throwaway fixture
# repo so the ancestry guard docker.yml's `gate` job now calls is proven
# against real `git` behaviour, not just read for plausibility.
#
# Covers the property the finding demands: a well-formed 40-hex SHA is not
# enough to authorize a build/publish. It must also be a commit object that
# actually exists AND be reachable from the dispatched ref's history — an
# unreviewed PR head commit, a different branch's tip, or a garbage string all
# fail closed.
#
# Usage: src/scripts/test-check-release-sha-ancestry.sh
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${HERE}/check-release-sha-ancestry.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail=0
pass() { echo "  ok: $*"; }
bad()  { echo "  FAIL: $*"; fail=1; }

# expect <expected-exit> <description> -- <guard args...>
expect() {
  local want="$1" desc="$2"; shift 3
  local out rc
  out="$(bash "$GUARD" "$@" 2>&1)"; rc=$?
  if [[ $rc -eq $want ]]; then
    pass "$desc"
  else
    bad "$desc (guard exited ${rc}, expected ${want})"
    echo "$out" | sed 's/^/      | /'
  fi
  LAST_OUT="$out"
}

expect_mentions() {
  local needle="$1" desc="$2"
  if grep -qF -- "$needle" <<< "$LAST_OUT"; then
    pass "$desc"
  else
    bad "$desc (no mention of '${needle}' in the guard's output)"
    echo "$LAST_OUT" | sed 's/^/      | /'
  fi
}

# ---------------------------------------------------------------------------
# Fixture repo: two histories, "release" (the dispatched branch) and a
# sibling "rogue" branch that shares no ancestry with it (simulating an
# unreviewed PR head).
# ---------------------------------------------------------------------------
REPO="${TMP}/repo"
git init -q "$REPO"
git -C "$REPO" config user.email "test@example.com"
git -C "$REPO" config user.name "test"

git -C "$REPO" commit -q --allow-empty -m "base"
BASE_SHA="$(git -C "$REPO" rev-parse HEAD)"

git -C "$REPO" commit -q --allow-empty -m "release commit"
RELEASE_SHA="$(git -C "$REPO" rev-parse HEAD)"
git -C "$REPO" branch release HEAD

# A rogue branch built from an unrelated root, so its tip shares no ancestry
# with `release` at all — the unreviewed-PR-head scenario from the finding.
git -C "$REPO" checkout -q --orphan rogue
git -C "$REPO" commit -q --allow-empty -m "rogue root"
git -C "$REPO" commit -q --allow-empty -m "rogue head"
ROGUE_SHA="$(git -C "$REPO" rev-parse HEAD)"

git -C "$REPO" checkout -q release

echo "== check-release-sha-ancestry.sh contract =="
echo

expect 0 "ancestor SHA on its own branch's history is accepted" -- \
  "$BASE_SHA" release "$REPO"

expect 0 "the branch tip itself is trivially its own ancestor" -- \
  "$RELEASE_SHA" release "$REPO"

expect 1 "a commit that exists but is on an unrelated branch (rogue PR head) is refused" -- \
  "$ROGUE_SHA" release "$REPO"
expect_mentions "not an ancestor" "refusal names the ancestry failure"
expect_mentions "$ROGUE_SHA" "refusal names the rejected SHA"

expect 1 "a well-formed hex SHA that names no real object is refused" -- \
  "ffffffffffffffffffffffffffffffffffffffff" release "$REPO"
expect_mentions "does not resolve to a commit object" "refusal names the missing-object failure"

expect 1 "a short/malformed SHA is refused before any git lookup" -- \
  "deadbeef" release "$REPO"
expect_mentions "40-character lowercase commit SHA" "refusal names the format failure"

expect 1 "an uppercase-hex SHA (still 40 chars, wrong case) is refused" -- \
  "$(tr 'a-f' 'A-F' <<< "$RELEASE_SHA")" release "$REPO"

expect 1 "no target SHA argument is refused" -- \
  "" release "$REPO"

echo
if [[ $fail -ne 0 ]]; then
  echo "RESULT: FAIL — check-release-sha-ancestry.sh does not enforce the ancestry contract."
  exit 1
fi
echo "RESULT: PASS — check-release-sha-ancestry.sh accepts only commits reachable from the dispatched ref and fails closed on everything else (malformed, absent, or off-branch)."
