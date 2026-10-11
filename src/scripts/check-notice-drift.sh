#!/usr/bin/env bash
# check-notice-drift.sh — compare the committed NOTICE with fresh output and
# explain the repository's byte-exact commit contract when they differ.
#
# Usage: src/scripts/check-notice-drift.sh [committed-notice] [generated-notice]
set -euo pipefail

COMMITTED_NOTICE="${1:-NOTICE}"
GENERATED_NOTICE="${2:-/tmp/NOTICE.generated}"

# go-licenses resolves each Source: URL with a remote lookup and emits
# "Unknown" when that lookup transiently fails (#11497). Package:, Version:
# and the verbatim license text still identify each entry, so the
# non-deterministic Source: lines are excluded from both sides of the diff.
strip_source() {
  sed '/^Source:[[:space:]]/d' -- "$1"
}

if ! diff -u --label "${COMMITTED_NOTICE}" --label "${GENERATED_NOTICE}" \
  <(strip_source "${COMMITTED_NOTICE}") <(strip_source "${GENERATED_NOTICE}"); then
  echo "::error::NOTICE is out of date with src/go.mod / src/go.sum. Run 'src/scripts/generate-notice.sh' locally (requires a Go toolchain). The comparison is byte-for-byte and whitespace-significant; commit the regenerated NOTICE verbatim without reformatting it." >&2
  exit 1
fi

echo "NOTICE matches the current module graph."
