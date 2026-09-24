#!/usr/bin/env bash
set -euo pipefail

report="${1:?rerun report path required}"
shard="${2:?shard name required}"

if [ ! -s "$report" ]; then
  echo "has-flakes=false" >> "$GITHUB_OUTPUT"
  exit 0
fi

echo "has-flakes=true" >> "$GITHUB_OUTPUT"

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "## ⚠️ Flaky tests re-run in this shard"
    echo ""
    echo "Shard: ${shard}"
    echo ""
    while IFS= read -r line; do
      [ -n "$line" ] || continue
      echo "- \`${line}\`"
    done < "$report"
    echo ""
    echo "A rerun is not a pass; fix the flaky test."
  } >> "$GITHUB_STEP_SUMMARY"
fi

while IFS= read -r line; do
  [ -n "$line" ] || continue
  safe_line="${line//'%'/'%25'}"
  safe_line="${safe_line//$'\r'/'%0D'}"
  safe_line="${safe_line//$'\n'/'%0A'}"
  echo "::warning title=Flaky test rerun::${shard}: ${safe_line}"
done < "$report"
