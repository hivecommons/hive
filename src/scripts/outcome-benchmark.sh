#!/usr/bin/env bash
# Regenerate the mediated-vs-unmediated outcome benchmark from stored review
# evidence. The root README is hand-written; its graphics are regenerated
# separately with node docs/readme/render-charts.mjs.
#
#   src/scripts/outcome-benchmark.sh --root ./evidence --month 2026-10 --spend cost.json
#   src/scripts/outcome-benchmark.sh --rerender   # redraw from the committed JSON
#
# Flags are passed through to `hivectl evidence benchmark`. Defaults: --root /data/evidence,
# --month = current UTC month, --out-dir src/docs/benchmarks. A saved
# GET /api/cost response (--spend) adds cost per merged PR; a saved
# GET /api/hub/evidence response (--hub) adds fleet context. --rerender
# skips the evidence tree and redraws the markdown and legacy SVG/PNG cards
# from the newest committed outcomes-YYYY-MM.json (use it
# after changing the card layout). The PNG cards are drawn in Go with an
# embedded font; no external renderer is needed. Nothing here contacts a
# hive or adds telemetry; a test (pkg/outcomebench
# TestCommittedReportAssets) fails when the report,
# charts or cards no longer match the committed JSON.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root/src"

args=()
month=""
rerender=""
out_dir="docs/benchmarks"
while [ $# -gt 0 ]; do
  case "$1" in
    --readme)
      echo "The root README is hand-written. Redraw its graphics with: node docs/readme/render-charts.mjs" >&2
      exit 2
      ;;
    --rerender) rerender=1; shift ;;
    --month) month="$2"; shift 2 ;;
    --out-dir) out_dir="$2"; shift 2 ;;
    -h|--help) sed -n '2,21p' "$0"; exit 0 ;;
    *) args+=("$1"); shift ;;
  esac
done
if [ -n "$rerender" ]; then
  latest="$(ls "$out_dir"/outcomes-*.json 2>/dev/null | sort | tail -n 1)"
  [ -n "$latest" ] || { echo "no $out_dir/outcomes-*.json to re-render" >&2; exit 1; }
  cmd=(go run ./cmd/hivectl evidence benchmark --from-json "$latest" --out-dir "$out_dir")
  exec "${cmd[@]}" "${args[@]}"
fi
[ -n "$month" ] || month="$(date -u +%Y-%m)"

cmd=(go run ./cmd/hivectl evidence benchmark --month "$month" --out-dir "$out_dir")
exec "${cmd[@]}" "${args[@]}"
