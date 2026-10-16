#!/usr/bin/env bash
# Affected-package Go test selection for v2-tests.yml pull requests (#12454).
#
# Usage (from the Go module root, src/):
#   affected-go-packages.sh <base-rev> <head-rev> <out-file>
#
# Writes <out-file> as EITHER the single line `all` (run the full suite) OR the
# sorted import paths of the packages the diff can affect, one per line. An
# empty file is a valid answer: no Go package is affected (for example a
# changelog.d-only diff), and every shard reports success without running.
#
# Selection is deliberately conservative. A package is selected when:
#   1. a changed .go file lives in its directory, or
#   2. it, its test variant or its test binary transitively imports a package
#      with a changed non-test .go file (reverse dependencies over the Deps of
#      `go list -test`; _test.go files are never compiled into importers), or
#   3. one of its _test.go files names a changed file or directory as a string
#      literal (tests that read other packages' sources as text, e.g.
#      "../../cmd/hive/main.go"), or
#   4. it is in ALWAYS_PKGS below (tests that walk the whole source tree).
#
# Anything this script cannot reason about falls back to `all`: a change
# outside src/ (workflows, CI scripts, bin/, config/, Justfile, Makefile,
# dashboard/openapi.json — all read or executed by tests or shards), go.mod or
# go.sum, src/scripts/, the shared test harness under src/internal/, any
# non-Go file under src/ (embedded static assets, docs, policies, fixtures —
# several are read across packages), any testdata/ file, a .go file whose
# directory is not a package in the module, an empty diff, or any git/go
# failure. Push and scheduled runs never call this script: they always run the
# full suite, which is the safety net for anything this misses.
#
# Never fails the job on an analysis problem: every error path writes `all`.
set -uo pipefail

base=${1:?base revision}
head=${2:?head revision}
out=${3:?output file}

# Packages whose tests scan the whole source tree as text rather than through
# imports, so any .go change anywhere can move their verdict. KEEP IN SYNC when
# adding such a test (the push/schedule full runs catch a stale entry).
#   internal/...  sleep/dashboard-import ratchets and settings parity
#   pkg/config    env-vars.md parity walks all of src/
#   pkg/rotation  walks src/ for credential-handling call sites
ALWAYS_PATTERNS=(./internal/... ./pkg/config ./pkg/rotation)

full() {
  echo "affected-go-packages: running the full suite: $*" >&2
  echo all > "$out"
  exit 0
}

repo_root=$(git rev-parse --show-toplevel 2>/dev/null) || full "not inside a git checkout"
module_root=$(pwd -P)
case "$module_root/" in
  "$repo_root/src/") ;;
  *) full "must run from $repo_root/src (ran from $module_root)" ;;
esac

git rev-parse -q --verify "${base}^{commit}" >/dev/null || full "base revision $base is not available"
git rev-parse -q --verify "${head}^{commit}" >/dev/null || full "head revision $head is not available"

# --no-renames lists both sides of a rename, so a moved file selects (or
# invalidates) both its old and new package.
changed=$(git diff --no-renames --name-only "$base" "$head") || full "git diff $base $head failed"
[ -n "$changed" ] || full "empty changed-file list"

mod_path=$(go list -m 2>/dev/null) || full "go list -m failed"

# Directory -> import path for every package in the module.
dir_map=$(go list -f '{{.Dir}}|{{.ImportPath}}' ./... 2>/dev/null) || full "go list ./... failed"
[ -n "$dir_map" ] || full "go list ./... returned no packages"

seeds=""
rdep_seeds=""
literals=""
while IFS= read -r f; do
  [ -n "$f" ] || continue
  case "$f" in
    changelog.d/*|CHANGELOG.md|docs/*) continue ;;  # never read by Go tests
    src/*) ;;
    *) full "$f is outside src/" ;;
  esac
  rel=${f#src/}
  case "$rel" in
    go.mod|go.sum) full "$f changes the module graph" ;;
    scripts/*) full "$f is a repository script" ;;
    internal/*) full "$f is part of the shared test harness" ;;
    testdata/*|*/testdata/*) full "$f is a test fixture (fixtures are read across packages)" ;;
    *.go) ;;
    *) full "$f is not a Go source file (assets, docs and fixtures can be read by any package's tests)" ;;
  esac
  dir="$repo_root/$(dirname "$f")"
  pkg=$(awk -F'|' -v d="$dir" '$1 == d { print $2; exit }' <<<"$dir_map")
  [ -n "$pkg" ] || full "$f is not in a package of $mod_path (deleted or build-constrained package)"
  seeds="$seeds$pkg"$'\n'
  # _test.go files are never compiled into importers, so only a non-test
  # change has to be expanded to reverse dependencies.
  case "$f" in
    *_test.go) ;;
    *) rdep_seeds="$rdep_seeds$pkg"$'\n' ;;
  esac
  # Literal forms a test uses to read this file or its package directory.
  literals="$literals$(basename "$f")\""$'\n'"$(dirname "$rel")\""$'\n'
done <<<"$changed"

selected="$out.unsorted"
: > "$selected"
trap 'rm -f "$selected"' EXIT

if [ -n "$seeds" ]; then
  # Every package, test variant ("p [p.test]", "p_test [p.test]") and test
  # binary ("p.test") with its transitive deps; a line's owning package is its
  # import path with the variant/binary decoration stripped.
  graph=$(go list -test -f '{{.ImportPath}}|{{join .Deps "|"}}' ./... 2>/dev/null) || full "go list -test ./... failed"
  printf '%s' "$seeds" >> "$selected"
  awk -F'|' '
    NR == FNR { if ($0 != "") seed[$0] = 1; next }
    {
      owner = $1
      sub(/ \[.*$/, "", owner)
      sub(/\.test$/, "", owner)
      sub(/_test$/, "", owner)
      hit = (owner in seed)
      for (i = 2; !hit && i <= NF; i++) {
        d = $i
        sub(/ \[.*$/, "", d)
        if (d in seed) hit = 1
      }
      if (hit) print owner
    }
  ' <(printf '%s' "$rdep_seeds") <(printf '%s\n' "$graph") >> "$selected" || full "reverse-dependency expansion failed"

  # Text references from _test.go files (see rule 3 in the header).
  refs=$(printf '%s' "$literals" | LC_ALL=C sort -u | grep -vx '\."' || true)
  if [ -n "$refs" ]; then
    while IFS= read -r tf; do
      d="$repo_root/src/$(dirname "${tf#./}")"
      awk -F'|' -v d="$d" '$1 == d { print $2; exit }' <<<"$dir_map" >> "$selected"
    done < <(grep -rlF --include='*_test.go' -f <(printf '%s\n' "$refs") . 2>/dev/null || true)
  fi

  go list "${ALWAYS_PATTERNS[@]}" >> "$selected" 2>/dev/null || full "go list ${ALWAYS_PATTERNS[*]} failed"
fi

# Bound the answer by the package set the shards actually test.
universe=$(go list ./pkg/... ./cmd/... ./internal/... 2>/dev/null) || full "go list of the tested package set failed"
LC_ALL=C sort -u "$selected" | grep -Fx -f <(printf '%s\n' "$universe") > "$out" || true

echo "affected-go-packages: $(grep -c . "$out" || true) of $(printf '%s\n' "$universe" | grep -c .) tested packages selected" >&2
sed 's/^/  /' "$out" >&2
