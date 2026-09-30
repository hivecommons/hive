#!/usr/bin/env bash
# Exercises the hive composite action's App-handle resolution and relay
# comment body (.github/actions/hive/app-handle.sh), then runs the action's
# real run: block with a gh test double to prove the posted comment mentions
# the configured handle. No network access or real GitHub API calls are made.
#
# Regression for #9707: the relay comment hard-coded "@hive", so a hive whose
# App handle is e.g. "hivecommons-hive" never recognised the relayed command.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ACTION_DIR="${ROOT}/.github/actions/hive"
ACTION_YML="${ACTION_DIR}/action.yml"
SMOKE_YML="${ROOT}/.github/workflows/hive-action-smoke.yml"
TMP_ROOT="${ROOT}/.github/.test-tmp"
mkdir -p "$TMP_ROOT"
TMP="$(mktemp -d "$TMP_ROOT/hive-action-app-handle.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

# shellcheck source=../actions/hive/app-handle.sh
source "${ACTION_DIR}/app-handle.sh"

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
assert_fails() {
  local msg="$1"
  shift
  if "$@" >/dev/null 2>&1; then bad "$msg"; else pass "$msg"; fi
}

echo 'hive_resolve_app_handle: resolution order'
assert_eq "$(hive_resolve_app_handle 'from-input' 'from-var')" 'from-input' 'explicit input wins over the variable'
assert_eq "$(hive_resolve_app_handle '' 'hivecommons-hive')" 'hivecommons-hive' 'variable is used when the input is empty'
assert_eq "$(hive_resolve_app_handle '   ' 'hivecommons-hive')" 'hivecommons-hive' 'whitespace-only input falls through to the variable'
assert_eq "$(hive_resolve_app_handle '' '')" 'hive' 'default is hive when nothing is configured'
assert_eq "$(hive_resolve_app_handle '')" 'hive' 'default is hive when the fallback is omitted'
assert_eq "$HIVE_DEFAULT_APP_HANDLE" 'hive' 'documented default constant is hive'

echo 'hive_normalize_app_handle: accepted spellings'
for spelling in 'x' '@x' 'x[bot]' '@x[bot]' ' @x[bot] '; do
  assert_eq "$(hive_normalize_app_handle "$spelling")" 'x' "'$spelling' normalizes to x"
done
assert_eq "$(hive_resolve_app_handle '@hivecommons-hive[bot]' '')" 'hivecommons-hive' 'input @hivecommons-hive[bot] resolves to hivecommons-hive'
assert_eq "$(hive_resolve_app_handle '' 'hivecommons-hive[bot]')" 'hivecommons-hive' 'variable hivecommons-hive[bot] resolves to hivecommons-hive'
assert_eq "$(hive_normalize_app_handle '')" '' 'blank normalizes to empty'

echo 'hive_normalize_app_handle: rejected values'
assert_fails 'handle with a space is rejected' hive_normalize_app_handle 'hive status'
assert_fails 'handle with a newline is rejected' hive_normalize_app_handle $'hive\nevil'
assert_fails 'handle starting with - is rejected' hive_normalize_app_handle '-hive'
assert_fails '[bot] prefix is rejected' hive_normalize_app_handle '@[bot]x'
assert_fails 'invalid input fails resolution instead of falling through' hive_resolve_app_handle 'bad handle' 'hivecommons-hive'

echo 'hive_relay_comment_body'
marker='<!-- hive:source=action run_id=1 run_attempt=1 workflow=w actor=a transport=comment -->'
assert_eq "$(hive_relay_comment_body 'hivecommons-hive' 'status' 'smoke' "$marker")" \
  $'@hivecommons-hive status smoke\n\n'"$marker" 'body mentions the configured handle, command, prompt, marker'
assert_eq "$(hive_relay_comment_body 'hive' 'review' '' "$marker")" \
  $'@hive review\n\n'"$marker" 'empty prompt adds no trailing space'

echo 'action.yml run block with a gh test double'
# Extract the composite step's run: block (indented 8 spaces under "run: |").
RUN_SCRIPT="${TMP}/run.sh"
awk '
  /^      run: \|$/ { inrun = 1; next }
  inrun && /^[^ ]/ { inrun = 0 }
  inrun { sub(/^        /, ""); print }
' "$ACTION_YML" >"$RUN_SCRIPT"
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

run_action() {
  local input="$1" env_handle="$2"
  local out="${TMP}/body.txt"
  rm -f "$out"
  (
    unset HIVE_APP_HANDLE
    if [[ -n "$env_handle" ]]; then export HIVE_APP_HANDLE="$env_handle"; fi
    PATH="${TMP}/bin:${PATH}" \
      GH_BODY_FILE="$out" \
      HIVE_COMMAND=status HIVE_ISSUE_INPUT=7 HIVE_PROMPT='manual smoke' \
      HIVE_TRANSPORT=comment HIVE_DISPATCH_URL='' HIVE_AUDIENCE=hive \
      HIVE_APP_HANDLE_INPUT="$input" HIVE_ACTION_PATH="$ACTION_DIR" \
      GITHUB_REPOSITORY_NAME=example/hive GITHUB_EVENT_ISSUE_NUMBER='' GITHUB_EVENT_PR_NUMBER='' \
      GITHUB_RUN_ID_VALUE=42 GITHUB_RUN_ATTEMPT_VALUE=1 GITHUB_WORKFLOW_NAME=smoke GITHUB_ACTOR_LOGIN=alice \
      bash "$RUN_SCRIPT" >/dev/null 2>&1
  ) || return 1
  head -n 1 "$out"
}

assert_eq "$(run_action '@hivecommons-hive[bot]' '')" '@hivecommons-hive status manual smoke' 'action posts the explicit input handle'
assert_eq "$(run_action '' 'hivecommons-hive')" '@hivecommons-hive status manual smoke' 'action falls back to HIVE_APP_HANDLE'
assert_eq "$(run_action '' '')" '@hive status manual smoke' 'action defaults to @hive'
assert_fails 'action fails on an invalid handle' run_action 'not a handle' ''

echo 'no hard-coded App login'
for f in "$ACTION_YML" "${ACTION_DIR}/app-handle.sh" "$SMOKE_YML"; do
  if grep -qE '(^|[^A-Za-z0-9-])@?hive\[bot\]' "$f"; then bad "$(basename "$f") hard-codes hive[bot]"; else pass "$(basename "$f") does not hard-code hive[bot]"; fi
done
if grep -qF 'body="@hive' "$ACTION_YML"; then bad 'action.yml hard-codes an @hive body'; else pass 'action.yml builds the body from the resolved handle'; fi
if grep -qF 'vars.HIVE_APP_HANDLE' "$SMOKE_YML"; then pass 'smoke workflow passes vars.HIVE_APP_HANDLE'; else bad 'smoke workflow does not pass vars.HIVE_APP_HANDLE'; fi

if [[ $failures -gt 0 ]]; then
  printf '%d failure(s)\n' "$failures"
  exit 1
fi
echo 'all hive action app-handle tests passed'
