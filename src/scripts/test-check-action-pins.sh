#!/usr/bin/env bash
# Exercises check-action-pins.sh with a hermetic `gh` test double: no network
# and no real workflow files are used. Each case writes a synthetic workflow
# directory, runs the check against it with a fake `gh` first on PATH that
# records every API call and can be told which SHA to report missing, then
# asserts the exit status and the verdict printed.
#
# The check's job is to fail closed. In CI it only ever sees this repository's
# own (correct) workflows, so every failure branch (NOT-PINNED, MISSING-SHA,
# EMPTY-EXPRESSION, BRACKETED-EMAIL), the numeric-noreply exclusion and the
# reusable-workflow owner/repo extraction would otherwise run for the first
# time on the day they matter. This test runs them on every change to the
# script.
#
# Usage: bash src/scripts/test-check-action-pins.sh   (from the repository root)
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="${ROOT}/src/scripts/check-action-pins.sh"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/check-action-pins-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

failures=0
pass() { printf '  ok: %s\n' "$*"; }
bad() {
  printf '  FAIL: %s\n' "$*"
  failures=$((failures + 1))
}
# Here-strings, not pipes, so a grep that stops reading early cannot turn
# pipefail into a false FAIL.
assert_contains() {
  local haystack="$1" needle="$2" msg="$3"
  if grep -qF -- "$needle" <<<"$haystack"; then pass "$msg"; else bad "$msg (missing: $needle)"; fi
}
assert_not_contains() {
  local haystack="$1" needle="$2" msg="$3"
  if grep -qF -- "$needle" <<<"$haystack"; then bad "$msg (unexpected: $needle)"; else pass "$msg"; fi
}
assert_exit() {
  local want="$1" got="$2" msg="$3"
  if [ "$want" = "$got" ]; then pass "$msg (exit $got)"; else bad "$msg (want exit $want, got $got)"; fi
}
assert_eq() {
  local want="$1" got="$2" msg="$3"
  if [ "$want" = "$got" ]; then pass "$msg ($got)"; else bad "$msg (want $want, got $got)"; fi
}

# Fake `gh`: only `gh api repos/<owner>/<repo>/commits/<sha> --jq .sha` is
# ever called by the check. Every call is appended to $FAKE_GH_LOG; a call
# whose SHA equals $FAKE_GH_MISSING fails like the real API does for a commit
# that does not exist in the repository.
BIN="$TMP/bin"
mkdir -p "$BIN"
cat >"$BIN/gh" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${FAKE_GH_LOG:?}"
case "$2" in
  *"/commits/${FAKE_GH_MISSING:-no-such-sha}") exit 1 ;;
esac
printf '%s\n' "${2##*/}"
exit 0
SH
chmod +x "$BIN/gh"

# A PATH with every external tool the check uses except gh, so the "no token,
# stay usable offline" branch can be exercised without uninstalling anything.
NOGH="$TMP/nogh"
mkdir -p "$NOGH"
for tool in grep sed sort cut head; do
  ln -s "$(command -v "$tool")" "$NOGH/$tool"
done

SHA_A=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
SHA_B=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
SHA_C=cccccccccccccccccccccccccccccccccccccccc

# workflow DIR NAME <<heredoc: writes one workflow file into a case directory.
workflow() { mkdir -p "$1" && cat >"$1/$2"; }

# run_check CASE [env VAR=VALUE ...] -- ARGS...
# Runs the check with the fake gh first on PATH. Leaves stdout+stderr in
# $TMP/<case>.out, the exit status in RC, the output in OUT and the fake gh's
# call log in GH_CALLS.
run_check() {
  local name="$1"; shift
  local envs=()
  while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do envs+=("$1"); shift; done
  shift
  : >"$TMP/$name.gh"
  (
    env FAKE_GH_LOG="$TMP/$name.gh" "${envs[@]}" PATH="$BIN:$PATH" bash "$SCRIPT" "$@"
  ) >"$TMP/$name.out" 2>&1
  RC=$?
  OUT="$(cat "$TMP/$name.out")"
  GH_CALLS="$(cat "$TMP/$name.gh")"
}

echo "case: every action pinned to a resolvable SHA"
D="$TMP/green"
workflow "$D" ci.yml <<EOF
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@$SHA_A # v4.2.2
      - uses: actions/setup-go@$SHA_B # v5.0.0
        with:
          go-version: '1.26'
      - uses: ./.github/actions/local-thing
      - run: echo "\${{ github.sha }}"
EOF
run_check green -- "$D"
assert_exit 0 "$RC" "pinned workflow passes"
assert_contains "$OUT" "action pin check OK" "prints the pass verdict"
assert_not_contains "$OUT" "FAILED" "no failure banner"
assert_contains "$GH_CALLS" "api repos/actions/checkout/commits/$SHA_A" "checkout SHA resolved against its repository"
assert_contains "$GH_CALLS" "api repos/actions/setup-go/commits/$SHA_B" "setup-go SHA resolved against its repository"
assert_not_contains "$GH_CALLS" "local-thing" "a local ./ action is not resolved against the API"
assert_eq 2 "$(grep -c . <<<"$GH_CALLS")" "one API call per unique pin"

echo "case: a tag instead of a SHA"
D="$TMP/tag"
workflow "$D" ci.yml <<EOF
jobs:
  build:
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@$SHA_B
EOF
run_check tag -- "$D"
assert_exit 1 "$RC" "a moving ref fails"
assert_contains "$OUT" "NOT-PINNED" "reports NOT-PINNED"
assert_contains "$OUT" "actions/checkout@v4 (use a 40-hex commit SHA)" "names the offending ref"
assert_contains "$OUT" "action pin check FAILED" "prints the failure banner"
assert_contains "$GH_CALLS" "api repos/actions/setup-go/commits/$SHA_B" "the correctly pinned action is still resolved"
assert_not_contains "$GH_CALLS" "checkout" "the unpinned ref is not sent to the API"

echo "case: a branch name and a short SHA are both moving refs"
D="$TMP/short"
workflow "$D" ci.yml <<EOF
jobs:
  build:
    steps:
      - uses: actions/checkout@main
      - uses: actions/setup-go@${SHA_B:0:39}
      - uses: actions/cache@${SHA_C^^}
EOF
run_check short -- "$D"
assert_exit 1 "$RC" "short and non-lowercase SHAs fail"
assert_contains "$OUT" "NOT-PINNED $D/ci.yml: actions/checkout@main" "branch ref is NOT-PINNED"
assert_contains "$OUT" "actions/setup-go@${SHA_B:0:39} (use a 40-hex commit SHA)" "39-hex ref is NOT-PINNED"
assert_contains "$OUT" "actions/cache@${SHA_C^^} (use a 40-hex commit SHA)" "upper-case hex is NOT-PINNED"
assert_eq 0 "$(grep -c . <<<"$GH_CALLS")" "nothing is resolved when no pin has the right shape"

echo "case: a well-formed SHA that does not exist in the repository"
D="$TMP/missing"
workflow "$D" release.yml <<EOF
jobs:
  release:
    steps:
      - uses: actions/checkout@$SHA_A
      - uses: anchore/sbom-action@$SHA_C # v0.20.9
EOF
run_check missing FAKE_GH_MISSING="$SHA_C" -- "$D"
assert_exit 1 "$RC" "a fabricated SHA fails"
assert_contains "$OUT" "MISSING-SHA anchore/sbom-action@$SHA_C does not exist in that repository" "names the repository and SHA"
assert_contains "$OUT" "action pin check FAILED" "prints the failure banner"
assert_not_contains "$OUT" "NOT-PINNED" "shape check does not also fire on a well-formed SHA"

echo "case: every missing SHA is reported, not just the first"
D="$TMP/missing-two"
workflow "$D" release.yml <<EOF
jobs:
  release:
    steps:
      - uses: anchore/sbom-action@$SHA_C
      - uses: actions/checkout@$SHA_A
      - uses: actions/setup-go@$SHA_B
EOF
FAKE_SECOND_MISSING="$SHA_A"
cat >"$BIN/gh" <<SH
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"\${FAKE_GH_LOG:?}"
case "\$2" in
  */commits/\${FAKE_GH_MISSING:-no-such-sha}|*/commits/$FAKE_SECOND_MISSING) exit 1 ;;
esac
printf '%s\n' "\${2##*/}"
exit 0
SH
run_check missing-two FAKE_GH_MISSING="$SHA_C" -- "$D"
assert_exit 1 "$RC" "two fabricated SHAs fail"
assert_contains "$OUT" "MISSING-SHA anchore/sbom-action@$SHA_C" "first missing SHA reported"
assert_contains "$OUT" "MISSING-SHA actions/checkout@$SHA_A" "second missing SHA reported too"
assert_contains "$GH_CALLS" "api repos/actions/setup-go/commits/$SHA_B" "resolution continues past a missing SHA"
assert_eq 3 "$(grep -c . <<<"$GH_CALLS")" "every unique pin is still resolved"
# Restore the single-miss double for the remaining cases.
cat >"$BIN/gh" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${FAKE_GH_LOG:?}"
case "$2" in
  *"/commits/${FAKE_GH_MISSING:-no-such-sha}") exit 1 ;;
esac
printf '%s\n' "${2##*/}"
exit 0
SH

echo "case: a directory with no third-party actions passes with gh present"
D="$TMP/nopins"
workflow "$D" ci.yml <<EOF
jobs:
  build:
    steps:
      - uses: ./.github/actions/local-thing
      - run: echo "\${{ github.sha }}"
EOF
run_check nopins -- "$D"
assert_exit 0 "$RC" "nothing to resolve is a pass, not a silent FAILED"
assert_contains "$OUT" "action pin check OK" "prints the pass verdict"
assert_eq 0 "$(grep -c . <<<"$GH_CALLS")" "no API calls when there is nothing to resolve"

echo "case: reusable workflow pins resolve against owner/repo, not the file path"
D="$TMP/reusable"
workflow "$D" caller.yml <<EOF
jobs:
  verify:
    uses: hivecommons/hive/.github/workflows/reusable-verify.yml@$SHA_A
  build:
    steps:
      - uses: actions/checkout@$SHA_B
EOF
run_check reusable -- "$D"
assert_exit 0 "$RC" "reusable workflow pin passes"
assert_contains "$GH_CALLS" "api repos/hivecommons/hive/commits/$SHA_A" "owner/repo taken from the first two path segments"
assert_not_contains "$GH_CALLS" ".github/workflows" "the workflow path is not sent as a repository"
run_check reusable-missing FAKE_GH_MISSING="$SHA_A" -- "$D"
assert_exit 1 "$RC" "a reusable workflow SHA that does not exist fails"
assert_contains "$OUT" "MISSING-SHA hivecommons/hive@$SHA_A" "reports the reusable workflow's repository"

echo "case: the same pin in two files is resolved once"
D="$TMP/dedupe"
workflow "$D" a.yml <<EOF
jobs:
  a:
    steps:
      - uses: actions/checkout@$SHA_A
EOF
workflow "$D" b.yml <<EOF
jobs:
  b:
    steps:
      - uses: actions/checkout@$SHA_A
      -   uses: actions/checkout@$SHA_A
EOF
run_check dedupe -- "$D"
assert_exit 0 "$RC" "duplicate pins pass"
assert_eq 1 "$(grep -c . <<<"$GH_CALLS")" "one API call for three identical uses"

echo "case: commented-out uses lines are ignored"
D="$TMP/comment"
workflow "$D" ci.yml <<EOF
jobs:
  build:
    steps:
      # - uses: actions/checkout@v4
      #uses: actions/setup-go@main
      - uses: actions/checkout@$SHA_A
EOF
run_check comment -- "$D"
assert_exit 0 "$RC" "comments do not count as pins"
assert_not_contains "$OUT" "NOT-PINNED" "no NOT-PINNED for commented refs"
assert_eq 1 "$(grep -c . <<<"$GH_CALLS")" "only the live pin is resolved"

echo "case: gh absent means shape check only"
D="$TMP/nogh-dir"
workflow "$D" ci.yml <<EOF
jobs:
  build:
    steps:
      - uses: actions/checkout@$SHA_A
EOF
: >"$TMP/nogh.gh"
OUT="$( env FAKE_GH_LOG="$TMP/nogh.gh" PATH="$NOGH" "$(command -v bash)" "$SCRIPT" "$D" 2>&1 )"
RC=$?
assert_exit 0 "$RC" "a pinned workflow passes without gh"
assert_contains "$OUT" "action pin check OK" "prints the pass verdict offline"
assert_eq 0 "$(grep -c . <"$TMP/nogh.gh")" "no API calls attempted without gh"
workflow "$D" bad.yml <<EOF
jobs:
  build:
    steps:
      - uses: actions/checkout@v4
EOF
OUT="$( env FAKE_GH_LOG="$TMP/nogh.gh" PATH="$NOGH" "$(command -v bash)" "$SCRIPT" "$D" 2>&1 )"
RC=$?
assert_exit 1 "$RC" "the shape check still fails without gh"
assert_contains "$OUT" "NOT-PINNED" "NOT-PINNED reported offline"

echo "case: an empty expression delimiter pair"
D="$TMP/emptyexpr"
workflow "$D" ci.yml <<EOF
jobs:
  build:
    steps:
      - uses: actions/checkout@$SHA_A
      - run: |
          # write the literal \${{ }} here and the parser dies
          echo hello
EOF
run_check emptyexpr -- "$D"
assert_exit 1 "$RC" "an empty \${{ }} fails even inside a shell comment"
assert_contains "$OUT" "EMPTY-EXPRESSION" "reports EMPTY-EXPRESSION"
assert_contains "$OUT" "$D/ci.yml:6:" "names the file and line"
assert_contains "$OUT" "action pin check FAILED" "prints the failure banner"
D="$TMP/emptyexpr-space"
workflow "$D" ci.yml <<EOF
jobs:
  build:
    steps:
      - run: echo "\${{   }}"
EOF
run_check emptyexpr-space -- "$D"
assert_exit 1 "$RC" "whitespace inside the delimiters is still empty"
D="$TMP/fullexpr"
workflow "$D" ci.yml <<EOF
jobs:
  build:
    steps:
      - run: echo "\${{ github.sha }}" "\${{github.ref}}"
EOF
run_check fullexpr -- "$D"
assert_exit 0 "$RC" "a populated expression passes"
assert_not_contains "$OUT" "EMPTY-EXPRESSION" "no EMPTY-EXPRESSION for a real expression"

echo "case: a bracketed git user.email"
D="$TMP/email"
workflow "$D" release.yml <<EOF
jobs:
  release:
    steps:
      - run: |
          git config user.name "hive-release[bot]"
          git config user.email "hive-release[bot]@users.noreply.github.com"
EOF
run_check email -- "$D"
assert_exit 1 "$RC" "a bracketed local part fails"
assert_contains "$OUT" "BRACKETED-EMAIL" "reports BRACKETED-EMAIL"
assert_contains "$OUT" "$D/release.yml:6:" "names the file and line of the email, not the name"
assert_contains "$OUT" "action pin check FAILED" "prints the failure banner"

echo "case: the numeric GitHub noreply form is allowed"
D="$TMP/noreply"
workflow "$D" guard.yml <<EOF
jobs:
  guard:
    steps:
      - run: |
          git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
          git config   user.email   "hive-release@hive.kubestellar.io"
EOF
run_check noreply -- "$D"
assert_exit 0 "$RC" "numeric noreply and bracket-free addresses pass"
assert_not_contains "$OUT" "BRACKETED-EMAIL" "no BRACKETED-EMAIL for the numeric form"
assert_contains "$OUT" "action pin check OK" "prints the pass verdict"

echo "case: every class of failure is reported in one run"
D="$TMP/all"
workflow "$D" ci.yml <<EOF
jobs:
  build:
    steps:
      - uses: actions/checkout@v4
      - uses: anchore/sbom-action@$SHA_C
      - run: echo "\${{ }}"
      - run: git config user.email "x[bot]@users.noreply.github.com"
EOF
run_check all FAKE_GH_MISSING="$SHA_C" -- "$D"
assert_exit 1 "$RC" "fails"
assert_contains "$OUT" "NOT-PINNED" "NOT-PINNED reported alongside the others"
assert_contains "$OUT" "MISSING-SHA anchore/sbom-action@$SHA_C" "MISSING-SHA reported alongside the others"
assert_contains "$OUT" "EMPTY-EXPRESSION" "EMPTY-EXPRESSION reported alongside the others"
assert_contains "$OUT" "BRACKETED-EMAIL" "BRACKETED-EMAIL reported alongside the others"
assert_eq 1 "$(grep -c 'action pin check FAILED' <<<"$OUT")" "one failure banner"

echo "case: the workflow directory defaults to .github/workflows"
D="$TMP/default-cwd"
workflow "$D/.github/workflows" ci.yml <<EOF
jobs:
  build:
    steps:
      - uses: actions/checkout@v4
EOF
: >"$TMP/default.gh"
OUT="$( cd "$D" && env FAKE_GH_LOG="$TMP/default.gh" PATH="$BIN:$PATH" bash "$SCRIPT" 2>&1 )"
RC=$?
assert_exit 1 "$RC" "the default directory is scanned when no argument is given"
assert_contains "$OUT" "NOT-PINNED .github/workflows/ci.yml: actions/checkout@v4" "reports the path relative to the default directory"

echo
if [ "$failures" -eq 0 ]; then
  echo "check-action-pins tests: all passed"
  exit 0
fi
echo "check-action-pins tests: $failures failure(s)"
exit 1
