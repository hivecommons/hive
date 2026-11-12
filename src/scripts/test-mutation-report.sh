#!/usr/bin/env bash
# test-mutation-report.sh — exercises mutation-report.sh against a throwaway
# git repository and a stub `gremlins` that replays a canned JSON report, so
# the verdict logic (error / misrouted / lived / uncovered / pass) is pinned without
# compiling or mutating any Go.
set -u -o pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${SCRIPT:-${HERE}/mutation-report.sh}"
TMP_ROOT="${HERE}/../.test-tmp"
mkdir -p "$TMP_ROOT"
TMP="$(mktemp -d "$TMP_ROOT/mutation-report.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

fail=0
pass() { echo "  ok: $*"; }
bad()  { echo "  FAIL: $*"; fail=1; }
out_grep() { grep "$@" <<<"$output"; }

# Stub gremlins: copies $GREMLINS_FIXTURE to the --output path. GREMLINS_EXIT=N
# makes it exit N without writing a report (gremlins returns before report.Do
# on error); GREMLINS_NO_OUTPUT=1 exits 0 without writing one.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/gremlins" <<'EOF'
#!/usr/bin/env bash
out=""
while [ $# -gt 0 ]; do
  case "$1" in --output) out="$2"; shift ;; esac
  shift
done
echo "stub gremlins ran in $(pwd)"
[ -n "${GREMLINS_EXIT:-}" ] && exit "$GREMLINS_EXIT"
[ -n "${GREMLINS_NO_OUTPUT:-}" ] && exit 0
[ -n "$out" ] && cp "$GREMLINS_FIXTURE" "$out"
EOF
chmod +x "$TMP/bin/gremlins"
export PATH="$TMP/bin:$PATH"

# Repo: base commit with src/pkg/x/x.go at 10 lines; HEAD adds lines 11-13.
repo="$TMP/repo"
mkdir -p "$repo/src/pkg/x"
cd "$repo" || exit 1
git init -q
git config user.name 'Test'
git config user.email 'test@example.com'
for i in $(seq 1 10); do echo "// line $i" >> src/pkg/x/x.go; done
printf 'package x\n' > src/pkg/x/doc.go
git add . && git commit -q -m base
git update-ref refs/remotes/origin/main HEAD
printf 'func added() int {\n\tif a > b {\n\t\treturn 1\n\t}\n\treturn 0\n}\n' >> src/pkg/x/x.go
# Unchanged file in the same package: its mutants must never count.
git add . && git commit -q -m change

fixture() {  # fixture <name> <mutations-json-array for x.go> [<for doc.go>]
  local extra=""
  [ -n "${3:-}" ] && extra=",{\"file_name\":\"doc.go\",\"mutations\":$3}"
  printf '{"go_module":"m","files":[{"file_name":"x.go","mutations":%s}%s]}' "$2" "$extra" > "$TMP/$1.json"
}

run_case() {  # run_case <fixture-name>
  export GREMLINS_FIXTURE="$TMP/$1.json"
  export MUTATION_OUT_DIR="$TMP/out-$1"
  rm -rf "$MUTATION_OUT_DIR"
  output=$(BASE_REF=main bash "$SCRIPT" run 2>&1)
  run_rc=$?
}

echo "added-line ranges"
fixture pass-killed '[{"line":12,"column":5,"type":"CONDITIONALS_BOUNDARY","status":"KILLED"},{"line":3,"column":1,"type":"ARITHMETIC_BASE","status":"SKIPPED"}]'
run_case pass-killed
[ "$run_rc" -eq 0 ] && pass "run phase exits 0" || bad "run phase exit $run_rc"
grep -qx $'src/pkg/x/x.go\t11\t16' "$MUTATION_OUT_DIR/added.tsv" \
  && pass "added.tsv records lines 11-16" || bad "added.tsv: $(cat "$MUTATION_OUT_DIR/added.tsv")"
grep -qx $'src/pkg/x/x.go\t11\t16' "$MUTATION_OUT_DIR/gremlins.tsv" \
  && pass "gremlins.tsv matches for a contiguous append" || bad "gremlins.tsv: $(cat "$MUTATION_OUT_DIR/gremlins.tsv")"
[ -s "$MUTATION_OUT_DIR/errors.tsv" ] && bad "errors.tsv not empty: $(cat "$MUTATION_OUT_DIR/errors.tsv")" || pass "errors.tsv empty"
[ "$(cat "$MUTATION_OUT_DIR/verdict")" = pass ] && pass "verdict pass" || bad "verdict $(cat "$MUTATION_OUT_DIR/verdict")"
out_grep -q 'All 1 mutant(s) in changed files were killed' && pass "all-killed summary" || bad "all-killed summary missing"
# The comment names the head it reports on so a stale comment is recognisable (#17604).
grep -qF "Head \`$(git rev-parse --short HEAD)\`" "$MUTATION_OUT_DIR/comment.md" \
  && pass "comment names the head it reports on" || bad "head sha missing: $(head -n 5 "$MUTATION_OUT_DIR/comment.md")"
(cd "$repo" && PR_LABELS= MUTATION_OUT_DIR="$MUTATION_OUT_DIR" bash "$SCRIPT" verdict >/dev/null 2>&1) \
  && pass "verdict phase exits 0 on pass" || bad "verdict phase failed on pass"

echo "nothing mutable in the added lines (#17557 shape)"
fixture nothing '[{"line":3,"column":1,"type":"ARITHMETIC_BASE","status":"SKIPPED"},{"line":7,"column":2,"type":"CONDITIONALS_NEGATION","status":"SKIPPED"}]'
run_case nothing
[ "$(cat "$MUTATION_OUT_DIR/verdict")" = pass ] && pass "skipped-only outside added range is pass" || bad "verdict $(cat "$MUTATION_OUT_DIR/verdict")"
out_grep -q 'No mutants in changed files: the 6 added line(s) contain no operator gremlins can mutate (2 mutant(s) on unchanged lines skipped as expected)' \
  && pass "zero-mutant explanation names added and skipped counts" || bad "zero-mutant explanation missing"
out_grep -q 'SKIPPED' && bad "skipped mutants outside the diff must not be listed" || pass "skipped mutants outside the diff are not listed"

echo "misrouted: SKIPPED inside an added range"
fixture misrouted '[{"line":12,"column":5,"type":"CONDITIONALS_BOUNDARY","status":"SKIPPED"},{"line":12,"column":5,"type":"CONDITIONALS_NEGATION","status":"SKIPPED"},{"line":3,"column":1,"type":"ARITHMETIC_BASE","status":"SKIPPED"}]'
run_case misrouted
[ "$(cat "$MUTATION_OUT_DIR/verdict")" = misrouted ] && pass "verdict misrouted" || bad "verdict $(cat "$MUTATION_OUT_DIR/verdict")"
out_grep -q '2 mutant(s) on lines this PR added were SKIPPED' && pass "misrouted count is 2 (line 3 excluded)" || bad "misrouted summary wrong"
out_grep -q '| `src/pkg/x/x.go` | 12 | CONDITIONALS_BOUNDARY |' && pass "misrouted table re-anchored to repo path" || bad "misrouted table missing"
output=$(cd "$repo" && MUTATION_OUT_DIR="$MUTATION_OUT_DIR" bash "$SCRIPT" verdict 2>&1); rc=$?
[ "$rc" -eq 1 ] && out_grep -q '::error::mutation: a mutant on a line this PR added was SKIPPED' \
  && pass "verdict phase exits 1 with ::error on misrouted" || bad "verdict phase rc=$rc: $output"

echo "lived"
fixture lived '[{"line":12,"column":5,"type":"CONDITIONALS_BOUNDARY","status":"KILLED"},{"line":12,"column":5,"type":"CONDITIONALS_NEGATION","status":"LIVED"},{"line":15,"column":2,"type":"ARITHMETIC_BASE","status":"NOT COVERED"}]'
run_case lived
[ "$(cat "$MUTATION_OUT_DIR/verdict")" = lived ] && pass "lived outranks uncovered" || bad "verdict $(cat "$MUTATION_OUT_DIR/verdict")"
out_grep -q '1 surviving mutant(s)' && pass "survivor count" || bad "survivor summary missing"
out_grep -q '| `src/pkg/x/x.go` | 12 | CONDITIONALS_NEGATION |' && pass "survivor row" || bad "survivor row missing"
out_grep -q '1 mutant(s) in changed files are not reached by any test' && pass "uncovered also listed" || bad "uncovered section missing"
output=$(cd "$repo" && MUTATION_OUT_DIR="$MUTATION_OUT_DIR" bash "$SCRIPT" verdict 2>&1); rc=$?
[ "$rc" -eq 1 ] && out_grep -q '::error::mutation: surviving mutant' \
  && pass "verdict phase exits 1 on lived" || bad "verdict phase rc=$rc: $output"
output=$(cd "$repo" && PR_LABELS='mutation/acknowledged' MUTATION_OUT_DIR="$MUTATION_OUT_DIR" bash "$SCRIPT" verdict 2>&1); rc=$?
[ "$rc" -eq 1 ] && pass "mutation/acknowledged does not waive a survivor" || bad "label waived a survivor (rc=$rc)"

echo "uncovered"
fixture uncovered '[{"line":15,"column":2,"type":"ARITHMETIC_BASE","status":"NOT COVERED"}]' '[{"line":1,"column":1,"type":"ARITHMETIC_BASE","status":"LIVED"}]'
run_case uncovered
[ "$(cat "$MUTATION_OUT_DIR/verdict")" = uncovered ] && pass "verdict uncovered (LIVED in unchanged doc.go ignored)" || bad "verdict $(cat "$MUTATION_OUT_DIR/verdict")"
out_grep -q 'doc.go' && bad "unchanged file leaked into the report" || pass "unchanged file not reported"
output=$(cd "$repo" && PR_LABELS='size/S, mutation/acknowledged' MUTATION_OUT_DIR="$MUTATION_OUT_DIR" bash "$SCRIPT" verdict 2>&1); rc=$?
[ "$rc" -eq 0 ] && out_grep -q 'acknowledged via the mutation/acknowledged label' \
  && pass "acknowledged label silences the warning" || bad "acknowledged rc=$rc: $output"
output=$(cd "$repo" && PR_LABELS='size/S' MUTATION_OUT_DIR="$MUTATION_OUT_DIR" bash "$SCRIPT" verdict 2>&1); rc=$?
[ "$rc" -eq 0 ] && out_grep -q '::warning::mutation: mutant(s) in changed files are not reached' \
  && pass "uncovered warns but exits 0" || bad "uncovered rc=$rc: $output"

echo "error: gremlins exits non-zero and writes no report"
fixture unused '[]'
export GREMLINS_EXIT=1
run_case gremlins-fails
unset GREMLINS_EXIT
[ "$run_rc" -eq 0 ] && pass "run phase still exits 0" || bad "run phase exit $run_rc"
[ "$(cat "$MUTATION_OUT_DIR/verdict")" = error ] && pass "verdict error" || bad "verdict $(cat "$MUTATION_OUT_DIR/verdict")"
grep -qx $'pkg/x\texited 1' "$MUTATION_OUT_DIR/errors.tsv" && pass "errors.tsv names the package and exit code" || bad "errors.tsv: $(cat "$MUTATION_OUT_DIR/errors.tsv")"
out_grep -q '::error::gremlins exited 1 for pkg/x' && pass "run log carries ::error" || bad "run log ::error missing"
out_grep -q 'gremlins did not produce a report for 1 package(s)' && pass "comment reports the failure" || bad "comment failure summary missing"
out_grep -q -- '- `pkg/x`: gremlins exited 1' && pass "comment names the package" || bad "comment package row missing"
out_grep -q 'No mutants in changed files' && bad "false 'nothing to mutate' explanation on a failed run" || pass "no 'nothing to mutate' explanation"
output=$(cd "$repo" && PR_LABELS='mutation/acknowledged' MUTATION_OUT_DIR="$MUTATION_OUT_DIR" bash "$SCRIPT" verdict 2>&1); rc=$?
[ "$rc" -eq 1 ] && out_grep -q '::error::mutation: gremlins failed or wrote no report' \
  && pass "verdict phase exits 1 on error (label does not waive)" || bad "verdict phase rc=$rc: $output"

echo "error: gremlins exits 0 but writes no report"
export GREMLINS_NO_OUTPUT=1
run_case gremlins-silent
unset GREMLINS_NO_OUTPUT
[ "$(cat "$MUTATION_OUT_DIR/verdict")" = error ] && pass "verdict error" || bad "verdict $(cat "$MUTATION_OUT_DIR/verdict")"
grep -qx $'pkg/x\twrote no report' "$MUTATION_OUT_DIR/errors.tsv" && pass "errors.tsv records the missing report" || bad "errors.tsv: $(cat "$MUTATION_OUT_DIR/errors.tsv")"

echo "split hunk: non-contiguous added lines sharing one default-context hunk"
# Base x.go is lines 1-10; insert two lines after 3 and two after 7. -U0 adds
# 4-5 and 10-11, but -U3 merges both into @@ -1,10 +1,14 @@ with 3 leading
# context lines, so gremlins v0.6.0 only honours 4-7.
git checkout -q -b split refs/remotes/origin/main
{ for i in 1 2 3; do echo "// line $i"; done; echo 'var a = 1 + 1'; echo 'var b = 2 + 2'
  for i in 4 5 6 7; do echo "// line $i"; done; echo 'var c = 3 + 3'; echo 'var d = 4 + 4'
  for i in 8 9 10; do echo "// line $i"; done; } > src/pkg/x/x.go
git add . && git commit -q -m split
fixture split '[{"line":4,"column":9,"type":"ARITHMETIC_BASE","status":"KILLED"},{"line":10,"column":9,"type":"ARITHMETIC_BASE","status":"SKIPPED"},{"line":11,"column":9,"type":"ARITHMETIC_BASE","status":"SKIPPED"}]'
run_case split
[ "$(tr '\n' ' ' < "$MUTATION_OUT_DIR/added.tsv")" = $'src/pkg/x/x.go\t4\t5 src/pkg/x/x.go\t10\t11 ' ] \
  && pass "added.tsv records 4-5 and 10-11" || bad "added.tsv: $(cat "$MUTATION_OUT_DIR/added.tsv")"
[ "$(cat "$MUTATION_OUT_DIR/gremlins.tsv")" = $'src/pkg/x/x.go\t4\t7' ] \
  && pass "gremlins.tsv records the honoured approximation 4-7" || bad "gremlins.tsv: $(cat "$MUTATION_OUT_DIR/gremlins.tsv")"
[ "$(cat "$MUTATION_OUT_DIR/verdict")" = pass ] && pass "SKIPPED on added lines gremlins does not honour is not misrouted" || bad "verdict $(cat "$MUTATION_OUT_DIR/verdict")"
[ -s "$MUTATION_OUT_DIR/misrouted-changed.tsv" ] && bad "misrouted: $(cat "$MUTATION_OUT_DIR/misrouted-changed.tsv")" || pass "misrouted list empty"
[ "$(wc -l < "$MUTATION_OUT_DIR/unhonoured-changed.tsv" | tr -d ' ')" = 2 ] && pass "two unhonoured mutants recorded" || bad "unhonoured: $(cat "$MUTATION_OUT_DIR/unhonoured-changed.tsv")"
out_grep -q '2 mutant(s) on added lines were SKIPPED because gremlins v0.6.0 only honours' && pass "hunk-approximation note" || bad "hunk-approximation note missing"
out_grep -q '| `src/pkg/x/x.go` | 10 | ARITHMETIC_BASE |' && pass "unhonoured row listed" || bad "unhonoured row missing"
out_grep -q 'All 1 mutant(s) in changed files were killed' && pass "killed summary still shown" || bad "killed summary missing"
(cd "$repo" && MUTATION_OUT_DIR="$MUTATION_OUT_DIR" bash "$SCRIPT" verdict >/dev/null 2>&1) \
  && pass "verdict phase exits 0" || bad "verdict phase failed on split hunk"
fixture split-misrouted '[{"line":4,"column":9,"type":"ARITHMETIC_BASE","status":"SKIPPED"},{"line":10,"column":9,"type":"ARITHMETIC_BASE","status":"SKIPPED"}]'
run_case split-misrouted
[ "$(cat "$MUTATION_OUT_DIR/verdict")" = misrouted ] && pass "SKIPPED inside the honoured range is still misrouted" || bad "verdict $(cat "$MUTATION_OUT_DIR/verdict")"
out_grep -q '1 mutant(s) on lines this PR added were SKIPPED' && pass "misrouted count excludes the unhonoured line" || bad "misrouted summary wrong"

echo "wall-clock budget"
export MUTATION_BUDGET_SECONDS=0
run_case pass-killed
[ "$run_rc" -eq 0 ] && pass "exhausted budget still exits 0" || bad "exhausted budget exit $run_rc"
grep -qx $'pkg/x\tskipped (not started)' "$MUTATION_OUT_DIR/budget.tsv" \
  && pass "package recorded as skipped" || bad "budget.tsv: $(cat "$MUTATION_OUT_DIR/budget.tsv")"
out_grep -q 'mutation budget ran out' && out_grep -q 'pkg/x`: skipped (not started)' \
  && pass "report lists the skipped package" || bad "skipped package not in report"
[ -s "$MUTATION_OUT_DIR/errors.tsv" ] && bad "budget skip recorded as error" || pass "skip is not an error"
export MUTATION_BUDGET_SECONDS=60 GREMLINS_EXIT=124
run_case pass-killed
[ "$run_rc" -eq 0 ] && pass "timeout exit 124 still exits 0" || bad "exit 124 run rc $run_rc"
grep -qx $'pkg/x\tcut short (partial, no report)' "$MUTATION_OUT_DIR/budget.tsv" \
  && pass "package recorded as cut short" || bad "budget.tsv: $(cat "$MUTATION_OUT_DIR/budget.tsv")"
[ -s "$MUTATION_OUT_DIR/errors.tsv" ] && bad "cut-short recorded as error" || pass "cut-short is not an error"
unset MUTATION_BUDGET_SECONDS GREMLINS_EXIT

echo "no changed Go files"
git checkout -q -b docs-only refs/remotes/origin/main
echo x > README.md && git add README.md && git commit -q -m docs
export MUTATION_OUT_DIR="$TMP/out-docs"
output=$(BASE_REF=main bash "$SCRIPT" run 2>&1)
[ "$(cat "$MUTATION_OUT_DIR/verdict")" = pass ] && pass "docs-only PR writes verdict pass" || bad "docs-only verdict: $(cat "$MUTATION_OUT_DIR/verdict" 2>&1)"
output=$(MUTATION_OUT_DIR="$TMP/missing" bash "$SCRIPT" verdict 2>&1); rc=$?
[ "$rc" -eq 1 ] && out_grep -q '::error::mutation: no verdict at' \
  && pass "missing verdict file fails the verdict phase" || bad "missing verdict rc=$rc: $output"

echo "post phase (#17863)"
# Stub gh: lists $GH_EXISTING_ID (if set) as the bot's marker comment and logs writes.
cat > "$TMP/bin/gh" <<'GHSTUB'
#!/usr/bin/env bash
case "$*" in
  *-X*) printf '%s\n' "$*" >> "$GH_WRITE_LOG"; exit 0 ;;
  *'/comments'*) if [ -n "${GH_EXISTING_ID:-}" ]; then printf '%s\n' "$GH_EXISTING_ID"; fi; exit 0 ;;
  *) echo "unexpected gh call: $*" >&2; exit 2 ;;
esac
GHSTUB
chmod +x "$TMP/bin/gh"
run_post() {  # run_post <out-dir> [existing-id]
  GH_WRITE_LOG="$TMP/gh-writes.log"; : > "$GH_WRITE_LOG"
  SUMMARY="$TMP/summary.md"; : > "$SUMMARY"
  output=$(cd "$repo" && GH_WRITE_LOG="$GH_WRITE_LOG" GH_EXISTING_ID="${2:-}" GITHUB_STEP_SUMMARY="$SUMMARY" \
    MUTATION_OUT_DIR="$1" PR_NUMBER=42 GITHUB_REPOSITORY=example/hive bash "$SCRIPT" post 2>&1)
  writes=$(cat "$GH_WRITE_LOG")
}
mkdir -p "$TMP/out-pass" "$TMP/out-lived"
printf '<!-- hive-mutation -->\n### Mutation testing\n\nAll 3 mutant(s) in changed files were killed.\n' > "$TMP/out-pass/comment.md"
echo pass > "$TMP/out-pass/verdict"
printf '<!-- hive-mutation -->\n### Mutation testing\n\ngremlins found **1 surviving mutant(s)**\n' > "$TMP/out-lived/comment.md"
echo lived > "$TMP/out-lived/verdict"
run_post "$TMP/out-pass"
[ -z "$writes" ] && pass "pass verdict with no earlier comment posts nothing" || bad "pass posted: $writes"
out_grep -q 'verdict pass and no earlier comment; not posting' && pass "skip is logged" || bad "skip not logged: $output"
grep -q 'All 3 mutant(s) in changed files were killed' "$SUMMARY" && pass "clean report still lands in the step summary" || bad "summary missing report"
run_post "$TMP/out-lived"
grep -q "^api -X POST repos/example/hive/issues/42/comments" <<<"$writes" && pass "lived verdict posts a new comment" || bad "lived did not post: $writes"
run_post "$TMP/out-lived" 99
grep -q "^api -X PATCH repos/example/hive/issues/comments/99" <<<"$writes" && pass "lived verdict edits the existing comment" || bad "lived did not edit: $writes"
grep -q 'POST' <<<"$writes" && bad "lived posted a duplicate" || pass "no duplicate comment"
run_post "$TMP/out-pass" 99
grep -q "^api -X PATCH repos/example/hive/issues/comments/99 -f body=<!-- hive-mutation -->" <<<"$writes" && pass "pass after a finding edits the existing comment, keeping the marker" || bad "pass did not edit: $writes"
grep -q 'Resolved: no surviving or uncovered mutants' <<<"$writes" && pass "edited body is the one-line resolved note" || bad "resolved note missing: $writes"
grep -q 'All 3 mutant' <<<"$writes" && bad "full report was written to the comment" || pass "full report stays out of the comment"

if [ "$fail" -ne 0 ]; then
  echo "test-mutation-report.sh: FAILED"
  exit 1
fi
echo "test-mutation-report.sh: all checks passed"
