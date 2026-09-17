#!/usr/bin/env bash
set -u -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT_DIR/bin/hive-baseline-check.sh"
TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

PASS=0
FAIL=0
MOCK_BIN="$TMP_ROOT/bin"
mkdir -p "$MOCK_BIN"

cat >"$MOCK_BIN/gh" <<'EOF'
#!/usr/bin/env bash
set -u
printf '%s\n' "$*" >>"$MOCK_GH_LOG"
if [[ "${MOCK_GH_FAIL:-}" == "1" ]]; then
  exit 1
fi
case "$1:$2" in
  api:repos/acme/widgets)
    printf '{"default_branch":"trunk"}\n'
    ;;
  api:repos/acme/widgets/commits/trunk/check-runs?per_page=100)
    cat "$MOCK_BASE_RUNS"
    ;;
  api:repos/acme/widgets/compare/trunk...*)
    # MOCK_COMPARE maps head oid → behind_by; an unmapped oid fails the call.
    oid="${2##*...}"
    behind="$(jq -r --arg oid "$oid" '.[$oid] // empty' <<<"${MOCK_COMPARE:-{}}")"
    [[ -n "$behind" ]] || exit 1
    printf '{"behind_by":%s,"ahead_by":1}\n' "$behind"
    ;;
  pr:list)
    cat "$MOCK_PRS"
    ;;
  pr:view)
    # MOCK_PR_VIEW is the JSON for whichever PR number is asked for.
    cat "$MOCK_PR_VIEW"
    ;;
  *)
    echo "unexpected gh invocation: $*" >&2
    exit 1
    ;;
esac
EOF
chmod +x "$MOCK_BIN/gh"

export MOCK_GH_LOG="$TMP_ROOT/gh.log"
export MOCK_BASE_RUNS="$TMP_ROOT/base.json"
export MOCK_PRS="$TMP_ROOT/prs.json"
export MOCK_PR_VIEW="$TMP_ROOT/pr.json"
export MOCK_COMPARE='{}'
# Pin "now" to 2026-08-30T12:00:00Z so the fixtures' timestamps have a stable
# age: a base run at 01:00 the same day is 11h old (fresh under the 24h
# default); one on 2026-08-25 is days old (stale).
export HIVE_BASELINE_NOW=1788091200

run_helper() {
  set +e
  OUTPUT="$(PATH="$MOCK_BIN:$PATH" "$SCRIPT" "$@" 2>&1)"
  STATUS=$?
  set -e
}

check_status() {
  local want="$1" label="$2"
  if [[ "$STATUS" -eq "$want" ]]; then
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label (want exit $want, got $STATUS; output: $OUTPUT)"
    FAIL=$((FAIL + 1))
  fi
}

check_output() {
  local needle="$1" label="$2"
  if [[ "$OUTPUT" == *"$needle"* ]]; then
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label (missing '$needle'; output: $OUTPUT)"
    FAIL=$((FAIL + 1))
  fi
}

check_file_contains() {
  local file="$1" needle="$2" label="$3"
  if grep -Fq -- "$needle" "$file"; then
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label ($file is missing '$needle')"
    FAIL=$((FAIL + 1))
  fi
}

write_base() {
  printf '%s\n' "$1" >"$MOCK_BASE_RUNS"
}

write_prs() {
  printf '%s\n' "$1" >"$MOCK_PRS"
}

echo "=== hive-baseline-check.sh tests ==="

write_base '{"check_runs":[{"name":"build","status":"completed","conclusion":"failure","completed_at":"2026-08-30T01:00:00Z"}]}'
write_prs '[]'
run_helper acme/widgets build
check_status 0 "red default-branch check is shared"
check_output 'default branch "trunk" is red' "human output names the real default branch"
check_output 'SHARED (DEFER_TO_INCIDENT)' "human output carries the action"

write_base '{"check_runs":[{"name":"build","status":"completed","conclusion":"success","completed_at":"2026-08-30T01:00:00Z"}]}'
write_prs '[
  {"number":11,"statusCheckRollup":[{"name":"build","status":"COMPLETED","conclusion":"FAILURE"}]},
  {"number":12,"statusCheckRollup":[{"name":"build","status":"COMPLETED","conclusion":"TIMED_OUT"}]},
  {"number":13,"statusCheckRollup":[{"context":"build","state":"ERROR"}]},
  {"number":14,"statusCheckRollup":[{"name":"build docs","status":"COMPLETED","conclusion":"FAILURE"}]}
]'
run_helper acme/widgets build --json
check_status 0 "three sibling PR failures are shared"
check_output '"reason":"sibling-prs"' "JSON output identifies sibling evidence"
check_output '"sibling_prs":[11,12,13]' "exact check names avoid substring false positives"
check_output '"action":"DEFER_TO_INCIDENT"' "JSON output carries the action"

write_prs '[
  {"number":21,"statusCheckRollup":[{"name":"build","status":"COMPLETED","conclusion":"FAILURE"}]},
  {"number":22,"statusCheckRollup":[{"name":"build","status":"COMPLETED","conclusion":"CANCELLED"}]},
  {"number":23,"statusCheckRollup":[{"name":"build","status":"IN_PROGRESS","conclusion":""}]}
]'
run_helper acme/widgets build
check_status 1 "fewer than three red siblings stay isolated"
check_output '2 open sibling PR(s)' "pending siblings are not counted as red"
check_output 'ISOLATED (FIX_DIFF)' "isolated verdict says fix the diff"

run_helper acme/widgets build --threshold 2
check_status 0 "threshold override is honored"

write_base '{"check_runs":[
  {"name":"build","status":"completed","conclusion":"failure","completed_at":"2026-08-30T01:00:00Z"},
  {"name":"build","status":"completed","conclusion":"success","completed_at":"2026-08-30T02:00:00Z"}
]}'
write_prs '[]'
run_helper acme/widgets build --json
check_status 1 "latest rerun wins over an older base failure"
check_output '"base_conclusion":"success"' "JSON reports the selected base conclusion"

# --- hivecommons/hive#7397: drift vs stale-green baseline, and the action ---

# Case 2 from the issue: base is green and FRESH, every red sibling is behind
# it. That is branch drift, not an incident: not shared, MERGE_BASE.
write_base '{"check_runs":[{"name":"pytest","status":"completed","conclusion":"success","completed_at":"2026-08-30T01:00:00Z"}]}'
write_prs '[
  {"number":31,"headRefOid":"aaa","isCrossRepository":true,"statusCheckRollup":[{"name":"pytest","status":"COMPLETED","conclusion":"FAILURE"}]},
  {"number":32,"headRefOid":"bbb","isCrossRepository":true,"statusCheckRollup":[{"name":"pytest","status":"COMPLETED","conclusion":"FAILURE"}]},
  {"number":33,"headRefOid":"ccc","isCrossRepository":false,"statusCheckRollup":[{"name":"pytest","status":"COMPLETED","conclusion":"FAILURE"}]}
]'
MOCK_COMPARE='{"aaa":6,"bbb":6,"ccc":6}' run_helper acme/widgets pytest --json
check_status 1 "red siblings that are all behind a fresh green base are drift, not an incident"
check_output '"reason":"stale-branches"' "JSON names the drift verdict"
check_output '"action":"MERGE_BASE"' "drift verdict tells the agent to merge base"
check_output '"sibling_drift":{"sampled":3,"behind":3,"current":0,"unknown":0}' "JSON carries the drift sample"
check_output '"base_stale":false' "an 11h-old base conclusion is fresh"

# Case 1 from the issue: the same picture but the base green is DAYS old.
# A date-gated check that rotted cannot be told from drift — say so and ask
# for a re-run rather than sending an agent to repair 18 innocent PRs.
write_base '{"check_runs":[{"name":"pytest","status":"completed","conclusion":"success","completed_at":"2026-08-25T01:00:00Z"}]}'
MOCK_COMPARE='{"aaa":6,"bbb":6,"ccc":6}' run_helper acme/widgets pytest
check_status 2 "all-behind siblings under a stale green base are UNKNOWN, not PR-local"
check_output 'UNKNOWN (RERUN_BASELINE)' "stale-green verdict asks for a baseline re-run"
check_output 're-run "pytest" on "trunk"' "human output says what to re-run"
MOCK_COMPARE='{"aaa":6,"bbb":6,"ccc":6}' run_helper acme/widgets pytest --json
check_output '"shared":null' "stale-baseline is neither shared nor local"
check_output '"base_stale":true' "JSON flags the stale base conclusion"

# An UP-TO-DATE red sibling refutes the base green, however old: shared.
MOCK_COMPARE='{"aaa":6,"bbb":0,"ccc":6}' run_helper acme/widgets pytest --json
check_status 0 "one up-to-date red sibling makes the check shared even under a stale base"
check_output '"action":"DEFER_TO_INCIDENT"' "shared verdict defers to the incident"
check_output '"current":1' "JSON counts the up-to-date sibling"

# A fresh base with a mixed sample keeps the conservative shared verdict.
write_base '{"check_runs":[{"name":"pytest","status":"completed","conclusion":"success","completed_at":"2026-08-30T01:00:00Z"}]}'
MOCK_COMPARE='{"aaa":6,"bbb":0}' run_helper acme/widgets pytest --json
check_status 0 "mixed drift sample (one current, one unreadable) stays shared"

# The PR under triage: a fork head is never pushable; a branch behind base
# gets base merged first; a current same-repo branch is a real diff to fix.
write_prs '[]'
printf '%s\n' '{"number":40,"headRefOid":"ddd","isCrossRepository":true}' >"$MOCK_PR_VIEW"
MOCK_COMPARE='{"ddd":3}' run_helper acme/widgets pytest 40 --json
check_status 1 "fork PR is PR-local"
check_output '"action":"NOT_REACHABLE_FORK"' "fork PR action is comment-only"
check_output '"pr_fork":true' "JSON flags the fork"
MOCK_COMPARE='{"ddd":3}' run_helper acme/widgets pytest 40
check_output 'you cannot push to it' "human output warns not to push to a fork"

printf '%s\n' '{"number":41,"headRefOid":"eee","isCrossRepository":false}' >"$MOCK_PR_VIEW"
MOCK_COMPARE='{"eee":4}' run_helper acme/widgets pytest 41 --json
check_status 1 "behind PR is PR-local"
check_output '"action":"MERGE_BASE"' "behind PR must merge base first"
check_output '"reason":"stale-branch"' "behind PR reason names the drift"
check_output '"pr_behind_by":4' "JSON carries behind_by"

MOCK_COMPARE='{"eee":0}' run_helper acme/widgets pytest 41 --json
check_status 1 "current PR is PR-local"
check_output '"action":"FIX_DIFF"' "current same-repo PR is a real diff to fix"

# The PR argument does not override a shared verdict.
write_base '{"check_runs":[{"name":"pytest","status":"completed","conclusion":"failure","completed_at":"2026-08-30T01:00:00Z"}]}'
MOCK_COMPARE='{"ddd":3}' run_helper acme/widgets pytest 40 --json
check_status 0 "a red base is shared regardless of the PR given"
check_output '"action":"DEFER_TO_INCIDENT"' "shared verdict wins over the PR's own state"

run_helper acme/widgets pytest 40 41
check_status 2 "a second positional is rejected"
run_helper acme/widgets pytest --max-baseline-age-hours x
check_status 2 "a non-numeric max age is rejected"

MOCK_GH_FAIL=1 run_helper acme/widgets build
check_status 2 "GitHub lookup failures are unknown, never isolated"

run_helper not-a-repo build
check_status 2 "malformed repository names are rejected"

check_file_contains "$ROOT_DIR/src/Dockerfile" \
  'COPY bin/hive-baseline-check.sh /usr/local/bin/hive-baseline-check.sh' \
  "agent image packages the classifier"
check_file_contains "$ROOT_DIR/bin/hive-deploy.sh" \
  "sudo install -m 0755 \"\$BASELINE_HELPER_SRC\" \"\$BASELINE_HELPER_DST\"" \
  "native deploy bootstraps the classifier"

echo ""
echo "$PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
