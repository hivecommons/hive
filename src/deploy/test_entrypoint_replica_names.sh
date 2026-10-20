#!/usr/bin/env bash
# #13166: the entrypoint provisions a user, private token group and token
# cache file for every name hive_collect_agent_names prints. `replicas:`
# copies (<agent>-2 .. <agent>-N) exist only in memory, so they must be
# derived from the config here or the replica can never read its token.
# Run: bash src/deploy/test_entrypoint_replica_names.sh
set -uo pipefail

PASS=0
FAIL=0
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENTRYPOINT="$HERE/entrypoint.sh"
WORK="$HERE/.test-entrypoint-replica-names-$$"

ok() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "        $2"; FAIL=$((FAIL + 1)); }
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

mkdir -p "$WORK"
HELPERS="$WORK/collect.sh"
awk '/^hive_collect_agent_names\(\) \{/{f=1} f{print} f&&/^}$/{exit}' "$ENTRYPOINT" > "$HELPERS"
if [ ! -s "$HELPERS" ]; then
  bad "hive_collect_agent_names is extractable from entrypoint.sh"
  echo "Results: $PASS passed, $FAIL failed"
  exit 1
fi
# shellcheck disable=SC1090
# Intentionally execute the extracted shipped helper.
. "$HELPERS"
unset HIVE_LEVEL

expect() {
  local desc="$1" want="$2" cfg="$WORK/hive.yaml" got
  cat > "$cfg"
  got="$(hive_collect_agent_names "$cfg" | paste -sd' ' -)"
  if [ "$got" = "$want" ]; then
    ok "$desc"
  else
    bad "$desc" "got '$got', want '$want'"
  fi
}

expect "agents without replicas are listed as-is" "quality scanner" <<'YAML'
agents:
  scanner: {backend: claude}
  quality: {backend: claude, replicas: 1}
YAML

expect "replicas: 3 adds <agent>-2 and <agent>-3" "code-reviewer code-reviewer-2 code-reviewer-3 scanner" <<'YAML'
agents:
  code-reviewer: {backend: claude, replicas: 3}
  scanner: {backend: codex}
YAML

expect "replicas: 5 (the maximum) expands fully" "a a-2 a-3 a-4 a-5" <<'YAML'
agents:
  a: {replicas: 5}
YAML

expect "out-of-range or malformed counts expand to nothing" "a b c d" <<'YAML'
agents:
  a: {replicas: 6}
  b: {replicas: -1}
  c: {replicas: many}
  d: null
YAML

expect "list-form agents expand too" "fixer fixer-2" <<'YAML'
agents:
  - name: fixer
    replicas: 2
YAML

expect "an empty config prints nothing" "" <<'YAML'
YAML

echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
