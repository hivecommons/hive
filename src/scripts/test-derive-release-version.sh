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
# Case 4: ### Security present does not force a major release line cut; Added
# still makes this a minor release unless an explicit marker overrides it.
# ---------------------------------------------------------------------------
out=$(run $'## Unreleased\n\n### Added\n\n- new thing\n\n### Security\n\n- closed a hole\n' v1.3.0)
if [[ "$(get "$out" release)" == "true" && "$(get "$out" bump)" == "minor" && "$(get "$out" version)" == "1.4.0" ]]; then
  note_ok "Security present does not force major; Added still bumps 1.3.0 -> 1.4.0"
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
# Case 8: version sort ignores tag creation order — only numeric value counts.
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
if [[ $fail -ne 0 ]]; then
  echo "RESULT: FAIL"
  exit 1
fi
echo "RESULT: PASS — derive-release-version.sh matches its documented rule."
