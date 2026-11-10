#!/usr/bin/env bash
# test-mutation-report.sh — exercises the run phase of mutation-report.sh
# against a stubbed gremlins and a throwaway git repository (#17349).
#
# The stub is the point: gremlins is slow and nondeterministic, and the cases
# that matter here (survivors in a changed file, a clean run, a package that
# outlives the time budget) are shapes of its output and its runtime, not
# anything a real mutation run can be asked to produce on demand.
#
# The budget section is the one that must never regress: a run that hits its
# time budget must still write comment.md, and that comment must say which
# packages were not mutated. Before #17602's sibling PRs were triaged, the job
# timeout killed the step first, nothing was written, and the PR kept the
# comment from an earlier head with no sign it was stale.
set -u -o pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${HERE}/mutation-report.sh"
TMP_ROOT="${HERE}/../.test-tmp"
mkdir -p "$TMP_ROOT"
TMP="$(mktemp -d "$TMP_ROOT/mutation-report.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

fail=0
pass() { echo "  ok: $*"; }
bad()  { echo "  FAIL: $*"; fail=1; }

# --- the gremlins stub -------------------------------------------------------
#
# Honours only `unleash ... --output <path>`. STUB_MODE picks the behaviour:
#   lived  one LIVED mutant in the package's changed file and one in a file
#          the PR did not touch (must be filtered out)
#   clean  a KILLED mutant only
#   hang   sleep past any budget without writing output; the script's
#          `timeout` must kill it
# STUB_CALLS records every package directory it was run from.
STUB_DIR="$TMP/stub"
mkdir -p "$STUB_DIR"
cat > "$STUB_DIR/gremlins" <<'STUB'
#!/usr/bin/env bash
set -u
pwd >> "$STUB_CALLS"
out=""
while [ $# -gt 0 ]; do
  case "$1" in --output) out="$2"; shift ;; esac
  shift
done
case "${STUB_MODE:-clean}" in
  hang) sleep 30; exit 0 ;;
  lived)
    cat > "$out" <<'JSON'
{"files":[
  {"file_name":"./changed.go","mutations":[{"line":3,"type":"CONDITIONALS_NEGATION","status":"LIVED"}]},
  {"file_name":"./untouched.go","mutations":[{"line":1,"type":"ARITHMETIC_BASE","status":"LIVED"}]}
]}
JSON
    ;;
  *)
    cat > "$out" <<'JSON'
{"files":[{"file_name":"./changed.go","mutations":[{"line":3,"type":"CONDITIONALS_NEGATION","status":"KILLED"}]}]}
JSON
    ;;
esac
exit 0
STUB
chmod +x "$STUB_DIR/gremlins"

# --- the repository ----------------------------------------------------------
#
# origin/main has two packages; the PR head edits one non-test file in each.
REPO="$TMP/repo"
mkdir -p "$REPO"
git -C "$REPO" init -q -b main
git -C "$REPO" config user.email t@example.com
git -C "$REPO" config user.name t
for p in alpha beta; do
  mkdir -p "$REPO/src/pkg/$p"
  printf 'package %s\n\nvar X = 1\n' "$p" > "$REPO/src/pkg/$p/changed.go"
  printf 'package %s\n' "$p" > "$REPO/src/pkg/$p/untouched.go"
done
git -C "$REPO" add -A && git -C "$REPO" commit -q -m base
git -C "$REPO" update-ref refs/remotes/origin/main HEAD
git -C "$REPO" checkout -q -b pr
for p in alpha beta; do
  printf 'package %s\n\nvar X = 2\n' "$p" > "$REPO/src/pkg/$p/changed.go"
done
printf 'package alpha\n' > "$REPO/src/pkg/alpha/changed_test.go"
git -C "$REPO" add -A && git -C "$REPO" commit -q -m change
HEAD_SHORT=$(git -C "$REPO" rev-parse --short HEAD)

run_phase() {
  # $1 = STUB_MODE, $2 = MUTATION_BUDGET_SECONDS; the report lands in $OUT,
  # the exit status in $RC.
  OUT="$TMP/out-$1-$2"
  rm -rf "$OUT"
  export STUB_CALLS="$TMP/calls-$1-$2"
  : > "$STUB_CALLS"
  (cd "$REPO" && PATH="$STUB_DIR:$PATH" STUB_MODE="$1" \
     MUTATION_BUDGET_SECONDS="$2" MUTATION_OUT_DIR="$OUT" BASE_REF=main \
     bash "$SCRIPT" run >"$OUT.log" 2>&1)
  RC=$?
}

echo "survivors in changed files"
run_phase lived 600
[ "$RC" = 0 ] || bad "run phase exited $RC"
BODY="$OUT/comment.md"
[ -s "$BODY" ] || bad "no comment.md written"
grep -qF '<!-- hive-mutation -->' "$BODY" && pass "comment starts with the upsert marker" || bad "marker missing"
grep -qF "Head \`${HEAD_SHORT}\`" "$BODY" && pass "comment names the head it reports on" || bad "head sha missing: $(cat "$BODY")"
grep -qF '**2 surviving mutant(s)**' "$BODY" && pass "one survivor per changed package counted" || bad "survivor count wrong: $(cat "$BODY")"
grep -qF '| `src/pkg/alpha/changed.go` | 3 | CONDITIONALS_NEGATION |' "$BODY" && pass "survivor re-anchored to a repo-relative path" || bad "alpha survivor row missing"
grep -qF 'untouched.go' "$BODY" && bad "survivor in an untouched file leaked into the report" || pass "survivors in untouched files filtered out"
grep -qF 'Time budget' "$BODY" && bad "complete run flagged as partial" || pass "complete run carries no budget note"
[ "$(wc -l < "$STUB_CALLS" | tr -d ' ')" = 2 ] && pass "gremlins ran once per changed package" || bad "gremlins calls: $(cat "$STUB_CALLS")"

echo "clean run"
run_phase clean 600
BODY="$OUT/comment.md"
grep -qxF 'No surviving mutants in changed files.' "$BODY" && pass "clean run says so without hedging" || bad "clean message wrong: $(cat "$BODY")"
grep -qF 'time budget' "$BODY" && bad "clean run still hedges about the time budget" || pass "clean run is not conflated with a timeout"

echo "time budget exhausted"
run_phase hang 2
[ "$RC" = 0 ] || bad "run phase exited $RC after the budget"
BODY="$OUT/comment.md"
[ -s "$BODY" ] || bad "budget run wrote no comment.md — the stale-comment bug is back"
grep -qF 'Time budget (2s) exhausted' "$BODY" && pass "partial report is labelled as partial" || bad "budget note missing: $(cat "$BODY")"
grep -qF '2 of 2 changed package(s) were not fully mutated' "$BODY" && pass "unfinished count covers both packages" || bad "unfinished count wrong: $(cat "$BODY")"
grep -qF -- '- `src/pkg/alpha` — unfinished' "$BODY" && pass "package killed by the budget is listed as unfinished" || bad "alpha not listed as unfinished"
grep -qF -- '- `src/pkg/beta` — not started' "$BODY" && pass "package after the budget is listed as not started" || bad "beta not listed as not started"
grep -qF 'No surviving mutants reported in the packages that finished' "$BODY" && pass "no-survivor line is scoped to finished packages" || bad "scoped no-survivor line missing"
[ "$(wc -l < "$STUB_CALLS" | tr -d ' ')" = 1 ] && pass "gremlins was not started once the budget was gone" || bad "gremlins calls after budget: $(cat "$STUB_CALLS")"
grep -qF 'hit the time budget in pkg/alpha' "$OUT.log" && pass "log names the package that outlived the budget" || bad "log line missing: $(cat "$OUT.log")"

echo "test files only"
git -C "$REPO" checkout -q -b tests-only origin/main
printf 'package alpha\n' > "$REPO/src/pkg/alpha/new_test.go"
git -C "$REPO" add -A && git -C "$REPO" commit -q -m tests
run_phase clean 600
[ -f "$OUT/comment.md" ] && bad "tests-only diff produced a report" || pass "tests-only diff writes no report"
grep -qF 'No changed non-test Go files' "$OUT.log" && pass "tests-only diff is skipped explicitly" || bad "skip message missing"

if [ "$fail" -ne 0 ]; then
  echo "FAILED"
  exit 1
fi
echo "all tests passed"
