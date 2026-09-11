#!/usr/bin/env bash
# test-hive-open-pr-coauthors.sh — pins only the hive-open-pr/issue-coauthor
# seam. src/scripts/test-issue-coauthor.sh owns identity resolution coverage.
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
mkdir -p "$TMP/stub" "$TMP/fixtures" "$TMP/requests"
trap 'rm -rf "$TMP"' EXIT

cat > "$TMP/stub/gh" <<'STUB'
#!/usr/bin/env bash
set -u
if [ "${1:-}" = "api" ] && [ "${2:-}" = "user" ]; then
  printf 'maintainer\n'
  exit 0
fi
if [ "${1:-}" = "api" ]; then
  case "${2:-}" in
    */issues/*)
      n="${2##*/}"
      f="${STUB_FIXTURES}/${n}.tsv"
      if [ ! -f "$f" ]; then
        echo "gh: Not Found (HTTP 404)" >&2
        exit 1
      fi
      cat "$f"
      exit 0
      ;;
  esac
fi
echo "stub: unexpected gh invocation: $*" >&2
exit 99
STUB
chmod +x "$TMP/stub/gh"

# login <TAB> id <TAB> type <TAB> name
printf 'filer\t12345\tUser\tFiona Filer\n'       > "$TMP/fixtures/100.tsv"
printf 'dependabot[bot]\t333\tBot\tDependabot\n' > "$TMP/fixtures/102.tsv"

repo="$TMP/repo"
mkdir -p "$repo"
cd "$repo" || exit 1
git init -q
git config user.name 'Hive Agent'
git config user.email 'agent@example.com'

echo one > file.txt
git add file.txt
git commit -q -s -m 'fix issue 100'

run_open_pr() { # run_open_pr <body>; sets rc and err
  set +e
  PATH="$TMP/stub:$PATH" HIVE_GH_BIN="$TMP/stub/gh" STUB_FIXTURES="$TMP/fixtures" \
    HIVE_OPEN_PR_REQ_DIR="$TMP/requests" \
    bash "$SCRIPT" --repo hivecommons/hive --title 'test PR' --body "$1" \
    >/dev/null 2>"$TMP/err"
  rc=$?
  err=$(cat "$TMP/err")
  set -e
}

run_open_pr $'## Fix\n\nFixes #100\nRefs #999'
body=$(git log -1 --format=%B)
if [ "$rc" -eq 0 ] && printf '%s\n' "$err" | grep -q 'HEAD is missing issue #100 trailer: Co-authored-by: Fiona Filer'; then
  pass "hive-open-pr asks issue-coauthor for closing issues and warns when HEAD lacks the trailer"
else
  bad "missing-trailer warning not reported (rc=${rc})"
  printf '%s\n' "$err" | sed 's/^/      | /'
fi
if ! printf '%s\n' "$body" | grep -q '^Co-authored-by:'; then
  pass "hive-open-pr does not amend commits or duplicate issue-coauthor --amend"
else
  bad "hive-open-pr amended the commit unexpectedly"
fi
if ! printf '%s\n' "$err" | grep -q '#999'; then
  pass "non-closing Refs issue is not sent to issue-coauthor"
else
  bad "non-closing Refs issue was processed"
fi

git commit --amend -q -m $'fix issue 100\n\nSigned-off-by: Hive Agent <agent@example.com>\nCo-authored-by: Fiona Filer <12345+filer@users.noreply.github.com>'
run_open_pr 'Fixes #100'
if [ "$rc" -eq 0 ] && ! printf '%s\n' "$err" | grep -q 'HEAD is missing issue #100'; then
  pass "existing expected trailer is accepted without a warning"
else
  bad "existing trailer still warned (rc=${rc})"
  printf '%s\n' "$err" | sed 's/^/      | /'
fi

run_open_pr 'Fixes #102'
if [ "$rc" -eq 0 ] && ! printf '%s\n' "$err" | grep -q 'HEAD is missing issue #102'; then
  pass "issue-coauthor exit 0 with empty stdout is not an error"
else
  bad "bot/empty-trailer case was treated as an error (rc=${rc})"
  printf '%s\n' "$err" | sed 's/^/      | /'
fi

run_open_pr 'Fixes #999'
if [ "$rc" -eq 0 ] && printf '%s\n' "$err" | grep -q 'continuing so the fix can ship'; then
  pass "issue-coauthor resolution failure warns and continues"
else
  bad "resolution failure policy wrong (rc=${rc})"
  printf '%s\n' "$err" | sed 's/^/      | /'
fi

run_open_pr 'Fixes #0'
if [ "$rc" -eq 2 ] && printf '%s\n' "$err" | grep -q 'usage error'; then
  pass "issue-coauthor usage errors stay hard failures"
else
  bad "usage-error handling wrong (rc=${rc})"
  printf '%s\n' "$err" | sed 's/^/      | /'
fi

if [ "$fail" -ne 0 ]; then
  echo "test-hive-open-pr-coauthors FAILED"
  exit 1
fi

echo "test-hive-open-pr-coauthors OK"
