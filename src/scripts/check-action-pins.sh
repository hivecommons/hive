#!/usr/bin/env bash
# check-action-pins.sh — assert every SHA-pinned GitHub Action actually exists.
#
# WHY: tagged-release.yml shipped `anchore/sbom-action@f4dccdb4...`, a 40-hex
# SHA that resolves to no commit in that repository (#4908). It LOOKS like a
# correct pin
# — right shape, plausible `# v0.20.9` comment beside it — and nothing catches a
# fabricated SHA until the workflow runs and dies at "Prepare all required
# actions". For a tag-triggered release workflow that means the break is only
# discovered when someone cuts a release, which is the worst time to find it.
#
# This checks the pin shape statically and, when a token is available, resolves
# each SHA against the GitHub API.
#
# Usage: src/scripts/check-action-pins.sh [workflow-dir]
set -uo pipefail

DIR="${1:-.github/workflows}"
fail=0

# Every `uses:` on a third-party action must be a 40-hex SHA, not a tag or
# branch. A moving ref is a supply-chain hole; that part is shape-only and needs
# no network.
while IFS= read -r line; do
  file="${line%%:*}"
  ref="$(printf '%s' "$line" | grep -oE '[a-zA-Z0-9._-]+/[a-zA-Z0-9._-]+@[^ ]+' | head -1)"
  [ -z "$ref" ] && continue
  sha="${ref#*@}"
  case "$sha" in
    [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;;
    *) echo "NOT-PINNED $file: $ref (use a 40-hex commit SHA)"; fail=1; continue ;;
  esac
done < <(grep -rhnE '^\s*(-\s*)?uses:\s*[a-zA-Z0-9._-]+/' "$DIR" 2>/dev/null | grep -v '^\s*#')

# Resolve each unique pin. Skipped without a token so the script stays usable
# offline; CI always has one.
if command -v gh >/dev/null 2>&1; then
  # Extract owner/repo from the `uses:` VALUE, taking only the first two path
  # segments: a reusable workflow is `owner/repo/.github/workflows/x.yml@SHA`,
  # so naively splitting on the last '/' asks the API about a path, not a repo.
  grep -rhoE 'uses:[[:space:]]*[a-zA-Z0-9._-]+/[a-zA-Z0-9._/-]+@[0-9a-f]{40}' "$DIR" 2>/dev/null \
    | sed -E 's/^uses:[[:space:]]*//' | sort -u | while read -r ref; do
    sha="${ref#*@}"; path="${ref%@*}"
    repo="$(printf '%s' "$path" | cut -d/ -f1,2)"
    if ! gh api "repos/$repo/commits/$sha" --jq '.sha' >/dev/null 2>&1; then
      echo "MISSING-SHA $repo@$sha does not exist in that repository"
      exit 1
    fi
  done || fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo "action pin check FAILED"
  exit 1
fi
echo "action pin check OK"
