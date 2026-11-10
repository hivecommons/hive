#!/usr/bin/env bash
# test-mutation-report.sh — exercises the in-script time budget of
# mutation-report.sh (#17605) with a fake `gremlins` on PATH.
set -u -o pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPORT="${REPORT:-${HERE}/mutation-report.sh}"
TMP_ROOT="${HERE}/../.test-tmp"
mkdir -p "$TMP_ROOT"
TMP="$(mktemp -d "$TMP_ROOT/mutation-report.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

fail=0
pass() { echo "  ok: $*"; }
bad()  { echo "  FAIL: $*"; fail=1; }

repo="$TMP/repo"
mkdir -p "$repo/src/foo" "$TMP/bin"
cd "$repo" || exit 1
git init -q
git config user.name 'Test Author'
git config user.email 'test@example.com'
echo base > README.md
git add README.md
git commit -q -m base
git update-ref refs/remotes/origin/base HEAD
printf 'package foo\n\nfunc Foo() int { return 1 }\n' > src/foo/foo.go
git add src/foo/foo.go
git commit -q -m 'add foo'

# $1 = sleep seconds, $2 = whether the fake writes a report
make_gremlins() {
  cat > "$TMP/bin/gremlins" <<EOF
#!/usr/bin/env bash
sleep $1
if [ "$2" = write ]; then
  while [ \$# -gt 0 ]; do
    [ "\$1" = --output ] && echo '{"files":[]}' > "\$2"
    shift
  done
fi
EOF
  chmod +x "$TMP/bin/gremlins"
}

run_report() { # $1 = budget; sets rc, elapsed
  rm -rf "$TMP/out"
  local t0=$SECONDS
  PATH="$TMP/bin:$PATH" BASE_REF=base MUTATION_BUDGET_SECONDS="$1" \
    MUTATION_OUT_DIR="$TMP/out" bash "$REPORT" run >"$TMP/run.log" 2>&1
  rc=$?
  elapsed=$((SECONDS - t0))
}

echo "case 1: gremlins outlives the budget"
make_gremlins 20 nowrite
run_report 1
[ "$rc" -eq 0 ] && pass "exit 0" || bad "exit code $rc"
[ "$elapsed" -lt 10 ] && pass "cut off after ${elapsed}s" || bad "ran ${elapsed}s; timeout did not fire"
grep -q 'Time budget (1s) exhausted' "$TMP/out/comment.md" 2>/dev/null \
  && pass "comment mentions budget" || bad "comment lacks budget line"
grep -q 'Advisory only' "$TMP/out/comment.md" 2>/dev/null \
  && pass "advisory footer kept" || bad "advisory footer missing"

echo "case 2: gremlins finishes within the budget"
make_gremlins 0 write
run_report 60
[ "$rc" -eq 0 ] && pass "exit 0" || bad "exit code $rc"
[ -s "$TMP/out/comment.md" ] && pass "comment written" || bad "comment missing"
if grep -q 'Time budget' "$TMP/out/comment.md" 2>/dev/null; then
  bad "unexpected budget line"
else
  pass "no budget line"
fi

[ "$fail" -eq 0 ] && echo "all passed" || { echo "FAILED"; exit 1; }
