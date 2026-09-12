#!/usr/bin/env bash
# test-check-dco-trailers.sh — exercises check-dco-trailers.sh against a small
# throwaway git repository with good, missing-trailer, and mismatched-trailer
# commits.
set -u -o pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECKER="${CHECKER:-${HERE}/check-dco-trailers.sh}"
TMP_ROOT="${HERE}/../.test-tmp"
mkdir -p "$TMP_ROOT"
TMP="$(mktemp -d "$TMP_ROOT/dco-trailers.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

fail=0
pass() { echo "  ok: $*"; }
bad()  { echo "  FAIL: $*"; fail=1; }

repo="$TMP/repo"
mkdir -p "$repo"
cd "$repo" || exit 1
git init -q
git config user.name 'Good Author'
git config user.email 'good@example.com'

echo good > file.txt
git add file.txt
git commit -q -m $'good commit\n\nSigned-off-by: Good Author <good@example.com>'
good_sha=$(git rev-parse HEAD)

git config user.name 'Missing Author'
git config user.email 'missing@example.com'
echo missing >> file.txt
git add file.txt
git commit -q -m 'missing signoff commit'
missing_sha=$(git rev-parse HEAD)

git config user.name 'Mismatch Author'
git config user.email 'mismatch@example.com'
echo mismatch >> file.txt
git add file.txt
git commit -q -m $'mismatched signoff commit\n\nSigned-off-by: Other Person <other@example.com>'
mismatch_sha=$(git rev-parse HEAD)

git config user.name 'Noreply Author'
git config user.email 'personal@example.com'
echo noreply-good >> file.txt
git add file.txt
git commit -q -m $'matching GitHub noreply signoff commit\n\nSigned-off-by: Noreply Author <12345+noreply-author@users.noreply.github.com>'
noreply_good_sha=$(git rev-parse HEAD)

git config user.name 'Different Noreply Author'
git config user.email 'different@example.com'
echo noreply-bad >> file.txt
git add file.txt
git commit -q -m $'different GitHub noreply signoff commit\n\nSigned-off-by: Someone Else <other-account@users.noreply.github.com>'
noreply_bad_sha=$(git rev-parse HEAD)

git config user.name 'Second Address Author'
git config user.email 'andy@clubanderson.com'
echo second-address >> file.txt
git add file.txt
git commit -q -m $'arbitrary second address signoff commit\n\nSigned-off-by: Second Address Author <clubanderson@gmail.com>'
second_address_sha=$(git rev-parse HEAD)

# Sign-off in an EARLIER trailer block than a trailing Co-authored-by:
# `git interpret-trailers --parse` sees only the last block, but the pre-merge
# DCO app scans every line, so this must pass (#6605, v5 639f49e9).
git config user.name 'Multi Block Author'
git config user.email 'multiblock@example.com'
echo multi-block >> file.txt
git add file.txt
git commit -q -m $'multi trailer block commit\n\nSigned-off-by: Multi Block Author <multiblock@example.com>\n\nCo-authored-by: Copilot <223556219+Copilot@users.noreply.github.com>'
multi_block_sha=$(git rev-parse HEAD)

set +e
output=$(DCO_AUTHOR_LOGIN_MAP="${noreply_good_sha}=noreply-author ${noreply_bad_sha}=different-author ${second_address_sha}=clubanderson" bash "$CHECKER" 10 HEAD 2>&1)
rc=$?
set -e

if [ "$rc" -ne 1 ]; then
  bad "checker exits 1 when four commits fail (got ${rc})"
  echo "$output" | sed 's/^/      | /'
else
  pass "checker exits 1 when bad commits are present"
fi

fail_lines=$(printf '%s\n' "$output" | grep -c '^FAIL ' || true)
if [ "$fail_lines" -eq 4 ]; then
  pass "exactly four failing commits are reported"
else
  bad "expected exactly four FAIL lines, got ${fail_lines}"
  echo "$output" | sed 's/^/      | /'
fi

if printf '%s\n' "$output" | grep -q "^FAIL ${missing_sha} missing-signoff"; then
  pass "missing sign-off commit is reported"
else
  bad "missing sign-off commit ${missing_sha} was not reported"
fi

if printf '%s\n' "$output" | grep -q "^FAIL ${mismatch_sha} mismatched-signoff"; then
  pass "mismatched sign-off commit is reported"
else
  bad "mismatched sign-off commit ${mismatch_sha} was not reported"
fi

if printf '%s\n' "$output" | grep -q "^FAIL ${noreply_good_sha}"; then
  bad "GitHub noreply sign-off for the authoring account should not be reported"
else
  pass "GitHub noreply sign-off for the authoring account is accepted"
fi

if printf '%s\n' "$output" | grep -q "^FAIL ${noreply_bad_sha} mismatched-signoff"; then
  pass "GitHub noreply sign-off for a different account is rejected"
else
  bad "GitHub noreply sign-off for a different account ${noreply_bad_sha} was not reported"
fi

if printf '%s\n' "$output" | grep -q "^FAIL ${second_address_sha} mismatched-signoff"; then
  pass "an arbitrary second personal address is rejected"
else
  bad "arbitrary second personal address ${second_address_sha} was not reported"
fi

if printf '%s\n' "$output" | grep -q "^FAIL ${good_sha}"; then
  bad "good commit ${good_sha} should not be reported"
else
  pass "good commit is not reported"
fi

if printf '%s\n' "$output" | grep -q "^FAIL ${multi_block_sha}"; then
  bad "sign-off in an earlier trailer block ${multi_block_sha} should not be reported"
else
  pass "sign-off separated from a trailing Co-authored-by block is accepted"
fi

# --- DCO_WAIVED_COMMITS (hivecommons/hive#6576) -------------------------------
#
# A protected branch cannot be repaired: policy forbids rewriting another
# contributor's commit or signing off on their behalf, so a commit already
# merged without a trailer has no fix — only a recorded maintainer disposition.
# The waiver is the instrument for that, and these cases pin the two properties
# that make it safe to have at all: it can only ever name history that already
# exists, and it is never silent.

run_checker() { # run_checker <env-assignments...> -- returns output, sets rc
  set +e
  output=$(env "DCO_AUTHOR_LOGIN_MAP=${noreply_good_sha}=noreply-author ${noreply_bad_sha}=different-author ${second_address_sha}=clubanderson" "$@" bash "$CHECKER" 10 HEAD 2>&1)
  rc=$?
  set -e
}

# A waiver clears the ONE failure it names and leaves the others failing.
# This is the property the email allowlist cannot provide: DCO_ALLOWLIST_EMAILS
# is consulted only after a trailer is found, so it can never clear a
# missing-signoff.
run_checker "DCO_WAIVED_COMMITS=${missing_sha}"
if [ "$rc" -ne 1 ]; then
  bad "waiving one of two failures should still exit 1 (got ${rc})"
  echo "$output" | sed 's/^/      | /'
else
  pass "a waiver does not mask an unrelated failure"
fi
if printf '%s\n' "$output" | grep -q "^WAIVED ${missing_sha} missing-signoff"; then
  pass "a waived missing-signoff is reported as WAIVED, not hidden"
else
  bad "waived commit ${missing_sha} was not reported on the WAIVED line"
  echo "$output" | sed 's/^/      | /'
fi
if printf '%s\n' "$output" | grep -q "^FAIL ${missing_sha}"; then
  bad "waived commit ${missing_sha} still reported as FAIL"
fi
if printf '%s\n' "$output" | grep -q "^FAIL ${mismatch_sha} mismatched-signoff"; then
  pass "the unwaived commit still fails"
else
  bad "unwaived commit ${mismatch_sha} stopped failing"
fi

# Waiving every failure turns the run green, and the summary still counts them.
run_checker "DCO_WAIVED_COMMITS=${missing_sha},${mismatch_sha},${noreply_bad_sha},${second_address_sha}"
if [ "$rc" -ne 0 ]; then
  bad "waiving every failure should exit 0 (got ${rc})"
  echo "$output" | sed 's/^/      | /'
else
  pass "waiving every failure returns green"
fi
if printf '%s\n' "$output" | grep -q '4 waived'; then
  pass "the summary line counts waived commits"
else
  bad "summary line did not report 4 waived"
  echo "$output" | sed 's/^/      | /'
fi
# Green-with-waivers must not read the same as clean history: a maintainer
# scanning runs has to be able to tell "nothing wrong" from "we are accepting
# known-bad history".
if printf '%s\n' "$output" | grep -q 'passed (with recorded waivers)'; then
  pass "a waived-green run is distinguishable from a clean one"
else
  bad "waived-green run did not say it was carrying waivers"
fi

# Separator handling matches DCO_ALLOWLIST_EMAILS (comma/space/newline), and
# SHA matching is case-insensitive.
upper_missing=$(printf '%s' "$missing_sha" | tr '[:lower:]' '[:upper:]')
run_checker "DCO_WAIVED_COMMITS=${upper_missing} ${mismatch_sha} ${noreply_bad_sha} ${second_address_sha}"
if [ "$rc" -eq 0 ]; then
  pass "space-separated and upper-case SHAs are accepted"
else
  bad "space-separated/upper-case waiver list was not honoured (rc=${rc})"
  echo "$output" | sed 's/^/      | /'
fi

# An abbreviated SHA is REFUSED rather than silently matching nothing. A
# prefix is ambiguous, and a waiver that quietly does nothing would leave the
# monitor red with a maintainer believing the disposition was recorded.
run_checker "DCO_WAIVED_COMMITS=${missing_sha:0:12}"
if [ "$rc" -eq 2 ]; then
  pass "an abbreviated SHA is rejected with a config error"
else
  bad "abbreviated SHA should exit 2, got ${rc}"
  echo "$output" | sed 's/^/      | /'
fi
if printf '%s\n' "$output" | grep -qi 'full 40-character'; then
  pass "the abbreviated-SHA error explains the requirement"
else
  bad "abbreviated-SHA error did not explain the requirement"
fi

# A non-hex entry is a typo, not a waiver.
run_checker "DCO_WAIVED_COMMITS=not-a-sha-0000000000000000000000000000"
if [ "$rc" -eq 2 ]; then
  pass "a non-hex waiver entry is rejected"
else
  bad "non-hex waiver entry should exit 2, got ${rc}"
  echo "$output" | sed 's/^/      | /'
fi

# A waiver on a commit that PASSES is dead weight — reported so it gets
# removed, but advisory: failing the monitor over config housekeeping would
# page the same people the waiver exists to stop paging.
run_checker "DCO_WAIVED_COMMITS=${good_sha}"
if [ "$rc" -eq 1 ]; then
  pass "a stale waiver does not change the exit code"
else
  bad "stale-waiver run should still exit 1 for the real failures, got ${rc}"
  echo "$output" | sed 's/^/      | /'
fi
if printf '%s\n' "$output" | grep -q "^STALE-WAIVER ${good_sha}"; then
  pass "a waiver on a now-passing commit is reported as stale"
else
  bad "stale waiver on ${good_sha} was not reported"
  echo "$output" | sed 's/^/      | /'
fi

# The default (no waivers set) is unchanged: everything still fails, and the
# summary reports zero waived rather than omitting the field.
run_checker "DCO_WAIVED_COMMITS="
if [ "$rc" -eq 1 ] && printf '%s\n' "$output" | grep -q '0 waived'; then
  pass "an empty waiver list is not a config error and reports 0 waived"
else
  bad "empty waiver list changed behaviour (rc=${rc})"
  echo "$output" | sed 's/^/      | /'
fi

# --- range mode (#6756 push gate) -------------------------------------------
#
# The push gate must inspect exactly the commits a push introduced. Passing a
# tip plus a count cannot express that: `git rev-list -n N <tip>` walks back in
# commit order, so on a merge push (every v4->v5 sync is one) it reports
# commits that were already on the branch. A two-dot range is the only form
# that means "just these commits".
run_checker_ref() { # run_checker_ref <ref-or-range> <env-assignments...>
  ref_arg="$1"
  shift
  set +e
  output=$(env "DCO_AUTHOR_LOGIN_MAP=${noreply_good_sha}=noreply-author ${noreply_bad_sha}=different-author ${second_address_sha}=clubanderson" "$@" bash "$CHECKER" 10 "$ref_arg" 2>&1)
  rc=$?
  set -e
}

# A range reports the failure INSIDE it and stays silent about the identical
# failure just outside it. If the range were being ignored and the tip scanned
# instead, missing_sha would appear too.
run_checker_ref "${missing_sha}..${mismatch_sha}" "DCO_WAIVED_COMMITS="
if printf '%s\n' "$output" | grep -q "^FAIL ${mismatch_sha}" &&
   ! printf '%s\n' "$output" | grep -qE "^(FAIL|WAIVED|STALE-WAIVER) ${missing_sha}"; then
  pass "a range inspects only the commits it contains"
else
  bad "range scan leaked commits from outside the range (rc=${rc})"
  echo "$output" | sed 's/^/      | /'
fi

# The summary must not call a range scan "recent commits on <tip>" — that
# wording is what made the tip-plus-count bug look correct in review.
if printf '%s\n' "$output" | grep -q "commits in ${missing_sha}..${mismatch_sha}"; then
  pass "a range scan is described as a range in the summary"
else
  bad "range summary still uses rolling-window wording"
  echo "$output" | sed 's/^/      | /'
fi

# A range whose commits are all clean passes, so the gate is green for an
# ordinary good push rather than inheriting older unrelated history.
run_checker_ref "${mismatch_sha}..${noreply_good_sha}" "DCO_WAIVED_COMMITS="
if [ "$rc" -eq 0 ]; then
  pass "a range containing only well-signed commits passes"
else
  bad "clean range should exit 0, got ${rc}"
  echo "$output" | sed 's/^/      | /'
fi

# A typo'd endpoint must be a config error, not an empty (silently green) scan.
run_checker_ref "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef..${mismatch_sha}" "DCO_WAIVED_COMMITS="
if [ "$rc" -eq 2 ]; then
  pass "an unresolvable range endpoint is a config error"
else
  bad "bad range endpoint should exit 2, got ${rc}"
  echo "$output" | sed 's/^/      | /'
fi

# Three-dot is symmetric difference; accepting it would silently scan commits
# on the other side of the fork, which is never what a push gate wants.
run_checker_ref "${missing_sha}...${mismatch_sha}" "DCO_WAIVED_COMMITS="
if [ "$rc" -eq 2 ]; then
  pass "a three-dot range is rejected rather than silently reinterpreted"
else
  bad "three-dot range should exit 2, got ${rc}"
  echo "$output" | sed 's/^/      | /'
fi

if [ "$fail" -ne 0 ]; then
  echo "test-check-dco-trailers FAILED"
  exit 1
fi

echo "test-check-dco-trailers OK"
