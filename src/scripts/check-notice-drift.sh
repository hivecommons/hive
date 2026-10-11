#!/usr/bin/env bash
# check-notice-drift.sh — compare the committed NOTICE with fresh output and
# explain the repository's byte-exact commit contract when they differ.
#
# Usage: src/scripts/check-notice-drift.sh [committed-notice] [generated-notice]
set -euo pipefail

COMMITTED_NOTICE="${1:-NOTICE}"
GENERATED_NOTICE="${2:-/tmp/NOTICE.generated}"

# A fresh run that renders "Source:   Unknown" for a package the committed
# NOTICE does not already record as Unknown is a failed go-licenses source lookup (a timed-out HTTP
# go-import fetch, logged by go-licenses only as a warning), not a change in the
# module graph. Name it as such so nobody "fixes" it by committing the degraded
# file — the only correct action is to re-run the generator.
unresolved="$(
  awk '
    /^Package:  / { pkg = $2 }
    /^Source:   / {
      if (FILENAME == ARGV[1]) { committed[pkg] = $2 }
      else if ($2 == "Unknown" && committed[pkg] != "Unknown") { print pkg }
    }
  ' "${COMMITTED_NOTICE}" "${GENERATED_NOTICE}"
)"
if [[ -n "${unresolved}" ]]; then
  echo "::error::generated NOTICE has 'Source:   Unknown' for package(s) the committed NOTICE does not record that way — go-licenses could not resolve the module source (network go-import lookup timed out). This is a generator failure, not NOTICE drift: do NOT commit the generated file; re-run the job. Affected: $(tr '\n' ' ' <<< "${unresolved}")" >&2
  exit 1
fi

if ! diff -u -- "${COMMITTED_NOTICE}" "${GENERATED_NOTICE}"; then
  echo "::error::NOTICE is out of date with src/go.mod / src/go.sum. Run 'src/scripts/generate-notice.sh' locally (requires a Go toolchain). The comparison is byte-for-byte and whitespace-significant; commit the regenerated NOTICE verbatim without reformatting it." >&2
  exit 1
fi

echo "NOTICE matches the current module graph."
