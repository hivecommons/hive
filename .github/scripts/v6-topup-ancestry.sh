#!/usr/bin/env bash
# v6-topup-ancestry.sh — the v5-ancestry bookkeeping behind
# .github/workflows/v6-topup.yml (#18931).
#
# WHY. A v5 -> v6 top-up PR merged with a squash or rebase puts v5's CONTENT on
# v6 but not v5's HISTORY. `git merge-base origin/v5 origin/v6` then stays at
# the previous merge commit, and every later forward-merge replays the whole
# range against v6's already-adapted copies of the same files: #18689 was
# squash-merged and the next top-up hit 109 conflicted files (17 add/add).
#
# The workflow records the v5 head each top-up carried in the PR body as
#   <!-- hive:topup carried=<40-hex sha> -->
# and, when that head is not an ancestor of origin/v6 (the PR was not merged
# with a merge commit), restores the ancestry with an ours-merge of it before
# merging origin/v5. An ours-merge records history and changes no file.
#
# Subcommands (run from the repo root):
#   marker-line <sha>                       print the PR-body marker line
#   marker <body-file>                      print the carried sha in the body, if any
#   verify <carried> <pr-head> <merge-commit>
#       exit 0 when the marker is trustworthy: the PR head is a two-parent
#       merge whose second parent IS <carried> and the PR's merge commit is on
#       v6. (Whether <carried> is still v5 history is classify's call.) A marker is only a claim in a PR body;
#       an ours-merge of a v5 commit whose content is NOT on v6 would silently
#       drop that content, so the claim is bound to the PR's own head commit.
#   classify <carried|"">                   print one state:
#       level        carried head is an ancestor of origin/v6: ancestry intact
#       unknown      no carried head is known: nothing can be checked
#       lost         carried head is v5 history but not on v6: heal with an
#                    ours-merge of it
#       unrecoverable  carried head is not on origin/v5 (v5 history rewritten)
# Environment: V5_REF (default origin/v5), V6_REF (default origin/v6).
set -euo pipefail

V5_REF="${V5_REF:-origin/v5}"
V6_REF="${V6_REF:-origin/v6}"
SHA_RE='^[0-9a-f]{40}$'
MARKER_RE='<!-- hive:topup carried=([0-9a-f]{40}) -->'

is_commit() { git cat-file -e "$1^{commit}" 2>/dev/null; }
is_ancestor() { git merge-base --is-ancestor "$1" "$2"; }

cmd="${1:-}"
shift || true
case "$cmd" in
  marker-line)
    [[ "${1:-}" =~ $SHA_RE ]] || { echo "marker-line: need a full 40-hex sha" >&2; exit 2; }
    printf '<!-- hive:topup carried=%s -->\n' "$1"
    ;;
  marker)
    [ -f "${1:-}" ] || { echo "marker: need a body file" >&2; exit 2; }
    # Last marker wins; a body with none prints nothing and still exits 0.
    while IFS= read -r line; do
      if [[ "$line" =~ $MARKER_RE ]]; then found="${BASH_REMATCH[1]}"; fi
    done < "$1"
    [ -z "${found:-}" ] || printf '%s\n' "$found"
    ;;
  verify)
    carried="${1:-}" head="${2:-}" merged="${3:-}"
    for s in "$carried" "$head" "$merged"; do
      [[ "$s" =~ $SHA_RE ]] || { echo "verify: need three full 40-hex shas" >&2; exit 2; }
      is_commit "$s" || { echo "verify: $s is not a fetched commit" >&2; exit 1; }
    done
    parents="$(git rev-list --parents -n 1 "$head")"
    # shellcheck disable=SC2086
    set -- $parents
    [ "$#" -eq 3 ] && [ "$3" = "$carried" ] || { echo "verify: PR head $head is not a merge of $carried" >&2; exit 1; }
    is_ancestor "$merged" "$V6_REF" || { echo "verify: merge commit $merged is not on $V6_REF" >&2; exit 1; }
    ;;
  classify)
    carried="${1:-}"
    if [ -z "$carried" ]; then echo unknown; exit 0; fi
    [[ "$carried" =~ $SHA_RE ]] || { echo "classify: not a full sha: $carried" >&2; exit 2; }
    if ! is_commit "$carried" || ! is_ancestor "$carried" "$V5_REF"; then echo unrecoverable; exit 0; fi
    if is_ancestor "$carried" "$V6_REF"; then echo level; else echo lost; fi
    ;;
  *)
    echo "usage: v6-topup-ancestry.sh marker-line|marker|verify|classify ..." >&2
    exit 2
    ;;
esac
