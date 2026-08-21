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
# stop, restart) is logged and refused, so a probe that starts drifting into
# mutation fails the suite rather than the host.
cat >"${FAKE_BIN}/systemctl" <<'EOF'
#!/usr/bin/env bash
printf 'systemctl %s\n' "$*" >>"$SYSTEMCTL_CALL_LOG"
args=("$@")
[ "${args[0]:-}" = "--user" ] && args=("${args[@]:1}")
case "${args[0]:-}" in
  show)
    prop=""
    for a in "${args[@]}"; do case "$a" in -p) prop="__next__" ;; *) [ "$prop" = "__next__" ] && { prop="$a"; break; } ;; esac; done
    var="FAKE_$prop"
    printf '%s\n' "${!var-}"
    ;;
  cat)       [ "${FAKE_UNIT_KNOWN:-yes}" = "yes" ] || exit 1 ;;
  is-enabled) printf '%s\n' "${FAKE_IS_ENABLED:-generated}" ;;
  is-active)  printf '%s\n' "${FAKE_IS_ACTIVE:-active}" ;;
  is-failed)  printf '%s\n' "${FAKE_IS_FAILED:-inactive}" ;;
  start|stop|restart)
    printf 'REFUSED-MUTATION %s\n' "$*" >>"$SYSTEMCTL_CALL_LOG"; exit 1 ;;
  *) exit 0 ;;
esac
EOF

cat >"${FAKE_BIN}/podman" <<'EOF'
#!/usr/bin/env bash
printf 'podman %s\n' "$*" >>"$PODMAN_CALL_LOG"
case "$1" in
  ps)     printf '%s\n' "${FAKE_PS:-hive Up 2 minutes (healthy)}" ;;
  volume) printf '%s\n' "${FAKE_VOLUME:-hive-data}" ;;
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
  export FAKE_ExecStart="/usr/bin/podman run --name hive --replace --rm --sdnotify=healthy -d --health-cmd curl -sf http://127.0.0.1:3002/api/health && curl -sf http://127.0.0.1:3001/api/health --health-interval 10s ghcr.io/kubestellar/hive:stable"
  export FAKE_ActiveState=active
  export FAKE_SubState=running
  export FAKE_Result=success
  export FAKE_ExecMainStatus=0
  export FAKE_WantedBy=default.target
  export FAKE_IS_ENABLED=generated
  export FAKE_IS_ACTIVE=active
  export FAKE_LINGER=yes
  export FAKE_ActiveEnterTimestampMonotonic=30000000
  export FAKE_UserspaceTimestampMonotonic=5000000
  GEN="${TEST_TMP}/gen"
  rm -rf "$GEN"; mkdir -p "$GEN/default.target.wants"
  : >"$GEN/hive.service"
  ln -sf ../hive.service "$GEN/default.target.wants/hive.service"
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
reset_env; export FAKE_ExecStart="/usr/bin/podman run --name hive --sdnotify=healthy -d ghcr.io/kubestellar/hive:stable"
case_expect "no healthcheck at all is a finding" 78 "reports started, not healthy" check

echo "== the pairing: SuccessExitStatus only works with Restart=always =="
reset_env; export FAKE_Restart="on-failure"
case_expect "on-failure plus SuccessExitStatus is a finding" 78 "would leave this unit inactive/dead/success" check
reset_env; export FAKE_Restart="on-failure" FAKE_SuccessExitStatus=""
case_expect "on-failure alone is only a warning, and the missing 143 is the finding" 78 "SuccessExitStatus is unset" check

echo
echo "== boot wiring is read from the generator and linger, never from is-enabled =="
reset_env; rm -f "${TEST_TMP}/gen/default.target.wants/hive.service"
case_expect "no generated wants symlink is a finding" 78 "will NOT start at boot" check
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
printf '%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
