#!/usr/bin/env bash
# test-derive-release-version.sh — exercises src/scripts/derive-release-version.sh
# against fixture CHANGELOG.md files and a scratch git repo carrying tags, so
# the release-worthiness and bump-type inference are proven rather than
# asserted in a PR description.
#
# Usage: src/scripts/test-derive-release-version.sh
set -uo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
derive="$script_dir/derive-release-version.sh"
tmp_root=${TMPDIR:-"$script_dir/../.test-tmp"}
mkdir -p "$tmp_root"
tmp=$(mktemp -d "$tmp_root/derive-version.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

fail=0
note_fail() { echo "  FAIL: $*"; fail=1; }
note_ok()   { echo "  ok: $*"; }

# A scratch git repo so `git tag -l` has somewhere real to read from. Every
# case runs inside this repo's worktree with its own CHANGELOG.md.
repo="$tmp/repo"
git init -q "$repo"
git -C "$repo" config user.email test@example.com
git -C "$repo" config user.name "Test"
touch "$repo/placeholder"
git -C "$repo" add placeholder
git -C "$repo" commit -q -m "init"

run() {
  # run <changelog-fixture-content-var-name> [tag...]
  local content=$1; shift
  printf '%s' "$content" > "$repo/CHANGELOG.md"
  for t in "$@"; do
    git -C "$repo" tag -f "$t" >/dev/null 2>&1
  done
  ( cd "$repo" && GITHUB_OUTPUT="" bash "$derive" CHANGELOG.md )
}

get() {
  # get <output-blob> <key>
  printf '%s\n' "$1" | grep "^${2}=" | tail -1 | cut -d= -f2-
}

# ---------------------------------------------------------------------------
# Case 1: empty Unreleased section => no release.
# ---------------------------------------------------------------------------
out=$(run $'## Unreleased\n\n## 2026-01-01\n\n### Added\n\n- old thing\n')
if [[ "$(get "$out" release)" == "false" ]]; then
  note_ok "empty Unreleased section => release=false"
else
  note_fail "empty Unreleased section should yield release=false, got: $out"
fi

# ---------------------------------------------------------------------------
# Case 2: Unreleased with only ### Fixed => patch bump, no prior tag => 0.0.1.
# ---------------------------------------------------------------------------
git -C "$repo" tag -d $(git -C "$repo" tag -l 'v*') >/dev/null 2>&1 || true
out=$(run $'## Unreleased\n\n### Fixed\n\n- squashed a bug\n')
if [[ "$(get "$out" release)" == "true" && "$(get "$out" bump)" == "patch" && "$(get "$out" version)" == "0.0.1" ]]; then
  note_ok "Fixed-only, no prior tag => patch bump to 0.0.1"
else
  note_fail "expected release=true bump=patch version=0.0.1, got: $out"
fi

# ---------------------------------------------------------------------------
# Case 3: ### Added present => minor bump, from an existing tag.
# ---------------------------------------------------------------------------
out=$(run $'## Unreleased\n\n### Added\n\n- new thing\n\n### Fixed\n\n- also a fix\n' v1.2.3)
if [[ "$(get "$out" release)" == "true" && "$(get "$out" bump)" == "minor" && "$(get "$out" version)" == "1.3.0" ]]; then
  note_ok "Added present with Fixed also present => minor wins, 1.2.3 -> 1.3.0"
else
  note_fail "expected release=true bump=minor version=1.3.0, got: $out"
fi

# ---------------------------------------------------------------------------
# Case 4: ### Security present is release-worthy but does not cut a new
# release line by itself, so Added still controls the minor bump.
# ---------------------------------------------------------------------------
out=$(run $'## Unreleased\n\n### Added\n\n- new thing\n\n### Security\n\n- closed a hole\n' v1.3.0)
if [[ "$(get "$out" release)" == "true" && "$(get "$out" bump)" == "minor" && "$(get "$out" version)" == "1.4.0" ]]; then
  note_ok "Security plus Added => minor bump, 1.3.0 -> 1.4.0"
else
  note_fail "expected release=true bump=minor version=1.4.0, got: $out"
fi

# ---------------------------------------------------------------------------
# Case 5: escape hatch <!-- release: none --> suppresses even a non-empty,
# Added-carrying section.
# ---------------------------------------------------------------------------
out=$(run $'## Unreleased\n\n<!-- release: none -->\n\n### Added\n\n- new thing but not yet\n' v2.0.0)
if [[ "$(get "$out" release)" == "false" ]]; then
  note_ok "release:none marker suppresses an otherwise release-worthy section"
else
  note_fail "expected release=false with release:none marker, got: $out"
fi

# ---------------------------------------------------------------------------
# Case 6: escape hatch forces a bump type regardless of headers present.
# ---------------------------------------------------------------------------
out=$(run $'## Unreleased\n\n<!-- release: major -->\n\n### Fixed\n\n- tiny fix, but maintainer wants major\n' v2.0.0)
if [[ "$(get "$out" release)" == "true" && "$(get "$out" bump)" == "major" && "$(get "$out" version)" == "3.0.0" ]]; then
  note_ok "release:major marker forces major bump over Fixed-only content"
else
  note_fail "expected release=true bump=major version=3.0.0, got: $out"
fi

# ---------------------------------------------------------------------------
# Case 7: conflicting markers is a loud error, not a silent pick.
# ---------------------------------------------------------------------------
if run $'## Unreleased\n\n<!-- release: major -->\n<!-- release: patch -->\n\n### Fixed\n\n- x\n' v2.0.0 >/dev/null 2>&1; then
  note_fail "conflicting release markers should fail, but the script exited 0"
else
  note_ok "conflicting release markers fail loudly"
fi

# ---------------------------------------------------------------------------
# Case 8: fenced prose that mentions `## Unreleased` is ignored; only the real
# heading controls the body extracted for release inference.
# ---------------------------------------------------------------------------
out=$(run $'```markdown\n## Unreleased\n\n### Added\n\n- example only\n```\n\n## Unreleased\n\n## 2026-01-01\n')
if [[ "$(get "$out" release)" == "false" ]]; then
  note_ok "fenced prose mention of ## Unreleased is ignored"
else
  note_fail "fenced prose mention should not make a release, got: $out"
fi

# ---------------------------------------------------------------------------
# Case 9: version sort ignores tag creation order — only numeric value counts.
# A repo with v1.9.0 tagged AFTER v1.10.0 (out-of-order creation, possible
# after a manual retag) must still treat v1.10.0 as the base.
# ---------------------------------------------------------------------------
git -C "$repo" tag -d $(git -C "$repo" tag -l 'v*') >/dev/null 2>&1 || true
git -C "$repo" tag v1.10.0
git -C "$repo" tag v1.9.0
out=$(run $'## Unreleased\n\n### Fixed\n\n- x\n')
if [[ "$(get "$out" version)" == "1.10.1" ]]; then
  note_ok "base version picked by numeric sort (v1.10.0), not tag creation order"
else
  note_fail "expected base v1.10.0 -> 1.10.1, got: $out"
fi

echo
# ---------------------------------------------------------------------------
# Case L1: on release line v5 with only v4.x tags => first release is v5.0.0,
# whatever the inferred bump. v4 tags must never seed a v5 number.
# ---------------------------------------------------------------------------
git -C "$repo" tag -d $(git -C "$repo" tag -l 'v*') >/dev/null 2>&1 || true
git -C "$repo" checkout -q -b v5
out=$(run $'## Unreleased\n\n### Added\n\n- brand new line\n' v4.65.0 v4.64.2)
if [[ "$(get "$out" release)" == "true" && "$(get "$out" version)" == "5.0.0" ]]; then
  note_ok "first release on line v5 with only v4 tags => 5.0.0"
else
  note_fail "expected first v5 release to be 5.0.0, got: $out"
fi

# ---------------------------------------------------------------------------
# Case L2: on line v5 with v5.0.0 present and a newer v4 patch tag => the v5
# line's own latest tag is the base (5.1.0 for Added), not v4's.
# ---------------------------------------------------------------------------
out=$(run $'## Unreleased\n\n### Added\n\n- more\n' v4.66.0 v5.0.0)
if [[ "$(get "$out" version)" == "5.1.0" ]]; then
  note_ok "line v5 bumps from its own latest tag => 5.1.0"
else
  note_fail "expected 5.1.0 on line v5, got: $out"
fi
git -C "$repo" checkout -q - 2>/dev/null || git -C "$repo" checkout -q master 2>/dev/null || git -C "$repo" checkout -q main

# ---------------------------------------------------------------------------
# Case L3: detached HEAD (what actions/checkout@<sha> produces) with no
# RELEASE_LINE => hard error. Falling through to the global latest tag here is
# how v4.73.3 was minted from v5; the script must refuse, not guess.
# ---------------------------------------------------------------------------
git -C "$repo" checkout -q --detach
printf '%s' $'## Unreleased\n\n### Fixed\n\n- x\n' > "$repo/CHANGELOG.md"
if ( cd "$repo" && GITHUB_OUTPUT="" RELEASE_LINE="" bash "$derive" CHANGELOG.md ) >/dev/null 2>"$tmp/l3.err"; then
  note_fail "detached HEAD without RELEASE_LINE must fail, but succeeded"
elif grep -q 'RELEASE_LINE' "$tmp/l3.err"; then
  note_ok "detached HEAD without RELEASE_LINE => refused, names RELEASE_LINE"
else
  note_fail "detached HEAD failed for the wrong reason: $(cat "$tmp/l3.err")"
fi

# ---------------------------------------------------------------------------
# Case L4: detached HEAD WITH RELEASE_LINE=v5 and only v4 tags => 5.0.0. The
# explicit line, not the branch name, scopes the base tag.
# ---------------------------------------------------------------------------
git -C "$repo" tag -d $(git -C "$repo" tag -l 'v*') >/dev/null 2>&1 || true
git -C "$repo" tag -f v4.66.0 >/dev/null 2>&1
out=$( cd "$repo" && GITHUB_OUTPUT="" RELEASE_LINE=v5 bash "$derive" CHANGELOG.md 2>&1 )
if [[ "$(get "$out" release)" == "true" && "$(get "$out" version)" == "5.0.0" ]]; then
  note_ok "detached HEAD with RELEASE_LINE=v5 and only v4 tags => 5.0.0"
else
  note_fail "expected 5.0.0 via RELEASE_LINE on detached HEAD, got: $out"
fi
git -C "$repo" checkout -q master 2>/dev/null || git -C "$repo" checkout -q main 2>/dev/null || true

if [[ $fail -ne 0 ]]; then
  echo "RESULT: FAIL"
  exit 1
fi
echo "RESULT: PASS — derive-release-version.sh matches its documented rule."
