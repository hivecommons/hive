#!/usr/bin/env bash
# Contract tests for bin/enumerate-actionable.sh.
# Run: bash bin/test_enumerate_actionable.sh
#
# enumerate-actionable.sh writes /var/run/hive-metrics/actionable.json — the
# ONE file merge-gate.sh, kick-agents.sh and the dashboard's contribute lane
# (src/pkg/dashboard/contribute_ws.go) read instead of running their own gh
# queries. Every hold/blocked/draft/ADOPTERS exclusion and the external-issue
# SHA hold/unhold lifecycle (which WRITES labels and comments to GitHub) live
# in this one script. It has been patched by fix PRs (#245, #260, #274, #394,
# #398, #4772) and had no tests: a filter regression would ship green and show
# up as agents picking up held work, or as a hold comment posted to every
# external issue every cycle.
#
# Like bin/test_gh_wrapper_gates.sh this EXECUTES the script rather than
# grepping it. The script hardcodes /usr/bin/gh, /var/run/hive-metrics and
# /var/log/kick-agents.log with no env override, so the harness runs a COPY
# with exactly those three paths rewritten to a temp dir (and asserts the
# rewrite landed — a refactor that renames them must fail here loudly, not
# silently run the tests against the real paths). The stub gh serves canned
# REST/GraphQL fixtures and runs the script's own `--jq` filters through real
# jq (gh's --jq is a jq implementation), so the field mapping and the
# `.pull_request == null` filter are under test, not bypassed.
#
# Doctrine (audit 6/7): every exclusion assertion sits next to a positive
# control that IS included, so an enumerator that emits nothing cannot pass.
# Hermetic: no network, no sleeps (the retry sleep is stubbed), never touches
# /var/run, /data or /tmp/hive.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${REPO_ROOT}/bin/enumerate-actionable.sh"

PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && echo "        $2"
  FAIL=$((FAIL + 1))
}

# A missing harness dependency must be red, not a silent exit 0 (#5388
# doctrine). ubuntu-latest ships jq and python3; the script itself needs
# python3 and the stub gh needs jq to honour --jq.
for dep in jq python3; do
  if ! command -v "$dep" >/dev/null 2>&1; then
    echo "harness-error: $dep is required (script under test needs python3; stub gh needs jq)"
    exit 1
  fi
done

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

RUN_DIR="${WORK}/run"            # stands in for /var/run/hive-metrics
LOG_FILE="${WORK}/kick-agents.log"  # stands in for /var/log/kick-agents.log
SHIM_DIR="${WORK}/shim"
FIX_ROOT="${WORK}/fixtures"
OUT="${RUN_DIR}/actionable.json"
GH_LOG="${WORK}/gh.log"
mkdir -p "$RUN_DIR" "$SHIM_DIR" "$FIX_ROOT" "${WORK}/home" "${WORK}/bin"

# ── The path-rewritten copy ──────────────────────────────────────────────────
SCRIPT_COPY="${WORK}/bin/enumerate-actionable.sh"
sed \
  -e "s|/var/run/hive-metrics|${RUN_DIR}|g" \
  -e "s|/var/log/kick-agents.log|${LOG_FILE}|g" \
  -e "s|/usr/bin/gh|${SHIM_DIR}/gh|g" \
  "$SCRIPT" >"$SCRIPT_COPY"
if grep -qE '/var/run/hive-metrics|/var/log/kick-agents.log|/usr/bin/gh' "$SCRIPT_COPY" \
   || ! grep -q "$RUN_DIR" "$SCRIPT_COPY" \
   || ! grep -q "${SHIM_DIR}/gh" "$SCRIPT_COPY"; then
  echo "harness-error: path rewrite did not land — the script's hardcoded paths moved; update the sed above"
  exit 1
fi
# The script sources $(dirname "$0")/hive-config.sh first. A no-op stub next to
# the copy means the real /usr/local/bin/hive-config.sh on a dev box is never
# consulted and PROJECT_* come only from the env this harness sets.
: >"${WORK}/bin/hive-config.sh"

# ── Stub gh ──────────────────────────────────────────────────────────────────
# Records every argv to $FAKE_GH_LOG. `api <endpoint>` is served from
# $FAKE_GH_FIXTURES/<key>.json where key = endpoint path (query string
# stripped, '/' -> '_'), e.g. repos/acme/primary/issues?state=open... ->
# repos_acme_primary_issues.json. A <key>.fail file makes the call fail like a
# 502. GraphQL is keyed by the issue number in the query: graphql_<n>.json.
# Any non-api subcommand (issue edit / issue comment) is accepted silently.
cat >"${SHIM_DIR}/gh" <<'STUBEOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${FAKE_GH_LOG}"
[ "${1:-}" = "api" ] || exit 0
shift
endpoint="${1:-}"; shift
jqf=""
query=""
while [ $# -gt 0 ]; do
  case "$1" in
    --jq) jqf="${2:-}"; shift 2 ;;
    -f)   query="${2:-}"; shift 2 ;;
    *)    shift ;;
  esac
done
path="${endpoint%%\?*}"
if [ "$path" = "graphql" ]; then
  num="$(printf '%s' "$query" | grep -o 'issue(number:[0-9]*)' | tr -dc '0-9')"
  key="graphql_${num}"
else
  key="$(printf '%s' "$path" | tr '/' '_')"
fi
if [ -f "${FAKE_GH_FIXTURES}/${key}.fail" ]; then
  echo "gh: HTTP 502 Bad Gateway (stub failure for ${key})" >&2
  exit 1
fi
fixture="${FAKE_GH_FIXTURES}/${key}.json"
if [ ! -f "$fixture" ]; then
  case "$path" in
    */files) data="[]" ;;
    *) echo "stub gh: no fixture for ${key}" >&2; exit 1 ;;
  esac
else
  data="$(cat "$fixture")"
fi
if [ -n "$jqf" ]; then
  printf '%s' "$data" | jq -r "$jqf"
else
  printf '%s\n' "$data"
fi
STUBEOF
chmod +x "${SHIM_DIR}/gh"

# The retry loop sleeps RETRY_DELAY_SECS between attempts. Stubbing sleep keeps
# the failure cases under a second; the retry COUNT is asserted from the log.
printf '#!/bin/sh\nexit 0\n' >"${SHIM_DIR}/sleep"
chmod +x "${SHIM_DIR}/sleep"

# Dev-box conveniences only: ubuntu-latest has util-linux flock, GNU date and
# GNU xargs; macOS has no `flock`, no `date -Is`, and a BSD xargs whose `-I`
# caps the assembled command line at 255 bytes ("command line cannot be
# assembled, too long") — the script's `xargs -P 8 -I {} bash -c '<script>'`
# fan-outs are longer than that. None of these is a contract under test; on
# CI (and in the production containers) the real tools run.
if ! xargs --version >/dev/null 2>&1; then
  # Sequential subset of GNU `xargs -P N -I REPL cmd args...`.
  cat >"${SHIM_DIR}/xargs" <<'XARGSEOF'
#!/usr/bin/env bash
# Sequential subset of GNU `xargs -P N [-I REPL | -n 1] cmd args...`.
# -I REPL  : substitute REPL everywhere in the initial arguments (the GNU
#            behaviour that caused #5528 — kept so the ADOPTERS batch at
#            line ~208, which legitimately uses -I, still runs here).
# -n 1     : append the line as one extra argument, leaving the initial
#            arguments byte-for-byte untouched. The SHA re-check uses this
#            form; a shim that only understood -I would silently fail the
#            fixed script and make this whole section vacuous.
repl=""
nargs=""
while [ $# -gt 0 ]; do
  case "$1" in
    -P) shift 2 ;;
    -I) repl="$2"; shift 2 ;;
    -n) nargs="$2"; shift 2 ;;
    *)  break ;;
  esac
done
if [ -z "$repl" ] && [ "$nargs" != "1" ]; then
  echo "xargs shim: unsupported invocation (need -I REPL or -n 1)" >&2
  exit 2
fi
while IFS= read -r line; do
  [ -n "$line" ] || continue
  args=()
  if [ -n "$repl" ]; then
    for a in "$@"; do args+=("${a//"$repl"/$line}"); done
  else
    args=("$@" "$line")
  fi
  "${args[@]}"
done
exit 0
XARGSEOF
  chmod +x "${SHIM_DIR}/xargs"
fi
if ! command -v flock >/dev/null 2>&1; then
  printf '#!/bin/sh\nexit 0\n' >"${SHIM_DIR}/flock"
  chmod +x "${SHIM_DIR}/flock"
fi
if ! date -Is >/dev/null 2>&1; then
  REAL_DATE="$(command -v date)"
  cat >"${SHIM_DIR}/date" <<DATEEOF
#!/bin/sh
[ "\${1:-}" = "-Is" ] && exec "${REAL_DATE}" -u +%Y-%m-%dT%H:%M:%S+00:00
exec "${REAL_DATE}" "\$@"
DATEEOF
  chmod +x "${SHIM_DIR}/date"
fi

# ── Fixtures ─────────────────────────────────────────────────────────────────
# Two repos so cross-repo behaviour is visible: acme/primary is
# PROJECT_PRIMARY_REPO (SHA enforcement + SLA apply), acme/secondary is not.
# created_at values are years old so every primary-repo issue is an SLA
# violation and the cross-repo sort order is deterministic.
BASE="${FIX_ROOT}/base"
mkdir -p "$BASE"

issue() { # issue <number> <title> <login> <type> <labels-json> <body> <created> [pull_request]
  local pr=""
  [ "${8:-}" = "pr" ] && pr=',"pull_request":{"url":"https://api.github.com/x"}'
  printf '{"number":%s,"title":"%s","body":"%s","user":{"login":"%s","type":"%s"},"created_at":"%s","labels":%s,"assignees":[{"login":"someone"}],"html_url":"https://github.com/x/%s"%s}' \
    "$1" "$2" "$6" "$3" "$4" "$7" "$5" "$1" "$pr"
}
labels() { # labels <name>... -> [{"name":..},..]
  local out="[" sep=""
  for l in "$@"; do out="${out}${sep}{\"name\":\"${l}\"}"; sep=","; done
  printf '%s]' "$out"
}
pr() { # pr <number> <title> <login> <labels-json> <draft> <created>
  printf '{"number":%s,"title":"%s","user":{"login":"%s"},"created_at":"%s","labels":%s,"draft":%s,"html_url":"https://github.com/x/pull/%s"}' \
    "$1" "$2" "$3" "$6" "$4" "$5" "$1"
}
files() { # files <name>... -> [{"filename":..},..]
  local out="[" sep=""
  for f in "$@"; do out="${out}${sep}{\"filename\":\"${f}\"}"; sep=","; done
  printf '%s]' "$out"
}

{
  echo "["
  issue 101 "plain bug from the hive author"  hive-bot          User "$(labels kind/bug)"              "no sha here"                      2024-01-02T00:00:00Z; echo ","
  issue 102 "on-hold issue"                   hive-bot          User "$(labels on-hold)"               "x"                                2024-01-03T00:00:00Z; echo ","
  issue 103 "Hold/Review mixed case"          hive-bot          User "$(labels Hold/Review)"           "x"                                2024-01-04T00:00:00Z; echo ","
  issue 104 "blocked issue (#4772)"           hive-bot          User "$(labels blocked)"               "x"                                2024-01-05T00:00:00Z; echo ","
  issue 105 "do-not-merge issue"              hive-bot          User "$(labels do-not-merge)"          "x"                                2024-01-06T00:00:00Z; echo ","
  issue 106 "auto-qa tuning report"           hive-bot          User "$(labels auto-qa-tuning-report)" "x"                                2024-01-07T00:00:00Z; echo ","
  issue 107 "LFX mentorship task"             hive-bot          User "$(labels "LFX Mentorship")"      "x"                                2024-01-08T00:00:00Z; echo ","
  issue 108 "a PR seen via the issues API"    hive-bot          User "$(labels)"                       "x"                                2024-01-09T00:00:00Z pr; echo ","
  issue 109 "external report without SHA"     newcomer          User "$(labels kind/bug)"              "It crashes on startup"            2024-01-10T00:00:00Z; echo ","
  issue 110 "external report with SHA"        newcomer          User "$(labels kind/bug)"              "Running at commit abc1234def"     2024-01-11T00:00:00Z; echo ","
  issue 111 "bot-authored issue without SHA"  "dependabot[bot]" Bot  "$(labels)"                       "bump things"                      2024-01-12T00:00:00Z
  echo "]"
} >"${BASE}/repos_acme_primary_issues.json"

{
  echo "["
  issue 201 "external issue in a non-primary repo" newcomer User "$(labels)"     "no sha, and that is fine here" 2024-01-01T00:00:00Z; echo ","
  issue 202 "held issue in secondary"              hive-bot User "$(labels hold)" "x"                             2024-01-13T00:00:00Z
  echo "]"
} >"${BASE}/repos_acme_secondary_issues.json"

{
  echo "["
  pr 301 "ready PR"                     hive-bot "$(labels)"              false 2024-02-01T00:00:00Z; echo ","
  pr 302 "draft PR"                     hive-bot "$(labels)"              true  2024-02-02T00:00:00Z; echo ","
  pr 303 "held PR"                      hive-bot "$(labels hold)"         false 2024-02-03T00:00:00Z; echo ","
  pr 304 "ADOPTERS PR"                  hive-bot "$(labels)"              false 2024-02-04T00:00:00Z; echo ","
  pr 305 "do-not-merge PR"              hive-bot "$(labels do-not-merge)" false 2024-02-05T00:00:00Z; echo ","
  pr 306 "blocked PR (#4772)"           hive-bot "$(labels blocked)"      false 2024-02-06T00:00:00Z
  echo "]"
} >"${BASE}/repos_acme_primary_pulls.json"

{
  echo "["
  pr 401 "ready PR in secondary"        hive-bot "$(labels)"              false 2024-02-07T00:00:00Z; echo ","
  pr 402 "lowercase adopters PR"        hive-bot "$(labels)"              false 2024-02-08T00:00:00Z
  echo "]"
} >"${BASE}/repos_acme_secondary_pulls.json"

files README.md src/main.go       >"${BASE}/repos_acme_primary_pulls_301_files.json"
files docs/ADOPTERS.md            >"${BASE}/repos_acme_primary_pulls_304_files.json"
files src/other.go                >"${BASE}/repos_acme_primary_pulls_305_files.json"
files src/main.go                 >"${BASE}/repos_acme_secondary_pulls_401_files.json"
files adopters.md                 >"${BASE}/repos_acme_secondary_pulls_402_files.json"

# Re-check fixture for #109, BASE state: the reporter has not answered yet, so
# the re-check must find no SHA and leave the hold in place. This must agree
# with the REST fixture that made #109 eligible for the hold — before #5528
# was fixed the re-check never ran, so the two could disagree unnoticed.
cat >"${BASE}/graphql_109.json" <<'EOF'
{"data":{"repository":{"issue":{"state":"OPEN","body":"It crashes on startup","author":{"login":"newcomer"},
 "comments":{"nodes":[{"author":{"login":"hive-bot"},"body":"please add the SHA"}]}}}}}
EOF

# ── Runner ───────────────────────────────────────────────────────────────────
# run_enum <fixture-dir> [VAR=value...] — runs the copy against the given
# fixtures, resets the gh log first, echoes the exit code.
run_enum() {
  local fixtures="$1"; shift
  : >"$GH_LOG"
  env "$@" \
    PATH="${SHIM_DIR}:${PATH}" \
    HOME="${WORK}/home" \
    FAKE_GH_LOG="$GH_LOG" \
    FAKE_GH_FIXTURES="$fixtures" \
    bash "$SCRIPT_COPY" >"${WORK}/stdout" 2>"${WORK}/stderr"
  echo $?
}
# Default project config for every run unless overridden.
PROJ=(PROJECT_REPOS="acme/primary acme/secondary" PROJECT_PRIMARY_REPO="acme/primary" PROJECT_AI_AUTHOR="hive-bot")

jget() { jq -r "$1" "$OUT" 2>/dev/null; }
gh_calls() { grep -cF -- "$1" "$GH_LOG" 2>/dev/null || true; }

assert_eq() { # assert_eq <label> <got> <want>
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "got '$2', want '$3'"; fi
}

echo "=== enumerate-actionable.sh contract tests ==="

# ── Run 1: fresh state ───────────────────────────────────────────────────────
rc="$(run_enum "$BASE" "${PROJ[@]}")"
assert_eq "script exits 0 and writes actionable.json" "${rc}:$( [ -s "$OUT" ] && echo written )" "0:written"
if ! jq -e . "$OUT" >/dev/null 2>&1; then
  echo "harness-error: actionable.json is not valid JSON; stderr was:"; cat "${WORK}/stderr"
  exit 1
fi
[ -e "${OUT}.tmp" ] && fail "temp file is renamed away, not left behind" || pass "temp file is renamed away, not left behind"

# ── 1. Issue label exclusions (header block + #4772) ─────────────────────────
echo "-- issue label exclusions --"
ISSUE_NUMS="$(jget '[.issues.items[].number] | join(",")')"
assert_eq "POSITIVE CONTROL: unlabeled-for-exclusion issues are enumerated, sorted by created_at across repos" \
  "$ISSUE_NUMS" "201,101,110,111"
for excl in "102:on-hold" "103:Hold/Review (case-insensitive substring)" "104:blocked (#4772)" "105:do-not-merge" "106:auto-qa-tuning-report" "107:LFX* prefix" "202:hold in the second repo"; do
  n="${excl%%:*}"; why="${excl#*:}"
  if [[ ",${ISSUE_NUMS}," == *",${n},"* ]]; then fail "issue #${n} excluded: ${why}"; else pass "issue #${n} excluded: ${why}"; fi
done
assert_eq "issues.count equals the number of items" "$(jget '.issues.count')" "$(jget '.issues.items | length')"

# ── 2. PRs returned by the issues API are not issues ─────────────────────────
echo "-- issues endpoint: pull requests filtered out by .pull_request == null --"
if [[ ",${ISSUE_NUMS}," == *",108,"* ]]; then fail "PR-typed issue #108 is not listed as an issue"; else pass "PR-typed issue #108 is not listed as an issue"; fi

# ── 3. PR exclusions: draft, hold, blocked; files fetched only for survivors ─
echo "-- PR exclusions --"
PR_NUMS="$(jget '[.prs.items[].number] | join(",")')"
assert_eq "POSITIVE CONTROL: ready PRs from both repos are enumerated in created_at order" "$PR_NUMS" "301,305,401"
for excl in "302:draft" "303:hold label" "306:blocked label (#4772)"; do
  n="${excl%%:*}"; why="${excl#*:}"
  if [[ ",${PR_NUMS}," == *",${n},"* ]]; then fail "PR #${n} excluded: ${why}"; else pass "PR #${n} excluded: ${why}"; fi
done
# The file-list pass is the expensive one (one API call per PR, #394). It must
# run only for PRs that survived the label/draft pass.
assert_eq "files endpoint hit once for surviving PR #301" "$(gh_calls 'api repos/acme/primary/pulls/301/files')" "1"
assert_eq "files endpoint hit once for surviving PR #401 (repo-qualified path)" "$(gh_calls 'api repos/acme/secondary/pulls/401/files')" "1"
assert_eq "files endpoint NOT hit for draft PR #302" "$(gh_calls 'pulls/302/files')" "0"
assert_eq "files endpoint NOT hit for held PR #303" "$(gh_calls 'pulls/303/files')" "0"
assert_eq "files endpoint NOT hit for blocked PR #306" "$(gh_calls 'pulls/306/files')" "0"
# DOCUMENTS THE CURRENT STATE: the script header (rewritten in #4772) says PRs
# carrying do-not-merge / auto-qa-tuning-report / LFX* are excluded, and the
# emitted exclusions.labels advertises do-not-merge — but the PR filter only
# drops hold-substring and `blocked`. Pinned so the harness fails loudly the
# moment behaviour changes in either direction; see the PR body for the finding.
if [[ ",${PR_NUMS}," == *",305,"* ]]; then
  pass "do-not-merge PR #305 is currently NOT excluded (header/code mismatch, pinned)"
else
  fail "do-not-merge PR #305 is currently NOT excluded (header/code mismatch, pinned)" "behaviour changed — update this pin and the script header together"
fi

# ── 4. ADOPTERS PRs excluded via the file list ───────────────────────────────
echo "-- ADOPTERS file exclusion --"
if [[ ",${PR_NUMS}," == *",304,"* ]]; then fail "PR #304 touching docs/ADOPTERS.md excluded"; else pass "PR #304 touching docs/ADOPTERS.md excluded"; fi
if [[ ",${PR_NUMS}," == *",402,"* ]]; then fail "PR #402 touching adopters.md excluded (case-insensitive)"; else pass "PR #402 touching adopters.md excluded (case-insensitive)"; fi
assert_eq "files endpoint was consulted for #304 (exclusion came from the file list, not a label)" "$(gh_calls 'api repos/acme/primary/pulls/304/files')" "1"

# ── 5. Output shape: what merge-gate.sh / contribute_ws.go depend on ────────
echo "-- output JSON shape --"
assert_eq "top-level keys" "$(jget 'keys | join(",")')" "exclusions,generated_at,hold,issues,prs"
assert_eq "issues keys" "$(jget '.issues | keys | join(",")')" "count,items,sla_violations"
assert_eq "prs keys" "$(jget '.prs | keys | join(",")')" "count,items"
assert_eq "hold keys" "$(jget '.hold | keys | join(",")')" "issues,items,prs,total"
assert_eq "issue item keys (body/author_type stripped, age_minutes added)" \
  "$(jget '.issues.items[0] | keys | join(",")')" "age_minutes,assignees,author,created_at,labels,number,repo,title,url"
assert_eq "PR item keys" "$(jget '.prs.items[0] | keys | join(",")')" "author,created_at,draft,labels,number,repo,title,url"
assert_eq "repo field is the configured org/repo (bare name would break merge-gate lookups)" \
  "$(jget '[.issues.items[].repo, .prs.items[].repo] | unique | join(",")')" "acme/primary,acme/secondary"
assert_eq "labels are flattened to names" "$(jget '.issues.items[] | select(.number==101) | .labels | join(",")')" "kind/bug"
assert_eq "assignees are flattened to logins" "$(jget '.issues.items[] | select(.number==101) | .assignees | join(",")')" "someone"
assert_eq "age_minutes is populated from created_at" "$(jget '[.issues.items[].age_minutes > 30] | all')" "true"
assert_eq "sla_violations counts only PRIMARY-repo issues older than 30m (3 of 4)" "$(jget '.issues.sla_violations')" "3"
assert_eq "hold.items lists every hold-substring issue and PR (drafts/blocked do not count as hold)" \
  "$(jget '[.hold.items[] | "\(.type):\(.number)"] | sort | join(",")')" "issue:102,issue:103,issue:202,pr:303"
assert_eq "hold.total = hold.issues + hold.prs" "$(jget '.hold | "\(.issues)+\(.prs)=\(.total)"')" "3+1=4"
assert_eq "generated_at is an ISO-8601 UTC timestamp" "$(jget '.generated_at | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T.*\\+00:00$")')" "true"

# ── 6. SHA enforcement lifecycle (writes to GitHub — the riskiest contract) ──
echo "-- external-issue SHA hold/unhold lifecycle --"
# Positive controls: every reason NOT to hold must keep the issue enumerated.
for keep in "110:external author WITH a SHA in the body" "111:Bot-typed author without SHA" "101:PROJECT_AI_AUTHOR without SHA" "201:external author without SHA in a NON-primary repo"; do
  n="${keep%%:*}"; why="${keep#*:}"
  if [[ ",${ISSUE_NUMS}," == *",${n},"* ]]; then pass "kept #${n}: ${why}"; else fail "kept #${n}: ${why}"; fi
done
if [[ ",${ISSUE_NUMS}," == *",109,"* ]]; then fail "#109 (external, no SHA, primary repo) withheld from actionable"; else pass "#109 (external, no SHA, primary repo) withheld from actionable"; fi
assert_eq "#109 gets hold added and kind/bug removed, exactly once" \
  "$(gh_calls 'issue edit 109 --repo acme/primary --add-label hold --remove-label kind/bug')" "1"
assert_eq "#109 gets exactly one comment asking for the SHA" "$(gh_calls 'issue comment 109 --repo acme/primary --body')" "1"
assert_eq "the comment mentions the commit SHA" "$(grep -c 'commit SHA' "$GH_LOG" || true)" "1"
assert_eq "no OTHER issue is labeled or commented on" "$(grep -cE '^issue (edit|comment) ' "$GH_LOG" || true)" "2"
MARKER="${RUN_DIR}/sha_hold_posted_acme_primary_109"
[ -f "$MARKER" ] && pass "marker file recorded for #109" || fail "marker file recorded for #109" "expected $MARKER"
# The marker just written is re-checked in the SAME cycle: the re-check loop
# runs after the hold step and only skips markers already stamped resolved=.
assert_eq "the fresh marker is re-checked via GraphQL in the same cycle" "$(gh_calls 'api graphql')" "1"

# Run 2: same GitHub state. The marker must stop a second hold/comment.
rc="$(run_enum "$BASE" "${PROJ[@]}")"
assert_eq "run 2 exits 0" "$rc" "0"
assert_eq "run 2: hold comment is NOT posted again (marker)" "$(gh_calls 'issue comment 109')" "0"
assert_eq "run 2: hold label is NOT re-added (marker)" "$(gh_calls '--add-label hold')" "0"
assert_eq "run 2: unresolved marker is re-checked via GraphQL" "$(gh_calls 'api graphql')" "1"
assert_eq "run 2: POSITIVE CONTROL — the other issues are unchanged" "$(jget '[.issues.items[].number | select(. != 109)] | join(",")')" "201,101,110,111"

# ── #5528: the SHA re-check actually runs now ───────────────────────────────
# These assertions were PINNED to the broken behaviour by #5527. The re-check
# fan-out used to be `xargs -I {} bash -c '<script>' _ {}`; -I substitutes
# EVERY `{}` in the initial arguments, including the python literals
# d.get("data",{}), (issue.get("author") or {}), issue.get("comments",{}) and
# (c.get("author") or {}) INSIDE the quoted script body, so the child python
# was a SyntaxError on every entry and `2>/dev/null || echo "skip"` laundered
# it into a plausible verdict. SHA-UNHOLD therefore never fired. The fan-out
# now uses `-n 1` (the line arrives as $1, the body is untouched) and a
# classifier crash reports `error` on stderr instead of masquerading as
# `skip`.
echo "-- SHA re-check: reporter supplies the SHA (#5528) --"
SHAADDED="${FIX_ROOT}/shaadded"
rm -rf "$SHAADDED"; cp -R "$BASE" "$SHAADDED"
# Same GitHub state EXCEPT the reporter has now answered with a SHA. This is
# the entry the re-check failed on for its entire existence.
cat >"${SHAADDED}/graphql_109.json" <<'EOF'
{"data":{"repository":{"issue":{"state":"OPEN","body":"It crashes on startup","author":{"login":"newcomer"},
 "comments":{"nodes":[{"author":{"login":"hive-bot"},"body":"please add the SHA"},
                      {"author":{"login":"newcomer"},"body":"sure: 9f8e7d6c5b4a"}]}}}}}
EOF
rc="$(run_enum "$SHAADDED" "${PROJ[@]}")"
assert_eq "run 3 exits 0" "$rc" "0"
assert_eq "run 3: SHA in the reporter's comment unholds #109 exactly once" \
  "$(gh_calls 'issue edit 109 --repo acme/primary --remove-label hold --add-label kind/bug')" "1"
if grep -q '^resolved=' "$MARKER"; then
  pass "run 3: marker is stamped resolved="
else
  fail "run 3: marker is stamped resolved=" "marker still unresolved: '$(cat "$MARKER" 2>/dev/null)'"
fi
assert_eq "run 3: #109 is re-admitted to actionable" \
  "$(jget '[.issues.items[].number] | sort | join(",")')" "101,109,110,111,201"

# Run 4: the marker is resolved, so it costs NOTHING — no GraphQL call at all.
# The unresolved marker used to be re-queried every cycle forever.
rc="$(run_enum "$SHAADDED" "${PROJ[@]}")"
assert_eq "run 4: a resolved marker is not re-queried (per-cycle cost is gone)" "${rc}:$(gh_calls 'api graphql')" "0:0"
assert_eq "run 4: no further label churn on #109" "$(gh_calls 'issue edit 109')" "0"

# Closed-issue branch: the other outcome the clobbered classifier could never
# reach. A closed held issue must resolve its marker and stop being queried.
echo "-- SHA re-check: closed issue resolves its marker (#5528) --"
CLOSEDQL="${FIX_ROOT}/closedql"
rm -rf "$CLOSEDQL"; cp -R "$BASE" "$CLOSEDQL"
printf '%s\n' '{"data":{"repository":{"issue":{"state":"CLOSED","body":"nvm","author":{"login":"newcomer"},"comments":{"nodes":[]}}}}}' \
  >"${CLOSEDQL}/graphql_109.json"
rm -f "$RUN_DIR"/sha_hold_posted_*
rc="$(run_enum "$CLOSEDQL" "${PROJ[@]}")"
assert_eq "closed re-check exits 0" "$rc" "0"
if grep -q '^resolved=.* closed' "$MARKER" 2>/dev/null; then
  pass "closed issue stamps the marker resolved=... closed"
else
  fail "closed issue stamps the marker resolved=... closed" "marker: '$(cat "$MARKER" 2>/dev/null)'"
fi
assert_eq "closed issue is NOT unheld (no label writes beyond the initial hold)" \
  "$(gh_calls 'issue edit 109 --repo acme/primary --remove-label hold')" "0"

# NEGATIVE CONTROL for the second masking layer. A payload that is valid JSON
# but has no data.repository.issue must NOT be laundered into a confident
# verdict: no unhold, no resolved= stamp. Before the fix every entry produced
# the string "skip" whether the classifier worked or crashed, so this case and
# a total failure of the loop were indistinguishable.
echo "-- SHA re-check: an unclassifiable payload does not act (#5528 layer 2) --"
BADQL="${FIX_ROOT}/badql"
rm -rf "$BADQL"; cp -R "$BASE" "$BADQL"
printf '%s\n' '{"data":{"repository":{"issue":null}}}' >"${BADQL}/graphql_109.json"
rm -f "$RUN_DIR"/sha_hold_posted_*
rc="$(run_enum "$BADQL" "${PROJ[@]}")"
assert_eq "unclassifiable re-check payload does not fail the run" "$rc" "0"
assert_eq "unclassifiable re-check payload does not unhold #109" \
  "$(gh_calls 'issue edit 109 --repo acme/primary --remove-label hold')" "0"
if grep -q '^resolved=' "$MARKER" 2>/dev/null; then
  fail "unclassifiable re-check payload leaves the marker unresolved" "marker was resolved on a null issue"
else
  pass "unclassifiable re-check payload leaves the marker unresolved"
fi

# Restore the plain held state for the sections below.
rm -rf "$SHAADDED" "$CLOSEDQL" "$BADQL"
rm -f "$RUN_DIR"/sha_hold_posted_*
rc="$(run_enum "$BASE" "${PROJ[@]}")"

# ── 7. Partial API failure: retry, degrade per repo, still publish ──────────
echo "-- partial API failure --"
PARTIAL="${FIX_ROOT}/partial"
cp -R "$BASE" "$PARTIAL"
touch "${PARTIAL}/repos_acme_secondary_issues.fail"
rm -rf "$RUN_DIR"/sha_hold_posted_*   # fresh hold state; avoids re-check noise
rc="$(run_enum "$PARTIAL" "${PROJ[@]}")"
assert_eq "one failing endpoint does not fail the run" "$rc" "0"
assert_eq "failing endpoint is retried MAX_RETRIES (3) times" "$(gh_calls 'api repos/acme/secondary/issues?')" "3"
assert_eq "POSITIVE CONTROL: healthy repo's issues still published" "$(jget '[.issues.items[] | select(.repo=="acme/primary") | .number] | join(",")')" "101,110,111"
assert_eq "failed repo contributes no issues (not stale, not fabricated)" "$(jget '[.issues.items[] | select(.repo=="acme/secondary")] | length')" "0"
assert_eq "failed repo's OTHER endpoint (pulls) is unaffected" "$(jget '[.prs.items[] | select(.repo=="acme/secondary") | .number] | join(",")')" "401"
grep -q 'WARN: 1 API calls failed after retries' "$LOG_FILE" && pass "partial failure is logged as a WARN" || fail "partial failure is logged as a WARN"

# ── 8. Total API failure: preserve the previous actionable.json ─────────────
echo "-- total API failure preserves previous output --"
TOTAL="${FIX_ROOT}/total"
mkdir -p "$TOTAL"
for k in repos_acme_primary_issues repos_acme_primary_pulls repos_acme_secondary_issues repos_acme_secondary_pulls; do touch "${TOTAL}/${k}.fail"; done
printf '{"sentinel":"previous-cycle"}\n' >"$OUT"
rc="$(run_enum "$TOTAL" "${PROJ[@]}")"
assert_eq "all calls failing exits 0 (kick cycle continues)" "$rc" "0"
assert_eq "previous actionable.json is left untouched" "$(jget '.sentinel')" "previous-cycle"
grep -q 'all 4 API calls failed' "$LOG_FILE" && pass "total failure is logged as ERROR with the call count" || fail "total failure is logged as ERROR with the call count"
[ -e "${OUT}.tmp" ] && fail "no temp file left behind on total failure" || pass "no temp file left behind on total failure"

# ── 9. Empty result and missing config ──────────────────────────────────────
echo "-- empty result / missing config --"
EMPTY="${FIX_ROOT}/empty"
mkdir -p "$EMPTY"
for k in repos_acme_primary_issues repos_acme_primary_pulls repos_acme_secondary_issues repos_acme_secondary_pulls; do echo "[]" >"${EMPTY}/${k}.json"; done
rc="$(run_enum "$EMPTY" "${PROJ[@]}")"
assert_eq "no open items: exits 0 and publishes an empty (not missing) document" \
  "${rc}:$(jget '"\(.issues.count)/\(.prs.count)/\(.hold.total)/\(.issues.items|length)"')" "0:0/0/0/0"
assert_eq "no open items: no GitHub writes" "$(grep -cE '^issue ' "$GH_LOG" || true)" "0"

printf '{"sentinel":"before-misconfig"}\n' >"$OUT"
rc="$(run_enum "$EMPTY" PROJECT_REPOS="" PROJECT_PRIMARY_REPO="acme/primary")"
assert_eq "empty PROJECT_REPOS fails closed with exit 1" "$rc" "1"
grep -q 'no repos found in config' "${WORK}/stderr" && pass "misconfig is reported on stderr" || fail "misconfig is reported on stderr" "stderr: $(head -n2 "${WORK}/stderr")"
assert_eq "misconfig does not touch the existing actionable.json" "$(jget '.sentinel')" "before-misconfig"
assert_eq "misconfig makes no GitHub calls" "$(wc -l <"$GH_LOG" | tr -d ' ')" "0"

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
