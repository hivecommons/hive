#!/usr/bin/env bash
# Hermetic API/git stubs: never contact GitHub or modify a real branch.
set -euo pipefail
unset GITHUB_STEP_SUMMARY  # Capture summaries on stdout, including on GitHub runners.
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
export FIXTURE="$TMP" GITHUB_REPOSITORY=example/repo
export GITHUB_REF_NAME=v5 GITHUB_SHA=after GITHUB_EVENT_BEFORE=before
export PATH="$TMP/bin:$PATH"
cat > "$TMP/bin/gh" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FIXTURE/calls"
[[ "$1" == api ]] || exit 90
shift
if [[ "$1" == -X ]]; then
  number=${3%/update-branch}; number=${number##*/}
  [[ "$number" != "${FAIL_PR:-}" ]] || { echo "HTTP ${FAIL_CODE:-500}" >&2; exit 1; }
  exit 0
fi
endpoint=$1
case "$endpoint" in
  *'/pulls?'*)
    [[ "$*" == *--paginate* && "$*" == *--slurp* ]] || exit 91
    [[ "${LOOKUP_FAIL:-}" != pulls ]] || exit 1
    if [[ "${EMPTY_PRS:-}" == 1 ]]; then printf '[[]]\n'; exit 0; fi
    printf '[[{"number":1,"title":"Body marker","body":"<!-- hive-shared-ci-42 -->","head":{"repo":{"full_name":"fork/repo"}}}],[{"number":2,"title":"Comment marker","body":"","head":{"repo":{"full_name":"example/repo"}}},{"number":3,"title":"Wrong incident","body":"<!-- hive-shared-ci-420 -->","head":{"repo":{"full_name":"example/repo"}}}]]\n'
    ;;
  *'/comments?'*)
    [[ "$*" == *--paginate* ]] || exit 92
    [[ "${LOOKUP_FAIL:-}" != comments ]] || exit 1
    # Includes a marker found after the first comments page.
    [[ "$endpoint" != */issues/2/* ]] || printf 'earlier comment\n<!-- hive-shared-ci-42 -->\n'
    [[ "$endpoint" != */issues/3/* ]] || printf '<!-- hive-shared-ci-420 -->\n'
    ;;
  *'/files?'*)
    [[ "$endpoint" != */pulls/1/* ]] || printf 'unrelated.go\n'
    [[ "$endpoint" != */pulls/2/* ]] || printf 'shared.go\n'
    [[ "$endpoint" != */pulls/3/* ]] || printf 'other.go\n'
    ;;
  */pulls/*)
    if [[ "${UNKNOWN_ONCE:-}" == 1 && ! -e "$FIXTURE/retried" ]]; then
      touch "$FIXTURE/retried"
      printf 'null\tunknown\n'
    else
      printf 'true\t%s\n' "${STATE:-behind}"
    fi
    ;;
  *) exit 93 ;;
esac
MOCK
cat > "$TMP/bin/git" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf 'git %s\n' "$*" >> "$FIXTURE/calls"
case "$1" in
  fetch) ;;
  diff) printf 'shared.go\n' ;;
  *) exit 94 ;;
esac
MOCK
cat > "$TMP/bin/sleep" <<'MOCK'
#!/usr/bin/env bash
printf 'sleep %s\n' "$*" >> "$FIXTURE/calls"
MOCK
chmod +x "$TMP/bin/"*
run() {
  : > "$TMP/calls"
  bash "$ROOT/.github/scripts/pr-auto-update.sh" > "$TMP/output" 2>&1
}
require() { grep -q -- "$1" "$2" || { echo "missing: $1" >&2; exit 1; }; }
reject() { if grep -q -- "$1" "$2"; then echo "unexpected: $1" >&2; exit 1; fi; }

export SHARED_CI_INCIDENT=42
run
require 'pulls/1/update-branch' "$TMP/calls"
require 'pulls/2/update-branch' "$TMP/calls"
reject 'pulls/3/update-branch' "$TMP/calls"
reject '/files?' "$TMP/calls"
reject 'git ' "$TMP/calls"
reject '&base=' "$TMP/calls"
require 'no marker for incident #42' "$TMP/output"

for state in clean dirty blocked unknown; do
  export STATE=$state
  run
  reject 'update-branch' "$TMP/calls"
  require "mergeable_state=$state" "$TMP/output"
done
unset STATE
export EMPTY_PRS=1
run
reject 'update-branch' "$TMP/calls"
require 'No open PRs in scope' "$TMP/output"
unset EMPTY_PRS
export UNKNOWN_ONCE=1
run
require 'sleep 5' "$TMP/calls"
require 'pulls/1/update-branch' "$TMP/calls"
unset UNKNOWN_ONCE

export FAIL_PR=1
for code in 422 403; do
  export FAIL_CODE=$code
  run
  require "($code)" "$TMP/output"
  require 'pulls/2/update-branch' "$TMP/calls"
done
export FAIL_CODE=500
if run; then echo 'unexpected update failure was swallowed' >&2; exit 1; fi
unset FAIL_PR FAIL_CODE
for failure in pulls comments; do
  export LOOKUP_FAIL=$failure
  if run; then echo 'lookup failure was swallowed' >&2; exit 1; fi
done
unset LOOKUP_FAIL
export SHARED_CI_INCIDENT='42; unsafe'
if run; then echo 'invalid incident accepted' >&2; exit 1; fi
reject 'api ' "$TMP/calls"

# The original push path still requires file overlap, even on marked PRs.
unset SHARED_CI_INCIDENT
run
require '&base=v5' "$TMP/calls"
reject 'pulls/1/update-branch' "$TMP/calls"
require 'pulls/2/update-branch' "$TMP/calls"
reject '/comments?' "$TMP/calls"
require 'no changed-file intersection' "$TMP/output"
export GITHUB_EVENT_BEFORE=0000000000000000000000000000000000000000
run
reject 'api ' "$TMP/calls"
require 'push has no comparable before SHA' "$TMP/output"
printf 'PASS: PR auto-update push and incident paths\n'
