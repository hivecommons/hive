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
  # LAST match wins: the chown stub appends a row per entry it re-owns, so the
  # table doubles as the simulated on-disk ownership (#9692).
  path="$3"
  found=""
  while IFS=$'\t' read -r p uid; do
    [ "$p" = "$path" ] && found="$uid"
  done < "${HIVE_TEST_OWNERS}"
  if [ -n "$found" ]; then
    printf '%s\n' "$found"
    exit 0
  fi
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
# chown stub: logs every call. For `chown -h UID:GROUP path...` (the re-own
# walk) it also appends each path's new owner to the owners table, so a later
# stat/find sees the change. HIVE_TEST_CHOWN_APPLY_LIMIT applies only the first
# N paths and HIVE_TEST_CHOWN_RC sets the exit status, which together simulate
# a container killed part-way through a batch (#9692).
cat > "$WORK/bin/chown" <<'EOFCHOWN'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${HIVE_TEST_CHOWN_LOG}"
if [ "$1" = "-h" ] && [ -n "${HIVE_TEST_OWNERS:-}" ]; then
  new_owner="${2%%:*}"
  shift 2
  applied=0
  for p in "$@"; do
    if [ -n "${HIVE_TEST_CHOWN_APPLY_LIMIT:-}" ] && [ "$applied" -ge "$HIVE_TEST_CHOWN_APPLY_LIMIT" ]; then
      break
    fi
    printf '%s\t%s\n' "$p" "$new_owner" >> "$HIVE_TEST_OWNERS"
    applied=$((applied + 1))
  done
fi
exit "${HIVE_TEST_CHOWN_RC:-0}"
EOFCHOWN
chmod +x "$WORK/bin/chown"

# find stub: an unprivileged test cannot create files owned by other uids, so
# ownership comes from the owners table (last match, default
# HIVE_TEST_TREE_DEFAULT_OWNER) while the TRAVERSAL is the real `find -depth`
# over a real tree. It accepts only the exact invocation the re-own uses, so a
# change to the predicate, the post-order walk, or the symlink-safe `chown -h`
# fails here instead of silently testing something else.
HIVE_TEST_REAL_FIND="$(command -v find)"
export HIVE_TEST_REAL_FIND
cat > "$WORK/bin/find" <<'EOFFIND'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${HIVE_TEST_FIND_LOG}"
root="$1"
shift
if [ "$#" -ne 11 ] || [ "$1" != "-depth" ] || [ "$2" != "!" ] || [ "$3" != "-uid" ] \
   || [ "$5" != "-exec" ] || [ "$6" != "chown" ] || [ "$7" != "-h" ] \
   || [ "$9" != "{}" ] || [ "${10}" != "+" ] || [ "${11}" != "-print" ]; then
  echo "find stub: unexpected invocation: $root $*" >&2
  exit 64
fi
want="$4"
owner_arg="$8"
owner_of() {
  local path="$1" found="" p uid
  while IFS=$'\t' read -r p uid; do
    [ "$p" = "$path" ] && found="$uid"
  done < "${HIVE_TEST_OWNERS}"
  printf '%s\n' "${found:-${HIVE_TEST_TREE_DEFAULT_OWNER:-0}}"
}
selected=()
while IFS= read -r entry; do
  [ "$(owner_of "$entry")" = "$want" ] || selected+=("$entry")
done < <("$HIVE_TEST_REAL_FIND" "$root" -depth)
rc=0
if [ "${#selected[@]}" -gt 0 ]; then
  printf '%s\n' "${selected[@]}"
  chown -h "$owner_arg" "${selected[@]}" || rc=1
fi
exit "$rc"
EOFFIND
chmod +x "$WORK/bin/find"
export HIVE_TEST_FIND_LOG="$WORK/find.log"
: > "$HIVE_TEST_FIND_LOG"
export HIVE_TEST_CHOWN_LOG="$WORK/chown.log"
export HIVE_TEST_TOKEN_OWNERS="$WORK/token-owners.tsv"
: > "$HIVE_TEST_CHOWN_LOG"
PATH="$WORK/bin:$PATH" hive_reown_agent_paths_if_needed scanner 2010 > "$WORK/reown.out"
if grep -q -- '-h 2010:node .*home/agents/scanner' "$HIVE_TEST_CHOWN_LOG" \
   && grep -q -- '-h 2010:node .*/data/agents/scanner' "$HIVE_TEST_CHOWN_LOG" \
   && ! grep -q -- 'beads/scanner' "$HIVE_TEST_CHOWN_LOG" \
   && ! grep -q -- '-R' "$HIVE_TEST_CHOWN_LOG" \
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

# ── #9692: incremental, resumable re-own ─────────────────────────────────────
# The first boot of the per-agent-UID build re-owned each agent home with a
# synchronous `chown -R`. Large homes outlasted the hosted startup probe, the
# container was killed mid-walk, and the next boot started the whole tree over.
# The re-own must now touch only mis-owned entries, resume after a kill, and be
# skipped by a completion marker once a tree is done.

# Every path the chown stub was asked to re-own via `chown -h`, one per line.
chowned_paths() {
  awk '$1 == "-h" { for (i = 3; i <= NF; i++) print $i }' "$HIVE_TEST_CHOWN_LOG"
}

TREE="$WORK/data/home/agents/builder"
mkdir -p "$TREE/sub"
: > "$TREE/a"
: > "$TREE/b"
: > "$TREE/sub/c"
# Mixed ownership: a and sub are already the target uid 2011, the rest are the
# legacy shared uid.
cat >> "$WORK/owners.tsv" <<EOFOWNERS
$TREE	1001
$TREE/a	2011
$TREE/b	1001
$TREE/sub	2011
$TREE/sub/c	1001
EOFOWNERS
MARKER="$(hive_reown_marker_path "$TREE")"
EXPECTED_MISOWNED="$(printf '%s\n' "$TREE" "$TREE/b" "$TREE/sub/c" | sort)"

case "$MARKER" in
  "$WORK/data/.hive/reown/"*) ok "re-own marker lives under /data/.hive, outside the agent-owned tree" ;;
  *) bad "re-own marker is not under the protected /data/.hive directory" "marker: $MARKER" ;;
esac

# Run 1: the container is "killed" after one entry of the batch.
: > "$HIVE_TEST_CHOWN_LOG"
: > "$HIVE_TEST_FIND_LOG"
PATH="$WORK/bin:$PATH" HIVE_TEST_CHOWN_RC=1 HIVE_TEST_CHOWN_APPLY_LIMIT=1 \
  hive_reown_path_if_needed "$TREE" 2011 > "$WORK/run1.out"
RUN1_PATHS="$(chowned_paths)"
RUN1_LAST="$(printf '%s\n' "$RUN1_PATHS" | tail -n 1)"
RUN1_APPLIED="$(printf '%s\n' "$RUN1_PATHS" | head -n 1)"
if [ "$(head -n 1 "$HIVE_TEST_FIND_LOG")" = "$TREE -depth ! -uid 2011 -exec chown -h 2011:node {} + -print" ]; then
  ok "re-own walks post-order, selects only entries not owned by the target uid, and uses symlink-safe chown -h"
else
  bad "re-own find invocation changed shape" "find log: $(tr '\n' ';' < "$HIVE_TEST_FIND_LOG")"
fi
if [ "$(printf '%s\n' "$RUN1_PATHS" | sort)" = "$EXPECTED_MISOWNED" ]; then
  ok "only mis-owned entries are handed to chown; already-owned entries are untouched"
else
  bad "re-own touched the wrong entries" "chowned: $(printf '%s' "$RUN1_PATHS" | tr '\n' ';') want: $(printf '%s' "$EXPECTED_MISOWNED" | tr '\n' ';')"
fi
if [ "$RUN1_LAST" = "$TREE" ]; then
  ok "the top-level entry is re-owned last, after everything below it"
else
  bad "top-level entry was not last in the post-order walk" "chowned: $(printf '%s' "$RUN1_PATHS" | tr '\n' ';')"
fi
if [ ! -e "$MARKER" ] && grep -q "WARN: re-own of $TREE to 2011 incomplete" "$WORK/run1.out"; then
  ok "an interrupted re-own writes no marker and logs that the next boot resumes"
else
  bad "an interrupted re-own recorded completion or did not warn" "marker exists: $([ -e "$MARKER" ] && echo yes || echo no) output: $(tr '\n' ';' < "$WORK/run1.out")"
fi

# Run 2: the next boot resumes with only what is still mis-owned.
: > "$HIVE_TEST_CHOWN_LOG"
PATH="$WORK/bin:$PATH" hive_reown_path_if_needed "$TREE" 2011 > "$WORK/run2.out"
RUN2_PATHS="$(chowned_paths | sort)"
RUN2_WANT="$(printf '%s\n' "$EXPECTED_MISOWNED" | grep -vxF -- "$RUN1_APPLIED")"
if [ -n "$RUN2_PATHS" ] && [ "$RUN2_PATHS" = "$RUN2_WANT" ]; then
  ok "a restarted re-own resumes: the entry finished before the kill is not re-chowned"
else
  bad "resume re-chowned finished work or missed remaining entries" "chowned: $(printf '%s' "$RUN2_PATHS" | tr '\n' ';') want: $(printf '%s' "$RUN2_WANT" | tr '\n' ';')"
fi
if [ "$(cat "$MARKER" 2>/dev/null)" = "2011" ] \
   && grep -q "re-owned $TREE 1001→2011 (2 entries changed, [0-9]*s)" "$WORK/run2.out"; then
  ok "a completed re-own records its uid marker and logs entries changed and elapsed time"
else
  bad "completed re-own did not write the marker or the progress line" "marker: $(cat "$MARKER" 2>/dev/null) output: $(tr '\n' ';' < "$WORK/run2.out")"
fi

# Run 3: a later boot is skipped by the marker - no walk, no chown, no output.
: > "$HIVE_TEST_CHOWN_LOG"
: > "$HIVE_TEST_FIND_LOG"
PATH="$WORK/bin:$PATH" hive_reown_path_if_needed "$TREE" 2011 > "$WORK/run3.out"
if [ ! -s "$HIVE_TEST_FIND_LOG" ] && [ ! -s "$HIVE_TEST_CHOWN_LOG" ] && [ ! -s "$WORK/run3.out" ]; then
  ok "a second run is a no-op once the marker records the target uid"
else
  bad "marker did not skip a completed tree" "find: $(tr '\n' ';' < "$HIVE_TEST_FIND_LOG") chown: $(tr '\n' ';' < "$HIVE_TEST_CHOWN_LOG") output: $(tr '\n' ';' < "$WORK/run3.out")"
fi

# Run 4: the marker is not trusted over the tree - a top-level entry that went
# back to the wrong owner is re-walked, and still only that entry is touched.
printf '%s\t%s\n' "$TREE" 1001 >> "$WORK/owners.tsv"
: > "$HIVE_TEST_CHOWN_LOG"
: > "$HIVE_TEST_FIND_LOG"
PATH="$WORK/bin:$PATH" hive_reown_path_if_needed "$TREE" 2011 > "$WORK/run4.out"
if [ -s "$HIVE_TEST_FIND_LOG" ] && [ "$(chowned_paths)" = "$TREE" ]; then
  ok "a marker with a mis-owned top-level entry is re-walked, incrementally"
else
  bad "marker masked a mis-owned top-level entry" "chowned: $(chowned_paths | tr '\n' ';')"
fi

# Run 5: a marker for a DIFFERENT uid (the map moved) forces a walk; a tree
# already fully owned by the new target changes nothing and re-records it.
printf '2099\n' > "$MARKER"
: > "$HIVE_TEST_CHOWN_LOG"
: > "$HIVE_TEST_FIND_LOG"
PATH="$WORK/bin:$PATH" hive_reown_path_if_needed "$TREE" 2011 > "$WORK/run5.out"
if [ -s "$HIVE_TEST_FIND_LOG" ] && [ ! -s "$HIVE_TEST_CHOWN_LOG" ] \
   && [ "$(cat "$MARKER" 2>/dev/null)" = "2011" ] \
   && grep -q "re-own: $TREE already owned by 2011 (0 entries changed" "$WORK/run5.out"; then
  ok "a marker for another uid is ignored; an already-owned tree is walked once and re-marked"
else
  bad "stale-uid marker handling wrong" "chown: $(tr '\n' ';' < "$HIVE_TEST_CHOWN_LOG") marker: $(cat "$MARKER" 2>/dev/null) output: $(tr '\n' ';' < "$WORK/run5.out")"
fi

echo
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1
