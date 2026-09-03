#!/usr/bin/env bash
# The changelog reminder must never fail a PR (#4440).
# Run: bash src/deploy/test_changelog_reminder.sh
#
# WHY THIS EXECUTES THE STEP RATHER THAN GREPPING IT. The bug was not a missing
# line, it was an exit status: `gh api ... comments` answered
# `Resource not accessible by integration (HTTP 403)` on every fork PR, and
# `shell: bash -e` turned that into a red check. A grep over the YAML would
# happily pass on a script that still dies on the first non-zero command. So
# this EXTRACTS the real `run:` block out of the workflow and runs it, with a
# stub `gh` that fails exactly the way GitHub does.
#
# The workflow's own header makes two promises — "it comments once and always
# succeeds" and "it never blocks a merge". Each case below is one of those
# promises, checked against the shipped script.
set -uo pipefail

PASS=0
FAIL=0

# Shared skip discipline (#5388): hive_test_skip is permissive by default and
# FATAL under HIVE_TEST_REQUIRE_BEHAVIOURAL=1, so a lane whose runner GUARANTEES
# the precondition below turns a silent skip into a red build.
# shellcheck source=src/deploy/test_lib.sh
. "$(cd "$(dirname "$0")" && pwd)/test_lib.sh"
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; [ $# -gt 1 ] && echo "        $2"; FAIL=$((FAIL + 1)); }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WF="${ROOT}/.github/workflows/changelog-reminder.yml"

echo "=== changelog reminder never blocks a merge (#4440) ==="

if [ ! -f "$WF" ]; then
  fail "the workflow exists" "not found at $WF"
  echo ""; echo "=== Results: $PASS passed, $FAIL failed ==="; exit 1
fi
if ! command -v python3 >/dev/null 2>&1 || ! python3 -c 'import yaml' >/dev/null 2>&1; then
  hive_test_skip "python3 + PyYAML unavailable — cannot extract the step script"
  hive_test_report; exit $?
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# THE SCRIPT UNDER TEST IS THE SHIPPED ONE. Extracted from the workflow, never
# restated here — a copy would keep passing after the real step regressed, which
# is the failure mode this repo keeps hitting.
if ! python3 - "$WF" >"${WORK}/step.sh" <<'PY'
import sys, yaml
wf = yaml.safe_load(open(sys.argv[1]))
steps = wf["jobs"]["remind"]["steps"]
for s in steps:
    if s.get("name") == "Remind once":
        sys.stdout.write(s["run"])
        sys.exit(0)
sys.exit(1)
PY
then
  fail "extract the 'Remind once' step from the workflow" "the step name or job id moved"
  echo ""; echo "=== Results: $PASS passed, $FAIL failed ==="; exit 1
fi
pass "the 'Remind once' script was extracted from the workflow (not restated here)"

STUB="${WORK}/bin"; mkdir -p "$STUB"

# A stub gh that reproduces GitHub's fork behaviour: reads succeed, writes 403.
make_gh() { # $1 = post behaviour: forbidden | ok
  cat >"${STUB}/gh" <<SH
#!/usr/bin/env bash
printf 'gh %s\n' "\$*" >>"${WORK}/gh-calls.log"
# A POST is any invocation carrying -F (the body upload).
for a in "\$@"; do
  if [ "\$a" = "-F" ]; then
    if [ "$1" = "forbidden" ]; then
      echo 'gh: Resource not accessible by integration (HTTP 403)' >&2
      exit 1
    fi
    echo '{"id":1}'
    exit 0
  fi
done
# Reads: return no existing comments so the dedupe does not short-circuit.
exit 0
SH
  chmod +x "${STUB}/gh"
}

run_step() { # $1 = IS_FORK value
  : >"${WORK}/gh-calls.log"
  : >"${WORK}/summary.md"
  OUT="$(cd "$WORK" && env PATH="${STUB}:${PATH}" \
    GH_TOKEN=stub PR=4439 REPO=hivecommons/hive IS_FORK="$1" \
    GITHUB_STEP_SUMMARY="${WORK}/summary.md" \
    bash "${WORK}/step.sh" 2>&1)"
  RC=$?
  SUMMARY="$(cat "${WORK}/summary.md" 2>/dev/null)"
  CALLS="$(cat "${WORK}/gh-calls.log" 2>/dev/null)"
}

# --- the bug: a fork PR must not fail ------------------------------------------
echo ""
echo "-- fork PR (GITHUB_TOKEN is read-only: the #4440 case) --"
make_gh forbidden
run_step true
[ "$RC" -eq 0 ] && pass "exits 0 — the check does not block the merge" \
                || fail "exits 0" "rc=$RC, output: $OUT"
printf '%s' "$SUMMARY" | grep -q "Changelog reminder" \
  && pass "the reminder IS delivered, as a job summary" \
  || fail "the reminder is delivered as a job summary" "summary was: '${SUMMARY}'"
printf '%s' "$SUMMARY" | grep -q "CHANGELOG.md" \
  && pass "the summary carries the actual advice" \
  || fail "the summary carries the advice" "got: '${SUMMARY}'"
printf '%s' "$OUT" | grep -q "::notice title=Changelog reminder::" \
  && pass "and as a check annotation" \
  || fail "emits a ::notice:: annotation" "got: $OUT"
printf '%s' "$CALLS" | grep -q '\-F' \
  && fail "a fork PR must not attempt the POST" "it called: $CALLS" \
  || pass "no doomed POST attempted — no misleading 403 in the log"
printf '%s' "$SUMMARY" | grep -q '<!-- changelog-reminder -->' \
  && fail "the HTML dedupe marker must not leak into the summary" "got: '${SUMMARY}'" \
  || pass "the HTML dedupe marker is kept out of the summary"

# --- same-repo PR: still comments ----------------------------------------------
echo ""
echo "-- same-repo PR (token can write) --"
make_gh ok
run_step false
[ "$RC" -eq 0 ] && pass "exits 0" || fail "exits 0" "rc=$RC"
printf '%s' "$CALLS" | grep -q '\-F' \
  && pass "still posts the PR comment where it can" \
  || fail "posts the PR comment on a same-repo PR" "calls: $CALLS"
printf '%s' "$SUMMARY" | grep -q "Changelog reminder" \
  && pass "and still writes the summary, so both surfaces agree" \
  || fail "writes the summary too"

# --- same-repo PR where the POST fails anyway ----------------------------------
# An org policy can narrow GITHUB_TOKEN even on a same-repo PR. The reminder has
# already been delivered by then, so a failed POST must still not fail the job.
echo ""
echo "-- same-repo PR, POST refused by policy --"
make_gh forbidden
run_step false
[ "$RC" -eq 0 ] \
  && pass "a refused POST is tolerated, not fatal" \
  || fail "a refused POST must not fail the job" "rc=$RC, output: $OUT"
printf '%s' "$OUT" | grep -q "could not post" \
  && pass "and says where the reminder went instead" \
  || fail "explains the fallback" "got: $OUT"

# --- the promise, stated as a property -----------------------------------------
echo ""
echo "-- the step never uses errexit, which is what made a 403 fatal --"
grep -qE '^[[:space:]]*set -e|^[[:space:]]*set -[a-z]*e[a-z]*o?' "${WORK}/step.sh" \
  && fail "the step must not run under errexit" "a non-zero gh call would kill it again" \
  || pass "the step does not enable errexit"
grep -q 'set -uo pipefail' "${WORK}/step.sh" \
  && pass "it still keeps nounset and pipefail" \
  || fail "nounset/pipefail retained"

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
