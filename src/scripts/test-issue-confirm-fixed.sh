#!/usr/bin/env bash
# test-issue-confirm-fixed.sh — exercises issue-confirm-fixed.sh (#9746) with a
# gh stub so no network calls are made: the /fixed command, the plain-language
# and negation grammar, the waiting-on-confirmation scope, and authorisation.
set -u -o pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${HERE}/issue-confirm-fixed.sh"
TMP_ROOT="${HERE}/../.test-tmp"
mkdir -p "$TMP_ROOT"
TMP="$(mktemp -d "$TMP_ROOT/issue-confirm-fixed.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

fail=0
pass() { echo "  ok: $*"; }
bad()  { echo "  FAIL: $*"; fail=1; }

CALL_LOG="$TMP/gh-calls.log"
BODY_LOG="$TMP/comment-body.log"

GH_STUB="$TMP/gh"
cat > "$GH_STUB" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
method=GET
path=""
body=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-X" ]; then method="$arg"; prev=""; continue; fi
  if [ "$prev" = "-f" ] || [ "$prev" = "-F" ]; then
    case "$arg" in body=*) body="${arg#body=}" ;; esac
    prev=""
    continue
  fi
  if [ "$prev" = "--jq" ]; then prev=""; continue; fi
  case "$arg" in
    api|--paginate|--slurp) ;;
    -X|-f|-F|--jq) prev="$arg" ;;
    repos/*) path="$arg" ;;
  esac
done
printf '%s %s\n' "$method" "$path" >> "$CALL_LOG"
if [ -n "$body" ]; then printf '%s\n---\n' "$body" >> "$BODY_LOG"; fi
case "$method $path" in
  "GET repos/hivecommons/hive/issues/"*"/labels?per_page=100")
    if [ -n "${STUB_LABELS:-}" ]; then printf '%s\n' "$STUB_LABELS"; fi
    ;;
  "GET repos/hivecommons/hive/issues/"*"/comments?per_page=100")
    printf '[%s]\n' "${STUB_COMMENTS:-[]}"
    ;;
  "GET repos/hivecommons/hive/collaborators/"*"/permission")
    if [ "${STUB_PERM:-none}" = "none" ]; then
      echo "HTTP 404: Not Found" >&2
      exit 1
    fi
    printf '%s\n' "$STUB_PERM"
    ;;
  GET*)
    echo "unexpected GET: $path" >&2
    exit 1
    ;;
  *) printf '{}\n' ;;
esac
STUB
chmod +x "$GH_STUB"

SWEEP_COMMENT='[{"body":"<!-- hive-post-merge-refs-sweep: issue=7 -->\nPR #717 has merged (https://github.com/hivecommons/hive/pull/717) and referenced this issue."}]'

# run BODY COMMENTER [LABELS] [COMMENTS] [PERM]
run() {
  : > "$CALL_LOG"
  : > "$BODY_LOG"
  output=$(CALL_LOG="$CALL_LOG" BODY_LOG="$BODY_LOG" GH_BIN="$GH_STUB" \
    STUB_LABELS="${3:-}" STUB_COMMENTS="${4:-[]}" STUB_PERM="${5:-none}" \
    CONFIRM_REPO=hivecommons/hive CONFIRM_ISSUE=7 CONFIRM_ISSUE_AUTHOR=reporter \
    CONFIRM_COMMENTER="$2" CONFIRM_COMMENT_BODY="$1" bash "$SCRIPT" 2>&1)
  rc=$?
}

closed() { grep -q '^PATCH repos/hivecommons/hive/issues/7$' "$CALL_LOG"; }
commented() { grep -q '^POST repos/hivecommons/hive/issues/7/comments$' "$CALL_LOG"; }
dump() {
  echo "$output" | sed 's/^/      | /'
  sed 's/^/      calls| /' "$CALL_LOG"
}

# --- Classification grammar ------------------------------------------------
check_class() {
  local want="$1" text="$2" got
  got=$(CONFIRM_COMMENT_BODY="$text" bash "$SCRIPT" --classify)
  if [ "$got" = "$want" ]; then
    pass "classify '${text//$'\n'/\\n}' => ${want}"
  else
    bad "classify '${text//$'\n'/\\n}' => ${got}, want ${want}"
  fi
}
check_class command "/fixed"
check_class command "/fixed verified on v5.97.0"
check_class command "/close"
check_class command $'\n/FIXED\nthanks'
check_class none "/closed"
check_class none "/fixedness"
check_class confirm "yes, this has been fixed"
check_class confirm "Confirmed fixed."
check_class confirm "verified, works now"
check_class confirm $'> Can this issue now be closed, or is there remaining work it should keep tracking?\n\nYes, resolved.'
check_class ambiguous "not fixed yet"
check_class ambiguous "fixed except the v6 case"
check_class ambiguous "it's fixed but the log still shows a warning"
check_class ambiguous "partially fixed"
check_class ambiguous "isn't fixed for me"
check_class ambiguous "is this fixed?"
check_class none "I can reproduce this on v5.96.3"
check_class none "Here is how the feature works in detail"

# --- /fixed from the reporter closes and relabels --------------------------
run "/fixed" reporter $'hive/likely-done\nneeds-reporter-confirmation' "$SWEEP_COMMENT"
if [ "$rc" -eq 0 ] && closed \
  && grep -q '^POST repos/hivecommons/hive/issues/7/labels$' "$CALL_LOG" \
  && grep -q '^DELETE repos/hivecommons/hive/issues/7/labels/hive%2Flikely-done$' "$CALL_LOG" \
  && grep -q '^DELETE repos/hivecommons/hive/issues/7/labels/needs-reporter-confirmation$' "$CALL_LOG"; then
  pass "/fixed from the reporter closes, applies the confirmed label and removes wait labels"
else
  bad "/fixed from the reporter did not close/relabel"
  dump
fi
if grep -q '@reporter, who reported this issue, confirmed the fix' "$BODY_LOG" \
  && grep -q 'merged PR(s) #717' "$BODY_LOG" && grep -q '/reopen' "$BODY_LOG"; then
  pass "close comment names the confirmer, the merged PR and /reopen"
else
  bad "close comment is missing the confirmer, PR or /reopen"
  sed 's/^/      body| /' "$BODY_LOG"
fi

# --- /fixed works on an issue not waiting on confirmation ------------------
run "/fixed" maint "" "[]" write
if [ "$rc" -eq 0 ] && closed \
  && ! grep -q '^DELETE ' "$CALL_LOG"; then
  pass "/fixed from a write-access user closes an issue with no likely-done label"
else
  bad "/fixed from a write-access user did not close a non-waiting issue"
  dump
fi

# --- Plain-language confirmation on a waiting issue ------------------------
run "yes, this has been fixed" reporter "hive/likely-done"
if [ "$rc" -eq 0 ] && closed; then
  pass "plain-language confirmation closes a likely-done issue"
else
  bad "plain-language confirmation did not close a likely-done issue"
  dump
fi

run "Confirmed fixed" reporter "needs-reporter-confirmation"
if [ "$rc" -eq 0 ] && closed; then
  pass "plain-language confirmation closes an issue with the reporter-confirmation label"
else
  bad "plain-language confirmation did not close a reporter-confirmation labeled issue"
  dump
fi

run "Confirmed fixed" maint "" "$SWEEP_COMMENT" maintain
if [ "$rc" -eq 0 ] && closed; then
  pass "plain-language confirmation from a maintainer closes an issue the refs sweep asked about"
else
  bad "plain-language confirmation did not close a refs-sweep issue"
  dump
fi

# --- Negation keeps the issue open and asks for /fixed ---------------------
for text in "not fixed yet" "fixed except the v6 case"; do
  run "$text" reporter "hive/likely-done"
  if [ "$rc" -eq 0 ] && ! closed && commented && grep -q '/fixed' "$BODY_LOG"; then
    pass "'${text}' does not close and asks for /fixed"
  else
    bad "'${text}' was not handled as ambiguous"
    dump
  fi
done

# --- Plain language on an issue not waiting on confirmation does nothing ---
run "yes, this has been fixed" reporter "kind/bug"
if [ "$rc" -eq 0 ] && ! closed && ! commented; then
  pass "plain-language confirmation on a non-waiting issue does nothing"
else
  bad "plain-language confirmation acted on a non-waiting issue"
  dump
fi

# --- Ordinary discussion makes no API calls at all -------------------------
run "I can reproduce this on v5.96.3" reporter "hive/likely-done"
if [ "$rc" -eq 0 ] && [ ! -s "$CALL_LOG" ]; then
  pass "a non-confirmation comment makes no API calls"
else
  bad "a non-confirmation comment called the API"
  dump
fi

# --- Unauthorised commenters are told no -----------------------------------
for perm in none read triage; do
  run "/fixed" bystander "hive/likely-done" "[]" "$perm"
  if [ "$rc" -eq 0 ] && ! closed && commented && grep -q 'limited to its reporter (@reporter)' "$BODY_LOG"; then
    pass "/fixed from a user with '${perm}' access does not close and gets a reply"
  else
    bad "/fixed from a user with '${perm}' access was not refused"
    dump
  fi
done

run "yes, this has been fixed" bystander "hive/likely-done" "[]" read
if [ "$rc" -eq 0 ] && ! closed && commented; then
  pass "plain-language confirmation from an unauthorised user does not close and gets a reply"
else
  bad "plain-language confirmation from an unauthorised user was not refused"
  dump
fi

run "not fixed yet" bystander "hive/likely-done" "[]" read
if [ "$rc" -eq 0 ] && ! closed && ! commented; then
  pass "an ambiguous comment from an unauthorised user is left alone"
else
  bad "an ambiguous comment from an unauthorised user was acted on"
  dump
fi

# --- DRY_RUN makes no writes -----------------------------------------------
: > "$CALL_LOG"
output=$(CALL_LOG="$CALL_LOG" BODY_LOG="$BODY_LOG" GH_BIN="$GH_STUB" DRY_RUN=1 \
  STUB_LABELS="hive/likely-done" CONFIRM_REPO=hivecommons/hive CONFIRM_ISSUE=7 \
  CONFIRM_ISSUE_AUTHOR=reporter CONFIRM_COMMENTER=reporter CONFIRM_COMMENT_BODY="/fixed" \
  bash "$SCRIPT" 2>&1)
rc=$?
if [ "$rc" -eq 0 ] && ! grep -qv '^GET ' "$CALL_LOG" && printf '%s\n' "$output" | grep -q 'DRY-RUN'; then
  pass "DRY_RUN=1 performs no writes"
else
  bad "DRY_RUN=1 performed writes"
  dump
fi

if [ "$fail" -ne 0 ]; then
  echo "test-issue-confirm-fixed FAILED"
  exit 1
fi

echo "test-issue-confirm-fixed OK"
