#!/usr/bin/env bash
# Contract tests for bin/nous-hive-gate.py (RFC #11585 part 3, #11696).
# Run: bash bin/test_nous_hive_gate.sh
#
# nous-hive-gate.py is the agent side of the Nous gate protocol: when a
# campaign reaches a design/findings gate it publishes
# <nous_dir>/gates/pending.json and waits for the hive to record the owner's
# answer in <nous_dir>/gates/decision.json (pkg/nousstore owns the Go side).
# Before #11696 it PUT the question to the owner-only gate-decision endpoint,
# which always failed, so every gate was silently rejected.
#
# These tests EXECUTE the script against a temp Nous root (HIVE_NOUS_DATA_DIR)
# with sub-second polling, and play the hive by writing decision.json. Every
# positive case sits next to a contrasting one (approve vs reject vs abort;
# matching vs stale gate id; publish vs auto-approve) so a bridge that returns
# a constant cannot pass. Hermetic: no network, never touches /data.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${REPO_ROOT}/bin/nous-hive-gate.py"
PY="${PYTHON:-python3}"

PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && echo "        $2"
  FAIL=$((FAIL + 1))
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# fresh_root prints a new empty Nous root.
fresh_root() {
  local d
  d="$(mktemp -d "$WORK/nous.XXXXXX")"
  echo "$d"
}

# run_gate <root> <mode> <scope> <question> [extra env...] — runs the CLI,
# prints the decision line, returns the exit code.
run_gate() {
  local root="$1" mode="$2" scope="$3" question="$4"; shift 4
  env HIVE_NOUS_DATA_DIR="$root" NOUS_HIVE_MODE="$mode" NOUS_HIVE_SCOPE="$scope" \
      NOUS_GATE_POLL_INTERVAL_SEC=0.05 NOUS_GATE_TIMEOUT_SEC="${GATE_TIMEOUT:-5}" "$@" \
      "$PY" "$SCRIPT" "$question" 2>"$root/stderr.log"
}

# answer_when_pending <root> <decision> [gate_id] — background hive: waits for
# pending.json, then writes decision.json for its id (or the given id). It
# exits on its own once it has answered or after ~10s, so no wait is needed.
answer_when_pending() {
  local root="$1" decision="$2" forced_id="${3:-}"
  (
    for _ in $(seq 1 200); do
      if [ -f "$root/gates/pending.json" ]; then
        gid="$forced_id"
        if [ -z "$gid" ]; then
          gid="$("$PY" -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "$root/gates/pending.json")"
        fi
        printf '{"gate_id":"%s","decision":"%s","reason":"t","decided_by":"owner"}\n' "$gid" "$decision" > "$root/gates/decision.json.tmp"
        mv "$root/gates/decision.json.tmp" "$root/gates/decision.json"
        # The real hive removes pending.json too.
        [ -z "$forced_id" ] && rm -f "$root/gates/pending.json"
        exit 0
      fi
      sleep 0.05
    done
    exit 1
  ) >/dev/null 2>&1 &
}

json_field() { "$PY" -c 'import json,sys; v=json.load(open(sys.argv[1])); print(v.get(sys.argv[2], ""))' "$1" "$2"; }

echo "=== nous-hive-gate.py: file-based gate protocol (#11696) ==="

echo
echo "-- observe/evolve auto-approve without publishing --"
for mode in observe evolve; do
  root="$(fresh_root)"
  out="$(run_gate "$root" "$mode" governor "Run it?")"; rc=$?
  if [ $rc -eq 0 ] && [[ "$out" == *"decision: approve"* ]] && [ ! -e "$root/gates/pending.json" ]; then
    pass "mode=$mode auto-approves and publishes nothing"
  else
    fail "mode=$mode auto-approve" "rc=$rc out=$out"
  fi
done

echo
echo "-- suggest mode publishes pending.json with the contract fields --"
root="$(fresh_root)"
answer_when_pending "$root" approve
out="$(run_gate "$root" suggest governor "Run the cache experiment?" NOUS_GATE_KIND=design)"; rc=$?
if [ $rc -eq 0 ] && [[ "$out" == *"decision: approve"* ]]; then
  pass "owner approve → exit 0, decision approve"
else
  fail "owner approve round-trip" "rc=$rc out=$out stderr=$(cat "$root/stderr.log")"
fi
# The answered gate's pending file is gone (hive removed it) and the decision stayed.
[ ! -e "$root/gates/pending.json" ] && pass "pending.json cleared by the hive side" || fail "pending.json still present after decision"
[ -f "$root/gates/decision.json" ] && pass "decision.json left in place for audit" || fail "decision.json missing"

# Inspect the published request by publishing one that is never answered.
root="$(fresh_root)"
GATE_TIMEOUT=0.3 run_gate "$root" suggest repo "Ship the findings?" NOUS_GATE_KIND=findings >/dev/null; rc=$?
# Timeout → reject (exit 1), and the stale pending file is withdrawn.
[ $rc -eq 1 ] && pass "unanswered gate times out as reject (exit 1)" || fail "timeout exit code" "rc=$rc"
grep -q "timeout waiting" "$root/stderr.log" && pass "timeout is reported on stderr" || fail "timeout not reported"
[ ! -e "$root/gates/pending.json" ] && pass "timed-out gate withdraws pending.json" || fail "pending.json left behind after timeout"

# Capture the request shape mid-flight: answer with a mismatching id so the
# file persists long enough to read (a stale id must never be accepted).
root="$(fresh_root)"
answer_when_pending "$root" approve stale-gate-id
GATE_TIMEOUT=0.6 run_gate "$root" suggest repo "Ship the findings?" NOUS_GATE_KIND=findings >/dev/null; rc=$?
[ $rc -eq 1 ] && pass "decision for a different gate_id is ignored (timeout → reject)" || fail "stale gate_id accepted" "rc=$rc"
req="$WORK/req.json"
# pending.json was withdrawn on timeout; the request shape is checked via a
# fresh publish through the module API instead.
HIVE_NOUS_DATA_DIR="$root" NOUS_HIVE_SCOPE=repo NOUS_GATE_KIND=findings "$PY" - "$SCRIPT" "$req" <<'PYEOF'
import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("gate", sys.argv[1])
gate = importlib.util.module_from_spec(spec); spec.loader.exec_module(gate)
req = gate.build_request("Ship the findings?", summary_path=None)
assert gate.post_pending(req)
json.dump(req, open(sys.argv[2], "w"))
PYEOF
if [ -f "$root/gates/pending.json" ]; then
  pass "post_pending writes <root>/gates/pending.json"
else
  fail "post_pending did not write pending.json"
fi
[ "$(json_field "$root/gates/pending.json" question)" = "Ship the findings?" ] && pass "pending carries the question" || fail "question missing"
[ "$(json_field "$root/gates/pending.json" scope)" = "repo" ] && pass "pending carries the scope" || fail "scope missing"
[ "$(json_field "$root/gates/pending.json" gate)" = "findings" ] && pass "pending carries the gate kind" || fail "gate kind missing"
gid="$(json_field "$root/gates/pending.json" id)"
[[ "$gid" == findings-* ]] && pass "gate id is prefixed with the gate kind ($gid)" || fail "gate id shape" "$gid"
[ -n "$(json_field "$root/gates/pending.json" requested_at)" ] && pass "pending carries requested_at" || fail "requested_at missing"
[ ! -e "$root/gates/pending.json.tmp" ] && pass "publish is atomic (no .tmp left)" || fail ".tmp left behind"

echo
echo "-- reject and abort are returned verbatim (exit 1) --"
for decision in reject abort; do
  root="$(fresh_root)"
  answer_when_pending "$root" "$decision"
  out="$(run_gate "$root" suggest governor "Run it?")"; rc=$?
    if [ $rc -eq 1 ] && [[ "$out" == *"decision: $decision"* ]]; then
    pass "owner $decision → exit 1, decision $decision"
  else
    fail "owner $decision" "rc=$rc out=$out"
  fi
done

echo
echo "-- repo scope forces suggest even in observe mode --"
root="$(fresh_root)"
answer_when_pending "$root" reject
out="$(run_gate "$root" observe repo "Patch the repo?")"; rc=$?
if [ $rc -eq 1 ] && [[ "$out" == *"decision: reject"* ]]; then
  pass "repo scope waited for the owner (observe did not auto-approve)"
else
  fail "repo scope forcing suggest" "rc=$rc out=$out"
fi

echo
echo "-- an invalid or unreadable decision is still pending --"
root="$(fresh_root)"
mkdir -p "$root/gates"
echo '{"gate_id":"x","decision":"maybe"}' > "$root/gates/decision.json"
GATE_TIMEOUT=0.3 run_gate "$root" suggest governor "Run it?" >/dev/null; rc=$?
[ $rc -eq 1 ] && grep -q "timeout waiting" "$root/stderr.log" && pass "unknown decision value is ignored" || fail "unknown decision accepted" "rc=$rc"
root="$(fresh_root)"
mkdir -p "$root/gates"
echo '{' > "$root/gates/decision.json"
GATE_TIMEOUT=0.3 run_gate "$root" suggest governor "Run it?" >/dev/null; rc=$?
[ $rc -eq 1 ] && grep -q "unreadable decision" "$root/stderr.log" && pass "corrupt decision.json is reported and ignored" || fail "corrupt decision handling" "rc=$rc"

echo
echo "-- publish failure rejects rather than hangs --"
root="$(fresh_root)"
touch "$root/gates"   # a regular file where the gates dir must go
out="$(run_gate "$root" suggest governor "Run it?")"; rc=$?
[ $rc -eq 1 ] && grep -q "failed to publish" "$root/stderr.log" && pass "unwritable gates dir → reject" || fail "publish failure" "rc=$rc out=$out"

echo
echo "-- NOUS_DATA_DIR is honoured when HIVE_NOUS_DATA_DIR is unset --"
root="$(fresh_root)"
answer_when_pending "$root" approve
out="$(env -u HIVE_NOUS_DATA_DIR NOUS_DATA_DIR="$root" NOUS_HIVE_MODE=suggest NOUS_HIVE_SCOPE=governor \
      NOUS_GATE_POLL_INTERVAL_SEC=0.05 NOUS_GATE_TIMEOUT_SEC=5 "$PY" "$SCRIPT" "Run it?" 2>/dev/null)"; rc=$?
[ $rc -eq 0 ] && [[ "$out" == *"decision: approve"* ]] && pass "NOUS_DATA_DIR fallback publishes to the right root" || fail "NOUS_DATA_DIR fallback" "rc=$rc out=$out"

echo
echo "-- usage --"
"$PY" "$SCRIPT" >/dev/null 2>&1; rc=$?
[ $rc -eq 1 ] && pass "no question → usage, exit 1" || fail "usage exit code" "rc=$rc"

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
