#!/usr/bin/env bash
# Regenerate the mediated-vs-unmediated outcome benchmark from stored review
# evidence and, with --readme, the README's <!-- outcomes:start/end --> block.
#
#   src/scripts/outcome-benchmark.sh --root ./evidence --month 2026-10 --spend cost.json --readme
#
# Flags are passed through to `hivectl evidence benchmark`; --readme (no
# argument) rewrites the repository README. Defaults: --root /data/evidence,
# --month = current UTC month, --out-dir src/docs/benchmarks. A saved
# GET /api/cost response (--spend) adds cost per merged PR; a saved
# GET /api/hub/evidence response (--hub) adds fleet context. Nothing here
# contacts a hive or adds telemetry; a test (pkg/outcomebench
# TestCommittedReportMatchesReadme) fails when the README block, report or
# charts no longer match the committed JSON.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root/src"

args=()
readme=""
month=""
out_dir="docs/benchmarks"
while [ $# -gt 0 ]; do
  case "$1" in
    --readme) readme="$repo_root/README.md"; shift ;;
    --month) month="$2"; shift 2 ;;
    --out-dir) out_dir="$2"; shift 2 ;;
    -h|--help) sed -n '2,15p' "$0"; exit 0 ;;
    *) args+=("$1"); shift ;;
  esac
done
[ -n "$month" ] || month="$(date -u +%Y-%m)"

cmd=(go run ./cmd/hivectl evidence benchmark --month "$month" --out-dir "$out_dir")
[ -n "$readme" ] && cmd+=(--readme "$readme")
exec "${cmd[@]}" "${args[@]}"
