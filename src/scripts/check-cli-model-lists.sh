#!/usr/bin/env bash
# Checks Hive's static model lists against the CLI pins in src/Dockerfile
# (#9805):
#   1. every model in cli-model-requirements.txt whose first-supported version
#      is <= the pinned version is present in the CLI's allowlist;
#   2. the "version pinned ... (X" / "matches codex X" comments next to the
#      lists equal the current pin.
# Model catalogs that live in a binary cannot be read at build time, so this is
# a version-comment + known-requirements check, not a full catalog diff.
#
# Usage: check-cli-model-lists.sh
# Exit: 0 when consistent. Drift exits 1 when HIVE_MODEL_CHECK_STRICT=1,
# otherwise prints GitHub ::warning lines and exits 0.
# Env: HIVE_PIN_ROOT, HIVE_PIN_DOCKERFILE, HIVE_MODELS_GO, HIVE_MODEL_REQUIREMENTS
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${HIVE_PIN_ROOT:-$(cd "$SCRIPT_DIR/../.." && pwd)}"
DOCKERFILE="${HIVE_PIN_DOCKERFILE:-$ROOT/src/Dockerfile}"
MODELS_GO="${HIVE_MODELS_GO:-$ROOT/src/pkg/dashboard/cli_models.go}"
REQS="${HIVE_MODEL_REQUIREMENTS:-$SCRIPT_DIR/cli-model-requirements.txt}"

PROBLEMS=0
problem() { PROBLEMS=$((PROBLEMS+1)); echo "::warning::$*"; }

pin_of() { sed -n "s/^ARG $1=\([^ ]*\).*/\1/p" "$DOCKERFILE" | head -1; }

# list_ids <go var>: the quoted ids inside `var <name> = []string{ ... }`.
list_ids() {
  awk -v v="$1" '$0 ~ "^var " v " = " {on=1; next} on && /^}/ {exit} on' "$MODELS_GO" \
    | grep -o '"[^"]*"' | tr -d '"' || true
}

# comment_version <go var> <regex with one capture group>: the comment block
# above the var, unwrapped, searched for the version it claims.
comment_version() {
  awk -v v="$1" '$0 ~ "^var " v " = " {print buf; exit} /^\/\// {sub(/^\/\/ ?/, ""); buf = buf " " $0; next} {buf=""}' "$MODELS_GO" \
    | sed -n -E "s/.*$2.*/\\1/p" | head -1
}

# ver_ge <a> <b>: a >= b by dotted numeric order.
ver_ge() { [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -1)" = "$2" ]; }

claude_pin="$(pin_of CLAUDE_CODE_VERSION)"
codex_pin="$(pin_of CODEX_VERSION)"
[ -n "$claude_pin" ] || { echo "no CLAUDE_CODE_VERSION in $DOCKERFILE" >&2; exit 2; }

claude_ids="$(list_ids claudePinnedCLIModels)"
while read -r cli model minver; do
  case "$cli" in ''|'#'*) continue ;; esac
  [ "$cli" = claude ] || continue
  if ver_ge "$claude_pin" "$minver" && ! printf '%s\n' "$claude_ids" | grep -qxF "$model"; then
    problem "claudePinnedCLIModels is missing $model, which Claude Code $claude_pin (>= $minver) accepts"
  fi
done < "$REQS"

c="$(comment_version claudePinnedCLIModels 'version pinned in src/Dockerfile \(([0-9][0-9.]*)')"
if [ -z "$c" ]; then
  problem "claudePinnedCLIModels has no 'version pinned in src/Dockerfile (X' comment to check"
elif [ "$c" != "$claude_pin" ]; then
  problem "claudePinnedCLIModels comment says Claude Code $c but src/Dockerfile pins $claude_pin"
fi

if [ -n "$codex_pin" ]; then
  c="$(awk '/^\/\/.*matches codex/ {print; exit}' "$MODELS_GO" | sed -n -E 's/.*matches codex ([0-9][0-9.]*).*/\1/p')"
  if [ -z "$c" ]; then
    problem "no 'matches codex X' comment found for the codex static list"
  elif [ "$c" != "$codex_pin" ]; then
    problem "codex static list comment says $c but src/Dockerfile pins $codex_pin"
  fi
fi

if [ "$PROBLEMS" -gt 0 ]; then
  echo "$PROBLEMS model-list drift finding(s) against the CLI pins; also review static/index.html and src/pkg/tokens/pricing.go by hand." >&2
  [ "${HIVE_MODEL_CHECK_STRICT:-}" = "1" ] && exit 1
  exit 0
fi
echo "model lists consistent with CLI pins (claude $claude_pin, codex ${codex_pin:-n/a})"
