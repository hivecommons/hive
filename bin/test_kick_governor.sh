#!/usr/bin/env bash
# Contract tests for bin/kick-governor.sh.
# Run: bash bin/test_kick_governor.sh
#
# kick-governor.sh is the adaptive kick timer: every 15 minutes it measures
# the actionable backlog, ladders scanner/ci-maintainer/architect/outreach
# through SURGE/BUSY/QUIET/IDLE cadences, downgrades models under token-budget
# pressure, and decides which agent gets kicked (or paused) this tick. It is
# 923 lines, called from bin/hive.sh, and had no tests.
#
# Like bin/test_enumerate_actionable.sh and bin/test_gh_wrapper_gates.sh this
# EXECUTES the script rather than grepping it. The script hardcodes
# /var/run/kick-governor, /var/log/kick-governor.log, /var/log/kick-audit.jsonl,
# /var/run/hive-metrics and /etc/hive with no env override, so the harness runs
# a COPY with exactly those paths rewritten to a temp dir (and asserts the
# rewrite landed — a refactor that renames them must fail here loudly, not
# silently run the tests against the real paths).
#
# Doctrine (audit 6/7): every exclusion/pause assertion sits next to a
# positive control that DOES fire, so a governor that kicks nothing (or kicks
# everything) cannot pass. Hermetic: no network, no sleeps (the buffer-clear
# sleeps and any script sleep are stubbed), never touches /var/run, /data or
# /tmp/hive. tmux is stubbed to "no session" unconditionally — none of the
# contracts pinned here exercise the stuck-buffer/tmux block, and CI images
# are not guaranteed to ship tmux.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${REPO_ROOT}/bin/kick-governor.sh"

PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && echo "        $2"
  FAIL=$((FAIL + 1))
}

# A missing harness dependency must be red, not a silent exit 0 (#5388
# doctrine). ubuntu-latest ships python3; the script itself needs it for
# budget math and queue measurement.
for dep in python3; do
  if ! command -v "$dep" >/dev/null 2>&1; then
    echo "harness-error: $dep is required (script under test needs it for budget/queue math)"
    exit 1
  fi
done

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

STATE_DIR_T="${WORK}/state"          # stands in for /var/run/kick-governor
METRICS_DIR_T="${WORK}/metrics"      # stands in for /var/run/hive-metrics
ETC_HIVE_T="${WORK}/etc-hive"        # stands in for /etc/hive
LOG_FILE_T="${WORK}/kick-governor.log"
AUDIT_LOG_T="${WORK}/kick-audit.jsonl"
SHIM_DIR="${WORK}/shim"
mkdir -p "$STATE_DIR_T" "$METRICS_DIR_T" "$ETC_HIVE_T" "$SHIM_DIR" "${WORK}/bin" "${WORK}/home"

# ── The path-rewritten copies ────────────────────────────────────────────────
# RAW: paths rewritten only. hive_is_paused is left undefined, exactly as the
# real script leaves it — used once, to pin the crash (contract 1).
# MAIN: paths rewritten, PLUS a hive_is_paused() shim injected right after
# `set -euo pipefail`. Every other contract runs against this copy; without
# the shim, every call to _is_agent_paused (6 call sites) aborts the script
# under set -e (verified: bash reports "hive_is_paused: command not found",
# exit 127) before any of the mode/budget/pin logic below it ever runs.
sed_rewrite() {
  sed \
    -e "s|/var/run/kick-governor|${STATE_DIR_T}|g" \
    -e "s|/var/log/kick-governor.log|${LOG_FILE_T}|g" \
    -e "s|/var/log/kick-audit.jsonl|${AUDIT_LOG_T}|g" \
    -e "s|/var/run/hive-metrics|${METRICS_DIR_T}|g" \
    -e "s|/etc/hive|${ETC_HIVE_T}|g" \
    "$SCRIPT"
}

SCRIPT_RAW="${WORK}/bin/kick-governor.raw.sh"
sed_rewrite >"$SCRIPT_RAW"

SCRIPT_COPY="${WORK}/bin/kick-governor.sh"
sed_rewrite | sed "/^set -euo pipefail\$/a\\
hive_is_paused() {\\
  local agent=\"\${1:?agent name required}\"\\
  [[ -f \"${STATE_DIR_T}/paused_\${agent}\" ]] || \\\\\\
  [[ -f \"${STATE_DIR_T}/operator_paused_\${agent}\" ]] || \\\\\\
  [[ -f \"${STATE_DIR_T}/cadence_paused_\${agent}\" ]]\\
}" >"$SCRIPT_COPY"

for copy in "$SCRIPT_RAW" "$SCRIPT_COPY"; do
  if grep -qE '/var/run/kick-governor|/var/log/kick-governor\.log|/var/log/kick-audit\.jsonl|/var/run/hive-metrics|/etc/hive' "$copy"; then
    echo "harness-error: path rewrite did not land in ${copy} — the script's hardcoded paths moved; update sed_rewrite() above"
    exit 1
  fi
done
if ! grep -q 'hive_is_paused()' "$SCRIPT_COPY"; then
  echo "harness-error: hive_is_paused shim injection did not land — 'set -euo pipefail' line moved; update the injection anchor"
  exit 1
fi
chmod +x "$SCRIPT_RAW" "$SCRIPT_COPY"

# The script sources ../config/backends.conf relative to its own dir for
# normalize_model_for_backend/model_tier — carry the real one along so the
# budget/model-selection contracts run against real logic, not a stub.
mkdir -p "${WORK}/config"
cp "${REPO_ROOT}/config/backends.conf" "${WORK}/config/backends.conf"

# ── Stub tools ───────────────────────────────────────────────────────────────
# sleep: the buffer-clear path sleeps 1s+2s; a local dev box may also lack
# passwordless sudo (used once, to touch a lock file on full pin).
printf '#!/bin/sh\nexit 0\n' >"${SHIM_DIR}/sleep"
printf '#!/bin/sh\nexec "$@"\n' >"${SHIM_DIR}/sudo"
chmod +x "${SHIM_DIR}/sleep" "${SHIM_DIR}/sudo"

# tmux: unconditionally "no session" — none of the pinned contracts exercise
# the stuck-buffer block, and CI images are not guaranteed to ship tmux.
cat >"${SHIM_DIR}/tmux" <<'TMUXEOF'
#!/bin/sh
case "$1" in
  has-session) exit 1 ;;
  *) exit 1 ;;
esac
TMUXEOF
chmod +x "${SHIM_DIR}/tmux"

# GNU date is required (-d, -Is). macOS ships BSD date; ubuntu-latest (CI)
# ships GNU date natively and this shim is not used there. Implemented as
# pure /bin/sh + BSD `date -v`/`-j -f`, not a python subprocess wrapper —
# spawning a real subprocess from python hung indefinitely in one dev
# sandbox during calibration; native BSD date has no such issue and this
# path never runs on CI regardless.
if ! date -d "+1 seconds" >/dev/null 2>&1 || ! date -Is >/dev/null 2>&1; then
  REAL_DATE="$(command -v date)"
  cat >"${SHIM_DIR}/date" <<DATEEOF
#!/bin/sh
REAL_DATE="${REAL_DATE}"
case "\$1" in
  -Is)
    exec "\$REAL_DATE" '+%Y-%m-%dT%H:%M:%S%z'
    ;;
  -d)
    spec="\$2"; shift 2
    case "\$spec" in
      +*)
        n="\${spec%% *}"; n="\${n#+}"
        exec "\$REAL_DATE" -v"+\${n}S" "\$@"
        ;;
      *)
        "\$REAL_DATE" -j -f '%Y-%m-%dT%H:%M:%S' "\${spec%%[+Z]*}" "\$@" 2>/dev/null || echo 0
        ;;
    esac
    ;;
  *)
    exec "\$REAL_DATE" "\$@"
    ;;
esac
DATEEOF
  chmod +x "${SHIM_DIR}/date"
fi

# ── notify.sh stub ───────────────────────────────────────────────────────────
# The real notify() (bin/notify.sh) shells out to curl for ntfy/Slack/Discord.
# ntfy()/notify() are called unconditionally on every mode change and kick
# (governor.sh only sources notify.sh, never defines notify() itself), so an
# absent NOTIFY_LIB is not "notifications are skipped" — it is
# "notify: command not found" under set -e. No network in this harness: stub
# notify() as a no-op that just logs the call was made.
NOTIFY_LIB_STUB="${WORK}/bin/notify-stub.sh"
cat >"$NOTIFY_LIB_STUB" <<'NOTIFYEOF'
notify() { echo "notify:$1" >> "${NOTIFY_LOG:-/dev/null}"; }
NOTIFYEOF
NOTIFY_LOG="${WORK}/notify.log"

# ── kick-agents.sh stub ──────────────────────────────────────────────────────
# KICK_SCRIPT is env-overridable in the real script — records every
# invocation to KICK_LOG and exits with $KICK_EXIT (default 0).
KICK_LOG="${WORK}/kicks.log"
KICK_SCRIPT_STUB="${WORK}/bin/kick-agents-stub.sh"
cat >"$KICK_SCRIPT_STUB" <<'KICKEOF'
#!/bin/sh
echo "kicked:$1" >> "${KICK_LOG}"
echo "kick-agents stub output for $1"
exit "${KICK_EXIT:-0}"
KICKEOF
chmod +x "$KICK_SCRIPT_STUB"

# ── Runner ───────────────────────────────────────────────────────────────────
# run_gov [VAR=value...] — runs the (shimmed) main copy, echoes exit code.
run_gov() {
  : >"${WORK}/stdout"; : >"${WORK}/stderr"
  env "$@" \
    PATH="${SHIM_DIR}:${PATH}" \
    HOME="${WORK}/home" \
    KICK_LOG="$KICK_LOG" \
    KICK_SCRIPT="$KICK_SCRIPT_STUB" \
    NOTIFY_LIB="$NOTIFY_LIB_STUB" \
    NOTIFY_LOG="$NOTIFY_LOG" \
    OUTCOME_TRACKER="/nonexistent-outcome-tracker.sh" \
    NOUS_SNAPSHOTS_DIR="${WORK}/nous-snapshots" \
    bash "$SCRIPT_COPY" >"${WORK}/stdout" 2>"${WORK}/stderr"
  echo $?
}
run_gov_raw() {
  env "$@" \
    PATH="${SHIM_DIR}:${PATH}" \
    HOME="${WORK}/home" \
    KICK_LOG="$KICK_LOG" \
    KICK_SCRIPT="$KICK_SCRIPT_STUB" \
    NOTIFY_LIB="$NOTIFY_LIB_STUB" \
    NOTIFY_LOG="$NOTIFY_LOG" \
    OUTCOME_TRACKER="/nonexistent-outcome-tracker.sh" \
    NOUS_SNAPSHOTS_DIR="${WORK}/nous-snapshots" \
    bash "$SCRIPT_RAW" >"${WORK}/stdout-raw" 2>"${WORK}/stderr-raw"
  echo $?
}

reset_state() {
  rm -rf "$STATE_DIR_T" "$METRICS_DIR_T" "$ETC_HIVE_T" "$KICK_LOG"
  mkdir -p "$STATE_DIR_T" "$METRICS_DIR_T" "$ETC_HIVE_T"
  : >"$LOG_FILE_T"
}

write_actionable() { # write_actionable <n-issues> <n-prs>
  local ni="$1" np="$2" i items_i="[]" items_p="[]" arr=()
  for ((i = 0; i < ni; i++)); do arr+=("{\"repo\":\"acme/primary\",\"number\":${i}}"); done
  items_i=$(printf '%s\n' "${arr[@]:-}" | paste -sd, -); [ "$ni" -eq 0 ] && items_i=""
  arr=()
  for ((i = 0; i < np; i++)); do arr+=("{\"repo\":\"acme/primary\",\"number\":${i}}"); done
  items_p=$(printf '%s\n' "${arr[@]:-}" | paste -sd, -); [ "$np" -eq 0 ] && items_p=""
  printf '{"issues":{"items":[%s]},"prs":{"items":[%s]}}\n' "$items_i" "$items_p" \
    >"${METRICS_DIR_T}/actionable.json"
}

assert_eq() { # assert_eq <label> <got> <want>
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "got '$2', want '$3'"; fi
}

# grep -c always prints a count (even "0") but exits 1 on no match, so a bare
# `|| echo 0` fallback double-prints on that path; only a genuinely missing
# file prints nothing. Normalize both to exactly one line.
kick_count() {
  [ -f "$KICK_LOG" ] || { echo 0; return; }
  grep -c "^kicked:$1\$" "$KICK_LOG" 2>/dev/null || true
}

# Matches bin/hive.sh's own AGENTS_ENABLED default (${AGENTS_ENABLED:-"supervisor
# scanner ci-maintainer architect outreach"}, used in 4 places) — see contract
# 7b below for what happens when an operator's AGENTS_ENABLED omits supervisor.
BASE_ENV=(AGENTS_ENABLED="supervisor scanner ci-maintainer architect outreach" HIVE_REPOS="acme/primary")

# Pin the governor's budget clock to one hour before the weekly reset. With
# TOKEN_BUDGET_RESET_DAY=4 (Friday 00:00 local) that is Thursday 23:00:
# hours_left=1, hours_elapsed=167, so the week-pace projection collapses to
# `projected == used` and the >85/>95/>99 ladder assertions below are exact.
# Without this the projection is `used * 168 / hours_elapsed` — 90% used on a
# Friday morning projects to >99% and every ladder step reads budget_critical
# (the suite only passed when run on a Thursday evening).
BUDGET_RESET_DAY_T=4
BUDGET_CLOCK_EPOCH_T="$(python3 -c "
import datetime
now = datetime.datetime.now()
reset_day = $BUDGET_RESET_DAY_T
days_back = (now.weekday() - (reset_day - 1)) % 7
pinned = (now - datetime.timedelta(days=days_back)).replace(hour=23, minute=0, second=0, microsecond=0)
print(int(pinned.timestamp()))
")"
BUDGET_ENV=(TOKEN_BUDGET_WEEKLY=100 TOKEN_BUDGET_SAFETY_PCT=85 TOKEN_BUDGET_RESET_DAY="$BUDGET_RESET_DAY_T" TOKEN_BUDGET_NOW_EPOCH="$BUDGET_CLOCK_EPOCH_T")

echo "=== kick-governor.sh contract tests ==="

# ── 0. Path rewrite sanity: a bare run reaches GOVERNOR DONE ────────────────
reset_state
write_actionable 0 0
rc="$(run_gov "${BASE_ENV[@]}")"
assert_eq "sanity: shimmed copy runs to completion (rewrite + hive_is_paused shim both hold)" "$rc" "0"
grep -q "GOVERNOR DONE" "$LOG_FILE_T" && pass "sanity: GOVERNOR DONE reached" || fail "sanity: GOVERNOR DONE reached" "log: $(cat "$LOG_FILE_T")"

# ── 1. Pause checks FAIL CLOSED with hive_is_paused undefined (#5549 fix 1) ──
# kick-governor.sh calls _is_agent_paused() at 6 call sites (maybe_kick,
# optimize_model_assignment, the model-cadence writer, the buffer-clear loop,
# the stale-status loop). hive_is_paused() itself lives in bin/hive-config.sh,
# which the script sources ONLY when HIVE_REPOS is unset — so whenever
# HIVE_REPOS arrives preset (from /etc/hive/governor.env or the env, the
# normal production configuration) hive_is_paused was never defined.
#
# That did NOT crash the script: every call site is inside an
# `if _is_agent_paused ...; then` or `_is_agent_paused ... && continue`, both
# set -e EXEMPT contexts. So "hive_is_paused: command not found" (exit 127)
# was silently read as "agent is NOT paused" at every call site, and the
# governor kicked paused agents anyway — fail-OPEN on a safety control.
#
# #5549 makes _is_agent_paused self-contained and fail CLOSED: it delegates to
# hive_is_paused when that IS defined, and otherwise checks the same three
# markers directly against STATE_DIR. These assertions run against SCRIPT_RAW
# (no shim injected — hive_is_paused genuinely undefined), which is exactly the
# preset-HIVE_REPOS production shape.
echo "-- pause checks fail CLOSED when hive_is_paused is undefined (#5549) --"
reset_state
write_actionable 0 0
touch "${STATE_DIR_T}/paused_scanner"            # dashboard pause
touch "${STATE_DIR_T}/operator_paused_outreach"  # operator pause
rc="$(run_gov_raw AGENTS_ENABLED="scanner ci-maintainer architect outreach" HIVE_REPOS="acme/primary")"
assert_eq "no hive_is_paused: script still exits 0 (pause handling is not an error path)" "$rc" "0"
grep -q 'hive_is_paused.*not found\|command not found' "${WORK}/stderr-raw" \
  && fail "no hive_is_paused: stderr must NOT carry 'command not found' — the check is self-contained now" "stderr: $(cat "${WORK}/stderr-raw")" \
  || pass "no hive_is_paused: stderr carries no 'command not found' — the check is self-contained now"
grep -q "GOVERNOR DONE" "${WORK}/stderr-raw" \
  && pass "no hive_is_paused: the script still reaches GOVERNOR DONE (does not abort)" \
  || fail "no hive_is_paused: the script still reaches GOVERNOR DONE (does not abort)" "stderr: $(cat "${WORK}/stderr-raw")"
grep -q '^kicked:scanner$' "$KICK_LOG" \
  && fail "no hive_is_paused: dashboard-paused scanner must NOT be kicked (fail closed)" "kicks: $(cat "$KICK_LOG" 2>/dev/null)" \
  || pass "no hive_is_paused: dashboard-paused scanner is NOT kicked (fail closed)"
grep -q '^kicked:outreach$' "$KICK_LOG" \
  && fail "no hive_is_paused: operator-paused outreach must NOT be kicked (fail closed)" "kicks: $(cat "$KICK_LOG" 2>/dev/null)" \
  || pass "no hive_is_paused: operator-paused outreach is NOT kicked (fail closed)"
# POSITIVE CONTROL: the fix must not simply refuse to kick anything. architect
# carries no pause marker and has cadence>0 in idle, so it MUST still be kicked
# on the very same run that skipped the two paused agents.
grep -q '^kicked:architect$' "$KICK_LOG" \
  && pass "no hive_is_paused: POSITIVE CONTROL — the UNpaused architect IS still kicked on the same run" \
  || fail "no hive_is_paused: POSITIVE CONTROL — the UNpaused architect IS still kicked on the same run" "kicks: $(cat "$KICK_LOG" 2>/dev/null)"

# ── 2. AGENTS_ENABLED unset fails closed and loud, not silent ───────────────
echo "-- AGENTS_ENABLED unset: fail-closed under set -u --"
reset_state
write_actionable 0 0
rc="$(run_gov HIVE_REPOS="acme/primary")"   # no AGENTS_ENABLED in env at all
assert_eq "unset AGENTS_ENABLED: exits non-zero, not a silent no-op" "$( [ "$rc" != "0" ] && echo nonzero || echo 0 )" "nonzero"
grep -q 'AGENTS_ENABLED.*unbound variable\|unbound variable' "${WORK}/stderr" \
  && pass "unset AGENTS_ENABLED: set -u reports unbound variable" \
  || fail "unset AGENTS_ENABLED: set -u reports unbound variable" "stderr: $(cat "${WORK}/stderr")"
assert_eq "unset AGENTS_ENABLED: POSITIVE CONTROL — a set value runs clean" "$(run_gov "${BASE_ENV[@]}")" "0"

# ── 3. Mode laddering thresholds (SURGE>20, BUSY>10, QUIET>2, else IDLE) ────
echo "-- mode laddering --"
for case in "0:idle" "2:idle" "3:quiet" "10:quiet" "11:busy" "20:busy" "21:surge" "50:surge"; do
  n="${case%%:*}"; want="${case#*:}"
  reset_state
  write_actionable "$n" 0
  run_gov "${BASE_ENV[@]}" >/dev/null
  got="$(cat "${STATE_DIR_T}/mode" 2>/dev/null || echo MISSING)"
  assert_eq "queue=${n} issues -> mode=${want} (boundary)" "$got" "$want"
done

# ── 4. Cadence lookup: hyphenated agent name -> underscored env var ─────────
echo "-- cadence lookup (ci-maintainer hyphen->underscore) --"
reset_state
write_actionable 25 0   # surge
run_gov "${BASE_ENV[@]}" CADENCE_CI_MAINTAINER_SURGE_SEC=777 >/dev/null
assert_eq "an EXPLICITLY set CADENCE_CI_MAINTAINER_*_SEC IS honoured (hyphen->underscore mapping works)" \
  "$(cat "${STATE_DIR_T}/cadence_ci-maintainer" 2>/dev/null)" "12min"
reset_state
write_actionable 25 0
run_gov "${BASE_ENV[@]}" >/dev/null
assert_eq "POSITIVE CONTROL: scanner is never paused by mode (cadence=15min in every mode)" \
  "$(cat "${STATE_DIR_T}/cadence_scanner" 2>/dev/null)" "15min"
assert_eq "surge mode: architect cadence=0 renders as off" "$(cat "${STATE_DIR_T}/cadence_architect" 2>/dev/null)" "off"

# FIXED in #5571 (was pinned as a bug by #5548). The header comment and
# `priority_agents=(scanner ci-maintainer)` both name the agent ci-maintainer,
# and AGENTS_ENABLED's default in bin/hive.sh is "supervisor scanner
# ci-maintainer architect outreach" — ci-maintainer IS the real agent name.
# But every CADENCE_*_SEC and MODEL_*_* default was written for an agent called
# "reviewer" (CADENCE_REVIEWER_*, MODEL_*_REVIEWER), and
# get_cadence()/get_model_selection() build the lookup key from the AGENT name.
# So ci-maintainer got cadence=0 (mode-paused, forever, in every mode) and its
# model fell through to the hardcoded copilot default. #5571 renames those keys
# to CI_MAINTAINER (keeping REVIEWER as a deprecated alias), which makes the
# ladder documented in the header comment reachable for the first time.
#
# NOTE ON SCOPE: this affects systemd/bare-metal hives only. kick-governor.sh
# is driven by kick-governor.timer (bin/hive.sh) and is NOT copied into the
# container image (src/Dockerfile), where the Go governor in src/pkg/governor
# runs instead and has no CADENCE_* variables at all.
reset_state
write_actionable 0 0   # idle: CADENCE_CI_MAINTAINER_IDLE_SEC defaults to 900 (15min)
run_gov "${BASE_ENV[@]}" >/dev/null
assert_eq "ci-maintainer's idle cadence is now the documented 15min, not 0/off" \
  "$(cat "${STATE_DIR_T}/cadence_ci-maintainer" 2>/dev/null)" "15min"
assert_eq "ci-maintainer IS kicked in idle mode now that its cadence resolves" \
  "$(kick_count ci-maintainer)" "1"

# The idle-mode model default happens to equal the old hardcoded fallback
# (copilot:claude-sonnet-4-6), so idle alone cannot distinguish fixed from
# broken. SURGE can: MODEL_SURGE_CI_MAINTAINER is metered claude:, matching
# "priority agents get metered Claude in surge/busy". Before the fix every
# mode fell through to copilot:.
reset_state
write_actionable 25 0   # surge
run_gov "${BASE_ENV[@]}" CADENCE_CI_MAINTAINER_SURGE_SEC=900 >/dev/null
assert_eq "ci-maintainer's surge model is metered claude:, proving MODEL_SURGE_CI_MAINTAINER is consulted" \
  "$(grep '^BACKEND=' "${STATE_DIR_T}/model_ci-maintainer" 2>/dev/null | cut -d= -f2):$(grep '^MODEL=' "${STATE_DIR_T}/model_ci-maintainer" 2>/dev/null | cut -d= -f2)" \
  "claude:claude-sonnet-4-6"

# Backward compatibility: an operator who set the OLD CADENCE_REVIEWER_* name
# in governor.env must not be silently changed underneath them.
reset_state
write_actionable 0 0
run_gov "${BASE_ENV[@]}" CADENCE_REVIEWER_IDLE_SEC=1800 >/dev/null
assert_eq "deprecated CADENCE_REVIEWER_IDLE_SEC is still honoured as an alias" \
  "$(cat "${STATE_DIR_T}/cadence_ci-maintainer" 2>/dev/null)" "30min"
# ...but the new name wins when both are set.
reset_state
write_actionable 0 0
run_gov "${BASE_ENV[@]}" CADENCE_REVIEWER_IDLE_SEC=1800 CADENCE_CI_MAINTAINER_IDLE_SEC=2700 >/dev/null
assert_eq "new CADENCE_CI_MAINTAINER_* takes precedence over the deprecated alias" \
  "$(cat "${STATE_DIR_T}/cadence_ci-maintainer" 2>/dev/null)" "45min"

# ── 4b. A missing cadence key is LOUD, a deliberate 0 is SILENT (#5571) ─────
# get_cadence used to return `${!var_name:-0}` — so "no cadence configured"
# and "operator deliberately paused this agent" were the same value, and a
# mistyped/unlisted agent name disabled that agent with no error and no log.
# That conflation is the root defect; the ci-maintainer key was one symptom.
echo "-- missing cadence key is distinguishable from a deliberate 0 (#5571) --"
reset_state
write_actionable 0 0
run_gov "${BASE_ENV[@]}" AGENTS_ENABLED="supervisor scanner ci-maintainer architect outreach notanagent" >/dev/null
grep -q "CONFIG ERROR: CADENCE_NOTANAGENT_IDLE_SEC is not defined" "$LOG_FILE_T" \
  && pass "an agent with NO cadence key logs a CONFIG ERROR naming the missing variable" \
  || fail "an agent with NO cadence key logs a CONFIG ERROR naming the missing variable" "log: $(cat "$LOG_FILE_T")"
assert_eq "an agent with NO cadence key is still not kicked (fails safe, but visibly)" \
  "$(kick_count notanagent)" "0"
# POSITIVE CONTROL: a deliberate 0 must stay silent, or every paused agent
# would spam CONFIG ERROR every 15 minutes and the signal would be worthless.
reset_state
write_actionable 0 0
run_gov "${BASE_ENV[@]}" CADENCE_OUTREACH_IDLE_SEC=0 >/dev/null
grep -q "CONFIG ERROR: CADENCE_OUTREACH_IDLE_SEC" "$LOG_FILE_T" \
  && fail "a DELIBERATE cadence of 0 must NOT log a CONFIG ERROR" "log: $(cat "$LOG_FILE_T")" \
  || pass "a DELIBERATE cadence of 0 is honoured silently (no CONFIG ERROR)"
assert_eq "a DELIBERATE cadence of 0 still pauses the agent" "$(kick_count outreach)" "0"

# ── 5. Dashboard pause / operator-resume / cadence=0 handling (maybe_kick) ──
echo "-- pause handling --"
reset_state
write_actionable 0 0   # idle: every agent has cadence>0
touch "${STATE_DIR_T}/paused_scanner"
run_gov "${BASE_ENV[@]}" >/dev/null
assert_eq "dashboard-paused agent is not kicked" "$(kick_count scanner)" "0"
assert_eq "POSITIVE CONTROL: an unpaused agent with cadence>0 and never-kicked-before IS kicked" \
  "$(kick_count outreach)" "1"
grep -q '"agent":"scanner","action":"SKIP","reason":"dashboard-paused"' "$AUDIT_LOG_T" \
  && pass "SKIP audit record carries reason=dashboard-paused" \
  || fail "SKIP audit record carries reason=dashboard-paused" "audit log: $(cat "$AUDIT_LOG_T")"

reset_state
write_actionable 25 0   # surge: architect+outreach cadence=0 (paused by mode)
run_gov "${BASE_ENV[@]}" >/dev/null
assert_eq "cadence=0 (mode-paused) agent is not kicked" "$(kick_count architect)" "0"
[ -f "${STATE_DIR_T}/cadence_paused_architect" ] \
  && pass "cadence=0 writes cadence_paused_<agent> marker" \
  || fail "cadence=0 writes cadence_paused_<agent> marker"

reset_state
write_actionable 25 0
touch "${STATE_DIR_T}/operator_resumed_architect"
run_gov "${BASE_ENV[@]}" >/dev/null
assert_eq "operator-resume overrides cadence=0: does not re-pause (marker NOT (re)written)" \
  "$( [ -f "${STATE_DIR_T}/cadence_paused_architect" ] && echo present || echo absent )" "absent"
assert_eq "operator-resume with cadence=0 still does not KICK (only stops re-pausing)" \
  "$(kick_count architect)" "0"

reset_state
write_actionable 0 0
touch "${STATE_DIR_T}/paused_scanner" "${STATE_DIR_T}/was_paused_scanner"
run_gov "${BASE_ENV[@]}" >/dev/null   # still paused this tick -> stays skipped, was_paused untouched by the un-pause branch
reset_state
write_actionable 0 0
touch "${STATE_DIR_T}/was_paused_scanner"   # paused LAST tick, not paused now
run_gov "${BASE_ENV[@]}" >/dev/null
assert_eq "unpause transition: agent is kicked immediately, ignoring its normal cadence" \
  "$(kick_count scanner)" "1"
assert_eq "unpause transition: was_paused marker is cleared" \
  "$( [ -f "${STATE_DIR_T}/was_paused_scanner" ] && echo present || echo absent )" "absent"

# ── 6. Model lock / pin precedence ───────────────────────────────────────────
echo "-- model lock / pin precedence --"
reset_state
write_actionable 0 0   # idle mode -> MODEL_IDLE_SCANNER default copilot:claude-sonnet-4-6
touch "${STATE_DIR_T}/model_lock_scanner"
printf 'BACKEND=claude\nMODEL=claude-opus-4-6\n' >"${STATE_DIR_T}/model_scanner"
run_gov "${BASE_ENV[@]}" >/dev/null
assert_eq "model_lock_<agent> freezes the model file (manual override wins)" \
  "$(grep '^BACKEND=' "${STATE_DIR_T}/model_scanner" | cut -d= -f2)" "claude"

reset_state
write_actionable 0 0
mkdir -p "$ETC_HIVE_T"
printf 'AGENT_CLI_PINNED=true\n' >"${ETC_HIVE_T}/scanner.env"
run_gov "${BASE_ENV[@]}" >/dev/null
[ -f "${STATE_DIR_T}/model_lock_scanner" ] \
  && pass "AGENT_CLI_PINNED=true creates the lock file (full pin escalates to a standing lock)" \
  || fail "AGENT_CLI_PINNED=true creates the lock file"
assert_eq "POSITIVE CONTROL: an agent with no pin/lock DOES get a model file written" \
  "$( [ -f "${STATE_DIR_T}/model_ci-maintainer" ] && echo written || echo missing )" "written"

# ── 7. Budget pressure ladder (optimize_model_assignment) ───────────────────
echo "-- budget pressure ladder --"
reset_state
write_actionable 25 0   # surge -> MODEL_SURGE_ARCHITECT defaults to claude:claude-opus-4-6
mkdir -p "$METRICS_DIR_T"
# used=90% of a 100-token budget with the clock pinned one hour before reset
# (BUDGET_ENV), so the pace projection is projected == used == 90%.
printf '{"weekly":{"billableTokens":90},"hourlyBurnRate":{"billable":0}}\n' \
  >"${METRICS_DIR_T}/tokens.json"
run_gov "${BASE_ENV[@]}" "${BUDGET_ENV[@]}" >/dev/null
# outreach's own MODEL_*_OUTREACH default is copilot in every mode, so it can
# never actually be observed being downgraded FROM claude; architect's surge
# default (claude:claude-opus-4-6) is the one that exercises this branch.
assert_eq "budget >85% safety: non-priority agent (architect) downgraded to copilot" \
  "$(grep '^BACKEND=' "${STATE_DIR_T}/model_architect" | cut -d= -f2)" "copilot"
assert_eq "budget >85% safety: reason recorded as budget_downgrade" \
  "$(grep '^REASON=' "${STATE_DIR_T}/model_architect" | cut -d= -f2)" "budget_downgrade"
assert_eq "POSITIVE CONTROL: priority agent (scanner) is NOT downgraded at 90% (<95% opus/sonnet ladder threshold)" \
  "$(grep '^BACKEND=' "${STATE_DIR_T}/model_scanner" | cut -d= -f2)" "claude"
# scanner defaults to copilot in idle mode already, so use surge (claude) to see the >95 ladder.
reset_state
write_actionable 25 0   # surge -> MODEL_SURGE_SCANNER=claude:claude-sonnet-4-6
mkdir -p "$METRICS_DIR_T"
printf '{"weekly":{"billableTokens":96},"hourlyBurnRate":{"billable":0}}\n' \
  >"${METRICS_DIR_T}/tokens.json"
run_gov "${BASE_ENV[@]}" "${BUDGET_ENV[@]}" >/dev/null
# #5549 fix 3: the sonnet->haiku downgrade used to be a literal string
# substitution, `${model/sonnet/haiku}`, applied to "claude-sonnet-4-6" — it
# swapped the tier word but left the version suffix untouched, producing
# "claude-haiku-4-6". Every OTHER haiku default in this file
# (MODEL_*_SUPERVISOR, MODEL_QUIET_SCANNER) and config/backends.conf's own
# model_tier() use "claude-haiku-4-5"; 4-6 is not a haiku version that exists,
# so the budget-critical path handed the CLI an unavailable model at exactly
# the moment it existed to protect against runaway spend. downgrade_model_one_tier()
# now maps tier AND version together onto GOVERNOR_MODEL_HAIKU.
assert_eq "budget >95%: sonnet downgrades to the real haiku constant (claude-haiku-4-5), not a version-mangled claude-haiku-4-6" \
  "$(grep '^BACKEND=' "${STATE_DIR_T}/model_scanner" | cut -d= -f2):$(grep '^MODEL=' "${STATE_DIR_T}/model_scanner" | cut -d= -f2)" \
  "claude:claude-haiku-4-5"
# The downgrade target must be a model config/backends.conf actually recognises
# — an assertion on the literal string alone would still pass if a future edit
# swapped in another nonexistent name.
assert_eq "budget >95%: the downgraded model is a tier backends.conf recognises (not 'unknown')" \
  "$(cd "$WORK" && . ./config/backends.conf && model_tier "$(grep '^MODEL=' "${STATE_DIR_T}/model_scanner" | cut -d= -f2)")" \
  "haiku"

reset_state
write_actionable 25 0
mkdir -p "$METRICS_DIR_T"
printf '{"weekly":{"billableTokens":99},"hourlyBurnRate":{"billable":0}}\n' \
  >"${METRICS_DIR_T}/tokens.json"
touch "${METRICS_DIR_T}/budget_ignore"
run_gov "${BASE_ENV[@]}" "${BUDGET_ENV[@]}" >/dev/null
assert_eq "BUDGET_IGNORE_FLAG bypasses every downgrade even above 99%" \
  "$(grep '^BACKEND=' "${STATE_DIR_T}/model_scanner" | cut -d= -f2)" "claude"

# #5549 fix 2: the budget-downgrade loop used to be
# `for agent in outreach architect supervisor` — hardcoded, not derived from
# AGENTS_ENABLED/agents[@]. The `assignments` associative array is populated
# ONLY for agents actually in AGENTS_ENABLED. bin/hive.sh's default
# AGENTS_ENABLED includes supervisor, but it is an operator-configurable env
# var. An operator running without it, whose budget then crossed
# TOKEN_BUDGET_SAFETY_PCT, hit `${assignments[supervisor]}` — an unset
# associative-array key under `set -u` — and the WHOLE
# optimize_model_assignment() call aborted (bare statement, so set -e DOES
# fire here, unlike the hive_is_paused case above). That happens before
# maybe_kick ever runs, so ONE missing name in the hardcoded list meant NO
# agent got kicked that cycle, the moment the budget crossed the threshold.
# The loop now iterates agents[@], and the >95% priority ladder guards its
# lookup with ${assignments[$agent]+set}.
echo "-- budget pressure survives an AGENTS_ENABLED without 'supervisor' (#5549) --"
# surge, so architect's default (claude:claude-opus-4-6) is a NON-copilot
# backend and the >85% loop has something real to downgrade — in idle its
# default is already copilot and the loop correctly skips it, which would make
# the positive control below vacuous.
reset_state
write_actionable 25 0
mkdir -p "$METRICS_DIR_T"
printf '{"weekly":{"billableTokens":90},"hourlyBurnRate":{"billable":0}}\n' \
  >"${METRICS_DIR_T}/tokens.json"
rc="$(run_gov AGENTS_ENABLED="scanner ci-maintainer architect outreach" HIVE_REPOS="acme/primary" \
  "${BUDGET_ENV[@]}")"
assert_eq "budget pressure without supervisor: governor exits 0, does not abort" "$rc" "0"
grep -q "assignments\[.*\]: unbound variable" "${WORK}/stderr" \
  && fail "budget pressure without supervisor: must NOT hit assignments[\$agent]: unbound variable" "stderr: $(cat "${WORK}/stderr")" \
  || pass "budget pressure without supervisor: no assignments[\$agent] unbound-variable crash"
grep -q "GOVERNOR DONE" "${WORK}/stderr" \
  && pass "budget pressure without supervisor: GOVERNOR DONE is reached" \
  || fail "budget pressure without supervisor: GOVERNOR DONE is reached" "stderr: $(cat "${WORK}/stderr")"
# POSITIVE CONTROL: reaching GOVERNOR DONE is not enough — the downgrade the
# loop exists to perform must still actually happen for the agents that ARE
# configured. architect's idle default is copilot already, so assert on the
# recorded reason rather than the backend.
assert_eq "budget pressure without supervisor: the downgrade STILL fires for a configured agent (architect)" \
  "$(grep '^REASON=' "${STATE_DIR_T}/model_architect" 2>/dev/null | cut -d= -f2)" "budget_downgrade"
assert_eq "budget pressure without supervisor: POSITIVE CONTROL — the same scenario WITH supervisor present also exits 0" \
  "$(run_gov "${BASE_ENV[@]}" TOKEN_BUDGET_WEEKLY=100 TOKEN_BUDGET_SAFETY_PCT=85)" "0"

# ── 8. What is written where — the file contract other tooling reads ────────
echo "-- state files written --"
reset_state
write_actionable 5 3   # quiet: 8 total (>2, <=10)
run_gov "${BASE_ENV[@]}" >/dev/null
assert_eq "mode file" "$(cat "${STATE_DIR_T}/mode")" "quiet"
assert_eq "queue_issues file" "$(cat "${STATE_DIR_T}/queue_issues")" "5"
assert_eq "queue_prs file" "$(cat "${STATE_DIR_T}/queue_prs")" "3"
assert_eq "queue_depth file (issues+prs)" "$(cat "${STATE_DIR_T}/queue_depth")" "8"
# busyness_pct is issues-only/threshold*100 (mode is determined from
# get_queue_depth(), which returns measure_queue's total_i — issues only, NOT
# the combined issues+prs total written to the queue_depth FILE): 5 issues *
# 100 / BUSY_THRESHOLD_ISSUES(10) = 50, even though 8 total items are queued.
assert_eq "busyness_pct file is issues-only/threshold*100 (NOT the combined total in the queue_depth file)" \
  "$(cat "${STATE_DIR_T}/busyness_pct")" "50"

# ── 9. get_queue_depth fallback: actionable.json missing -> cached depth ────
echo "-- queue-measurement fallback on missing actionable.json --"
reset_state
write_actionable 25 0
run_gov "${BASE_ENV[@]}" >/dev/null   # seed queue_depth=25 (surge) in state
rm -f "${METRICS_DIR_T}/actionable.json"   # now simulate the collector being down
rm -f "${STATE_DIR_T}/cadence_paused_"*    # avoid stale pause markers muddying this run
run_gov "${BASE_ENV[@]}" >/dev/null
assert_eq "measure_queue falls back to per-repo caches (none present) -> total=0 -> idle, NOT the stale surge depth" \
  "$(cat "${STATE_DIR_T}/mode")" "idle"

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
