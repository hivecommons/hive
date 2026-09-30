#!/usr/bin/env bash
# Self-test for check-cli-model-lists.sh (#9805): the Sonnet 5.5 case and the
# version-comment checks, against fixtures.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/check-cli-model-lists.sh"
PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "  ok: $1"; }
fail() { FAIL=$((FAIL+1)); echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "        $2"; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fixtures() { # fixtures <claude pin> <comment version> <extra model line>
  printf 'FROM scratch\nARG CODEX_VERSION=0.159.0\nARG CLAUDE_CODE_VERSION=%s\n' "$1" > "$TMP/Dockerfile"
  cat > "$TMP/models.go" <<GO
package dashboard

// codexStaticModels snapshot matches codex 0.159.0 (catalog).
var codexStaticModels = []string{"a"}

// claudePinnedCLIModels is the closed model set accepted by the Claude Code
// version pinned in src/Dockerfile ($2 as of #1).
var claudePinnedCLIModels = []string{
	"claude-sonnet-5",
	$3
	"sonnet",
}
GO
}
run() { HIVE_PIN_DOCKERFILE="$TMP/Dockerfile" HIVE_MODELS_GO="$TMP/models.go" \
  HIVE_MODEL_REQUIREMENTS="$HERE/cli-model-requirements.txt" "$@" bash "$SCRIPT" >"$TMP/out" 2>&1 && RC=0 || RC=$?; }

fixtures 2.1.284 2.1.284 '"claude-sonnet-5-5",'
run env; [ $RC -eq 0 ] && grep -q consistent "$TMP/out" && pass "consistent lists pass" || fail "consistent" "$(cat "$TMP/out")"

fixtures 2.1.284 2.1.284 ''
run env HIVE_MODEL_CHECK_STRICT=1
[ $RC -eq 1 ] && grep -q 'missing claude-sonnet-5-5' "$TMP/out" && pass "Sonnet 5.5 missing under Claude Code 2.1.284 fails in strict mode" || fail "sonnet 5.5" "$(cat "$TMP/out")"
run env
[ $RC -eq 0 ] && grep -q '::warning::.*claude-sonnet-5-5' "$TMP/out" && pass "non-strict mode warns without failing" || fail "warn mode" "$(cat "$TMP/out")"

fixtures 2.1.280 2.1.280 ''
run env HIVE_MODEL_CHECK_STRICT=1
[ $RC -eq 0 ] && pass "model not yet supported by the pin is not required" || fail "older pin" "$(cat "$TMP/out")"

fixtures 2.1.284 2.1.280 '"claude-sonnet-5-5",'
run env HIVE_MODEL_CHECK_STRICT=1
[ $RC -eq 1 ] && grep -q 'comment says Claude Code 2.1.280 but src/Dockerfile pins 2.1.284' "$TMP/out" && pass "stale claude version comment is caught" || fail "claude comment" "$(cat "$TMP/out")"

fixtures 2.1.284 2.1.284 '"claude-sonnet-5-5",'
sed -i 's/CODEX_VERSION=0.159.0/CODEX_VERSION=0.160.0/' "$TMP/Dockerfile"
run env HIVE_MODEL_CHECK_STRICT=1
[ $RC -eq 1 ] && grep -q 'codex static list comment says 0.159.0 but src/Dockerfile pins 0.160.0' "$TMP/out" && pass "stale codex version comment is caught" || fail "codex comment" "$(cat "$TMP/out")"

echo
echo "passed: $PASS  failed: $FAIL"
[ "$FAIL" -eq 0 ]
