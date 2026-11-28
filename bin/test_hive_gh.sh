#!/usr/bin/env bash
# Contract tests for bin/hive-gh.sh (#12127).
# Run: bash bin/test_hive_gh.sh
#
# hive-gh is the compact-projection front for the common gh READ verbs. It must
# (a) add a fixed --json field set and --jq filter in front of the gated `gh`,
# (b) never project away a field an authorization/merge gate reads,
# (c) hand the command to gh UNTOUCHED on explicit demand (--full, HIVE_GH_FULL,
#     or a caller-supplied --json/--jq/--comments/--web), and
# (d) pass unknown verbs through unchanged.
#
# Like bin/test_issue_classifier.sh this EXECUTES the script. A stub `gh` is
# placed first on PATH: it records its argv and, when asked for --json, prints a
# canned payload and applies --jq with the real jq so the reshaping filters are
# exercised for real rather than assumed. Hermetic: no network.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${REPO_ROOT}/bin/hive-gh.sh"

PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && echo "        $2"
  FAIL=$((FAIL + 1))
}

if ! command -v jq >/dev/null 2>&1; then
  echo "harness-error: jq is required (the stub applies --jq with the real jq)"
  exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
STUB_DIR="${WORK}/bin"
ARGV_LOG="${WORK}/argv"
mkdir -p "$STUB_DIR"

# Stub gh: log argv one-per-line; if --json was requested emit the fixture for
# the verb (a superset of every compact field set, shaped like the real API),
# projected to the requested fields like the real gh, then apply --jq if given.
cat >"${STUB_DIR}/gh" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$@" >"$STUB_ARGV_LOG"
json=""; jq_expr=""; expect=""
for a in "$@"; do
  if [ -n "$expect" ]; then
    case "$expect" in json) json="$a" ;; jq) jq_expr="$a" ;; esac
    expect=""; continue
  fi
  case "$a" in
    --json) expect=json ;; --json=*) json="${a#--json=}" ;;
    --jq) expect=jq ;; --jq=*) jq_expr="${a#--jq=}" ;;
  esac
done
if [ -z "$json" ]; then echo "HUMAN OUTPUT FROM GH: $*"; exit 0; fi
# Like the real gh, return ONLY the requested fields (on each item for arrays).
payload="$(printf '%s' "$STUB_FIXTURE" | jq -c --arg f "$json" '
  ($f | split(",")) as $keys
  | def pick: with_entries(select(.key as $k | $keys | index($k)));
  if type == "array" then map(pick) else pick end')"
if [ -n "$jq_expr" ]; then printf '%s' "$payload" | jq -c "$jq_expr"; else printf '%s\n' "$payload"; fi
STUB
chmod +x "${STUB_DIR}/gh"

PR_FIXTURE='{"number":7,"title":"t","state":"OPEN","isDraft":false,"url":"u","author":{"login":"octo","is_bot":false},"headRefName":"feat","headRefOid":"abc123","baseRefName":"v5","labels":[{"name":"lgtm","color":"0"},{"name":"hold","color":"1"}],"mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED","reviewDecision":"APPROVED","statusCheckRollup":[{"name":"ci","status":"COMPLETED","conclusion":"FAILURE"},{"context":"legacy","state":"SUCCESS"}],"additions":1,"deletions":2,"changedFiles":1,"updatedAt":"x","body":"B","comments":[{"body":"long"}]}'
PR_LIST_FIXTURE="[${PR_FIXTURE}]"
ISSUE_FIXTURE='{"number":3,"title":"i","state":"OPEN","stateReason":"","url":"u","author":{"login":"bot","is_bot":true},"labels":[{"name":"bug"}],"assignees":[{"login":"a1"}],"milestone":{"title":"m1"},"createdAt":"c","updatedAt":"x","body":"B","comments":[]}'
ISSUE_LIST_FIXTURE="[${ISSUE_FIXTURE}]"
RUN_FIXTURE='{"databaseId":9,"name":"CI","workflowName":"CI","displayTitle":"d","status":"completed","conclusion":"failure","event":"push","headBranch":"b","headSha":"deadbeef","attempt":2,"url":"u","jobs":[{"name":"build","status":"completed","conclusion":"failure","steps":[{"name":"ok","conclusion":"success"},{"name":"test","conclusion":"failure"}]}]}'
CHECKS_FIXTURE='[{"name":"ci","bucket":"fail","state":"FAILURE","workflow":"CI","link":"l","description":"d"}]'

# run <fixture> <hive-gh args…> → stdout; argv in $ARGV_LOG
run() {
  local fixture="$1"; shift
  PATH="${STUB_DIR}:${PATH}" STUB_ARGV_LOG="$ARGV_LOG" STUB_FIXTURE="$fixture" bash "$SCRIPT" "$@" 2>&1
}
argv_has() { grep -qxF -- "$1" "$ARGV_LOG"; }
argv_after_json() {
  # the value following --json
  awk 'f{print; exit} $0=="--json"{f=1}' "$ARGV_LOG"
}

echo "=== compact projection adds --json/--jq in front of gh ==="
out="$(run "$PR_FIXTURE" pr view 7 --repo o/r)"
if argv_has "--json" && argv_has "--jq" && argv_has "--repo" && argv_has "o/r"; then
  pass "pr view gets --json and --jq; caller flags preserved"
else fail "pr view gets --json and --jq" "$(tr '\n' ' ' <"$ARGV_LOG")"; fi
fields="$(argv_after_json)"
for f in labels author headRefOid baseRefName isDraft state mergeable mergeStateStatus reviewDecision statusCheckRollup; do
  if [[ ",${fields}," == *",${f},"* ]]; then pass "pr view keeps gate field: $f"; else fail "pr view keeps gate field: $f" "$fields"; fi
done
if [[ ",${fields}," != *",comments,"* ]]; then pass "pr view drops comments (compact)"; else fail "pr view drops comments"; fi

echo "=== jq reshaping keeps gate values, flattens verbose sub-objects ==="
if [ "$(jq -r '.labels | join(",")' <<<"$out")" = "lgtm,hold" ]; then pass "labels flattened to names (hold label still visible)"; else fail "labels flattened" "$out"; fi
if [ "$(jq -r '.author' <<<"$out")" = "octo" ]; then pass "author flattened to login"; else fail "author flattened" "$out"; fi
if [ "$(jq -r '.headRefOid' <<<"$out")" = "abc123" ]; then pass "head SHA retained"; else fail "head SHA retained" "$out"; fi
if [ "$(jq -r '.mergeStateStatus + "/" + .reviewDecision' <<<"$out")" = "BLOCKED/APPROVED" ]; then pass "mergeStateStatus/reviewDecision retained"; else fail "merge state retained" "$out"; fi
if [ "$(jq -r '[.statusCheckRollup[].conclusion] | join(",")' <<<"$out")" = "FAILURE,SUCCESS" ]; then
  pass "check conclusions retained for BOTH CheckRun and StatusContext kinds"
else fail "check conclusions retained" "$out"; fi
if [ "$(jq -r '.statusCheckRollup[1].name' <<<"$out")" = "legacy" ]; then pass "StatusContext .context normalised to .name"; else fail "StatusContext normalised" "$out"; fi
if [ "$(jq -r 'has("comments")' <<<"$out")" = "false" ]; then pass "comments absent from compact output"; else fail "comments absent" "$out"; fi

out="$(run "$PR_LIST_FIXTURE" pr list --repo o/r --limit 5)"
if [ "$(jq -r 'length' <<<"$out")" = "1" ] && [ "$(jq -r '.[0].labels[1]' <<<"$out")" = "hold" ]; then pass "pr list: array preserved (no rows dropped), labels flattened"; else fail "pr list reshaped" "$out"; fi
fields="$(argv_after_json)"
if [[ ",${fields}," == *",headRefOid,"* && ",${fields}," == *",labels,"* && ",${fields}," != *",body,"* ]]; then pass "pr list keeps headRefOid+labels, drops body"; else fail "pr list field set" "$fields"; fi

out="$(run "$ISSUE_FIXTURE" issue view 3)"
if [ "$(jq -r '.author' <<<"$out")" = "bot[bot]" ]; then pass "issue view: bot author carries [bot] suffix (gate-visible)"; else fail "bot author" "$out"; fi
if [ "$(jq -r '.assignees[0] + "/" + .milestone + "/" + .labels[0]' <<<"$out")" = "a1/m1/bug" ]; then pass "issue view: assignees/milestone/labels flattened"; else fail "issue view flatten" "$out"; fi
if [ "$(jq -r '.body' <<<"$out")" = "B" ]; then pass "issue view keeps body (the work content)"; else fail "issue view body" "$out"; fi

out="$(run "$ISSUE_LIST_FIXTURE" issue list -R o/r)"
if [ "$(jq -r '.[0].labels[0]' <<<"$out")" = "bug" ] && [[ ",$(argv_after_json)," != *",body,"* ]]; then pass "issue list: labels kept, body dropped"; else fail "issue list" "$out"; fi

out="$(run "$RUN_FIXTURE" run view 9)"
if [ "$(jq -r '.headSha + "/" + .conclusion + "/" + (.attempt|tostring)' <<<"$out")" = "deadbeef/failure/2" ]; then pass "run view keeps headSha/conclusion/attempt"; else fail "run view gate fields" "$out"; fi
if [ "$(jq -r '.jobs[0].failed_steps[0]' <<<"$out")" = "test" ]; then pass "run view: jobs reduced to name/status/conclusion/failed_steps"; else fail "run view jobs" "$out"; fi

out="$(run "$CHECKS_FIXTURE" pr checks 7)"
if [ "$(jq -r '.[0].bucket + "/" + .[0].state' <<<"$out")" = "fail/FAILURE" ] && [[ ",$(argv_after_json)," != *",description,"* ]]; then pass "pr checks keeps bucket/state, drops description"; else fail "pr checks" "$out"; fi

echo "=== explicit full fetch escape hatches pass through UNTOUCHED ==="
run "$PR_FIXTURE" --full pr view 7 --repo o/r >/dev/null
if ! argv_has "--json" && ! argv_has "--full" && argv_has "pr" && argv_has "view" && argv_has "7"; then pass "--full strips itself and adds no projection"; else fail "--full passthrough" "$(tr '\n' ' ' <"$ARGV_LOG")"; fi
PATH="${STUB_DIR}:${PATH}" STUB_ARGV_LOG="$ARGV_LOG" STUB_FIXTURE="$PR_FIXTURE" HIVE_GH_FULL=1 bash "$SCRIPT" pr view 7 >/dev/null 2>&1
if ! argv_has "--json"; then pass "HIVE_GH_FULL=1 disables projection"; else fail "HIVE_GH_FULL" "$(tr '\n' ' ' <"$ARGV_LOG")"; fi
run "$PR_FIXTURE" pr view 7 --json number,comments >/dev/null
if [ "$(grep -c -- '--json' "$ARGV_LOG")" = "1" ] && argv_has "number,comments" && ! argv_has "--jq"; then pass "caller's own --json wins; no second projection appended"; else fail "own --json" "$(tr '\n' ' ' <"$ARGV_LOG")"; fi
run "$PR_FIXTURE" pr view 7 --jq .title >/dev/null
if ! argv_has "--json"; then pass "caller's own --jq passes through"; else fail "own --jq" "$(tr '\n' ' ' <"$ARGV_LOG")"; fi
run "$ISSUE_FIXTURE" issue view 3 --comments >/dev/null
if ! argv_has "--json" && argv_has "--comments"; then pass "--comments (full thread on demand) passes through"; else fail "--comments passthrough" "$(tr '\n' ' ' <"$ARGV_LOG")"; fi
run "$PR_FIXTURE" pr view 7 --web >/dev/null
if ! argv_has "--json"; then pass "--web passes through"; else fail "--web passthrough"; fi

echo "=== verbs outside the table are untouched ==="
run "$PR_FIXTURE" pr diff 7 >/dev/null
if ! argv_has "--json" && argv_has "diff"; then pass "pr diff passes through"; else fail "pr diff passthrough"; fi
run "$PR_FIXTURE" api repos/o/r/pulls/7 >/dev/null
if ! argv_has "--json"; then pass "gh api passes through"; else fail "gh api passthrough"; fi
run "$PR_FIXTURE" issue create --title t --body b >/dev/null
if ! argv_has "--json" && argv_has "create"; then pass "writes (issue create) pass through to the gated gh"; else fail "issue create passthrough"; fi
run "$PR_FIXTURE" --repo o/r pr view 7 >/dev/null
if argv_has "--json"; then pass "global --repo BEFORE the subcommand is skipped when locating the verb"; else fail "global --repo parsing" "$(tr '\n' ' ' <"$ARGV_LOG")"; fi
run "$PR_FIXTURE" >/dev/null
if ! argv_has "--json"; then pass "bare hive-gh passes through (gh help)"; else fail "bare passthrough"; fi

echo "=== --fields documents the table and names the gate fields ==="
out="$(bash "$SCRIPT" --fields)"
for v in "pr view" "pr list" "pr checks" "issue view" "issue list" "run view"; do
  if grep -q "^${v} " <<<"$out"; then pass "--fields lists '$v'"; else fail "--fields lists '$v'"; fi
done
if grep -q "Gate fields retained" <<<"$out" && grep -q "headRefOid" <<<"$out"; then pass "--fields enumerates retained gate fields"; else fail "--fields gate enumeration"; fi
if grep -q "dropped:" <<<"$out"; then pass "--fields says what --full restores"; else fail "--fields dropped column"; fi

echo "=== Forgejo repos: pr/issue view and pr checks go through the hive's read relay (#20230) ==="
READ_DIR="${WORK}/read-requests"
mkdir -p "$READ_DIR"
# fake_relay <ok> answers the first request the way forge_read_relay.go does:
# a 0600 view file named after the request and a result naming it, or a
# refusal that renames the request.
fake_relay() {
  local ok="$1" i req base view
  for i in $(seq 1 100); do
    req="$(find "$READ_DIR" -maxdepth 1 -name '*.json' ! -name '*.result.json' ! -name '*.tmp' | head -n1)"
    if [ -n "$req" ]; then
      base="${req%.json}"
      cp "$req" "${WORK}/last-read-request"
      if [ "$ok" = true ]; then
        view="${base}.x1.view"
        (umask 077; jq -c '{number: .number, title: "from forge", kind: .kind, repo: .repo, headRefOid: "abc", forge: "forgejo"}' "$req" >"$view")
        jq -n --arg v "$view" '{ok: true, view: $v}' >"${base}.result.json"
        rm -f "$req"
      else
        jq -n '{ok: false, error: "read rejected: repo \"x\" is not managed by this hive"}' >"${base}.result.json"
        mv "$req" "${req}.rejected"
      fi
      return 0
    fi
    sleep 0.1
  done
}
run_forge() {
  PATH="${STUB_DIR}:${PATH}" STUB_ARGV_LOG="$ARGV_LOG" STUB_FIXTURE="$PR_FIXTURE" HIVE_GH_READ_REQUEST_DIR="$READ_DIR" HIVE_GH_FORGE_TIMEOUT=10 bash "$SCRIPT" "$@" 2>&1
}

: >"$ARGV_LOG"
fake_relay true &
out="$(run_forge pr view 50 --repo acme/widgets)"; wait
if [ "$(jq -r '.title + "/" + .kind + "/" + .repo + "/" + (.number|tostring)' <<<"$out" 2>/dev/null)" = "from forge/pr/acme/widgets/50" ] && [ ! -s "$ARGV_LOG" ]; then
  pass "pr view on a forge repo is read through the relay, not gh"
else fail "pr view through the relay" "$out / gh argv: $(tr '\n' ' ' <"$ARGV_LOG")"; fi
if [ -z "$(find "$READ_DIR" -name '*.view')" ]; then pass "the delivered view is removed after it is printed"; else fail "view removed"; fi

: >"$ARGV_LOG"
fake_relay true &
out="$(run_forge pr checks 50 -R fj.example/acme/widgets)"; wait
if [ "$(jq -r '.kind + "/" + .repo' "${WORK}/last-read-request")" = "checks/acme/widgets" ] && [ ! -s "$ARGV_LOG" ]; then
  pass "pr checks with HOST/OWNER/REPO goes to the relay as kind checks"
else fail "pr checks through the relay" "$out"; fi

CHECKOUT="${WORK}/checkout"
git init -q "$CHECKOUT"
git -C "$CHECKOUT" remote add origin https://fj.example/acme/widgets.git
: >"$ARGV_LOG"
fake_relay true &
out="$(cd "$CHECKOUT" && run_forge issue view 3)"; wait
if [ "$(jq -r '.kind + "/" + .repo + "/" + (.number|tostring)' "${WORK}/last-read-request")" = "issue/acme/widgets/3" ] && [ ! -s "$ARGV_LOG" ]; then
  pass "issue view in a forge checkout takes the repo from origin"
else fail "issue view from origin" "$out"; fi

: >"$ARGV_LOG"
fake_relay false &
out="$(run_forge pr view 50 --repo acme/other)"; rc=$?; wait
if [ "$rc" -ne 0 ] && grep -q "the hive refused the read" <<<"$out" && grep -q "not managed" <<<"$out"; then
  pass "a refusal is reported with the relay's reason and a non-zero exit"
else fail "refusal reported" "rc=$rc $out"; fi

echo "=== GitHub repos are unchanged even where the read queue exists ==="
run_forge pr view 7 --repo github.com/o/r >/dev/null
if argv_has "--json" && argv_has "github.com/o/r"; then pass "--repo github.com/... still goes to gh"; else fail "github.com repo to gh" "$(tr '\n' ' ' <"$ARGV_LOG")"; fi
git -C "$CHECKOUT" remote set-url origin https://github.com/o/r.git
(cd "$CHECKOUT" && run_forge pr view 7 >/dev/null)
if argv_has "--json"; then pass "a github.com origin still goes to gh"; else fail "github origin to gh" "$(tr '\n' ' ' <"$ARGV_LOG")"; fi
run_forge pr view feat-branch --repo acme/widgets >/dev/null
if argv_has "--json" && argv_has "feat-branch"; then pass "a non-numeric target still goes to gh"; else fail "branch target to gh"; fi
run_forge --full pr view 50 --repo acme/widgets >/dev/null
if ! argv_has "--json" && argv_has "50"; then pass "--full still hands the call to gh untouched"; else fail "--full on forge repo"; fi
if [ -z "$(find "$READ_DIR" -name '*.json' ! -name '*.result.json' ! -name '*.rejected')" ]; then pass "no request left behind by the gh paths"; else fail "stray request"; fi

echo
echo "=== ${PASS} passed, ${FAIL} failed ==="
[ "$FAIL" -eq 0 ]
