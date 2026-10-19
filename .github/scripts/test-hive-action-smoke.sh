#!/usr/bin/env bash
# Checks the default-branch (v5) hive-action-smoke workflow (#12964): it must
# register with the v6 inputs, pin the composite action to a v6 commit, and
# relay a comment that @-mentions the app_handle input. The relay body test
# runs the pinned action's real run: block with a gh test double, so it proves
# the comment the workflow would post starts with "@<app_handle> status".
# The only network access is a git fetch of the pinned commit when it is not
# already present; no GitHub API calls are made.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SMOKE_YML="${ROOT}/.github/workflows/hive-action-smoke.yml"
TMP_ROOT="${ROOT}/.github/.test-tmp"
mkdir -p "$TMP_ROOT"
TMP="$(mktemp -d "$TMP_ROOT/hive-action-smoke.XXXXXX")"
trap 'rm -rf "$TMP"; rmdir "$TMP_ROOT" 2>/dev/null || true' EXIT

failures=0
pass() { printf '  ok: %s\n' "$*"; }
bad() {
  printf '  FAIL: %s\n' "$*"
  failures=$((failures + 1))
}
assert_eq() {
  local got="$1" want="$2" msg="$3"
  if [[ "$got" == "$want" ]]; then pass "$msg"; else bad "$msg (got '$got', want '$want')"; fi
}
assert_grep() {
  local pattern="$1" msg="$2"
  if grep -qE -- "$pattern" "$SMOKE_YML"; then pass "$msg"; else bad "$msg"; fi
}

# input_default prints the default: of one workflow_dispatch input.
input_default() {
  awk -v name="      $1:" '
    $0 == name { found = 1; next }
    found && /^      [a-z_]+:$/ { exit }
    found && /^        default:/ { sub(/^        default: */, ""); print; exit }
  ' "$SMOKE_YML"
}

echo 'workflow_dispatch inputs'
for input in issue prompt transport hive_url audience app_handle; do
  assert_grep "^      ${input}:\$" "input ${input} is declared"
done
assert_grep '^  workflow_dispatch:$' 'workflow is dispatchable'
MENTION_LOGIN="$(input_default app_handle)"
assert_eq "$MENTION_LOGIN" 'hivecommons-hive' 'app_handle defaults to hivecommons-hive'
assert_eq "$(input_default transport)" 'comment' 'transport defaults to comment'
assert_grep 'app_handle: \$\{\{ inputs\.app_handle \|\| vars\.HIVE_APP_HANDLE \}\}' \
  'app_handle is passed to the action, then vars.HIVE_APP_HANDLE'

echo 'composite action pinned to v6'
ACTION_SHA="$(sed -nE 's|^ *uses: hivecommons/hive/\.github/actions/hive@([0-9a-f]{40}) # v6.*|\1|p' "$SMOKE_YML")"
if [[ -n "$ACTION_SHA" ]]; then pass "action pinned to ${ACTION_SHA}"; else bad 'action is not pinned to a v6 commit SHA'; fi
if grep -qF 'uses: ./.github/actions/hive' "$SMOKE_YML"; then bad 'workflow uses a local action path that does not exist on v5'; else pass 'workflow does not use a local action path'; fi

echo 'relay comment body uses app_handle'
ACTION_DIR="${TMP}/action"
if [[ -n "$ACTION_SHA" ]]; then
  if ! git -C "$ROOT" cat-file -e "${ACTION_SHA}^{commit}" 2>/dev/null; then
    git -C "$ROOT" fetch --quiet --depth=1 origin "$ACTION_SHA" >/dev/null 2>&1 || true
  fi
  mkdir -p "$ACTION_DIR"
  if git -C "$ROOT" archive "$ACTION_SHA" .github/actions/hive | tar -x -C "$ACTION_DIR" 2>/dev/null; then
    ACTION_DIR="${ACTION_DIR}/.github/actions/hive"
    pass 'fetched the pinned action'
  else
    bad "could not fetch .github/actions/hive at ${ACTION_SHA}"
  fi
fi

RUN_SCRIPT="${TMP}/run.sh"
if [[ -f "${ACTION_DIR}/action.yml" ]]; then
  awk '
    /^      run: \|$/ { inrun = 1; next }
    inrun && /^[^ ]/ { inrun = 0 }
    inrun { sub(/^        /, ""); print }
  ' "${ACTION_DIR}/action.yml" >"$RUN_SCRIPT"
fi
if [[ -s "$RUN_SCRIPT" ]]; then pass 'extracted the action run block'; else bad 'could not extract the action run block'; fi

mkdir -p "${TMP}/bin"
cat >"${TMP}/bin/gh" <<'GH'
#!/usr/bin/env bash
# Records the comment body the action would post.
for arg in "$@"; do
  case "$arg" in
    body=*) printf '%s' "${arg#body=}" >"${GH_BODY_FILE}" ;;
  esac
done
GH
chmod +x "${TMP}/bin/gh"

relay_body() {
  local login="$1"
  local out="${TMP}/body.txt"
  rm -f "$out"
  [[ -s "$RUN_SCRIPT" ]] || return 1
  (
    unset HIVE_APP_HANDLE
    PATH="${TMP}/bin:${PATH}" \
      GH_BODY_FILE="$out" \
      HIVE_COMMAND=status HIVE_ISSUE_INPUT=7 HIVE_PROMPT='manual smoke from workflow_dispatch' \
      HIVE_TRANSPORT=comment HIVE_DISPATCH_URL='' HIVE_AUDIENCE=hive \
      HIVE_APP_HANDLE_INPUT="$login" HIVE_ACTION_PATH="$ACTION_DIR" \
      GITHUB_REPOSITORY_NAME=example/hive GITHUB_EVENT_ISSUE_NUMBER='' GITHUB_EVENT_PR_NUMBER='' \
      GITHUB_RUN_ID_VALUE=42 GITHUB_RUN_ATTEMPT_VALUE=1 GITHUB_WORKFLOW_NAME=smoke GITHUB_ACTOR_LOGIN=alice \
      bash "$RUN_SCRIPT" >/dev/null 2>&1
  ) || return 1
  cat "$out"
}

body="$(relay_body "$MENTION_LOGIN")"
assert_eq "$(head -n 1 <<<"$body")" '@hivecommons-hive status manual smoke from workflow_dispatch' \
  'default app_handle is the relay mention'
if grep -qF 'hive:source=action run_id=42 run_attempt=1' <<<"$body"; then pass 'relay body carries the action marker'; else bad 'relay body is missing the action marker'; fi
assert_eq "$(relay_body '@other-hive[bot]' | head -n 1)" '@other-hive status manual smoke from workflow_dispatch' \
  'a custom app_handle is normalized and mentioned'

if [[ $failures -gt 0 ]]; then
  printf '%d failure(s)\n' "$failures"
  exit 1
fi
echo 'all hive-action-smoke tests passed'
