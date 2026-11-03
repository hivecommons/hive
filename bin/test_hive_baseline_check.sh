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
    # MOCK_PARENT makes acme/widgets a GitHub fork of that repository.
    if [[ -n "${MOCK_PARENT:-}" ]]; then
      printf '{"default_branch":"trunk","fork":true,"parent":{"full_name":"%s"}}\n' "$MOCK_PARENT"
    else
      printf '{"default_branch":"trunk"}\n'
    fi
    ;;
  api:repos/upstream/widgets)
    [[ -n "${MOCK_UPSTREAM_RUNS:-}" ]] || exit 1
    printf '{"default_branch":"main"}\n'
    ;;
  api:repos/upstream/widgets/commits/main/check-runs\?*)
    cat "$MOCK_UPSTREAM_RUNS"
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
  api:repos/acme/widgets/pulls\?state=open\&per_page=100)
    # MOCK_PRS keeps the GraphQL rollup shape; serve it as REST pulls. A PR
    # with no headRefOid gets a synthetic head no compare mapping knows.
    jq -c '[.[] | {number, head: {sha: (.headRefOid // "sha-\(.number)")}}]' "$MOCK_PRS"
    ;;
  api:repos/acme/widgets/commits/*/check-runs\?*)
    [[ -z "${MOCK_CHECKS_FAIL:-}" ]] || exit 1
    sha="${2#repos/acme/widgets/commits/}"; sha="${sha%%/*}"
    jq -c --arg sha "$sha" '
      [.[] | select((.headRefOid // "sha-\(.number)") == $sha) | .statusCheckRollup[]? | select(.name != null)
       | {id: (.id // null), name, status: ((.status // "") | ascii_downcase), conclusion: ((.conclusion // "") | ascii_downcase | if . == "" then null else . end), completed_at: (.completedAt // null)}]
      | {total_count: length, check_runs: .}' "$MOCK_PRS"
    ;;
  api:repos/acme/widgets/commits/*/status\?*)
    sha="${2#repos/acme/widgets/commits/}"; sha="${sha%%/*}"
    jq -c --arg sha "$sha" '
      [.[] | select((.headRefOid // "sha-\(.number)") == $sha) | .statusCheckRollup[]? | select(.context != null)
       | {context, state: ((.state // "") | ascii_downcase)}]
      | {statuses: .}' "$MOCK_PRS"
    ;;
  pr:list)
    # The agent gh wrapper refuses this for staff agents (#11608).
    echo "⛔ BLOCKED: gh pr list is disabled for agents — but ONLY listing/enumeration is blocked." >&2
    exit 1
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

# #11608: the sibling lookup must work under the agent gh wrapper, which
# refuses `gh pr list` — the mock refuses it the same way. A check name with
# spaces and parentheses reaches the per-PR lookup URL-encoded.
write_base '{"check_runs":[{"name":"Check ogc-lts (44)","status":"completed","conclusion":"success","completed_at":"2026-08-30T01:00:00Z"}]}'
write_prs '[
  {"number":61,"headRefOid":"f61","statusCheckRollup":[{"name":"Check ogc-lts (44)","status":"COMPLETED","conclusion":"FAILURE"}]},
  {"number":62,"headRefOid":"f62","statusCheckRollup":[{"name":"Check ogc-lts (44)","status":"COMPLETED","conclusion":"FAILURE"}]},
  {"number":63,"headRefOid":"f63","statusCheckRollup":[{"name":"Check ogc-lts (44)","status":"COMPLETED","conclusion":"FAILURE"}]},
  {"number":64,"headRefOid":"f64","statusCheckRollup":[{"name":"Check ogc-lts (44)","status":"COMPLETED","conclusion":"SUCCESS"}]}
]'
: >"$MOCK_GH_LOG"
MOCK_COMPARE='{"f61":0,"f62":0,"f63":0}' run_helper acme/widgets "Check ogc-lts (44)" --json
check_status 0 "sibling lookup reaches a shared verdict without gh pr list"
check_output '"sibling_prs":[61,62,63]' "only the red siblings are counted"
check_file_contains "$MOCK_GH_LOG" 'check-runs?check_name=Check%20ogc-lts%20%2844%29&per_page=100' \
  "the per-PR lookup URL-encodes the exact check name"

# One sibling whose checks cannot be read is an API failure, never evidence.
MOCK_CHECKS_FAIL=1 run_helper acme/widgets "Check ogc-lts (44)"
check_status 2 "an unreadable sibling check makes the verdict unknown"

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

# --- hivecommons/hive#11609: upstream baseline for a fork ---

# A fork's PR fails a check that is also red on the upstream default branch,
# and the fork's own default branch has no run of it: the failure is
# upstream's. The verdict is DEFER_EXTERNAL with a marker pinned to the PR's
# head, the URL-encoded check name and the PR's newest (red) run of it.
UP_HEAD=54ff622aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export MOCK_UPSTREAM_RUNS="$TMP_ROOT/upstream.json"
printf '%s\n' '{"check_runs":[{"name":"Check ogc-lts (44)","status":"completed","conclusion":"failure","completed_at":"2026-08-30T01:00:00Z"}]}' >"$MOCK_UPSTREAM_RUNS"
write_base '{"check_runs":[]}'
write_prs "[{\"number\":66,\"headRefOid\":\"$UP_HEAD\",\"statusCheckRollup\":[
  {\"id\":4400,\"name\":\"Check ogc-lts (44)\",\"status\":\"COMPLETED\",\"conclusion\":\"SUCCESS\",\"completedAt\":\"2026-08-30T01:00:00Z\"},
  {\"id\":4401,\"name\":\"Check ogc-lts (44)\",\"status\":\"COMPLETED\",\"conclusion\":\"FAILURE\",\"completedAt\":\"2026-08-30T02:00:00Z\"}]}]"
printf '{"number":66,"headRefOid":"%s","isCrossRepository":false}\n' "$UP_HEAD" >"$MOCK_PR_VIEW"
MOCK_PARENT=upstream/widgets MOCK_COMPARE="{\"$UP_HEAD\":0}" run_helper acme/widgets "Check ogc-lts (44)" 66 --json
check_status 0 "a check red on the upstream default branch is shared"
check_output '"action":"DEFER_EXTERNAL"' "red upstream defers as external"
check_output '"reason":"upstream"' "JSON names the upstream evidence"
check_output '"upstream":{"repo":"upstream/widgets","branch":"main","conclusion":"failure"' "JSON carries the upstream baseline"
check_output "\"external_marker\":\"<!-- hive-ci-external head=$UP_HEAD check=Check%20ogc-lts%20%2844%29 run=4401 -->\"" \
  "the marker pins the PR head, the encoded check name and the newest red run"
check_output "\"transient_marker\":\"<!-- hive-ci-transient head=$UP_HEAD check=Check%20ogc-lts%20%2844%29 run=4401 -->\"" \
  "the transient marker pins the same head, encoded check name and newest red run"
MOCK_PARENT=upstream/widgets MOCK_COMPARE="{\"$UP_HEAD\":0}" run_helper acme/widgets "Check ogc-lts (44)" 66
check_output 'is red (failure) on upstream upstream/widgets default branch "main"' "human output names the upstream"
check_output "End that comment with this marker: <!-- hive-ci-external head=$UP_HEAD" "human output prints the marker"

# The newest run at that head went green (a re-run passed): there is nothing
# to defer, so no marker — a later red re-run gets its own run id.
write_prs "[{\"number\":66,\"headRefOid\":\"$UP_HEAD\",\"statusCheckRollup\":[
  {\"id\":4401,\"name\":\"Check ogc-lts (44)\",\"status\":\"COMPLETED\",\"conclusion\":\"FAILURE\",\"completedAt\":\"2026-08-30T02:00:00Z\"},
  {\"id\":4402,\"name\":\"Check ogc-lts (44)\",\"status\":\"COMPLETED\",\"conclusion\":\"SUCCESS\",\"completedAt\":\"2026-08-30T03:00:00Z\"}]}]"
MOCK_PARENT=upstream/widgets MOCK_COMPARE="{\"$UP_HEAD\":0}" run_helper acme/widgets "Check ogc-lts (44)" 66 --json
if [[ "$OUTPUT" == *_marker* ]]; then
  echo "  FAIL: no marker when the PR's newest run is green (output: $OUTPUT)"; FAIL=$((FAIL + 1))
else
  echo "  PASS: no marker when the PR's newest run is green"; PASS=$((PASS + 1))
fi

# Without a PR there is no head to pin: the verdict stands, no marker.
MOCK_PARENT=upstream/widgets run_helper acme/widgets "Check ogc-lts (44)" --json
check_status 0 "red upstream is shared without a PR argument"
if [[ "$OUTPUT" == *_marker* ]]; then
  echo "  FAIL: no marker without a PR head (output: $OUTPUT)"; FAIL=$((FAIL + 1))
else
  echo "  PASS: no marker without a PR head"; PASS=$((PASS + 1))
fi

# --upstream names the upstream of a repository GitHub does not call a fork.
MOCK_COMPARE="{\"$UP_HEAD\":0}" run_helper acme/widgets "Check ogc-lts (44)" 66 --upstream upstream/widgets --json
check_output '"action":"DEFER_EXTERNAL"' "--upstream supplies the upstream for a non-fork"

# A fresh green on the fork's own default branch proves the check can pass
# here: upstream red does not excuse the PR.
write_base '{"check_runs":[{"name":"Check ogc-lts (44)","status":"completed","conclusion":"success","completed_at":"2026-08-30T01:00:00Z"}]}'
MOCK_PARENT=upstream/widgets MOCK_COMPARE="{\"$UP_HEAD\":0}" run_helper acme/widgets "Check ogc-lts (44)" 66 --json
check_status 1 "a fresh green fork base keeps the PR-local verdict despite red upstream"
check_output '"action":"FIX_DIFF"' "fresh green fork base means fix the diff"
check_output '"conclusion":"failure"' "the upstream evidence is still reported"

# A green upstream adds nothing.
write_base '{"check_runs":[]}'
printf '%s\n' '{"check_runs":[{"name":"Check ogc-lts (44)","status":"completed","conclusion":"success","completed_at":"2026-08-30T01:00:00Z"}]}' >"$MOCK_UPSTREAM_RUNS"
MOCK_PARENT=upstream/widgets MOCK_COMPARE="{\"$UP_HEAD\":0}" run_helper acme/widgets "Check ogc-lts (44)" 66 --json
check_status 1 "green upstream leaves the verdict PR-local"

# An unreadable upstream is never evidence: the local verdict stands.
MOCK_PARENT=upstream/widgets MOCK_UPSTREAM_RUNS='' MOCK_COMPARE="{\"$UP_HEAD\":0}" run_helper acme/widgets "Check ogc-lts (44)" 66 --json
check_status 1 "an unreadable upstream keeps the local verdict"
check_output '"conclusion":"unknown"' "JSON reports the upstream as unknown"

run_helper acme/widgets build --upstream not-a-repo
check_status 2 "a malformed --upstream is rejected"
unset MOCK_UPSTREAM_RUNS

# --- hivecommons/hive#13025: transient_marker on any verdict ---

# A same-repo PR whose newest run of the check is red, on a fresh green base
# with no upstream: the verdict is PR-local (FIX_DIFF), yet the helper prints
# a transient_marker pinned to the head, encoded check name and red run so
# the agent copies it rather than assembling one by hand. external_marker is
# reserved for DEFER_EXTERNAL and must not appear.
TR_HEAD=7a11c0ffee00000000000000000000000000beef
write_base '{"check_runs":[{"name":"Check ogc-lts (44)","status":"completed","conclusion":"success","completed_at":"2026-08-30T01:00:00Z"}]}'
write_prs "[{\"number\":77,\"headRefOid\":\"$TR_HEAD\",\"statusCheckRollup\":[
  {\"id\":5500,\"name\":\"Check ogc-lts (44)\",\"status\":\"COMPLETED\",\"conclusion\":\"SUCCESS\",\"completedAt\":\"2026-08-30T01:00:00Z\"},
  {\"id\":5501,\"name\":\"Check ogc-lts (44)\",\"status\":\"COMPLETED\",\"conclusion\":\"FAILURE\",\"completedAt\":\"2026-08-30T02:00:00Z\"}]}]"
printf '{"number":77,"headRefOid":"%s","isCrossRepository":false}\n' "$TR_HEAD" >"$MOCK_PR_VIEW"
MOCK_COMPARE="{\"$TR_HEAD\":0}" run_helper acme/widgets "Check ogc-lts (44)" 77 --json
check_status 1 "a current same-repo PR with a red newest run is PR-local"
check_output '"action":"FIX_DIFF"' "the verdict is FIX_DIFF, not a deferral"
check_output "\"transient_marker\":\"<!-- hive-ci-transient head=$TR_HEAD check=Check%20ogc-lts%20%2844%29 run=5501 -->\"" \
  "FIX_DIFF still prints the transient marker pinned to the head, encoded check and newest red run"
if [[ "$OUTPUT" == *external_marker* ]]; then
  echo "  FAIL: no external marker outside DEFER_EXTERNAL (output: $OUTPUT)"; FAIL=$((FAIL + 1))
else
  echo "  PASS: no external marker outside DEFER_EXTERNAL"; PASS=$((PASS + 1))
fi

# A branch behind base gets the same transient marker with its MERGE_BASE verdict.
MOCK_COMPARE="{\"$TR_HEAD\":4}" run_helper acme/widgets "Check ogc-lts (44)" 77 --json
check_output '"action":"MERGE_BASE"' "a behind PR must merge base first"
check_output "\"transient_marker\":\"<!-- hive-ci-transient head=$TR_HEAD check=Check%20ogc-lts%20%2844%29 run=5501 -->\"" \
  "MERGE_BASE also prints the transient marker"
if [[ "$OUTPUT" == *external_marker* ]]; then
  echo "  FAIL: no external marker on MERGE_BASE (output: $OUTPUT)"; FAIL=$((FAIL + 1))
else
  echo "  PASS: no external marker on MERGE_BASE"; PASS=$((PASS + 1))
fi

# Only --json carries the transient marker; the human text never prints it.
MOCK_COMPARE="{\"$TR_HEAD\":0}" run_helper acme/widgets "Check ogc-lts (44)" 77
check_output 'ISOLATED (FIX_DIFF)' "human output gives the PR-local verdict"
if [[ "$OUTPUT" == *hive-ci-* ]]; then
  echo "  FAIL: human output prints no marker on a PR-local verdict (output: $OUTPUT)"; FAIL=$((FAIL + 1))
else
  echo "  PASS: human output prints no marker on a PR-local verdict"; PASS=$((PASS + 1))
fi

# The newest run at that head is green (a re-run passed): nothing to triage,
# so no transient marker either.
write_prs "[{\"number\":77,\"headRefOid\":\"$TR_HEAD\",\"statusCheckRollup\":[
  {\"id\":5501,\"name\":\"Check ogc-lts (44)\",\"status\":\"COMPLETED\",\"conclusion\":\"FAILURE\",\"completedAt\":\"2026-08-30T02:00:00Z\"},
  {\"id\":5502,\"name\":\"Check ogc-lts (44)\",\"status\":\"COMPLETED\",\"conclusion\":\"SUCCESS\",\"completedAt\":\"2026-08-30T03:00:00Z\"}]}]"
MOCK_COMPARE="{\"$TR_HEAD\":0}" run_helper acme/widgets "Check ogc-lts (44)" 77 --json
check_output '"action":"FIX_DIFF"' "a green newest run keeps the PR-local verdict"
if [[ "$OUTPUT" == *_marker* ]]; then
  echo "  FAIL: no marker of either kind when the PR's newest run is green (output: $OUTPUT)"; FAIL=$((FAIL + 1))
else
  echo "  PASS: no marker of either kind when the PR's newest run is green"; PASS=$((PASS + 1))
fi

check_file_contains "$ROOT_DIR/src/Dockerfile" \
  'COPY bin/hive-baseline-check.sh /usr/local/bin/hive-baseline-check.sh' \
  "agent image packages the classifier"
check_file_contains "$ROOT_DIR/bin/hive-deploy.sh" \
  "sudo install -m 0755 \"\$BASELINE_HELPER_SRC\" \"\$BASELINE_HELPER_DST\"" \
  "native deploy bootstraps the classifier"

echo ""
echo "$PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
