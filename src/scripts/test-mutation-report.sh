#!/usr/bin/env bash
# test-mutation-report.sh — exercises mutation-report.sh against a throwaway
# git repository and a stub `gremlins` that replays a canned JSON report, so
# the verdict logic (misrouted / lived / uncovered / pass) is pinned without
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

# Stub gremlins: copies $GREMLINS_FIXTURE to the --output path.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/gremlins" <<'EOF'
#!/usr/bin/env bash
out=""
while [ $# -gt 0 ]; do
  case "$1" in --output) out="$2"; shift ;; esac
  shift
done
[ -n "$out" ] && cp "$GREMLINS_FIXTURE" "$out"
echo "stub gremlins ran in $(pwd)"
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
[ "$(cat "$MUTATION_OUT_DIR/verdict")" = pass ] && pass "verdict pass" || bad "verdict $(cat "$MUTATION_OUT_DIR/verdict")"
out_grep -q 'All 1 mutant(s) in changed files were killed' && pass "all-killed summary" || bad "all-killed summary missing"
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

echo "no changed Go files"
git checkout -q -b docs-only refs/remotes/origin/main
echo x > README.md && git add README.md && git commit -q -m docs
export MUTATION_OUT_DIR="$TMP/out-docs"
output=$(BASE_REF=main bash "$SCRIPT" run 2>&1)
[ "$(cat "$MUTATION_OUT_DIR/verdict")" = pass ] && pass "docs-only PR writes verdict pass" || bad "docs-only verdict: $(cat "$MUTATION_OUT_DIR/verdict" 2>&1)"
output=$(MUTATION_OUT_DIR="$TMP/missing" bash "$SCRIPT" verdict 2>&1); rc=$?
[ "$rc" -eq 0 ] && pass "missing verdict file defaults to pass" || bad "missing verdict rc=$rc"

if [ "$fail" -ne 0 ]; then
  echo "test-mutation-report.sh: FAILED"
  exit 1
fi
echo "test-mutation-report.sh: all checks passed"
