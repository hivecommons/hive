#!/usr/bin/env bash
# test-check-dco-trailers.sh — exercises check-dco-trailers.sh against a small
# throwaway git repository with good, missing-trailer, and mismatched-trailer
# commits.
set -u -o pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECKER="${HERE}/check-dco-trailers.sh"
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

set +e
output=$(bash "$CHECKER" 10 HEAD 2>&1)
rc=$?
set -e

if [ "$rc" -ne 1 ]; then
  bad "checker exits 1 when two commits fail (got ${rc})"
  echo "$output" | sed 's/^/      | /'
else
  pass "checker exits 1 when bad commits are present"
fi

fail_lines=$(printf '%s\n' "$output" | grep -c '^FAIL ' || true)
if [ "$fail_lines" -eq 2 ]; then
  pass "exactly two failing commits are reported"
else
  bad "expected exactly two FAIL lines, got ${fail_lines}"
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

if printf '%s\n' "$output" | grep -q "^FAIL ${good_sha}"; then
  bad "good commit ${good_sha} should not be reported"
else
  pass "good commit is not reported"
fi

if [ "$fail" -ne 0 ]; then
  echo "test-check-dco-trailers FAILED"
  exit 1
fi

echo "test-check-dco-trailers OK"
