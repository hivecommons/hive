#!/usr/bin/env bash
# check-go-file-size-ratchet.sh — line-count ratchet for non-test Go files.
#
# Copy of hivecommons/crux .github/scripts/check-file-size-ratchet.sh for the
# hive Go module (hivecommons/hive#20019). The only behavioural change is that
# the find also prunes ./crux, which keeps its own ratchet
# (.github/workflows/crux-file-size-ratchet.yml).
#
# Blocks new growth in oversized ("god") files while grandfathering the ones
# that already exist (crux#20):
#
#   - a non-test .go file listed in the baseline fails the gate when it grows
#     beyond its recorded line count;
#   - an unlisted non-test .go file fails the gate when it exceeds LIMIT lines.
#
# Shrinking is always allowed. When a listed file is now smaller than its
# baseline (or no longer over LIMIT, or gone), the gate still passes and prints
# the exact baseline line to edit, so a split can lower the ratchet in the same
# pull request.
#
# Baseline format: one "<lines> <path>" entry per line, path relative to the
# module root; blank lines and lines starting with # are ignored.
#
# Usage: scripts/check-go-file-size-ratchet.sh [baseline]
#   Run from src/ (the module root). The baseline defaults to
#   .go-file-size-ratchet.txt. Exit 0 = pass, 1 = ratchet violated,
#   2 = usage or malformed baseline.
set -euo pipefail

LIMIT=800
BASELINE="${1:-.go-file-size-ratchet.txt}"

[ "$#" -le 1 ] || { echo "usage: $0 [baseline]" >&2; exit 2; }
[ -f "$BASELINE" ] || { echo "::error::baseline $BASELINE not found" >&2; exit 2; }

# Validate the baseline up front: a typo must not silently grandfather a file
# (or un-grandfather one and fail for the wrong reason).
if ! awk '
  /^[[:space:]]*(#|$)/ { next }
  NF != 2 || $1 !~ /^[0-9]+$/ { printf "%s:%d: malformed entry: %s\n", FILENAME, FNR, $0; bad = 1; next }
  seen[$2]++ { printf "%s:%d: duplicate entry for %s\n", FILENAME, FNR, $2; bad = 1 }
  END { exit bad }
' "$BASELINE" >&2; then
  echo "::error::$BASELINE is malformed (expected \"<lines> <path>\" per line)" >&2
  exit 2
fi

# "<lines> <path>" for every non-test Go file, paths without the leading ./.
current="$(
  find . \( -path ./.git -o -path ./crux \) -prune -o -type f -name '*.go' ! -name '*_test.go' -print |
    LC_ALL=C sort |
    while IFS= read -r f; do
      printf '%d %s\n' "$(wc -l <"$f")" "${f#./}"
    done
)"

# First input is the baseline, second the current counts. Match on FILENAME,
# not FNR == NR: with a zero-byte baseline NR never advances during it, and the
# first current file would be read as a baseline entry and grandfathered.
# Errors and notices are GitHub annotations, so they also show on the PR diff.
status=0
awk -v limit="$LIMIT" -v baseline="$BASELINE" '
  FILENAME == baseline {
    if ($0 ~ /^[[:space:]]*(#|$)/) next
    base[$2] = $1
    next
  }
  NF == 0 { next }
  {
    lines = $1; path = $2; present[path] = 1
    if (path in base) {
      if (lines > base[path]) {
        printf "::error file=%s::%s has %d lines, over its ratchet baseline of %d. Split it or move code out instead of growing it.\n", path, path, lines, base[path]
        fail = 1
      } else if (lines <= limit) {
        printf "::notice file=%s::%s is down to %d lines (limit %d). Remove this line from %s: %d %s\n", path, path, lines, limit, baseline, base[path], path
      } else if (lines < base[path]) {
        printf "::notice file=%s::%s shrank to %d lines. Lower its baseline in %s: change \"%d %s\" to \"%d %s\"\n", path, path, lines, baseline, base[path], path, lines, path
      }
    } else if (lines > limit) {
      printf "::error file=%s::%s has %d lines, over the %d-line limit for non-test Go files. Split it; new files are not added to %s.\n", path, path, lines, limit, baseline
      fail = 1
    }
  }
  END {
    for (path in base)
      if (!(path in present))
        printf "::notice::%s no longer exists. Remove this line from %s: %d %s\n", path, baseline, base[path], path
    exit fail
  }
' "$BASELINE" - <<<"$current" || status=$?

if [ "$status" -eq 0 ]; then
  echo "file-size ratchet: ok (limit ${LIMIT} lines, baseline ${BASELINE})"
fi
exit "$status"
