#!/usr/bin/env bash
# extract-release-notes.sh — print the body of a CHANGELOG.md release section.
#
# Usage: src/scripts/extract-release-notes.sh CHANGELOG.md 4.12.3 [fallback.md]
# The section heading must contain `(v<VERSION>)`. Leading/trailing blank lines
# are trimmed. If the section is absent or empty and fallback.md is provided,
# the fallback is printed instead so release creation still gets useful notes.
set -euo pipefail

CHANGELOG="${1:?usage: extract-release-notes.sh CHANGELOG.md VERSION [fallback.md]}"
VERSION="${2:?usage: extract-release-notes.sh CHANGELOG.md VERSION [fallback.md]}"
FALLBACK="${3:-}"

if [[ ! -f "$CHANGELOG" ]]; then
  echo "changelog not found: ${CHANGELOG}" >&2
  exit 1
fi

rc=0
notes="$(
  awk -v marker="(v${VERSION})" '
    /^[[:space:]]*(```|~~~)/ { in_fence = !in_fence }
    !in_fence && /^## / {
      if (found) exit
      if (index($0, marker) > 0) { found = 1; next }
    }
    found { lines[++n] = $0 }
    END {
      if (!found) exit 1
      while (n > 0 && lines[n] ~ /^[[:space:]]*$/) n--
      start = 1
      while (start <= n && lines[start] ~ /^[[:space:]]*$/) start++
      if (start > n) exit 2
      for (i = start; i <= n; i++) print lines[i]
    }
  ' "$CHANGELOG"
)" || rc=$?

if [[ "$rc" -eq 0 && -n "$notes" ]]; then
  printf '%s\n' "$notes"
  exit 0
fi

if [[ -n "$FALLBACK" && -s "$FALLBACK" ]]; then
  cat "$FALLBACK"
  exit 0
fi

if [[ "$rc" -eq 2 ]]; then
  printf 'No CHANGELOG.md entries were recorded for v%s.\n' "$VERSION"
  exit 0
fi

echo "release section for v${VERSION} not found in ${CHANGELOG}" >&2
exit 1
