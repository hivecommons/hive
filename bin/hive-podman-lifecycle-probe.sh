#!/usr/bin/env bash
# Quadlet lifecycle probe: stop, start, restart, recreate, boot wiring (#4377).
#
# ADR-0017 chose Quadlet for the Podman persistent lifecycle and #4354 shipped
# the units. What neither established is what the unit actually REPORTS as an
# operator drives it, and that turned out to be where the surprises live:
#
#   * A clean `systemctl stop` left the unit `failed`, because the Hive
#     entrypoint exits 143 on SIGTERM and systemd's default success set is
#     exit 0 alone. Fixed in hive.container; this probe is what caught it and
#     is what keeps it caught.
#   * `systemctl enable hive.service` does not work at all on these units and
#     never did. Quadlet units are GENERATED, and systemd refuses to enable a
#     generated unit. What wires them to boot is the `[Install]` section inside
#     the .container file, which the generator turns into a
#     `hive-boot.target.wants/` symlink in its own output directory on every
#     `daemon-reload` — plus hive-boot-gate.service, the real unit that starts
#     that target once the boot has settled (#4478). So `is-enabled` reports
#     `generated` for hive.service, never `enabled`,
#     and an operator following "enable it" gets an error they may well read
#     as cosmetic.
#   * For a ROOTLESS install that symlink is necessary and not sufficient: at
#     boot there is no login session, so logind starts `user@UID.service` only
#     for a user with lingering enabled. Without linger the generated symlink
#     is still there, `is-enabled` still says the same thing, and the service
#     simply never runs.
#
# Those last two are why this probe reports boot wiring from the generator
# output and from linger rather than from `is-enabled`: `is-enabled` cannot
# distinguish a host that will bring Hive back from one that will not.
#
# MODES
#
#   check      (default) read-only. Reports unit wiring, boot wiring, and
#              current state. Starts, stops, and changes nothing. Safe on a
#              host that is serving.
#   exercise   drives the full lifecycle -- stop, start, restart, recreate --
#              and prints what the unit reports at each step. THIS STOPS AND
#              STARTS HIVE. Not for a host you care about. A final restore
#              step puts the stack back the way the probe found it -- in
#              particular hive-gateway.service, which Requires= propagates a
#              stop to but which nothing brings back on the later start
#              (#4491) -- and a restore that fails is a FINDING, so the probe
#              never reports "no findings" over a deployment it broke.
#   boot-check reports post-boot state: whether the unit came back, and how
#              long after boot it became active. Run it after a reboot; it
#              reads the journal, so it needs no prior arming.
#
# Rootless by default; --rootful drives the system manager through sudo.
#
# Operator guide, including the measured results: src/docs/podman-quadlet-lifecycle.md
#
# Run: bin/hive-podman-lifecycle-probe.sh [check|exercise|boot-check] [--rootful]
# Exit codes: 0 nothing wrong, 78 at least one finding (EX_CONFIG),
#             64 unusable invocation (EX_USAGE)

set -uo pipefail

EX_USAGE=64
EX_CONFIG=78

MODE="check"
ROOTFUL=0
UNIT="hive.service"
GATEWAY_UNIT="hive-gateway.service"
VOLUME_UNIT="hive-data-volume.service"
VOLUME="hive-data"
CONTAINER="hive"
MARKER="/data/.lifecycle-probe"
# The one published port (src/deploy/quadlet/hive-gateway.container). The URL
# is overridable as a test seam only; on a real host there is nothing else it
# could sensibly point at.
DASHBOARD_URL="${HIVE_LIFECYCLE_DASHBOARD_URL:-http://127.0.0.1:3001/api/health}"

findings=0

c_reset=""; c_bold=""; c_red=""; c_green=""; c_yellow=""
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  c_reset=$'\033[0m'; c_bold=$'\033[1m'; c_red=$'\033[31m'
  c_green=$'\033[32m'; c_yellow=$'\033[33m'
fi

say()  { printf '%s\n' "$*"; }
head1() { printf '\n%s%s%s\n' "$c_bold" "$*" "$c_reset"; }
ok()   { printf '  %sPASS%s  %s\n' "$c_green" "$c_reset" "$*"; }
warn() { printf '  %sWARN%s  %s\n' "$c_yellow" "$c_reset" "$*"; }
bad()  { printf '  %sFAIL%s  %s\n' "$c_red" "$c_reset" "$*"; findings=$((findings + 1)); }
info() { printf '        %s\n' "$*"; }

usage() {
  sed -n '2,/^set -uo/p' "$0" | sed 's/^# \{0,1\}//; $d'
  exit "$EX_USAGE"
}

while [ $# -gt 0 ]; do
  case "$1" in
    check|exercise|boot-check) MODE="$1" ;;
    --rootful) ROOTFUL=1 ;;
    --rootless) ROOTFUL=0 ;;
    -h|--help) usage ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; usage ;;
  esac
  shift
done

# Every systemd and podman call goes through these two, so the rootful and
# rootless paths are the same code with a different prefix rather than two
# transcriptions that can drift apart.
if [ "$ROOTFUL" -eq 1 ]; then
  MODE_LABEL="rootful (system manager)"
  SCTL_LABEL="sudo systemctl"
  sctl()   { sudo systemctl "$@"; }
  pod()    { sudo podman "$@"; }
  # HIVE_LIFECYCLE_GEN_DIR is a test seam only. The generator output directory
  # is not configurable in systemd, so overriding it in anger would report on a
  # directory the manager does not read.
  GEN_DIR="${HIVE_LIFECYCLE_GEN_DIR:-/run/systemd/generator}"
  gen_ls() { sudo find "$GEN_DIR" -name 'hive*' -printf '%y %p\n' 2>/dev/null; }
else
  MODE_LABEL="rootless (user manager, uid $(id -u))"
  SCTL_LABEL="systemctl --user"
  sctl()   { systemctl --user "$@"; }
  pod()    { podman "$@"; }
  GEN_DIR="${HIVE_LIFECYCLE_GEN_DIR:-/run/user/$(id -u)/systemd/generator}"
  gen_ls() { find "$GEN_DIR" -name 'hive*' -printf '%y %p\n' 2>/dev/null; }
fi

show() { sctl show "$UNIT" -p "$1" --value 2>/dev/null; }
gshow() { sctl show "$GATEWAY_UNIT" -p "$1" --value 2>/dev/null; }
gateway_known() { sctl cat "$GATEWAY_UNIT" >/dev/null 2>&1; }
dashboard_up() { curl -sf --max-time 5 "$DASHBOARD_URL" >/dev/null 2>&1; }

state() {
  printf '%s/%s/%s/%s' \
    "$(show ActiveState)" "$(show SubState)" "$(show Result)" "$(show ExecMainStatus)"
}

require_unit() {
  if ! sctl cat "$UNIT" >/dev/null 2>&1; then
    bad "$UNIT is not known to this manager -- install the units and run daemon-reload"
    info "see src/docs/podman-standalone-quadlet.md"
    exit "$EX_CONFIG"
  fi
}

# --- check -----------------------------------------------------------------

check_unit_wiring() {
  head1 "Unit wiring -- $MODE_LABEL"
  local t na tmo req sxs restart
  t="$(show Type)"; na="$(show NotifyAccess)"; tmo="$(show TimeoutStartUSec)"
  req="$(show Requires)"; sxs="$(show SuccessExitStatus)"; restart="$(show Restart)"

  [ "$t" = "notify" ] && ok "Type=notify -- start returns only once /api/health answered" \
                      || bad "Type=$t -- expected notify; Notify=healthy is not in effect"
  [ "$na" = "all" ] && ok "NotifyAccess=all" || bad "NotifyAccess=$na -- expected all"
  info "TimeoutStartUSec=$tmo (must exceed HealthStartPeriod plus the retry budget)"

  case "$req" in
    *"$VOLUME_UNIT"*) ok "Requires=$VOLUME_UNIT -- the volume is ordered before the service" ;;
    *) bad "$VOLUME_UNIT is not in Requires= -- the service may start without its volume" ;;
  esac

  # The #4377 stop-is-not-a-crash contract. 143 is 128+SIGTERM; the Hive
  # entrypoint persists state and exits with it on a clean shutdown.
  case "$sxs" in
    *143*) ok "SuccessExitStatus=$sxs -- a deliberate stop is not recorded as a crash" ;;
    "")    bad "SuccessExitStatus is unset -- 'systemctl stop' will leave this unit failed" ;;
    *)     bad "SuccessExitStatus=$sxs does not contain 143 -- a clean stop exits 143" ;;
  esac

  # Paired with the line above and meaningless without it: once 143 is a
  # success, on-failure no longer covers an externally removed container.
  case "$restart" in
    always) ok "Restart=always -- an externally removed container is recovered" ;;
    on-failure)
      if [ -n "$sxs" ]; then
        bad "Restart=on-failure with SuccessExitStatus set -- an external 'podman rm -f' would leave this unit inactive/dead/success and NOT restart"
      else
        warn "Restart=on-failure"
      fi
      ;;
    *) warn "Restart=$restart" ;;
  esac

  # WHAT `active` IS WORTH HERE (#4476). Everything above establishes that this
  # unit reports readiness rather than spawn -- and this probe, and `boot-check`
  # below, read this unit and nothing else. So it matters which listener the
  # healthcheck reads. The container serves two: the Go API, and the Node auth
  # proxy that src/deploy/nginx.conf dials and every operator reaches. A unit
  # generated from a HealthCmd naming only the first reported `active` and
  # `healthy` for two minutes in which the dashboard refused connections, so a
  # green run of this probe would have been green and wrong.
  #
  # Read from the GENERATED ExecStart, not from the checkout: the installed
  # unit is what this manager will run, and it may predate the fix.
  #
  # Counted over the whole ExecStart rather than parsed out of the --health-cmd
  # argument, deliberately. Quadlet escapes the spaces inside that argument as
  # \x20 and `systemctl show` renders argv[] space-joined, so the argument's
  # boundaries look different depending on which one you read. The count does
  # not care: nothing else on a generated podman-run line is an /api/health URL.
  local exec_start health_cmd probe_count
  exec_start="$(show ExecStart)"
  health_cmd="$(printf '%s' "$exec_start" | grep -c -- '--health-cmd' || true)"
  probe_count="$(printf '%s' "$exec_start" | grep -oE '127\.0\.0\.1:[0-9]+/api/health' | wc -l)"
  if [ "$health_cmd" -eq 0 ]; then
    bad "no --health-cmd in the generated ExecStart -- Notify=healthy then reports started, not healthy"
  elif [ "$probe_count" -ge 2 ]; then
    ok "the healthcheck probes both listeners -- 'healthy' covers the auth proxy the gateway reaches"
  else
    bad "the healthcheck probes one listener -- this unit can report healthy while the dashboard is dead (#4476)"
    info "installed unit predates the fix; reinstall hive.container from the checkout and daemon-reload"
  fi
}

check_boot_wiring() {
  head1 "Boot wiring -- what actually decides whether Hive comes back"

  # Deliberately NOT is-enabled. It reports `generated` for a Quadlet unit in
  # every case -- linger on, linger off, symlink present or absent -- so it
  # cannot answer the question an operator is asking.
  info "is-enabled reports '$(sctl is-enabled "$UNIT" 2>&1)' -- 'generated' is expected and proves nothing"

  local wants
  wants="$(gen_ls | awk '$1=="l"' | grep -c "hive-boot.target.wants/$UNIT")"
  if [ "${wants:-0}" -ge 1 ]; then
    ok "the generator installed hive-boot.target.wants/$UNIT"
    info "from [Install] WantedBy= in hive.container, rewritten on every daemon-reload"
  else
    bad "no hive-boot.target.wants/$UNIT in $GEN_DIR -- this unit will NOT start at boot"
    info "check [Install] WantedBy=hive-boot.target in hive.container, then daemon-reload"
  fi

  # The other half of the wiring since #4478: the symlink above only matters
  # if something starts hive-boot.target, and the only thing that does is
  # hive-boot-gate.service -- a real (not generated) unit, so is-enabled is
  # meaningful for it. Without it Hive never starts at boot; it is also what
  # keeps a rootful Hive off the boot's critical path, so the boot never waits
  # on Hive becoming healthy.
  local gate_state
  gate_state="$(sctl is-enabled hive-boot-gate.service 2>/dev/null || true)"
  if [ "$gate_state" = "enabled" ]; then
    ok "hive-boot-gate.service is enabled -- it starts hive-boot.target after the boot settles (#4478)"
  else
    bad "hive-boot-gate.service is '${gate_state:-missing}' -- nothing starts hive-boot.target, so Hive will NOT start at boot"
    info "install src/deploy/systemd/hive-boot.target and hive-boot-gate.service, then: ${SCTL_LABEL} enable hive-boot-gate.service"
  fi

  if [ "$ROOTFUL" -eq 1 ]; then
    ok "rootful needs no lingering -- the system manager is PID 1"
    info "WantedBy resolves to $(show WantedBy)"
  else
    local linger
    linger="$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null || echo unknown)"
    if [ "$linger" = "yes" ]; then
      ok "lingering is enabled for $(id -un) -- logind starts the user manager at boot"
    else
      bad "lingering is OFF for $(id -un) -- at boot there is no session, so the user manager never starts and Hive never runs"
      info "fix: loginctl enable-linger $(id -un)"
      info "note the generated symlink above is still present and is-enabled still reads the same; neither of them notices this"
    fi
  fi
}

check_current_state() {
  head1 "Current state"
  info "unit        $(state)   (ActiveState/SubState/Result/ExecMainStatus)"
  info "container   $(pod ps --filter "name=^${CONTAINER}$" --format '{{.Names}} {{.Status}}' 2>/dev/null | head -1)"
  info "volume      $(pod volume ls --filter "name=^${VOLUME}$" --format '{{.Name}}' 2>/dev/null | head -1)"
  if [ "$(show ActiveState)" = "failed" ]; then
    bad "the unit is in failed state -- 'journalctl -u $UNIT' has the reason"
  fi
}

do_check() {
  require_unit
  check_unit_wiring
  check_boot_wiring
  check_current_state
}

# --- exercise --------------------------------------------------------------

marker_read()  { pod exec "$CONTAINER" cat "$MARKER" 2>/dev/null; }
marker_write() {
  pod exec "$CONTAINER" sh -c \
    "printf 'lifecycle-probe-%s\n' \"\$(date -u +%Y%m%dT%H%M%SZ)\" > $MARKER" >/dev/null 2>&1
}

# `systemctl start` on this unit returns only once the healthcheck has passed,
# so it needs no polling. Recreate does: RestartSec elapses first, then the
# container starts and stays `activating` until healthy.
wait_active() {
  local limit="${1:-180}" i=0
  while [ "$i" -lt "$limit" ]; do
    [ "$(show ActiveState)" = "active" ] && return 0
    sleep 2; i=$((i + 2))
  done
  return 1
}

wait_dashboard() {
  local limit="${1:-60}" i=0
  while [ "$i" -lt "$limit" ]; do
    dashboard_up && return 0
    sleep 2; i=$((i + 2))
  done
  return 1
}

# Pre-probe state, captured before the first mutation so the restore step has
# something to restore TO. Globals, not locals: the INT/TERM trap reads them.
GATEWAY_HAVE="no"
PRE_UNIT_STATE=""
PRE_GATEWAY_STATE=""
PRE_DASHBOARD="no"

# Put the stack back the way the probe found it, and treat failure to do so as
# a finding. This is the #4491 fix: `systemctl stop hive.service` propagates
# through the gateway's Requires=hive.service, but the later `start
# hive.service` does not -- Requires= propagates stop, not start -- so without
# this step the exercise strands hive-gateway.service inactive, the dashboard
# on :3001 dead, and still prints "no findings". (`restart` is immune: it is
# one systemd job, and the units that Requires= the restarted one come back in
# the same transaction. Only the deliberate stop/start pair in steps 2 and 3
# splits that into two jobs, and those steps are the point of the exercise, so
# the repair belongs here rather than in them.)
restore_stack() {
  head1 "6. restore -- leave the stack as the probe found it"
  local pre_gw=""
  [ "$GATEWAY_HAVE" = "yes" ] && pre_gw=" $GATEWAY_UNIT=$PRE_GATEWAY_STATE"
  info "before the probe: $UNIT=$PRE_UNIT_STATE${pre_gw} dashboard=$PRE_DASHBOARD"

  if [ "$PRE_UNIT_STATE" != "active" ]; then
    # The probe started a stack that was deliberately down; put it back down.
    # The stop propagates through Requires= to the gateway too.
    sctl stop "$UNIT" >/dev/null 2>&1
    if [ "$(show ActiveState)" = "active" ]; then
      bad "$UNIT was $PRE_UNIT_STATE before the exercise and is still running -- could not restore the stopped state"
    else
      ok "$UNIT returned to its pre-probe state ($PRE_UNIT_STATE -- it was not running before, so it is not running now)"
    fi
    return
  fi

  # hive.service first: the gateway Requires= it, so it cannot come back alone.
  if [ "$(show ActiveState)" != "active" ]; then
    sctl start "$UNIT" >/dev/null 2>&1
  fi
  if [ "$(show ActiveState)" = "active" ]; then
    ok "$UNIT is active, as it was before the probe"
  else
    bad "$UNIT was active before the exercise and could NOT be restored: $(state)"
  fi

  local user_flag=""
  [ "$ROOTFUL" -eq 0 ] && user_flag="--user "
  if [ "$GATEWAY_HAVE" = "yes" ]; then
    if [ "$PRE_GATEWAY_STATE" = "active" ]; then
      if [ "$(gshow ActiveState)" != "active" ]; then
        info "starting $GATEWAY_UNIT -- the stop in step 2 propagated to it through Requires=, and the start in step 3 did not bring it back (#4491)"
        sctl start "$GATEWAY_UNIT" >/dev/null 2>&1
      fi
      if [ "$(gshow ActiveState)" = "active" ]; then
        ok "$GATEWAY_UNIT is active, as it was before the probe"
      else
        bad "$GATEWAY_UNIT was active before the exercise and could NOT be restored -- the dashboard on :3001 is DOWN"
        info "fix: systemctl ${user_flag}start $GATEWAY_UNIT"
      fi
    else
      info "$GATEWAY_UNIT was $PRE_GATEWAY_STATE before the probe; leaving it that way"
    fi
  else
    info "$GATEWAY_UNIT is not known to this manager; nothing to restore"
  fi

  # Serving is the assertion an operator actually cares about: an `active`
  # gateway in front of a dead listener is exactly the failure shape #4489
  # describes, so the unit state above is not accepted as the whole answer.
  if [ "$PRE_DASHBOARD" = "yes" ]; then
    if wait_dashboard 60; then
      ok "the dashboard answers on $DASHBOARD_URL, as it did before the probe"
    else
      bad "the dashboard answered on $DASHBOARD_URL before the exercise and does NOT now"
      info "fix: systemctl ${user_flag}start $GATEWAY_UNIT, then curl $DASHBOARD_URL"
    fi
  fi
}

# shellcheck disable=SC2329 # invoked indirectly, via the INT/TERM trap
exercise_interrupted() {
  trap - INT TERM
  head1 "Interrupted"
  bad "the exercise was interrupted mid-lifecycle"
  restore_stack
  finish
}

do_exercise() {
  require_unit
  head1 "Lifecycle exercise -- $MODE_LABEL"
  say "  This stops and starts Hive. Ctrl-C now if that is not what you want."
  say "  A final restore step returns the stack -- including $GATEWAY_UNIT -- to its pre-probe state."

  gateway_known && GATEWAY_HAVE="yes"
  PRE_UNIT_STATE="$(show ActiveState)"
  [ "$GATEWAY_HAVE" = "yes" ] && PRE_GATEWAY_STATE="$(gshow ActiveState)"
  dashboard_up && PRE_DASHBOARD="yes"

  # From here on the probe mutates the host. Whatever happens -- a failed
  # step's early return, a Ctrl-C, a kill -- restore_stack runs before the
  # verdict, and a restore that fails is itself a finding, so the probe can
  # no longer report "no findings" over a deployment it left broken.
  trap exercise_interrupted INT TERM

  exercise_steps

  trap - INT TERM
  restore_stack
}

exercise_steps() {
  local t0 t1 elapsed before after

  head1 "1. start"
  # Timing a start is only meaningful from a cold unit. Started from active,
  # systemd returns immediately without running ExecStart at all, and printing
  # that as a start time would read as a Hive that comes up in milliseconds.
  local was_active="no"
  [ "$(show ActiveState)" = "active" ] && was_active="yes"
  t0=$(date +%s%N); sctl start "$UNIT"; local rc=$?; t1=$(date +%s%N)
  elapsed=$(( (t1 - t0) / 1000000 ))
  if [ "$rc" -eq 0 ] && [ "$(show ActiveState)" = "active" ]; then
    if [ "$was_active" = "yes" ]; then
      ok "start returned $rc, state $(state)"
      info "the unit was ALREADY active, so ${elapsed}ms is a no-op return and not a start time"
      info "step 3 below starts it from cold; that is the number to read"
    else
      ok "start returned $rc after ${elapsed}ms, state $(state)"
      info "with Notify=healthy that return means /api/health answered, not merely that a process spawned"
    fi
  else
    bad "start returned $rc after ${elapsed}ms, state $(state)"
    return
  fi
  marker_write
  before="$(marker_read)"
  info "marker written into the $VOLUME volume: $before"

  head1 "2. stop"
  t0=$(date +%s%N); sctl stop "$UNIT"; t1=$(date +%s%N)
  elapsed=$(( (t1 - t0) / 1000000 ))
  info "stop returned after ${elapsed}ms, state $(state)"
  if [ "$(show ActiveState)" = "failed" ]; then
    bad "a deliberate stop left the unit FAILED -- SuccessExitStatus is missing 143"
  else
    ok "a deliberate stop left the unit $(show ActiveState)/$(show Result), is-failed=$(sctl is-failed "$UNIT" 2>&1)"
  fi
  if [ -n "$(pod volume ls --filter "name=^${VOLUME}$" --format '{{.Name}}' 2>/dev/null)" ]; then
    ok "the $VOLUME volume outlived the container (ExecStop's 'podman rm -v' removes anonymous volumes only)"
  else
    bad "the $VOLUME volume is gone after a stop"
  fi

  head1 "3. start again -- from cold, so this one is a real start time"
  t0=$(date +%s%N); sctl start "$UNIT"; rc=$?; t1=$(date +%s%N)
  elapsed=$(( (t1 - t0) / 1000000 ))
  [ "$rc" -eq 0 ] && ok "start returned $rc after ${elapsed}ms (time to HEALTHY), state $(state)" \
                  || bad "start returned $rc after ${elapsed}ms, state $(state)"
  after="$(marker_read)"
  [ -n "$after" ] && [ "$after" = "$before" ] \
    && ok "the volume reattached: marker still $after" \
    || bad "marker is now '${after:-<absent>}', was '$before' -- the service started on an EMPTY volume"

  head1 "4. restart"
  sctl restart "$UNIT" && ok "state $(state)" || bad "state $(state)"
  after="$(marker_read)"
  [ "$after" = "$before" ] && ok "the volume reattached across a restart" \
                           || bad "marker is now '${after:-<absent>}', was '$before'"

  head1 "5. recreate -- remove the container out from under the running unit"
  pod rm -f "$CONTAINER" >/dev/null 2>&1
  sleep 3
  info "immediately after removal: $(state)"
  if wait_active 180; then
    ok "the unit recovered on its own to $(state)"
    info "Restart=always plus RestartSec is what did that; with on-failure and SuccessExitStatus set it would have stayed down"
  else
    bad "the unit did not return to active within 180s: $(state)"
    return
  fi
  after="$(marker_read)"
  [ "$after" = "$before" ] && ok "the recreated container reattached the existing $VOLUME volume: $after" \
                           || bad "marker is now '${after:-<absent>}', was '$before' -- recreate started on an EMPTY volume"
}

# --- boot-check ------------------------------------------------------------

do_boot_check() {
  require_unit
  head1 "Boot persistence -- $MODE_LABEL"

  local boot_time active_time
  boot_time="$(sctl show -p UserspaceTimestampMonotonic --value 2>/dev/null)"
  # A user manager has no UserspaceTimestamp; fall back to the system one,
  # which is the boot both managers are measured against anyway.
  if [ -z "$boot_time" ] || [ "$boot_time" = "0" ]; then
    boot_time="$(systemctl show -p UserspaceTimestampMonotonic --value 2>/dev/null)"
  fi
  active_time="$(show ActiveEnterTimestampMonotonic)"

  info "uptime      $(uptime -p 2>/dev/null || true)"
  info "booted at   $(uptime -s 2>/dev/null || true)"
  info "is-active   $(sctl is-active "$UNIT" 2>&1)"
  info "state       $(state)"

  if [ "$(sctl is-active "$UNIT" 2>/dev/null)" = "active" ]; then
    ok "$UNIT is active after boot"
    if [ -n "$active_time" ] && [ "$active_time" != "0" ] \
       && [ -n "$boot_time" ] && [ "$active_time" -gt "$boot_time" ]; then
      local delta=$(( (active_time - boot_time) / 1000000 ))
      # A unit an operator started by hand hours after boot also has an
      # ActiveEnterTimestamp later than boot. Only a delta small enough to BE
      # the boot sequence is evidence about boot, so say which one this is
      # rather than presenting every delta as a boot measurement.
      if [ "$delta" -le 600 ]; then
        info "became active ${delta}s after userspace started"
        info "(Notify=healthy, so that is time-to-HEALTHY, not time-to-spawned)"
      else
        warn "active since ${delta}s after userspace started -- too late to be the boot sequence"
        info "this unit was almost certainly started by hand, not by boot; re-run right after a reboot"
      fi
    fi
  else
    bad "$UNIT is NOT active after boot: $(state)"
    check_boot_wiring
  fi
}

finish() {
  head1 "Result"
  if [ "$findings" -eq 0 ]; then
    say "  no findings"
    exit 0
  fi
  printf '  %s%d finding(s)%s\n' "$c_red" "$findings" "$c_reset"
  exit "$EX_CONFIG"
}

case "$MODE" in
  check)      do_check ;;
  exercise)   do_exercise ;;
  boot-check) do_boot_check ;;
esac

finish
