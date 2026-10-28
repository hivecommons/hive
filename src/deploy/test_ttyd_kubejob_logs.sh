#!/usr/bin/env bash
# The browser terminal must stream a sandbox.runtime: job agent's Job pod log
# instead of waiting for a tmux session that never appears (#14798).
# Run: bash src/deploy/test_ttyd_kubejob_logs.sh
#
# THE BUG. A job-runtime agent runs every kick as a Kubernetes Job in its own
# pod (src/crux/kubejob), so there is never a hive-<agent> tmux session in the
# hive pod. ttyd-tmux.sh only knew how to wait for one, so the dashboard's
# terminal showed "tmux session not created yet" for the whole wait window and
# then "no tmux socket found" — never what the sandboxed agent was doing.
#
# HOW THIS TESTS IT. `curl` is stubbed on PATH with a fake API server that
# answers the pod list, pod and pod log endpoints from fixture files, and the
# ServiceAccount directory is a scratch one. TMUX_TMPDIR points at an empty
# directory, so no tmux session can be found and the helper must take the Job
# path. HIVE_TTYD_WAIT_SEC=0 keeps the pre-fix behaviour (exit 1, no log) fast.
set -uo pipefail

PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; [ $# -gt 1 ] && echo "        $2"; FAIL=$((FAIL + 1)); }
finish() {
  echo ""
  echo "=== Results: $PASS passed, $FAIL failed ==="
  [ "$FAIL" -eq 0 ]
}

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ATTACH="${ROOT}/src/deploy/ttyd-tmux.sh"

echo "=== ttyd-tmux.sh streams Job pod logs for sandbox.runtime: job agents (#14798) ==="

if [ ! -f "$ATTACH" ]; then
  fail "locate $ATTACH" "the layout moved — this test cannot verify anything"
  finish; exit $?
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/bin" "$WORK/sa" "$WORK/tmux" "$WORK/api"
echo "test-token" > "$WORK/sa/token"
echo "hive-ns" > "$WORK/sa/namespace"

# Fake API server. Every URL is logged; responses come from $WORK/api. When
# pods.after exists, the pod list is empty until that many list calls have
# been made (a Job that starts while the terminal is waiting).
cat > "$WORK/bin/curl" <<'EOF'
#!/usr/bin/env bash
for url; do :; done
echo "$url" >> "$FAKE_API/urls"
case "$url" in
  *"/pods?labelSelector="*)
    n=$(( $(cat "$FAKE_API/list-calls" 2>/dev/null || echo 0) + 1 ))
    echo "$n" > "$FAKE_API/list-calls"
    if [ -f "$FAKE_API/pods.json" ] && [ "$n" -gt "$(cat "$FAKE_API/pods.after" 2>/dev/null || echo 0)" ]; then
      cat "$FAKE_API/pods.json"
    else
      echo '{"kind":"PodList","metadata":{"resourceVersion":"1"},"items":[]}'
    fi ;;
  *"/log?"*) cat "$FAKE_API/log.txt" ;;
  *"/pods/"*) cat "$FAKE_API/pod.json" ;;
  *) exit 22 ;;
esac
EOF
chmod +x "$WORK/bin/curl"

# Two runs of agent My_Agent: the older one first in the list, ownerReferences
# naming the Jobs (which must not be mistaken for pods).
POD_OLD="hive-kick-hive-my-agent-1700000000-abcde"
POD_NEW="hive-kick-hive-my-agent-1700009999-x7k2p"
write_pods() {
  printf '{"kind":"PodList","metadata":{"resourceVersion":"9"},"items":[{"metadata":{"name":"%s","ownerReferences":[{"kind":"Job","name":"hive-kick-hive-my-agent-1700000000"}]}},{"metadata":{"name":"%s","ownerReferences":[{"kind":"Job","name":"hive-kick-hive-my-agent-1700009999"}]}}]}\n' "$POD_OLD" "$POD_NEW" > "$WORK/api/pods.json"
  echo '{"metadata":{"name":"'"$POD_NEW"'"},"status":{"phase":"Failed","containerStatuses":[{"name":"kick","state":{"terminated":{"exitCode":3,"reason":"Error"}}}]}}' > "$WORK/api/pod.json"
  printf 'kick: cloning workspace\nkick: running tests\n' > "$WORK/api/log.txt"
}

reset_api() { rm -f "$WORK/api/"*; }

run_attach() {
  local wait="$1"
  OUT=$(env PATH="$WORK/bin:$PATH" FAKE_API="$WORK/api" TMUX_TMPDIR="$WORK/tmux" \
    HIVE_TTYD_KUBE_SA_DIR="$WORK/sa" HIVE_TTYD_KUBE_API="https://api.test" \
    HIVE_TTYD_WAIT_SEC="$wait" HIVE_TTYD_POLL_SEC=1 \
    timeout 30 bash "$ATTACH" "hive-My_Agent" 2>&1)
  RC=$?
}

# --- 1. a finished Job pod: its log and exit status are shown ---------------
reset_api; write_pods
run_attach 0
if [ "$RC" -eq 0 ]; then pass "exits 0 after streaming the Job pod log"; else fail "exits 0 after streaming the Job pod log" "rc=$RC: $OUT"; fi
if grep -q "kick: running tests" <<<"$OUT"; then pass "the Job pod's log is shown"; else fail "the Job pod's log is shown" "$OUT"; fi
if grep -q "exit code 3" <<<"$OUT"; then pass "the run's exit status is shown"; else fail "the run's exit status is shown" "$OUT"; fi
if grep -q "/pods/${POD_NEW}/log?container=kick&follow=true" "$WORK/api/urls" 2>/dev/null; then
  pass "follows the NEWEST run's kick container log"
else
  fail "follows the NEWEST run's kick container log" "$(cat "$WORK/api/urls" 2>/dev/null)"
fi
if grep -q "/namespaces/hive-ns/pods?labelSelector=app.kubernetes.io%2Fmanaged-by%3Dhive-kubejob%2Chive.kubestellar.io%2Fagent%3Dhive-my-agent-sandbox" "$WORK/api/urls" 2>/dev/null; then
  pass "selects pods by the launcher's managed-by and sanitized agent labels"
else
  fail "selects pods by the launcher's managed-by and sanitized agent labels" "$(cat "$WORK/api/urls" 2>/dev/null)"
fi
if grep -q "tmux session not created yet" <<<"$OUT"; then
  fail "does not claim to wait for a tmux session when a Job pod exists" "$OUT"
else
  pass "does not claim to wait for a tmux session when a Job pod exists"
fi

# --- 2. no run yet: said clearly ---------------------------------------------
reset_api
run_attach 0
if [ "$RC" -ne 0 ] && grep -q "has had no run yet" <<<"$OUT"; then
  pass "says clearly that a job-runtime agent has had no run yet"
else
  fail "says clearly that a job-runtime agent has had no run yet" "rc=$RC: $OUT"
fi

# --- 3. a Job that starts while the terminal waits is picked up -------------
reset_api; write_pods; echo 2 > "$WORK/api/pods.after"
run_attach 10
if [ "$RC" -eq 0 ] && grep -q "kick: cloning workspace" <<<"$OUT"; then
  pass "streams a Job pod that appears during the wait"
else
  fail "streams a Job pod that appears during the wait" "rc=$RC: $OUT"
fi

# --- 4. no ServiceAccount: the API is never contacted -----------------------
reset_api; write_pods
OUT=$(env PATH="$WORK/bin:$PATH" FAKE_API="$WORK/api" TMUX_TMPDIR="$WORK/tmux" \
  HIVE_TTYD_KUBE_SA_DIR="$WORK/no-such-sa" HIVE_TTYD_WAIT_SEC=0 HIVE_TTYD_POLL_SEC=1 \
  timeout 30 bash "$ATTACH" "hive-My_Agent" 2>&1)
RC=$?
if [ "$RC" -ne 0 ] && [ ! -s "$WORK/api/urls" ] && grep -q "tmux session not created yet" <<<"$OUT"; then
  pass "outside Kubernetes the tmux wait is unchanged and no API call is made"
else
  fail "outside Kubernetes the tmux wait is unchanged and no API call is made" "rc=$RC: $OUT"
fi

finish
