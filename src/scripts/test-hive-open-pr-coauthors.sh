#!/usr/bin/env bash
# test-hive-open-pr-coauthors.sh — pin issue-author co-author trailers added by
# bin/hive-open-pr.sh.
set -u -o pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
SCRIPT="$ROOT/bin/hive-open-pr.sh"
TMP_ROOT="$ROOT/src/.test-tmp"
TMP="$TMP_ROOT/hive-open-pr-coauthors.$$"

fail=0
pass() { echo "  ok: $*"; }
bad()  { echo "  FAIL: $*"; fail=1; }

rm -rf "$TMP"
mkdir -p "$TMP/fakebin" "$TMP/requests"
trap 'rm -rf "$TMP"' EXIT

cat > "$TMP/fakebin/gh" <<'SH'
#!/usr/bin/env sh
if [ "$1" != "api" ]; then
  exit 1
fi
case "$2" in
  user)
    printf 'bob\t222\n'
    ;;
  repos/hivecommons/hive/issues/101)
    printf 'alice\t111\tUser\n'
    ;;
  repos/hivecommons/hive/issues/201)
    printf 'buildbot\t333\tBot\n'
    ;;
  repos/hivecommons/hive/issues/202)
    printf 'ci-maintainer\t444\tUser\n'
    ;;
  repos/hivecommons/hive/issues/203)
    printf 'bob\t222\tUser\n'
    ;;
  *)
    exit 1
    ;;
esac
SH
chmod +x "$TMP/fakebin/gh"

repo="$TMP/repo"
remote="$TMP/remote.git"
git init -q --bare "$remote"
mkdir -p "$repo"
cd "$repo" || exit 1
git init -q
git config user.name 'Hive Agent'
git config user.email 'agent@example.com'
git remote add origin "$remote"

echo one > file.txt
git add file.txt
git commit -q -s -m 'fix issue 101'
git push -q -u origin HEAD

PATH="$TMP/fakebin:$PATH" HIVE_OPEN_PR_REQ_DIR="$TMP/requests" \
  bash "$SCRIPT" --repo hivecommons/hive --title 'fix 101' \
  --body $'## Fix\n\nFixes #101\nRefs #102' >/dev/null 2>"$TMP/case1.err"
body=$(git log -1 --format=%B)
if printf '%s\n' "$body" | grep -q 'Co-authored-by: alice <111+alice@users.noreply.github.com>'; then
  pass "closing issue author is added with the GitHub noreply address"
else
  bad "closing issue author was not added"
  sed 's/^/      | /' "$TMP/case1.err"
fi
if printf '%s\n' "$body" | grep -q '102+'; then
  bad "non-closing Refs issue should not be credited"
else
  pass "non-closing Refs issue is ignored"
fi

echo two >> file.txt
git add file.txt
git commit -q -s -m 'skip bot and self authors'
git push -q origin HEAD

PATH="$TMP/fakebin:$PATH" HIVE_OPEN_PR_REQ_DIR="$TMP/requests" \
  bash "$SCRIPT" --repo hivecommons/hive --title 'skip authors' \
  --body $'Fixes #201\nFixes #202\nFixes #203' >/dev/null 2>"$TMP/case2.err"
body=$(git log -1 --format=%B)
if printf '%s\n' "$body" | grep -q 'Co-authored-by:'; then
  bad "bot or self-authored issues should not add co-author trailers"
  printf '%s\n' "$body" | sed 's/^/      | /'
else
  pass "bot and self-authored issues are skipped"
fi
if grep -q 'skipping issue #201 co-author credit for bot author buildbot' "$TMP/case2.err" &&
   grep -q 'skipping issue #202 co-author credit for bot author ci-maintainer' "$TMP/case2.err" &&
   grep -q 'skipping issue #203 co-author credit for PR author bob' "$TMP/case2.err"; then
  pass "skip reasons are reported"
else
  bad "expected skip reasons were not reported"
  sed 's/^/      | /' "$TMP/case2.err"
fi

if [ "$fail" -ne 0 ]; then
  echo "test-hive-open-pr-coauthors FAILED"
  exit 1
fi

echo "test-hive-open-pr-coauthors OK"
