#!/usr/bin/env bash
# outcome-benchmark.sh — render the mediated-vs-unmediated outcome benchmark
# (hivecommons/hive#14577) for one calendar month from a spoke's evidence root.
# A thin wrapper over `hivectl evidence benchmark` that pins the window to
# absolute UTC month bounds, so re-running it on the same evidence root
# reproduces the same report byte for byte.
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
src_root=$(cd -- "${script_dir}/.." && pwd)

hivectl_bin=${HIVECTL:-hivectl}
root=${EVIDENCE_ROOT:-/data/evidence}
spend=${FLEET_SPEND_USD:-}
repo=""
out=""
month=""

usage() {
  cat <<'USAGE'
Usage: src/scripts/outcome-benchmark.sh [options] YYYY-MM

Renders src/docs/benchmarks/outcomes-YYYY-MM.md from the evidence bundles of
PRs opened in that UTC calendar month.

Options:
  --root <dir>             Evidence root (default: $EVIDENCE_ROOT or /data/evidence)
  --repo <owner/name>      Only this repository
  --fleet-spend-usd <usd>  Fleet estimated spend over the month (default: $FLEET_SPEND_USD)
  --out <path>             Output file (default: src/docs/benchmarks/outcomes-YYYY-MM.md, "-" for stdout)
  -h, --help               Show this help

Environment:
  HIVECTL                  hivectl binary to run (default: hivectl on PATH)
USAGE
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --root)
      root=${2:?--root requires a value}; shift 2 ;;
    --repo)
      repo=${2:?--repo requires a value}; shift 2 ;;
    --fleet-spend-usd)
      spend=${2:?--fleet-spend-usd requires a value}; shift 2 ;;
    --out)
      out=${2:?--out requires a value}; shift 2 ;;
    -h|--help)
      usage; exit 0 ;;
    -*)
      echo "outcome-benchmark: unknown option $1" >&2; usage >&2; exit 2 ;;
    *)
      [ -z "$month" ] || { echo "outcome-benchmark: one month only" >&2; exit 2; }
      month=$1; shift ;;
  esac
done

if ! [[ "$month" =~ ^([0-9]{4})-(0[1-9]|1[0-2])$ ]]; then
  echo "outcome-benchmark: expected a month as YYYY-MM, got '${month}'" >&2
  usage >&2
  exit 2
fi
year=$((10#${BASH_REMATCH[1]}))
mon=$((10#${BASH_REMATCH[2]}))
if [ "$mon" -eq 12 ]; then
  next=$(printf '%04d-01' $((year + 1)))
else
  next=$(printf '%04d-%02d' "$year" $((mon + 1)))
fi

[ -n "$out" ] || out="${src_root}/docs/benchmarks/outcomes-${month}.md"

args=(evidence benchmark --root "$root" --since "${month}-01T00:00:00Z" --until "${next}-01T00:00:00Z" --out "$out")
[ -z "$repo" ] || args+=(--repo "$repo")
[ -z "$spend" ] || args+=(--fleet-spend-usd "$spend")

exec "$hivectl_bin" "${args[@]}"
