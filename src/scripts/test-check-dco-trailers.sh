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

# Search $output without a pipe. `out_grep -q PAT` is a
# race under `set -o pipefail`: grep -q exits at the first match, and when that
# match is on an early line (the summary line is the checker's FIRST line) the
# still-writing printf takes EPIPE, so the pipeline reports failure even though
# the match succeeded (#7080). A herestring has no pipe for pipefail to see.
out_grep() { grep "$@" <<<"$output"; }

# Every section except the PR-evidence one runs with that rule off, so no case
# here ever reaches the real GitHub API (CI sets GITHUB_REPOSITORY). run_pe
# turns it back on against a fixture directory.
export DCO_PR_EVIDENCE=false

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

fail_lines=$(out_grep -c '^FAIL ' || true)
if [ "$fail_lines" -eq 4 ]; then
  pass "exactly four failing commits are reported"
else
  bad "expected exactly four FAIL lines, got ${fail_lines}"
  echo "$output" | sed 's/^/      | /'
fi

if out_grep -q "^FAIL ${missing_sha} missing-signoff"; then
  pass "missing sign-off commit is reported"
else
  bad "missing sign-off commit ${missing_sha} was not reported"
fi

if out_grep -q "^FAIL ${mismatch_sha} mismatched-signoff"; then
  pass "mismatched sign-off commit is reported"
else
  bad "mismatched sign-off commit ${mismatch_sha} was not reported"
fi

if out_grep -q "^FAIL ${noreply_good_sha}"; then
  bad "GitHub noreply sign-off for the authoring account should not be reported"
else
  pass "GitHub noreply sign-off for the authoring account is accepted"
fi

if out_grep -q "^FAIL ${noreply_bad_sha} mismatched-signoff"; then
  pass "GitHub noreply sign-off for a different account is rejected"
else
  bad "GitHub noreply sign-off for a different account ${noreply_bad_sha} was not reported"
fi

if out_grep -q "^FAIL ${second_address_sha} mismatched-signoff"; then
  pass "an arbitrary second personal address is rejected"
else
  bad "arbitrary second personal address ${second_address_sha} was not reported"
fi

if out_grep -q "^FAIL ${good_sha}"; then
  bad "good commit ${good_sha} should not be reported"
else
  pass "good commit is not reported"
fi

if out_grep -q "^FAIL ${multi_block_sha}"; then
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
if out_grep -q "^WAIVED ${missing_sha} missing-signoff"; then
  pass "a waived missing-signoff is reported as WAIVED, not hidden"
else
  bad "waived commit ${missing_sha} was not reported on the WAIVED line"
  echo "$output" | sed 's/^/      | /'
fi
if out_grep -q "^FAIL ${missing_sha}"; then
  bad "waived commit ${missing_sha} still reported as FAIL"
fi
if out_grep -q "^FAIL ${mismatch_sha} mismatched-signoff"; then
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
if out_grep -q '4 waived'; then
  pass "the summary line counts waived commits"
else
  bad "summary line did not report 4 waived"
  echo "$output" | sed 's/^/      | /'
fi
# Green-with-waivers must not read the same as clean history: a maintainer
# scanning runs has to be able to tell "nothing wrong" from "we are accepting
# known-bad history".
if out_grep -q 'passed (with recorded waivers)'; then
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
if out_grep -qi 'full 40-character'; then
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
if out_grep -q "^STALE-WAIVER ${good_sha}"; then
  pass "a waiver on a now-passing commit is reported as stale"
else
  bad "stale waiver on ${good_sha} was not reported"
  echo "$output" | sed 's/^/      | /'
fi

# The default (no waivers set) is unchanged: everything still fails, and the
# summary reports zero waived rather than omitting the field.
run_checker "DCO_WAIVED_COMMITS="
if [ "$rc" -eq 1 ] && out_grep -q '0 waived'; then
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
if out_grep -q "^FAIL ${mismatch_sha}" &&
   ! out_grep -qE "^(FAIL|WAIVED|STALE-WAIVER) ${missing_sha}"; then
  pass "a range inspects only the commits it contains"
else
  bad "range scan leaked commits from outside the range (rc=${rc})"
  echo "$output" | sed 's/^/      | /'
fi

# The summary must not call a range scan "recent commits on <tip>" — that
# wording is what made the tip-plus-count bug look correct in review.
if out_grep -q "commits in ${missing_sha}..${mismatch_sha}"; then
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

# --- same-person noreply author (hivecommons/hive#15401) ---------------------
#
# GitHub's squash writes the PR author's GitHub identity on the landing commit.
# For a private-email account that is `<id>+<login>@users.noreply.github.com`,
# while the Signed-off-by carried over from the branch is the contributor's real
# address (v5 687c3cb0: author 91499691+odttlnt@…, sign-off
# sachin.naik@ibm.com). Same person; the checker must accept it only when the
# branch's own history shows the link, and must say which commit showed it.
sp_repo="$TMP/same-person"
mkdir -p "$sp_repo"
cd "$sp_repo" || exit 1
git init -q

# Evidence commit A: the contributor authored under the real address before,
# under the same name (case differs, as it does on v5 97a705ca vs 687c3cb0).
git config user.name 'SACHIN NAIK'
git config user.email 'sachin.naik@example.com'
echo one > f.txt
git add f.txt
git commit -q -m $'earlier commit under work address\n\nSigned-off-by: SACHIN NAIK <sachin.naik@example.com>'
sp_evidence_a_sha=$(git rev-parse HEAD)

# The squash shape under test: noreply author, work-address sign-off.
git config user.name 'Sachin Naik'
git config user.email '91499691+odttlnt@users.noreply.github.com'
echo two >> f.txt
git add f.txt
git commit -q -m $'squash of a private-email PR\n\nSigned-off-by: SACHIN NAIK <sachin.naik@example.com>'
sp_squash_sha=$(git rev-parse HEAD)

# Negative: a noreply author whose foreign sign-off has NO history anywhere.
git config user.name 'Nobody Known'
git config user.email '555+nobody-known@users.noreply.github.com'
echo three >> f.txt
git add f.txt
git commit -q -m $'noreply author, unknown sign-off\n\nSigned-off-by: Nobody Known <nobody@example.com>'
sp_unknown_sha=$(git rev-parse HEAD)

# Negative: the sign-off address HAS history, but under a different name. A
# shared address alone is not a same-person link.
git config user.name 'Imposter Noreply'
git config user.email '777+imposter@users.noreply.github.com'
echo four >> f.txt
git add f.txt
git commit -q -m $'noreply author, someone else\'s address\n\nSigned-off-by: SACHIN NAIK <sachin.naik@example.com>'
sp_imposter_sha=$(git rev-parse HEAD)

# Negative: the only "evidence" is another commit of the SAME ambiguous shape
# (noreply author + the same foreign sign-off). Repetition is not resolution.
git config user.name 'Repeat Offender'
git config user.email '888+repeat-offender@users.noreply.github.com'
echo five >> f.txt
git add f.txt
git commit -q -m $'first of two identical mismatches\n\nSigned-off-by: Repeat Offender <repeat@example.com>'
sp_repeat_a_sha=$(git rev-parse HEAD)
echo six >> f.txt
git add f.txt
git commit -q -m $'second of two identical mismatches\n\nSigned-off-by: Repeat Offender <repeat@example.com>'
sp_repeat_b_sha=$(git rev-parse HEAD)

# Evidence commit B (co-occurrence): authored under the real address and
# signed off as the login's own noreply — the two identities tied together in
# the orientation the login-lookup path already accepts. The author NAME
# differs from the squash's, so the name-based rule cannot be what accepts it.
git config user.name 'P. Cooccur (work)'
git config user.email 'pat@example.com'
echo seven >> f.txt
git add f.txt
git commit -q -m $'authored as work address, signed as own noreply\n\nSigned-off-by: Pat Cooccur <424242+patco@users.noreply.github.com>'
sp_evidence_b_sha=$(git rev-parse HEAD)
git config user.name 'Pat Cooccur'
git config user.email '424242+patco@users.noreply.github.com'
echo eight >> f.txt
git add f.txt
git commit -q -m $'squash for patco\n\nSigned-off-by: Pat Cooccur <pat@example.com>'
sp_cooccur_sha=$(git rev-parse HEAD)

run_sp() { # run_sp <env-assignments...>
  set +e
  output=$(env "DCO_AUTHOR_LOGIN_MAP=${sp_evidence_b_sha}=patco" "$@" bash "$CHECKER" 20 HEAD 2>&1)
  rc=$?
  set -e
}

run_sp "DCO_WAIVED_COMMITS="
if out_grep -q "^same-person-noreply ${sp_squash_sha} .*evidence=authored-under-signoff-email:$(git rev-parse --short "$sp_evidence_a_sha")"; then
  pass "a noreply author is accepted when the same name authored under the sign-off address before, citing that commit"
else
  bad "same-person noreply squash ${sp_squash_sha} was not accepted with authored-under-signoff-email evidence"
  echo "$output" | sed 's/^/      | /'
fi
if out_grep -q "^FAIL ${sp_squash_sha}"; then
  bad "same-person noreply squash ${sp_squash_sha} still reported as FAIL"
fi
if out_grep -q "^same-person-noreply ${sp_cooccur_sha} .*evidence=login-and-email-cooccur:$(git rev-parse --short "$sp_evidence_b_sha")"; then
  pass "a noreply author is accepted when the sign-off address and the login's noreply co-occur on another commit"
else
  bad "co-occurrence squash ${sp_cooccur_sha} was not accepted with login-and-email-cooccur evidence"
  echo "$output" | sed 's/^/      | /'
fi
if out_grep -q "^FAIL ${sp_unknown_sha} mismatched-signoff"; then
  pass "a noreply author with an unknown sign-off address still fails"
else
  bad "unknown-address noreply commit ${sp_unknown_sha} was not reported"
  echo "$output" | sed 's/^/      | /'
fi
if out_grep -q "^FAIL ${sp_imposter_sha} mismatched-signoff"; then
  pass "a noreply author signing off as a different-named known address still fails"
else
  bad "imposter noreply commit ${sp_imposter_sha} was not reported"
  echo "$output" | sed 's/^/      | /'
fi
if out_grep -q "^FAIL ${sp_repeat_a_sha} mismatched-signoff" && out_grep -q "^FAIL ${sp_repeat_b_sha} mismatched-signoff"; then
  pass "two commits of the same ambiguous shape are not evidence for each other"
else
  bad "repeated noreply/foreign-signoff pair was accepted without evidence"
  echo "$output" | sed 's/^/      | /'
fi
if out_grep -q '2 same-person-noreply)'; then
  pass "the summary line counts same-person acceptances"
else
  bad "summary did not report 2 same-person-noreply"
  echo "$output" | sed 's/^/      | /'
fi

# The heuristic is a strict addition: with the window disabled the squash is
# an ordinary mismatched-signoff again, and a waiver covers it exactly as before.
run_sp "DCO_WAIVED_COMMITS=" "DCO_SAME_PERSON_WINDOW=0"
if out_grep -q "^FAIL ${sp_squash_sha} mismatched-signoff" && ! out_grep -q '^same-person-noreply '; then
  pass "DCO_SAME_PERSON_WINDOW=0 disables the heuristic"
else
  bad "window=0 did not disable the same-person heuristic (rc=${rc})"
  echo "$output" | sed 's/^/      | /'
fi
run_sp "DCO_WAIVED_COMMITS=${sp_squash_sha}" "DCO_SAME_PERSON_WINDOW=0"
if out_grep -q "^WAIVED ${sp_squash_sha} mismatched-signoff"; then
  pass "a waiver still covers the squash when no evidence is available"
else
  bad "waiver on ${sp_squash_sha} was not honoured with the heuristic disabled"
  echo "$output" | sed 's/^/      | /'
fi

# A waiver on a commit the heuristic now accepts is dead weight, and is
# reported as such so it gets removed — the same convergence rule as #6770.
run_sp "DCO_WAIVED_COMMITS=${sp_squash_sha}"
if out_grep -q "^STALE-WAIVER ${sp_squash_sha}"; then
  pass "a waiver on a same-person-accepted commit is reported as stale"
else
  bad "stale waiver on ${sp_squash_sha} was not reported"
  echo "$output" | sed 's/^/      | /'
fi

# Evidence may come from an extra ref the operator fetched on purpose (e.g. the
# PR's own branch), and only from refs that resolve.
git checkout -q -b evidence-only "$sp_evidence_a_sha"
git checkout -q -b scan-tip "$sp_squash_sha"
git branch -q -D master 2>/dev/null || git branch -q -D main 2>/dev/null || true
set +e
output=$(DCO_SAME_PERSON_WINDOW=0 bash "$CHECKER" 1 scan-tip 2>&1); rc=$?
set -e
if out_grep -q "^FAIL ${sp_squash_sha} mismatched-signoff"; then
  pass "with no evidence ref the squash alone is a mismatch"
else
  bad "squash-only scan did not fail as expected (rc=${rc})"
  echo "$output" | sed 's/^/      | /'
fi
set +e
output=$(DCO_SAME_PERSON_EVIDENCE_REFS=evidence-only bash "$CHECKER" 1 "${sp_evidence_a_sha}..${sp_squash_sha}" 2>&1); rc=$?
set -e
if [ "$rc" -eq 0 ] && out_grep -q "^same-person-noreply ${sp_squash_sha} .*evidence=authored-under-signoff-email:"; then
  pass "DCO_SAME_PERSON_EVIDENCE_REFS supplies evidence from outside the scanned range"
else
  bad "evidence ref was not consulted (rc=${rc})"
  echo "$output" | sed 's/^/      | /'
fi
set +e
output=$(DCO_SAME_PERSON_EVIDENCE_REFS=no-such-ref bash "$CHECKER" 1 scan-tip 2>&1); rc=$?
set -e
if [ "$rc" -eq 2 ]; then
  pass "an unresolvable evidence ref is a config error"
else
  bad "bad evidence ref should exit 2, got ${rc}"
  echo "$output" | sed 's/^/      | /'
fi
set +e
output=$(DCO_SAME_PERSON_WINDOW=lots bash "$CHECKER" 1 scan-tip 2>&1); rc=$?
set -e
if [ "$rc" -eq 2 ]; then
  pass "a non-numeric DCO_SAME_PERSON_WINDOW is a config error"
else
  bad "non-numeric window should exit 2, got ${rc}"
  echo "$output" | sed 's/^/      | /'
fi
cd "$repo" || exit 1

# --- PR evidence for a first private-email squash (hivecommons/hive#19660) ---
#
# The same squash shape as above, but as the contributor's FIRST commit on the
# branch, so local history has no evidence. The merged PR supplies it: the
# squash author is the PR author's own <id>+<login> noreply address, every PR
# commit is GitHub-attributed to that account, and the PR commit authored under
# the sign-off address carried that sign-off. Fixture names follow
# github_api_get: the API path with every `/ ? & =` turned into `_`.
pe_repo="$TMP/pr-evidence"
pe_fix="$TMP/pr-evidence-fixtures"
mkdir -p "$pe_repo" "$pe_fix"
cd "$pe_repo" || exit 1
git init -q
pe_slug='test-org/test-repo'

pe_n=0
pe_commit() { # pe_commit <author-name> <author-email> <message>; sets pe_sha
  git config user.name "$1"
  git config user.email "$2"
  pe_n=$((pe_n + 1))
  echo "$pe_n" >> f.txt
  git add f.txt
  git commit -q -m "$3"
  pe_sha=$(git rev-parse HEAD)
}

pe_fixture_name() { printf '%s/%s.json' "$pe_fix" "$(printf '%s' "$1" | tr '/?&=' '____')"; }

# pe_pulls <squash-sha> <pr-number> <pr-login> <pr-id> [merge-commit-sha]
pe_pulls() {
  python3 - "$1" "$2" "$3" "$4" "${5:-$1}" > "$(pe_fixture_name "repos/${pe_slug}/commits/$1/pulls")" <<'PY'
import json, sys
_, sha, number, login, uid, merge = sys.argv
print(json.dumps([{"number": int(number), "merge_commit_sha": merge,
                   "merged_at": "2026-11-24T12:39:06Z",
                   "user": {"login": login, "id": int(uid)}}]))
PY
}

# pe_commits <pr-number> <spec>... where each spec is
# sha|author-login|author-id|author-email|committer-login|committer-email|message
# and an empty author-login stands for a commit GitHub could not attribute.
# A spec starting `merge|` is a merge commit as GitHub reports it:
# merge|sha|author-login|author-id|author-type|committer-login|verified|parents
pe_commits() {
  number="$1"; shift
  python3 - "$@" > "$(pe_fixture_name "repos/${pe_slug}/pulls/${number}/commits?per_page=100&page=1")" <<'PY'
import json, sys
out = []
for spec in sys.argv[1:]:
    if spec.startswith("merge|"):
        _, sha, al, aid, atype, cl, verified, parents = spec.split("|")
        out.append({
            "sha": sha,
            "parents": [{"sha": "%040x" % i} for i in range(int(parents))],
            "author": {"login": al, "id": int(aid), "type": atype},
            "committer": {"login": cl, "id": 19864447, "type": "User"},
            "commit": {"author": {"email": "%s+%s@users.noreply.github.com" % (aid, al)},
                       "committer": {"email": "noreply@github.com"},
                       "message": "Merge branch 'v5' into topic",
                       "verification": {"verified": verified == "true"}},
        })
        continue
    sha, al, aid, ae, cl, ce, msg = spec.split("|", 6)
    out.append({
        "sha": sha,
        "author": {"login": al, "id": int(aid)} if al else None,
        "committer": {"login": cl, "id": int(aid)} if cl else None,
        "commit": {"author": {"email": ae}, "committer": {"email": ce},
                   "message": msg.replace("\\n", "\n")},
    })
print(json.dumps(out))
PY
}

# Positive, 50df3121 shape (#19637): single branch commit authored and signed
# off under a personal gmail; squash written as the mixed-case noreply.
pe_commit 'Fayez Bast' '96446332+FayezBast@users.noreply.github.com' \
  $'🐛 dashboard: honor startup options when creating agents (#19637)\n\nSigned-off-by: Fayez Bast <fayezbast15@gmail.com>'
pe_fayez=$pe_sha
pe_pulls "$pe_fayez" 19637 FayezBast 96446332
pe_commits 19637 "9ce6d34d00000000000000000000000000000000|FayezBast|96446332|fayezbast15@gmail.com|FayezBast|fayezbast15@gmail.com|wip\\n\\nSigned-off-by: Fayez Bast <fayezbast15@gmail.com>"

# Positive, 687c3cb0 shape (#15315): long body with Hive trailers, sign-off
# name in a different case from the landing author name, work address.
pe_commit 'Sachin Naik' '91499691+odttlnt@users.noreply.github.com' \
  $'🐛 fix: stop filing the pre-restart kick-undeliverable warning (#15315)\n\nMark the message benign for that feed.\n\nHive-Run: hivecommons/hive#15313\nHive-Plan: selfreport agent_pane_timeout\n\nSigned-off-by: SACHIN NAIK <sachin.naik@example.com>'
pe_sachin=$pe_sha
pe_pulls "$pe_sachin" 15315 odttlnt 91499691
pe_commits 15315 "97a705ca00000000000000000000000000000000|odttlnt|91499691|sachin.naik@example.com|odttlnt|sachin.naik@example.com|fix\\n\\nSigned-off-by: SACHIN NAIK <sachin.naik@example.com>"

# Positive, 64f4815a shape (#19386): branch name differs from landing name,
# body carries a Fixes line before the sign-off.
pe_commit 'Riccardo Bifulco' '154237075+RiccardoBifulco@users.noreply.github.com' \
  $'🐛 fix(msteams): make TestListenBackoffStopsOnContext deterministic (#19386)\n\nFixes #19383\n\nSigned-off-by: RiccardoBifulco <riccardoxxbif@gmail.com>'
pe_riccardo=$pe_sha
pe_pulls "$pe_riccardo" 19386 RiccardoBifulco 154237075
pe_commits 19386 "3c7de54200000000000000000000000000000000|RiccardoBifulco|154237075|riccardoxxbif@gmail.com|RiccardoBifulco|riccardoxxbif@gmail.com|fix\\n\\nFixes #19383\\n\\nSigned-off-by: RiccardoBifulco <riccardoxxbif@gmail.com>"

# Positive, 69794fd6 shape (#19766): the author's single signed commit plus
# two GitHub "Update branch" merges of v5 made by the hive App (#19775).
pe_commit 'Fayez Bast' '96446332+FayezBast@users.noreply.github.com' \
  $'🐛 fix(hub): save weekly auto-upgrade settings in bulk (#19766)\n\nSigned-off-by: Fayez Bast <fayezbast15@gmail.com>'
pe_updated=$pe_sha
pe_pulls "$pe_updated" 19766 FayezBast 96446332
pe_commits 19766 \
  "9e90910600000000000000000000000000000000|FayezBast|96446332|fayezbast15@gmail.com|FayezBast|fayezbast15@gmail.com|fix\\n\\nSigned-off-by: Fayez Bast <fayezbast15@gmail.com>" \
  "merge|5b9ea82200000000000000000000000000000000|hivecommons-hive[bot]|280983584|Bot|web-flow|true|2" \
  "merge|bcc3588b00000000000000000000000000000000|hivecommons-hive[bot]|280983584|Bot|web-flow|true|2"

# Negative: the noreply login is not the PR author's (same numeric id).
pe_commit 'Alice' '1001+alice@users.noreply.github.com' $'login mismatch\n\nSigned-off-by: Alice <alice@example.com>'
pe_login=$pe_sha
pe_pulls "$pe_login" 9001 mallory 1001
pe_commits 9001 "a100000000000000000000000000000000000000|mallory|1001|alice@example.com|mallory|alice@example.com|x\\n\\nSigned-off-by: Alice <alice@example.com>"

# Negative: the login matches but the numeric id does not.
pe_commit 'Bob' '1002+bob@users.noreply.github.com' $'id mismatch\n\nSigned-off-by: Bob <bob@example.com>'
pe_id=$pe_sha
pe_pulls "$pe_id" 9002 bob 9999
pe_commits 9002 "b100000000000000000000000000000000000000|bob|9999|bob@example.com|bob|bob@example.com|x\\n\\nSigned-off-by: Bob <bob@example.com>"

# Negative: the squash signs off as someone else; the PR's commits are the
# author's own and say nothing about that address.
pe_commit 'Carol' '1003+carol@users.noreply.github.com' $'sign-off for someone else\n\nSigned-off-by: Dave <dave@example.com>'
pe_other=$pe_sha
pe_pulls "$pe_other" 9003 carol 1003
pe_commits 9003 "c100000000000000000000000000000000000000|carol|1003|carol@example.com|carol|carol@example.com|x\\n\\nSigned-off-by: Carol <carol@example.com>"

# Negative: a multi-author PR. The sign-off is backed by the PR author's own
# commit, but another account also has a commit on the PR.
pe_commit 'Erin' '1004+erin@users.noreply.github.com' $'multi-author pr\n\nSigned-off-by: Erin <erin@example.com>'
pe_multi=$pe_sha
pe_pulls "$pe_multi" 9004 erin 1004
pe_commits 9004 \
  "e100000000000000000000000000000000000000|erin|1004|erin@example.com|erin|erin@example.com|x\\n\\nSigned-off-by: Erin <erin@example.com>" \
  "e200000000000000000000000000000000000000|frank|1005|frank@example.com|frank|frank@example.com|y\\n\\nSigned-off-by: Frank <frank@example.com>"

# Negative: a PR commit GitHub could not attribute to any account.
pe_commit 'Gina' '1006+gina@users.noreply.github.com' $'unattributed pr commit\n\nSigned-off-by: Gina <gina@example.com>'
pe_unattr=$pe_sha
pe_pulls "$pe_unattr" 9006 gina 1006
pe_commits 9006 "f100000000000000000000000000000000000000||1006|gina@example.com||gina@example.com|x\\n\\nSigned-off-by: Gina <gina@example.com>"

# Negative: the PR commit is authored under the sign-off address but never
# carried that Signed-off-by itself.
pe_commit 'Hank' '1007+hank@users.noreply.github.com' $'pr commit lacked the sign-off\n\nSigned-off-by: Hank <hank@example.com>'
pe_unsigned_pr=$pe_sha
pe_pulls "$pe_unsigned_pr" 9007 hank 1007
pe_commits 9007 "d100000000000000000000000000000000000000|hank|1007|hank@example.com|hank|hank@example.com|no trailer here"

# Negative: no PR resolves (empty list), and a PR that only CONTAINS the
# commit without having merged as it.
pe_commit 'Ivy' '1008+ivy@users.noreply.github.com' $'no pr\n\nSigned-off-by: Ivy <ivy@example.com>'
pe_nopr=$pe_sha
printf '[]\n' > "$(pe_fixture_name "repos/${pe_slug}/commits/${pe_nopr}/pulls")"
pe_commit 'Jack' '1009+jack@users.noreply.github.com' $'pr merged as another commit\n\nSigned-off-by: Jack <jack@example.com>'
pe_othermerge=$pe_sha
pe_pulls "$pe_othermerge" 9009 jack 1009 0000000000000000000000000000000000000001
pe_commits 9009 "a900000000000000000000000000000000000000|jack|1009|jack@example.com|jack|jack@example.com|x\\n\\nSigned-off-by: Jack <jack@example.com>"

# Negative: API error. No fixture at all, so the lookup fails closed.
pe_commit 'Kate' '1010+kate@users.noreply.github.com' $'api error\n\nSigned-off-by: Kate <kate@example.com>'
pe_apierr=$pe_sha
# And an API error on the commits page after the PR resolved.
pe_commit 'Liam' '1011+liam@users.noreply.github.com' $'commits api error\n\nSigned-off-by: Liam <liam@example.com>'
pe_commitserr=$pe_sha
pe_pulls "$pe_commitserr" 9011 liam 1011

# Negative: the PR is fine, but the squash has no sign-off at all. The new rule
# never relaxes the sign-off requirement.
pe_commit 'Fayez Bast' '96446332+FayezBast@users.noreply.github.com' 'squash with no sign-off'
pe_nosignoff=$pe_sha
pe_pulls "$pe_nosignoff" 19638 FayezBast 96446332
pe_commits 19638 "9ce6d34e00000000000000000000000000000000|FayezBast|96446332|fayezbast15@gmail.com|FayezBast|fayezbast15@gmail.com|wip\\n\\nSigned-off-by: Fayez Bast <fayezbast15@gmail.com>"

# Negative: a plain login@ noreply (no numeric id) cannot prove the id half.
pe_commit 'Mo' 'mo@users.noreply.github.com' $'plain noreply author\n\nSigned-off-by: Mo <mo@example.com>'
pe_plain=$pe_sha
pe_pulls "$pe_plain" 9012 mo 1012
pe_commits 9012 "b900000000000000000000000000000000000000|mo|1012|mo@example.com|mo|mo@example.com|x\\n\\nSigned-off-by: Mo <mo@example.com>"

# Negatives for the update-branch exception (#19775): it covers only a
# verified web-flow merge authored by the hive App (login and id). A web-flow
# merge by another human (the conflict editor can add content), one by another
# GitHub App, one by a bot reusing the hive App login with another id, an
# unverified bot merge, a single-parent bot commit, and a PR holding nothing
# but update-branch merges all still fail.
pe_ub_author="n100000000000000000000000000000000000000|nina|1013|nina@example.com|nina|nina@example.com|x\\n\\nSigned-off-by: Nina <nina@example.com>"
pe_commit 'Nina' '1013+nina@users.noreply.github.com' $'human web-flow merge\n\nSigned-off-by: Nina <nina@example.com>'
pe_ub_human=$pe_sha
pe_pulls "$pe_ub_human" 9013 nina 1013
pe_commits 9013 "$pe_ub_author" "merge|n200000000000000000000000000000000000000|oscar|1014|User|web-flow|true|2"
pe_commit 'Nina' '1013+nina@users.noreply.github.com' $'unverified bot merge\n\nSigned-off-by: Nina <nina@example.com>'
pe_ub_unverified=$pe_sha
pe_pulls "$pe_ub_unverified" 9014 nina 1013
pe_commits 9014 "$pe_ub_author" "merge|n300000000000000000000000000000000000000|hivecommons-hive[bot]|280983584|Bot|web-flow|false|2"
pe_commit 'Nina' '1013+nina@users.noreply.github.com' $'single-parent bot commit\n\nSigned-off-by: Nina <nina@example.com>'
pe_ub_single=$pe_sha
pe_pulls "$pe_ub_single" 9015 nina 1013
pe_commits 9015 "$pe_ub_author" "merge|n400000000000000000000000000000000000000|hivecommons-hive[bot]|280983584|Bot|web-flow|true|1"
pe_commit 'Nina' '1013+nina@users.noreply.github.com' $'only update-branch merges\n\nSigned-off-by: Nina <nina@example.com>'
pe_ub_only=$pe_sha
pe_pulls "$pe_ub_only" 9016 nina 1013
pe_commits 9016 "merge|n500000000000000000000000000000000000000|hivecommons-hive[bot]|280983584|Bot|web-flow|true|2"
pe_commit 'Nina' '1013+nina@users.noreply.github.com' $'other app merge\n\nSigned-off-by: Nina <nina@example.com>'
pe_ub_otherbot=$pe_sha
pe_pulls "$pe_ub_otherbot" 9017 nina 1013
pe_commits 9017 "$pe_ub_author" "merge|n600000000000000000000000000000000000000|dependabot[bot]|49699333|Bot|web-flow|true|2"
pe_commit 'Nina' '1013+nina@users.noreply.github.com' $'hive login wrong id\n\nSigned-off-by: Nina <nina@example.com>'
pe_ub_wrongid=$pe_sha
pe_pulls "$pe_ub_wrongid" 9018 nina 1013
pe_commits 9018 "$pe_ub_author" "merge|n700000000000000000000000000000000000000|hivecommons-hive[bot]|1015|Bot|web-flow|true|2"

run_pe() { # run_pe <env-assignments...>
  set +e
  output=$(env DCO_GITHUB_REPOSITORY="$pe_slug" DCO_GITHUB_API_FIXTURES="$pe_fix" \
    DCO_SAME_PERSON_WINDOW=0 DCO_WAIVED_COMMITS= DCO_PR_EVIDENCE=true "$@" bash "$CHECKER" 30 HEAD 2>&1)
  rc=$?
  set -e
}

run_pe
for pe_pos in "$pe_fayez:19637:9ce6d34d" "$pe_sachin:15315:97a705ca" "$pe_riccardo:19386:3c7de542" "$pe_updated:19766:9e909106"; do
  pe_s=${pe_pos%%:*}; pe_rest=${pe_pos#*:}
  if out_grep -q "^pr-noreply ${pe_s} .*evidence=pr#${pe_rest}\$" && ! out_grep -q "^FAIL ${pe_s}"; then
    pass "first private-email squash ${pe_s:0:8} is accepted on PR evidence (pr#${pe_rest})"
  else
    bad "first private-email squash ${pe_s} was not accepted on PR evidence"
    echo "$output" | sed 's/^/      | /'
  fi
done
pe_expect_fail() { # pe_expect_fail <sha> <label> <pattern>
  if out_grep -q "^FAIL $1 $3"; then
    pass "$2 still fails"
  else
    bad "$2 ($1) did not fail with '$3'"
    echo "$output" | sed 's/^/      | /'
  fi
  if out_grep -q "^pr-noreply $1 "; then
    bad "$2 ($1) was accepted on PR evidence"
  fi
}
pe_expect_fail "$pe_login" 'a noreply login that is not the PR author' 'mismatched-signoff .*pr-evidence=author-not-pr-author-noreply$'
pe_expect_fail "$pe_id" 'a noreply id that is not the PR author' 'mismatched-signoff .*pr-evidence=author-not-pr-author-noreply$'
pe_expect_fail "$pe_other" 'a sign-off for a different person' 'mismatched-signoff .*pr-evidence=signoff-not-on-pr-commits$'
pe_expect_fail "$pe_multi" 'a multi-author PR' 'mismatched-signoff .*pr-evidence=pr-commit-not-by-pr-author$'
pe_expect_fail "$pe_unattr" 'a PR commit GitHub cannot attribute' 'mismatched-signoff .*pr-evidence=pr-commit-not-by-pr-author$'
pe_expect_fail "$pe_unsigned_pr" 'a PR commit that never carried the sign-off' 'mismatched-signoff .*pr-evidence=signoff-not-on-pr-commits$'
pe_expect_fail "$pe_nopr" 'a commit with no resolvable PR' 'mismatched-signoff .*pr-evidence=no-merged-pr$'
pe_expect_fail "$pe_othermerge" 'a PR that merged as a different commit' 'mismatched-signoff .*pr-evidence=no-merged-pr$'
pe_expect_fail "$pe_apierr" 'an API error on the PR lookup' 'mismatched-signoff .*pr-evidence=api-error$'
pe_expect_fail "$pe_commitserr" 'an API error on the PR commits lookup' 'mismatched-signoff .*pr-evidence=api-error$'
pe_expect_fail "$pe_nosignoff" 'a squash with no sign-off at all' 'missing-signoff '
pe_expect_fail "$pe_plain" 'a plain (id-less) noreply author' 'mismatched-signoff .*signed-off-by=mo@example.com$'
pe_expect_fail "$pe_ub_human" 'a web-flow merge by another human' 'mismatched-signoff .*pr-evidence=pr-commit-not-by-pr-author$'
pe_expect_fail "$pe_ub_unverified" 'an unverified bot merge' 'mismatched-signoff .*pr-evidence=pr-commit-not-by-pr-author$'
pe_expect_fail "$pe_ub_single" 'a single-parent bot commit' 'mismatched-signoff .*pr-evidence=pr-commit-not-by-pr-author$'
pe_expect_fail "$pe_ub_otherbot" 'a web-flow merge by another GitHub App' 'mismatched-signoff .*pr-evidence=pr-commit-not-by-pr-author$'
pe_expect_fail "$pe_ub_wrongid" 'a bot merge with the hive App login but another id' 'mismatched-signoff .*pr-evidence=pr-commit-not-by-pr-author$'
pe_expect_fail "$pe_ub_only" 'a PR of only update-branch merges' 'mismatched-signoff .*pr-evidence=pr-has-no-commits$'
if [ "$rc" -eq 1 ] && out_grep -q '4 pr-noreply; 0 same-person-noreply)'; then
  pass "the run still fails on the negatives and the summary counts 4 pr-noreply"
else
  bad "expected rc=1 and 4 pr-noreply in the summary (rc=${rc})"
  echo "$output" | sed 's/^/      | /'
fi

# The rule is a strict addition: switched off, the positives fail as before.
run_pe DCO_PR_EVIDENCE=false
if out_grep -q "^FAIL ${pe_fayez} mismatched-signoff .*pr-evidence=disabled\$" && ! out_grep -q '^pr-noreply '; then
  pass "DCO_PR_EVIDENCE=false disables the PR-evidence rule"
else
  bad "DCO_PR_EVIDENCE=false did not disable the rule"
  echo "$output" | sed 's/^/      | /'
fi

# Existing waivers keep working, and a waiver the rule would now cover reads
# as WAIVED, never STALE-WAIVER, so leaving it in place pages nobody.
run_pe "DCO_WAIVED_COMMITS=${pe_fayez} ${pe_sachin}"
if out_grep -q "^WAIVED ${pe_fayez} mismatched-signoff" && out_grep -q "^WAIVED ${pe_sachin} mismatched-signoff" \
   && ! out_grep -q '^STALE-WAIVER ' && out_grep -q "^pr-noreply ${pe_riccardo} "; then
  pass "waivers on PR-evidence-covered commits stay WAIVED and are not reported stale"
else
  bad "waiver interplay with the PR-evidence rule is wrong"
  echo "$output" | sed 's/^/      | /'
fi

# Waiving every negative leaves only the positives, which pass on their own.
run_pe "DCO_WAIVED_COMMITS=${pe_login} ${pe_id} ${pe_other} ${pe_multi} ${pe_unattr} ${pe_unsigned_pr} ${pe_nopr} ${pe_othermerge} ${pe_apierr} ${pe_commitserr} ${pe_nosignoff} ${pe_plain} ${pe_ub_human} ${pe_ub_otherbot} ${pe_ub_wrongid} ${pe_ub_unverified} ${pe_ub_single} ${pe_ub_only}"
if [ "$rc" -eq 0 ] && ! out_grep -q '^FAIL '; then
  pass "with the negatives waived the PR-evidence positives pass the run"
else
  bad "positives did not pass once the negatives were waived (rc=${rc})"
  echo "$output" | sed 's/^/      | /'
fi
cd "$repo" || exit 1

# Bot authors (#18203): DCO_SKIP_BOT_AUTHORS skips the sign-off identity match
# for a bot, never the missing-trailer check. A bot commit with no
# Signed-off-by (the shape of f33758f5a) fails; a signed bot commit is skipped.
bot_repo="$TMP/bot-repo"
mkdir -p "$bot_repo"
cd "$bot_repo" || exit 1
git init -q
git config user.name 'hivecommons-hive[bot]'
git config user.email '280983584+hivecommons-hive[bot]@users.noreply.github.com'
echo signed > file.txt
git add file.txt
git commit -q -m $'bot signed commit\n\nSigned-off-by: scanner <280983584+hivecommons-hive[bot]@users.noreply.github.com>'
bot_signed_sha=$(git rev-parse HEAD)
echo unsigned >> file.txt
git add file.txt
git commit -q -m $'bot squash without a sign-off\n\nCo-authored-by: scanner <280983584+hivecommons-hive[bot]@users.noreply.github.com>'
bot_unsigned_sha=$(git rev-parse HEAD)
set +e
output=$(DCO_SKIP_BOT_AUTHORS=true bash "$CHECKER" 10 HEAD 2>&1); rc=$?
set -e
if [ "$rc" -eq 1 ] && out_grep -q "^FAIL ${bot_unsigned_sha} missing-signoff bot-author="; then
  pass "a bot-authored commit with no sign-off is reported even when bots are skipped"
else
  bad "unsigned bot commit ${bot_unsigned_sha} was not reported (rc=${rc})"
  echo "$output" | sed 's/^/      | /'
fi
if out_grep -q "^FAIL ${bot_signed_sha}"; then
  bad "a signed bot-authored commit should still be skipped"
  echo "$output" | sed 's/^/      | /'
else
  pass "a signed bot-authored commit is still skipped"
fi
cd "$repo" || exit 1

if [ "$fail" -ne 0 ]; then
  echo "test-check-dco-trailers FAILED"
  exit 1
fi

echo "test-check-dco-trailers OK"
