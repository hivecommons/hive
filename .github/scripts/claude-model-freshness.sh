#!/usr/bin/env bash
# Builds the weekly Claude model freshness change in the working tree (#18754).
# Run by .github/workflows/claude-model-freshness.yml; see its header.
#
#   1. fetches OpenRouter's public model catalogue once (both steps below read
#      the same snapshot);
#   2. installs the Claude Code version pinned in src/Dockerfile exactly the
#      way the image does (npm --ignore-scripts, then the vendor's install.cjs)
#      so the candidates can be probed against it, unauthenticated;
#   3. check-static-model-freshness.py --apply claude: candidates, the probe
#      gate, and the edits to claudePinnedCLIModels, claudeStaticModels,
#      CLAUDE_CLI_MODELS and cli-model-requirements.txt;
#   4. for the accepted ids: `pricetable --add` (new modelPrices rows, only
#      through its plausibility rules) and `make dashboard-gen`.
#
# Usage: claude-model-freshness.sh <out dir>
# Env:   DRY_RUN=1 prints every edit and writes nothing.
#        SOURCE_URL overrides the catalogue URL.
# Out:   <out>/report.md (the PR body), <out>/accepted.txt (one id per line;
#        empty when nothing is added), <out>/pin.txt.
set -euo pipefail

out="${1:?usage: claude-model-freshness.sh <out dir>}"
mkdir -p "$out"
out="$(cd "$out" && pwd)"
cd "$(git rev-parse --show-toplevel)"

source_url="${SOURCE_URL:-https://openrouter.ai/api/v1/models}"
catalog="$out/openrouter.json"
curl -fsSL --retry 3 --max-time 60 -H 'Accept: application/json' -A hive-claude-model-freshness \
  "$source_url" -o "$catalog"

pin="$(sed -n 's/^ARG CLAUDE_CODE_VERSION=\([^ ]*\).*/\1/p' src/Dockerfile | head -1)"
[ -n "$pin" ] || { echo "no ARG CLAUDE_CODE_VERSION in src/Dockerfile" >&2; exit 1; }
printf '%s\n' "$pin" > "$out/pin.txt"

# Without the binary nothing is added: every candidate is reported under
# "needs a check" instead (check-static-model-freshness.py gate_candidates).
claude_bin=""
prefix="$out/claude-code"
if npm install --prefix "$prefix" --ignore-scripts --no-fund --no-audit \
     "@anthropic-ai/claude-code@${pin}" >&2 \
   && node "$prefix/node_modules/@anthropic-ai/claude-code/install.cjs" >&2 \
   && "$prefix/node_modules/.bin/claude" --version >&2; then
  claude_bin="$prefix/node_modules/.bin/claude"
else
  echo "::warning::Could not install Claude Code ${pin} to probe; no model will be added this run."
fi

dry=()
[ "${DRY_RUN:-}" = "1" ] && dry=(--dry-run)
probe=()
[ -n "$claude_bin" ] && probe=(--claude-bin "$claude_bin")

python3 -I .github/scripts/check-static-model-freshness.py --apply claude \
  --catalog "$catalog" --accepted-out "$out/accepted.txt" "${probe[@]}" "${dry[@]}" > "$out/lists.md"
cp "$out/lists.md" "$out/report.md"

if [ ! -s "$out/accepted.txt" ]; then
  echo "no Claude model to add"
  exit 0
fi

ids="$(paste -sd, "$out/accepted.txt")"
(cd src && go run ./internal/tools/pricetable --source "$catalog" --add "$ids" \
  --report "$out/prices.md" "${dry[@]}")
{ echo; cat "$out/prices.md"; } >> "$out/report.md"

if [ "${DRY_RUN:-}" != "1" ]; then
  make dashboard-gen
  make dashboard-gen-check
  test -z "$(gofmt -l src/pkg/tokens/pricing.go src/pkg/dashboard/cli_models.go)" \
    || { echo "::error::generated Go files are not gofmt-clean" >&2; exit 1; }
  {
    echo
    echo "### Model lists against the CLI pins"
    echo
    echo '```'
    HIVE_MODEL_CHECK_STRICT=0 bash src/scripts/check-cli-model-lists.sh 2>&1 || true
    echo '```'
  } >> "$out/report.md"
fi
echo "Claude models to add: $ids"
