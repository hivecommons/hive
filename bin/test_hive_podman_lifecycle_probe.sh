#!/usr/bin/env bash
# Contract tests for bin/hive-podman-lifecycle-probe.sh (#4377).
# Run: bash bin/test_hive_podman_lifecycle_probe.sh
#
# Every input is mocked. Stub `systemctl`, `podman`, `loginctl`, `sudo`, and
# `uptime` answer from the environment, and the generator output directory is
# a real temporary directory pointed at by HIVE_LIFECYCLE_GEN_DIR. The whole
# matrix therefore runs on a host with no Podman, no Quadlet, no lingering
# user, and no privileges, and never starts a container.
#
# The cases that matter most are the negative ones. This probe exists because
# three things looked fine and were not -- a clean stop recorded as a crash,
# `systemctl enable` failing on a generated unit, and lingering off while
# `is-enabled` read exactly as it does when lingering is on. Each of those has
# a case here that asserts the probe FAILS, because a probe that cannot fail
# is not evidence.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROBE="${ROOT}/bin/hive-podman-lifecycle-probe.sh"
BASH_BIN="$(command -v bash)"
TEST_TMP="$(mktemp -d)"
trap 'rm -rf "$TEST_TMP"' EXIT

FAKE_BIN="${TEST_TMP}/bin"
mkdir -p "$FAKE_BIN"

# systemctl: `show -p X --value` answers from FAKE_<X>; is-enabled and
# is-active answer from their own variables; `cat` decides whether the unit is
# known at all. Anything the probe should never call in check mode (start,
# stop, restart) is logged and refused unless FAKE_ALLOW_MUTATION=yes, so a
# probe that starts drifting into mutation fails the suite rather than the
# host. The exercise cases set FAKE_ALLOW_MUTATION and get a mutable
# hive-gateway.service whose ActiveState lives in a FILE, because the #4491
# mechanic under test is stateful: a stop of hive.service propagates to the
# gateway through Requires=, and a later start of hive.service does NOT bring
# it back -- Requires= propagates stop, not start.
cat >"${FAKE_BIN}/systemctl" <<'EOF'
#!/usr/bin/env bash
printf 'systemctl %s\n' "$*" >>"$SYSTEMCTL_CALL_LOG"
args=("$@")
[ "${args[0]:-}" = "--user" ] && args=("${args[@]:1}")
gw_state() { cat "$FAKE_GATEWAY_STATE_FILE" 2>/dev/null || printf 'inactive'; }
is_gateway() { local a; for a in "${args[@]}"; do [ "$a" = "hive-gateway.service" ] && return 0; done; return 1; }
case "${args[0]:-}" in
  show)
    prop=""
    for a in "${args[@]}"; do case "$a" in -p) prop="__next__" ;; *) [ "$prop" = "__next__" ] && { prop="$a"; break; } ;; esac; done
    if is_gateway && [ "$prop" = "ActiveState" ]; then gw_state; exit 0; fi
    var="FAKE_$prop"
    printf '%s\n' "${!var-}"
    ;;
  cat)
    if is_gateway; then [ "${FAKE_GATEWAY_KNOWN:-yes}" = "yes" ] || exit 1; exit 0; fi
    [ "${FAKE_UNIT_KNOWN:-yes}" = "yes" ] || exit 1 ;;
  is-enabled)
    # The gate is a real unit with its own enablement state (#4478); the
    # generated hive.service answers `generated` in every configuration.
    if [ "${args[1]:-}" = "hive-boot-gate.service" ]; then
      printf '%s\n' "${FAKE_GATE_IS_ENABLED:-enabled}"
    else
      printf '%s\n' "${FAKE_IS_ENABLED:-generated}"
    fi
    ;;
  is-active)  printf '%s\n' "${FAKE_IS_ACTIVE:-active}" ;;
  is-failed)  printf '%s\n' "${FAKE_IS_FAILED:-inactive}" ;;
  start|stop|restart)
    if [ "${FAKE_ALLOW_MUTATION:-no}" != "yes" ]; then
      printf 'REFUSED-MUTATION %s\n' "$*" >>"$SYSTEMCTL_CALL_LOG"; exit 1
    fi
    case "${args[0]}" in
      stop)
        # Any stop takes the gateway down: directly, or -- when hive.service
        # is the unit stopped -- through Requires= propagation (#4491).
        printf 'inactive' >"$FAKE_GATEWAY_STATE_FILE" ;;
      start)
        if is_gateway; then
          [ "${FAKE_GATEWAY_START_FAILS:-no}" = "yes" ] && exit 1
          printf 'active' >"$FAKE_GATEWAY_STATE_FILE"
        fi
        # Starting hive.service does NOT start the gateway. That asymmetry
        # -- Requires= propagates stop, not start -- is the bug under test.
        ;;
      restart) : ;;
    esac
    exit 0 ;;
  *) exit 0 ;;
esac
EOF

cat >"${FAKE_BIN}/podman" <<'EOF'
#!/usr/bin/env bash
printf 'podman %s\n' "$*" >>"$PODMAN_CALL_LOG"
case "$1" in
  ps)     printf '%s\n' "${FAKE_PS:-hive Up 2 minutes (healthy)}" ;;
  volume) printf '%s\n' "${FAKE_VOLUME:-hive-data}" ;;
  exec)   printf '%s\n' "${FAKE_MARKER:-lifecycle-probe-20260101T000000Z}" ;;
  *) exit 0 ;;
esac
EOF

cat >"${FAKE_BIN}/loginctl" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *Linger*) printf '%s\n' "${FAKE_LINGER:-yes}" ;;
  *) exit 0 ;;
esac
EOF

# sudo just runs the command, so the rootful path exercises the same stubs.
cat >"${FAKE_BIN}/sudo" <<'EOF'
#!/usr/bin/env bash
while [ "${1:-}" = "-n" ]; do shift; done
exec "$@"
EOF

cat >"${FAKE_BIN}/uptime" <<'EOF'
#!/usr/bin/env bash
printf 'up 1 minute\n'
EOF

# curl is the probe's dashboard check on the gateway's published :3001. It
# follows the gateway's simulated state, so a stranded gateway is a dead
# dashboard exactly as on a real host. FAKE_DASHBOARD overrides: `dead` never
# answers, `dies` answers the first call (the pre-probe capture) and then
# stops -- an active gateway in front of a dead listener, #4489's shape.
cat >"${FAKE_BIN}/curl" <<'EOF'
#!/usr/bin/env bash
n="$(cat "${FAKE_CURL_COUNT:-/dev/null}" 2>/dev/null || printf 0)"
n=$((n + 1)); [ -n "${FAKE_CURL_COUNT:-}" ] && printf '%s' "$n" >"$FAKE_CURL_COUNT"
case "${FAKE_DASHBOARD:-follow-gateway}" in
  dead) exit 22 ;;
  dies) [ "$n" -le 1 ] && { printf '{"status":"ok"}'; exit 0; }; exit 7 ;;
esac
[ "$(cat "$FAKE_GATEWAY_STATE_FILE" 2>/dev/null)" = "active" ] && { printf '{"status":"ok"}'; exit 0; }
exit 7
EOF

# The exercise's wait loops are bounded by iteration count, not wall time, so
# a no-op sleep keeps the suite fast without changing what they observe.
cat >"${FAKE_BIN}/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

chmod +x "${FAKE_BIN}"/*

PASS=0
FAIL=0

# Defaults describe a CORRECT install; each case overrides only what it breaks.
reset_env() {
  export SYSTEMCTL_CALL_LOG="${TEST_TMP}/systemctl.log"
  export PODMAN_CALL_LOG="${TEST_TMP}/podman.log"
  : >"$SYSTEMCTL_CALL_LOG"; : >"$PODMAN_CALL_LOG"
  export FAKE_UNIT_KNOWN=yes
  export FAKE_Type=notify
  export FAKE_NotifyAccess=all
  export FAKE_TimeoutStartUSec=5min
  export FAKE_Requires="hive-data-volume.service basic.target"
  export FAKE_SuccessExitStatus="143 TERM"
  export FAKE_Restart=always
  # The generated podman-run line, as `systemctl show -p ExecStart` renders it.
  # It carries the healthcheck, and the healthcheck is what `active` is worth
  # (#4476): the default names BOTH listeners.
  export FAKE_ExecStart="/usr/bin/podman run --name hive --replace --rm --sdnotify=healthy -d --health-cmd curl -sf http://127.0.0.1:3002/api/health && curl -sf http://127.0.0.1:3001/api/health --health-interval 10s ghcr.io/hivecommons/hive:stable"
  export FAKE_ActiveState=active
  export FAKE_SubState=running
  export FAKE_Result=success
  export FAKE_ExecMainStatus=0
  export FAKE_WantedBy=hive-boot.target
  export FAKE_IS_ENABLED=generated
  export FAKE_GATE_IS_ENABLED=enabled
  export FAKE_IS_ACTIVE=active
  export FAKE_LINGER=yes
  export FAKE_ActiveEnterTimestampMonotonic=30000000
  export FAKE_UserspaceTimestampMonotonic=5000000
  # Mutation is refused by default; only the exercise cases opt in.
  export FAKE_ALLOW_MUTATION=no
  export FAKE_GATEWAY_KNOWN=yes
  export FAKE_GATEWAY_START_FAILS=no
  export FAKE_DASHBOARD=follow-gateway
  export FAKE_GATEWAY_STATE_FILE="${TEST_TMP}/gateway.state"
  printf 'active' >"$FAKE_GATEWAY_STATE_FILE"
  export FAKE_CURL_COUNT="${TEST_TMP}/curl.count"
  printf '0' >"$FAKE_CURL_COUNT"
  GEN="${TEST_TMP}/gen"
  rm -rf "$GEN"; mkdir -p "$GEN/hive-boot.target.wants"
  : >"$GEN/hive.service"
  ln -sf ../hive.service "$GEN/hive-boot.target.wants/hive.service"
  export HIVE_LIFECYCLE_GEN_DIR="$GEN"
}

run_probe() {
  PATH="${FAKE_BIN}:${PATH}" NO_COLOR=1 "$BASH_BIN" "$PROBE" "$@" 2>&1
}

# name, expected exit, expected-substring, probe args...
case_expect() {
  local name="$1" want_rc="$2" want_txt="$3"; shift 3
  local out rc
  out="$(run_probe "$@")"; rc=$?
  local why=""
  [ "$rc" != "$want_rc" ] && why="exit $rc, wanted $want_rc"
  if [ -n "$want_txt" ] && ! printf '%s' "$out" | grep -qF -- "$want_txt"; then
    why="${why:+$why; }missing text: $want_txt"
  fi
  if [ -z "$why" ]; then
    PASS=$((PASS + 1)); printf 'ok   %s\n' "$name"
  else
    FAIL=$((FAIL + 1)); printf 'FAIL %s (%s)\n' "$name" "$why"
    printf '%s\n' "$out" | sed 's/^/       | /'
  fi
}

echo "== check mode, a correct install =="
reset_env
case_expect "correct rootless install has no findings" 0 "no findings" check
reset_env
case_expect "correct rootful install has no findings" 0 "no findings" check --rootful

echo
echo "== the #4377 stop-is-not-a-crash contract =="
reset_env; export FAKE_SuccessExitStatus=""
case_expect "unset SuccessExitStatus is a finding" 78 "will leave this unit failed" check
reset_env; export FAKE_SuccessExitStatus="0"
case_expect "SuccessExitStatus without 143 is a finding" 78 "does not contain 143" check

echo
echo "== what 'active' is worth: the healthcheck behind Notify=healthy (#4476) =="
reset_env; export FAKE_ExecStart="/usr/bin/podman run --name hive --sdnotify=healthy -d --health-cmd curl -sf http://127.0.0.1:3002/api/health --health-interval 10s img"
case_expect "probing one listener is a finding" 78 "can report healthy while the dashboard is dead" check
reset_env; export FAKE_ExecStart="/usr/bin/podman run --name hive --sdnotify=healthy -d --health-cmd \"curl\\x20-sf\\x20http://127.0.0.1:3002/api/health\\x20&&\\x20curl\\x20-sf\\x20http://127.0.0.1:3001/api/health\" --health-interval 10s img"
case_expect "the Quadlet-escaped rendering is read the same way" 0 "probes both listeners" check
reset_env; export FAKE_ExecStart="/usr/bin/podman run --name hive --sdnotify=healthy -d ghcr.io/hivecommons/hive:stable"
case_expect "no healthcheck at all is a finding" 78 "reports started, not healthy" check

echo "== the pairing: SuccessExitStatus only works with Restart=always =="
reset_env; export FAKE_Restart="on-failure"
case_expect "on-failure plus SuccessExitStatus is a finding" 78 "would leave this unit inactive/dead/success" check
reset_env; export FAKE_Restart="on-failure" FAKE_SuccessExitStatus=""
case_expect "on-failure alone is only a warning, and the missing 143 is the finding" 78 "SuccessExitStatus is unset" check

echo
echo "== boot wiring is read from the generator and linger, never from is-enabled =="
reset_env; rm -f "${TEST_TMP}/gen/hive-boot.target.wants/hive.service"
case_expect "no generated wants symlink is a finding" 78 "will NOT start at boot" check
reset_env; export FAKE_GATE_IS_ENABLED=disabled
case_expect "a disabled boot gate is a finding (#4478)" 78 "nothing starts hive-boot.target" check
reset_env; export FAKE_LINGER=no
case_expect "lingering off is a finding, rootless" 78 "lingering is OFF" check
reset_env; export FAKE_LINGER=no
case_expect "lingering off is NOT a finding, rootful" 0 "needs no lingering" check --rootful
reset_env; export FAKE_LINGER=no FAKE_IS_ENABLED=enabled
case_expect "is-enabled reading 'enabled' does not rescue a non-lingering host" 78 "lingering is OFF" check
reset_env
case_expect "is-enabled is reported as proving nothing" 0 "proves nothing" check

echo
echo "== check mode is read-only =="
reset_env
run_probe check >/dev/null
if grep -q 'REFUSED-MUTATION' "$SYSTEMCTL_CALL_LOG"; then
  FAIL=$((FAIL + 1)); printf 'FAIL check mode attempted a start/stop/restart\n'
else
  PASS=$((PASS + 1)); printf 'ok   check mode ran no start, stop, or restart\n'
fi

echo
echo "== unit not installed =="
reset_env; export FAKE_UNIT_KNOWN=no
case_expect "an uninstalled unit is a finding, not a crash" 78 "is not known to this manager" check

echo
echo "== a failed unit is reported =="
reset_env; export FAKE_ActiveState=failed FAKE_SubState=failed FAKE_Result=exit-code FAKE_ExecMainStatus=143
case_expect "failed state is a finding" 78 "the unit is in failed state" check

echo
echo "== boot-check =="
reset_env
case_expect "active shortly after boot is evidence of boot persistence" 0 "became active 25s after userspace started" boot-check
reset_env; export FAKE_ActiveEnterTimestampMonotonic=99999000000
case_expect "active far after boot is refused as boot evidence" 0 "too late to be the boot sequence" boot-check
reset_env; export FAKE_IS_ACTIVE=inactive FAKE_ActiveState=inactive FAKE_SubState=dead
case_expect "not active after boot is a finding" 78 "is NOT active after boot" boot-check
reset_env; export FAKE_IS_ACTIVE=inactive FAKE_ActiveState=inactive FAKE_LINGER=no
case_expect "a failed boot-check explains itself with the boot wiring" 78 "lingering is OFF" boot-check

echo
echo "== invocation =="
reset_env
case_expect "an unknown argument is EX_USAGE" 64 "unknown argument" --nonsense

echo
echo "== exercise restores the gateway it strands (#4491) =="
# The stop of hive.service propagates to hive-gateway.service through
# Requires=; the later start does not bring it back. Before the fix the
# exercise ended right there: gateway inactive, :3001 dead, "no findings",
# exit 0. The restore step must start the gateway, verify :3001 answers, and
# turn any failure to do so into a finding.
reset_env; export FAKE_ALLOW_MUTATION=yes
case_expect "exercise ends with the gateway active and no findings" 0 "hive-gateway.service is active, as it was before the probe" exercise
reset_env; export FAKE_ALLOW_MUTATION=yes
case_expect "exercise re-verifies that the dashboard answers" 0 "the dashboard answers on" exercise

# The restore must actually START the gateway, not merely observe one that
# never went down: after the run, the stop propagation must have happened and
# a start of the gateway must be in the log.
reset_env; export FAKE_ALLOW_MUTATION=yes
run_probe exercise >/dev/null
if grep -q 'systemctl --user start hive-gateway.service' "$SYSTEMCTL_CALL_LOG" \
   && [ "$(cat "$FAKE_GATEWAY_STATE_FILE")" = "active" ]; then
  PASS=$((PASS + 1)); printf 'ok   the restore step started the stranded gateway\n'
else
  FAIL=$((FAIL + 1)); printf 'FAIL the restore step never started hive-gateway.service\n'
  sed 's/^/       | /' "$SYSTEMCTL_CALL_LOG"
fi

reset_env; export FAKE_ALLOW_MUTATION=yes FAKE_GATEWAY_START_FAILS=yes
case_expect "a gateway that cannot be restored is a finding, not silence" 78 "could NOT be restored" exercise
reset_env; export FAKE_ALLOW_MUTATION=yes FAKE_GATEWAY_START_FAILS=yes
out="$(run_probe exercise)"
if printf '%s' "$out" | grep -qF 'no findings'; then
  FAIL=$((FAIL + 1)); printf 'FAIL "no findings" printed over a stack the probe broke\n'
  printf '%s\n' "$out" | sed 's/^/       | /'
else
  PASS=$((PASS + 1)); printf 'ok   "no findings" is never printed over a stack the probe broke\n'
fi

reset_env; export FAKE_ALLOW_MUTATION=yes FAKE_DASHBOARD=dies
case_expect "an active gateway in front of a dead :3001 is still a finding" 78 "does NOT now" exercise

reset_env; export FAKE_ALLOW_MUTATION=yes FAKE_GATEWAY_KNOWN=no FAKE_DASHBOARD=dead
case_expect "no gateway unit installed means nothing to restore, and no finding" 0 "not known to this manager; nothing to restore" exercise

reset_env; export FAKE_ALLOW_MUTATION=yes; printf 'inactive' >"$FAKE_GATEWAY_STATE_FILE"
case_expect "a gateway that was already down is left down" 0 "leaving it that way" exercise

echo
printf '%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
