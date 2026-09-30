#!/usr/bin/env bash
# #9592: persisted agent UID allocations must be stable across agent additions,
# and the first boot after this logic lands adopts existing PVC ownership.
# Run: bash src/deploy/test_entrypoint_uid_stable.sh
set -uo pipefail

PASS=0
FAIL=0
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENTRYPOINT="$HERE/entrypoint.sh"
WORK="$HERE/.test-entrypoint-uid-stable-$$"

ok() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "        $2"; FAIL=$((FAIL + 1)); }
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

mkdir -p "$WORK/bin" "$WORK/data/.hive" "$WORK/run" "$WORK/tokens"
HELPERS="$WORK/uid-helpers.sh"
awk '/^hive_build_uid_map\(\) \{/{f=1} /^# hive_harden_runtime_config/{f=0} f' "$ENTRYPOINT" > "$HELPERS"
if [ ! -s "$HELPERS" ]; then
  bad "stable UID helpers are extractable from entrypoint.sh"
  echo "Results: $PASS passed, $FAIL failed"
  exit 1
fi
# shellcheck disable=SC1090
# Intentionally execute extracted shipped helpers.
. "$HELPERS"

export HIVE_UID_BASE=2001
export PROXY_UID=1001
export HIVE_UID_ISOLATION_MARKER_DIR="$WORK/markers"
export HIVE_UID_ISOLATION_REVISION=2
export HIVE_DATA_ROOT="$WORK/data"
export HIVE_AGENT_TOKEN_ROOT="$WORK/tokens"
export HIVE_UID_MAP_PERSISTED="$WORK/data/.hive/uid-map.json"
export HIVE_UID_MAP_RUNTIME="$WORK/run/uid-map.json"

json_assert() {
  python3 - "$HIVE_UID_MAP_PERSISTED" "$@" <<'PY'
import json
import sys
with open(sys.argv[1]) as f:
    agents = json.load(f)['agents']
for spec in sys.argv[2:]:
    name, want = spec.split('=')
    got = agents.get(name)
    if got != int(want):
        raise SystemExit(f'{name}={got}, want {want}')
PY
}

cat > "$HIVE_UID_MAP_PERSISTED" <<'JSON'
{"agents":{"architect":2001,"scanner":2009},"base_uid":2001,"proxy_uid":1001,"iptables_active":false}
JSON
AGENT_NAMES=$'adjudicator\narchitect\nscanner'
export AGENT_NAMES
if hive_build_uid_map >/dev/null && json_assert architect=2001 scanner=2009 adjudicator=2010; then
  ok "adding an alphabetically-first agent preserves existing UIDs and appends max+1"
else
  bad "stable allocation from a persisted map failed"
fi

rm -f "$HIVE_UID_MAP_PERSISTED" "$HIVE_UID_MAP_RUNTIME"
cat > "$WORK/owners.tsv" <<EOFOWNERS
$WORK/data/home/agents/architect	2001
$WORK/data/home/agents/scanner	2009
EOFOWNERS
cat > "$WORK/bin/stat" <<'EOFSTAT'
#!/usr/bin/env bash
if [ "$1" = "-c" ] && [ "$2" = "%u" ]; then
  path="$3"
  while IFS=$'\t' read -r p uid; do
    if [ "$p" = "$path" ]; then
      printf '%s\n' "$uid"
      exit 0
    fi
  done < "${HIVE_TEST_OWNERS}"
fi
if [ "$1" = "-c" ] && [ "$2" = "%u:%G" ]; then
  path="$3"
  while IFS=$'\t' read -r p owner; do
    if [ "$p" = "$path" ]; then
      printf '%s\n' "$owner"
      exit 0
    fi
  done < "${HIVE_TEST_TOKEN_OWNERS}"
fi
exit 1
EOFSTAT
chmod +x "$WORK/bin/stat"
export HIVE_TEST_OWNERS="$WORK/owners.tsv"
PATH="$WORK/bin:$PATH" hive_build_uid_map >/dev/null
if json_assert architect=2001 scanner=2009 adjudicator=2010; then
  ok "first boot without a persisted map adopts unique existing directory owners"
else
  bad "adoption from existing directory owners failed"
fi

mkdir -p "$WORK/data/home/agents/scanner" "$WORK/data/beads/scanner" "$WORK/data/agents/scanner"
printf 'token' > "$WORK/tokens/gh-token-scanner.cache"
cat > "$WORK/owners.tsv" <<EOFOWNERS
$WORK/data/home/agents/scanner	2009
$WORK/data/beads/scanner	2010
$WORK/data/agents/scanner	2008
EOFOWNERS
cat > "$WORK/token-owners.tsv" <<EOFOWNERS
$WORK/tokens/gh-token-scanner.cache	2007:node
EOFOWNERS
cat > "$WORK/bin/chown" <<'EOFCHOWN'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${HIVE_TEST_CHOWN_LOG}"
exit 0
EOFCHOWN
chmod +x "$WORK/bin/chown"
export HIVE_TEST_CHOWN_LOG="$WORK/chown.log"
export HIVE_TEST_TOKEN_OWNERS="$WORK/token-owners.tsv"
: > "$HIVE_TEST_CHOWN_LOG"
PATH="$WORK/bin:$PATH" hive_reown_agent_paths_if_needed scanner 2010 > "$WORK/reown.out"
if grep -q -- '-R 2010:node .*home/agents/scanner' "$HIVE_TEST_CHOWN_LOG" \
   && grep -q -- '-R 2010:node .*agents/scanner' "$HIVE_TEST_CHOWN_LOG" \
   && ! grep -q -- '-R 2010:node .*beads/scanner' "$HIVE_TEST_CHOWN_LOG" \
   && grep -q 're-owned .*2009→2010' "$WORK/reown.out"; then
  ok "re-own guard repairs mismatched agent trees only"
else
  bad "re-own guard did not chown the expected paths" "chown log: $(tr '\n' ';' < "$HIVE_TEST_CHOWN_LOG") output: $(tr '\n' ';' < "$WORK/reown.out")"
fi

: > "$HIVE_TEST_CHOWN_LOG"
PATH="$WORK/bin:$PATH" hive_reown_agent_token_caches_if_needed scanner hive-scanner > "$WORK/token-reown.out"
if grep -q -- 'dev:hive-scanner .*gh-token-scanner.cache' "$HIVE_TEST_CHOWN_LOG" \
   && grep -q 're-owned .*2007:node→1001:hive-scanner' "$WORK/token-reown.out"; then
  ok "token cache guard preserves dev-writable, per-agent-group-readable ownership"
else
  bad "token cache guard did not repair the expected cache ownership" "chown log: $(tr '\n' ';' < "$HIVE_TEST_CHOWN_LOG") output: $(tr '\n' ';' < "$WORK/token-reown.out")"
fi

echo
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1
