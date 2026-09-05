#!/usr/bin/env bash
# Contract tests for bin/hive-podman-update.sh (#4378).
# Run: bash bin/test_hive_podman_update.sh
#
# Every input is mocked. Stub `systemctl`, `podman`, `skopeo`, and `sudo`
# answer from files under a temporary directory, and the Quadlet drop-in
# directory is a real temporary directory pointed at by
# HIVE_UPDATE_QUADLET_DIR. The whole matrix therefore runs on a host with no
# Podman, no Quadlet, and no privileges, and never pulls an image or restarts
# a service.
#
# The cases that matter are the ones about a FAILED update, because that is
# the case the feature exists for. A rollback that only works from a healthy
# unit is not a rollback, so there are cases here asserting that rollback
# skips the entry that just failed, that it stops the failing unit before
# starting (a start job queued behind a bad one waits out TimeoutStartSec),
# and that it refuses rather than half-applies when the earlier image is no
# longer pullable.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UPDATE="${ROOT}/bin/hive-podman-update.sh"
BASH_BIN="$(command -v bash)"
TEST_TMP="$(mktemp -d)"
# HIVE_TEST_KEEP_TMP=1 leaves the mocked drop-in directory and call logs behind,
# which is the only way to see what a failing case actually wrote.
if [ -z "${HIVE_TEST_KEEP_TMP:-}" ]; then
  trap 'rm -rf "$TEST_TMP"' EXIT
else
  trap 'printf "kept: %s\n" "$TEST_TMP"' EXIT
fi

FAKE_BIN="${TEST_TMP}/bin"
mkdir -p "$FAKE_BIN"

DIGEST_OLD="sha256:1111111111111111111111111111111111111111111111111111111111111111"
DIGEST_NEW="sha256:2222222222222222222222222222222222222222222222222222222222222222"
DIGEST_BAD="sha256:3333333333333333333333333333333333333333333333333333333333333333"
# The per-architecture manifest digest of the same tag. Resolving a tag through
# `podman image inspect ... RepoDigests` returns THIS, and pinning it produces
# an amd64-only pin. No case may ever end up writing it.
DIGEST_ARCH="sha256:4444444444444444444444444444444444444444444444444444444444444444"
REPO="ghcr.io/hivecommons/hive"

# systemctl: unit state lives in files so `restart` can change it, which is
# what lets the suite exercise the report the script makes AFTER a restart
# rather than only the command it issued. A successful restart also moves the
# recorded running image to whatever the drop-in now names, the way a real
# recreate does.
cat >"${FAKE_BIN}/systemctl" <<'EOF'
#!/usr/bin/env bash
printf 'systemctl %s\n' "$*" >>"$SYSTEMCTL_CALL_LOG"
args=("$@")
[ "${args[0]:-}" = "--user" ] && args=("${args[@]:1}")
sd="$STATE_DIR"
dropin_image() {
  grep -m1 '^Image=' "${QUADLET_DIR}/hive.container.d/10-image.conf" 2>/dev/null | sed 's/^Image=//'
}
case "${args[0]:-}" in
  show)
    prop=""
    for a in "${args[@]}"; do
      case "$a" in -p) prop="__next__" ;; *) [ "$prop" = "__next__" ] && { prop="$a"; break; } ;; esac
    done
    [ -f "$sd/$prop" ] && cat "$sd/$prop" || printf '\n'
    ;;
  cat)
    # `cat <unit>`: the gateway (#4493) and hive are modelled separately, so a
    # case can remove one without the other.
    if [ "${args[1]:-}" = "hive-gateway.service" ]; then
      [ "${FAKE_GATEWAY_UNIT_KNOWN:-yes}" = "yes" ] || exit 1
      printf '# generated\n[Service]\nExecStart=/usr/bin/podman run --name hive-gateway --rm docker.io/library/nginx@sha256:aaaa\n'
      exit 0
    fi
    [ "${FAKE_UNIT_KNOWN:-yes}" = "yes" ] || exit 1
    img="$(dropin_image)"
    [ -n "$img" ] || img="ghcr.io/hivecommons/hive:stable"
    printf '# /run/user/1000/systemd/generator/hive.service\n'
    printf '[Service]\nExecStart=/usr/bin/podman run --name hive --rm %s\n' "$img"
    ;;
  daemon-reload) : ;;
  is-enabled)
    # podman-auto-update.timer (#4411). FAKE_TIMER_STATE is what the manager
    # reports; FAKE_TIMER_RC lets a case model a host with no podman systemd
    # units installed, which is the `enable` failure path.
    printf '%s\n' "${FAKE_TIMER_STATE:-disabled}"
    [ "${FAKE_TIMER_STATE:-disabled}" = "enabled" ] || exit 1
    ;;
  enable|disable)
    printf '%s\n' "${args[*]}" >>"${sd}/timer.log"
    exit "${FAKE_TIMER_RC:-0}"
    ;;
  stop)
    printf 'inactive\n' >"$sd/ActiveState"; printf 'dead\n' >"$sd/SubState"
    printf 'success\n' >"$sd/Result"
    ;;
  restart|start)
    # The gateway is its own unit with its own start outcome (#4493); its
    # start must never overwrite hive's recorded state.
    if [ "${args[1]:-}" = "hive-gateway.service" ]; then
      exit "${FAKE_GATEWAY_START_RC:-0}"
    fi
    if [ "${FAKE_RESTART_RC:-0}" = "0" ]; then
      printf 'active\n' >"$sd/ActiveState"; printf 'running\n' >"$sd/SubState"
      printf 'success\n' >"$sd/Result"
      dropin_image >"$sd/running_image"
      exit 0
    fi
    printf 'failed\n' >"$sd/ActiveState"; printf 'failed\n' >"$sd/SubState"
    printf 'timeout\n' >"$sd/Result"
    dropin_image >"$sd/running_image"
    exit "${FAKE_RESTART_RC}"
    ;;
  *) exit 0 ;;
esac
EOF

# podman: `pull` fails for any reference containing FAKE_PULL_FAIL_SUBSTR, so a
# rollback target that is no longer in the registry can be tested without one.
# `images --digests` is the fallback resolution path and deliberately reports
# the manifest-LIST digest, while `image inspect` reports the per-arch one --
# the same disagreement measured on the real registry.
cat >"${FAKE_BIN}/podman" <<'EOF'
#!/usr/bin/env bash
printf 'podman %s\n' "$*" >>"$PODMAN_CALL_LOG"
case "${1:-}" in
  pull)
    ref="${!#}"
    if [ -n "${FAKE_PULL_FAIL_SUBSTR:-}" ] && [ "${ref#*"$FAKE_PULL_FAIL_SUBSTR"}" != "$ref" ]; then
      printf 'Error: pulling %s: manifest unknown\n' "$ref" >&2; exit 125
    fi
    printf '%s\n' "$ref"
    ;;
  images)  printf '%s %s\n' "${FAKE_TAG_REF}" "${FAKE_TAG_LIST_DIGEST}" ;;
  inspect)
    case "$*" in
      *ImageName*)      cat "${STATE_DIR}/running_image" 2>/dev/null ;;
      *io.containers.autoupdate*) printf '%s\n' "${FAKE_AUTOUPDATE_LABEL:-}" ;;
      *)                printf '%s@%s\n' "${FAKE_TAG_REF%:*}" "${FAKE_TAG_ARCH_DIGEST}" ;;
    esac
    ;;
  exec) printf 'hive 3.0.0 (commit deadbee, branch v4)\n' ;;
  *) exit 0 ;;
esac
EOF

cat >"${FAKE_BIN}/skopeo" <<'EOF'
#!/usr/bin/env bash
printf 'skopeo %s\n' "$*" >>"$SKOPEO_CALL_LOG"
[ "${FAKE_SKOPEO_RC:-0}" = "0" ] || exit "$FAKE_SKOPEO_RC"
printf '%s\n' "${FAKE_TAG_LIST_DIGEST}"
EOF

# curl: the host-side end-to-end probe of the gateway's published port
# (#4493). FAKE_GATEWAY_CURL_RC=7 models the measured lie: hive healthy,
# gateway unit up or startable, and the dashboard port dead anyway.
cat >"${FAKE_BIN}/curl" <<'EOF'
#!/usr/bin/env bash
printf 'curl %s\n' "$*" >>"$CURL_CALL_LOG"
exit "${FAKE_GATEWAY_CURL_RC:-0}"
EOF

cat >"${FAKE_BIN}/sudo" <<'EOF'
#!/usr/bin/env bash
printf 'sudo %s\n' "$*" >>"$SUDO_CALL_LOG"
while [ "${1:-}" = "-n" ]; do shift; done
exec "$@"
EOF

chmod +x "${FAKE_BIN}"/*

PASS=0
FAIL=0

# Defaults describe a host with the units installed, Hive running and healthy
# on DIGEST_OLD, and every pull succeeding. Each case overrides only what it
# needs to break.
reset_env() {
  export SYSTEMCTL_CALL_LOG="${TEST_TMP}/systemctl.log"
  export PODMAN_CALL_LOG="${TEST_TMP}/podman.log"
  export SKOPEO_CALL_LOG="${TEST_TMP}/skopeo.log"
  export SUDO_CALL_LOG="${TEST_TMP}/sudo.log"
  export CURL_CALL_LOG="${TEST_TMP}/curl.log"
  : >"$SYSTEMCTL_CALL_LOG"; : >"$PODMAN_CALL_LOG"
  : >"$SKOPEO_CALL_LOG";    : >"$SUDO_CALL_LOG"
  : >"$CURL_CALL_LOG"

  export QUADLET_DIR="${TEST_TMP}/quadlet"
  export HIVE_UPDATE_QUADLET_DIR="$QUADLET_DIR"
  rm -rf "$QUADLET_DIR"; mkdir -p "$QUADLET_DIR"

  # The managed-file destinations (#6078). These MUST be redirected: without
  # them `pin` writes the refreshed gateway config into the real
  # ~/.config/hive/nginx.conf of whoever runs the suite, and `status` compares
  # against their actual host. Every managed-file case below drives these three
  # directories instead.
  export CONF_DIR="${TEST_TMP}/conf"
  export HIVE_UPDATE_CONF_DIR="$CONF_DIR"
  rm -rf "$CONF_DIR"; mkdir -p "$CONF_DIR"
  export BOOT_UNIT_DIR="${TEST_TMP}/systemd-user"
  export HIVE_UPDATE_SYSTEMD_UNIT_DIR="$BOOT_UNIT_DIR"
  rm -rf "$BOOT_UNIT_DIR"; mkdir -p "$BOOT_UNIT_DIR"
  # The real checkout by default, so the comparison runs against the files this
  # PR actually ships; cases that want a host with no repo override it.
  export HIVE_UPDATE_SRC_ROOT="$ROOT"

  export STATE_DIR="${TEST_TMP}/state"
  rm -rf "$STATE_DIR"; mkdir -p "$STATE_DIR"
  printf 'active\n'  >"$STATE_DIR/ActiveState"
  printf 'running\n' >"$STATE_DIR/SubState"
  printf 'success\n' >"$STATE_DIR/Result"
  printf '%s@%s\n' "$REPO" "$DIGEST_OLD" >"$STATE_DIR/running_image"

  export FAKE_UNIT_KNOWN=yes
  export FAKE_RESTART_RC=0
  export FAKE_PULL_FAIL_SUBSTR=""
  export FAKE_SKOPEO_RC=0
  export FAKE_TAG_REF="${REPO}:stable"
  export FAKE_TAG_LIST_DIGEST="$DIGEST_NEW"
  export FAKE_TAG_ARCH_DIGEST="$DIGEST_ARCH"
  export FAKE_TIMER_STATE=disabled
  export FAKE_TIMER_RC=0
  export FAKE_AUTOUPDATE_LABEL=""
  # The gateway (#4493): known, startable, and answering by default; each case
  # breaks exactly the link it is about. Retries are collapsed so a dead
  # gateway costs the suite nothing.
  export FAKE_GATEWAY_UNIT_KNOWN=yes
  export FAKE_GATEWAY_START_RC=0
  export FAKE_GATEWAY_CURL_RC=0
  export HIVE_UPDATE_GATEWAY_RETRIES=2
  export HIVE_UPDATE_GATEWAY_DELAY=0
  # The opt-in file the script installs. Pointed at the tracked one so a case
  # that renames or deletes it fails here rather than on a host.
  export HIVE_UPDATE_AUTOUPDATE_SRC="${ROOT}/src/deploy/quadlet/optional/hive-autoupdate.conf"
  rm -f "${STATE_DIR}/timer.log"
  unset HIVE_UPDATE_SKOPEO || true
}

dropin() { printf '%s/hive.container.d/10-image.conf' "$QUADLET_DIR"; }

# Seeds a drop-in with a given Image= and history, as an earlier run would have
# left it. History lines are given newest-first, one per argument.
seed_dropin() {
  local image="$1"; shift
  mkdir -p "$(dirname "$(dropin)")"
  {
    printf '# seeded by the test suite\n#\n'
    local h; for h in "$@"; do printf '# HIVE-PIN %s\n' "$h"; done
    printf '\n[Container]\nImage=%s\n' "$image"
  } >"$(dropin)"
}

run_update() {
  PATH="${FAKE_BIN}:${PATH}" NO_COLOR=1 "$BASH_BIN" "$UPDATE" "$@" 2>&1
}

# name, expected exit, expected-substring, args...
case_expect() {
  local name="$1" want_rc="$2" want_txt="$3"; shift 3
  local out rc why=""
  out="$(run_update "$@")"; rc=$?
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

check() {
  local name="$1" cond="$2"
  if eval "$cond"; then
    PASS=$((PASS + 1)); printf 'ok   %s\n' "$name"
  else
    FAIL=$((FAIL + 1)); printf 'FAIL %s\n' "$name"
  fi
}

# Asserts that `first` appears before `second` in a call log.
ordered_in() {
  local log="$1" first="$2" second="$3" a b
  a="$(grep -nF -- "$first" "$log" | head -1 | cut -d: -f1)"
  b="$(grep -nF -- "$second" "$log" | head -1 | cut -d: -f1)"
  [ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ]
}

echo "== status =="
reset_env
case_expect "no drop-in says the unit is on a floating tag" 0 "no drop-in" status
reset_env
case_expect "no drop-in explains why a floating tag cannot be rolled back to" 0 "cannot be rolled back to" status
reset_env; seed_dropin "${REPO}@${DIGEST_OLD}" "healthy 2026-08-20T00:00:00Z ${DIGEST_OLD} ${REPO}:stable"
case_expect "a pinned unit reports its digest" 0 "$DIGEST_OLD" status
reset_env; seed_dropin "${REPO}@${DIGEST_NEW}" "pending 2026-08-21T00:00:00Z ${DIGEST_NEW} ${REPO}:stable"
case_expect "a pin not yet restarted into is called out" 0 "NOT on the pinned image" status
reset_env; seed_dropin "${REPO}@${DIGEST_OLD}" "healthy 2026-08-20T00:00:00Z ${DIGEST_OLD} ${REPO}:stable"
case_expect "one healthy pin only means rollback has nowhere to go" 0 "nowhere to go" status
reset_env
run_update status >/dev/null
check "status pulls nothing and restarts nothing" \
  '! grep -q "pull" "$PODMAN_CALL_LOG" && ! grep -qE "systemctl (restart|stop|start)" "$SYSTEMCTL_CALL_LOG"'

echo
echo "== resolve: a tag must become the manifest LIST digest, never the per-arch one =="
reset_env
case_expect "a tag resolves to the registry's list digest" 0 "${REPO}@${DIGEST_NEW}" resolve "${REPO}:stable"
reset_env
out="$(run_update resolve "${REPO}:stable")"
check "resolve never returns the per-architecture digest" '! printf "%s" "$out" | grep -q "$DIGEST_ARCH"'
reset_env; export HIVE_UPDATE_SKOPEO="skopeo-not-installed"
case_expect "without skopeo it falls back to the podman digest column, still the list digest" 0 "${REPO}@${DIGEST_NEW}" resolve "${REPO}:stable"
reset_env
run_update resolve "${REPO}@${DIGEST_OLD}" >/dev/null
check "a reference that is already a digest contacts no registry" \
  '[ ! -s "$SKOPEO_CALL_LOG" ] && ! grep -q "pull" "$PODMAN_CALL_LOG"'
reset_env; export FAKE_SKOPEO_RC=1 FAKE_TAG_REF="${REPO}:nope" HIVE_UPDATE_SKOPEO=skopeo
export FAKE_PULL_FAIL_SUBSTR="nope"
case_expect "an unresolvable tag is a finding, not a silent tag pin" 78 "could not resolve" resolve "${REPO}:nope"
# image_repo carries a deliberate comment about why `${ref%:*}` would eat
# everything after `localhost` in a ported registry ref; this is the case that
# keeps that comment honest.
reset_env; export FAKE_TAG_REF="localhost:5000/hive:v1"
case_expect "resolve keeps a registry port in the repo part" 0 "localhost:5000/hive@${DIGEST_NEW}" resolve "localhost:5000/hive:v1"

echo
echo "== pin =="
reset_env
case_expect "pin writes a digest and reports it serving" 0 "updated to $DIGEST_NEW" pin "${REPO}:stable"
reset_env
run_update pin "${REPO}:stable" >/dev/null
check "the drop-in pins by digest, never by the tag it was given" \
  'grep -q "^Image=${REPO}@${DIGEST_NEW}\$" "$(dropin)" && ! grep -q "^Image=.*:stable" "$(dropin)"'
check "the tag it came from is kept as provenance in the history" \
  'grep -q "^# HIVE-PIN healthy .* ${DIGEST_NEW} ${REPO}:stable\$" "$(dropin)"'
check "pin pulls BEFORE it restarts, so the pull is not spent inside TimeoutStartSec" \
  'grep -q "podman pull" "$PODMAN_CALL_LOG" \
   && [ "$(grep -c . "$PODMAN_CALL_LOG")" -gt 0 ] \
   && awk "/podman pull/{p=NR} END{exit !(p>0)}" "$PODMAN_CALL_LOG" \
   && ordered_in "$SYSTEMCTL_CALL_LOG" "daemon-reload" "restart"'
reset_env
run_update pin "${REPO}:stable" >/dev/null
check "pin runs daemon-reload before the restart" \
  'ordered_in "$SYSTEMCTL_CALL_LOG" "daemon-reload" "restart"'
reset_env; export FAKE_PULL_FAIL_SUBSTR="$DIGEST_NEW"
case_expect "a pull that fails stops the update before anything is touched" 78 "nothing has been changed" pin "${REPO}:stable"
check "a failed pull leaves no drop-in behind" '[ ! -f "$(dropin)" ]'
check "a failed pull restarts nothing" '! grep -q "systemctl restart" "$SYSTEMCTL_CALL_LOG"'
reset_env; export FAKE_UNIT_KNOWN=no
case_expect "an uninstalled unit is a finding, not a crash" 78 "is not known to this manager" pin "${REPO}:stable"

echo
echo "== a FAILED update, which is the case the feature exists for =="
reset_env; export FAKE_RESTART_RC=1
case_expect "an image that never becomes healthy exits non-zero" 78 "did not become healthy" pin "${REPO}:stable"
reset_env; export FAKE_RESTART_RC=1
case_expect "a failed update names the rollback command" 78 "rollback" pin "${REPO}:stable"
reset_env; export FAKE_RESTART_RC=1
case_expect "a failed update says the unit will keep retrying it" 78 "keep retrying" pin "${REPO}:stable"
reset_env; export FAKE_RESTART_RC=1
run_update pin "${REPO}:stable" >/dev/null
check "the failed pin is recorded as failed, not as healthy" \
  'grep -q "^# HIVE-PIN failed .* ${DIGEST_NEW} " "$(dropin)"'
check "the drop-in still names the failed digest, so status tells the truth" \
  'grep -q "^Image=${REPO}@${DIGEST_NEW}\$" "$(dropin)"'

echo
echo "== rollback =="
reset_env
seed_dropin "${REPO}@${DIGEST_BAD}" \
  "failed  2026-08-21T10:00:00Z ${DIGEST_BAD} ${REPO}:stable" \
  "healthy 2026-08-20T10:00:00Z ${DIGEST_OLD} ${REPO}:b35e9cc"
case_expect "rollback from a FAILED update returns to the last healthy digest" 0 "rolled back to ${DIGEST_OLD}" rollback
reset_env
seed_dropin "${REPO}@${DIGEST_BAD}" \
  "failed  2026-08-21T10:00:00Z ${DIGEST_BAD} ${REPO}:stable" \
  "healthy 2026-08-20T10:00:00Z ${DIGEST_OLD} ${REPO}:b35e9cc"
run_update rollback >/dev/null
check "rollback skips the entry that just failed" \
  'grep -q "^Image=${REPO}@${DIGEST_OLD}\$" "$(dropin)"'
check "rollback stops the failing unit before starting, rather than queueing behind it" \
  'ordered_in "$SYSTEMCTL_CALL_LOG" "stop" "restart"'
check "rollback records the restored pin as healthy" \
  'grep -q "^# HIVE-PIN healthy .* ${DIGEST_OLD} " "$(dropin)"'
reset_env
seed_dropin "${REPO}@${DIGEST_BAD}" \
  "failed  2026-08-21T10:00:00Z ${DIGEST_BAD} ${REPO}:stable" \
  "failed  2026-08-20T10:00:00Z ${DIGEST_OLD} ${REPO}:b35e9cc"
case_expect "with no HEALTHY predecessor rollback refuses rather than guessing" 78 "nowhere to roll back to" rollback
reset_env
case_expect "rollback with no drop-in at all refuses" 78 "nowhere to roll back to" rollback
reset_env
seed_dropin "${REPO}@${DIGEST_BAD}" \
  "failed  2026-08-21T10:00:00Z ${DIGEST_BAD} ${REPO}:stable" \
  "healthy 2026-08-20T10:00:00Z ${DIGEST_OLD} ${REPO}:b35e9cc"
export FAKE_PULL_FAIL_SUBSTR="$DIGEST_OLD"
# shellcheck disable=SC2034 # read back inside the single-quoted check below
before="$(cat "$(dropin)")"
case_expect "a rollback target that no longer pulls refuses" 78 "nothing has been changed" rollback
check "and leaves the drop-in byte-identical" '[ "$before" = "$(cat "$(dropin)")" ]'
check "and restarts nothing" '! grep -q "systemctl restart" "$SYSTEMCTL_CALL_LOG"'
# The worst state the tool can leave a host in: the rollback target ALSO never
# becomes healthy, and Hive stays DOWN. The report must say so, and the history
# must not pretend the restored pin worked.
reset_env; export FAKE_RESTART_RC=1
seed_dropin "${REPO}@${DIGEST_BAD}" \
  "failed  2026-08-21T10:00:00Z ${DIGEST_BAD} ${REPO}:stable" \
  "healthy 2026-08-20T10:00:00Z ${DIGEST_OLD} ${REPO}:b35e9cc"
case_expect "a rollback target that also fails to become healthy exits 78" 78 "did not become healthy either" rollback
check "a double failure marks the restored pin failed, not healthy" \
  'grep -q "^# HIVE-PIN failed .* ${DIGEST_OLD} " "$(dropin)"'
# An interrupted pin leaves a `pending` entry on top of the history. rollback
# must skip it the same way it skips `failed`: pending was never seen healthy.
reset_env
seed_dropin "${REPO}@${DIGEST_BAD}" \
  "failed  2026-08-21T10:00:00Z ${DIGEST_BAD} ${REPO}:stable" \
  "pending 2026-08-21T09:00:00Z ${DIGEST_NEW} ${REPO}:stable" \
  "healthy 2026-08-20T10:00:00Z ${DIGEST_OLD} ${REPO}:b35e9cc"
case_expect "rollback skips a pending entry from an interrupted pin" 0 "rolled back to ${DIGEST_OLD}" rollback

echo
echo "== rollback restores the WHOLE deployment, gateway included (#4493) =="
# The measured hole: after a failed update, Requires= has already stopped
# hive-gateway.service, and `systemctl restart hive.service` only cycles
# dependents that are ACTIVE when the job runs -- so nothing ever started the
# gateway again, and the script said "and it is serving" over a dead :3001.
seed_failed_over_healthy() {
  seed_dropin "${REPO}@${DIGEST_BAD}" \
    "failed  2026-08-21T10:00:00Z ${DIGEST_BAD} ${REPO}:stable" \
    "healthy 2026-08-20T10:00:00Z ${DIGEST_OLD} ${REPO}:b35e9cc"
}
reset_env; seed_failed_over_healthy
case_expect "rollback from a failed update claims end to end, not just hive" 0 "healthy end to end" rollback
check "and it actually STARTED the gateway rather than assuming restart would" \
  'grep -q "systemctl --user start hive-gateway.service" "$SYSTEMCTL_CALL_LOG"'
check "and it probed the gateway's published port from the host" \
  'grep -q "127.0.0.1:3001/api/health" "$CURL_CALL_LOG"'
# The gateway's port belongs to the INSTALLED unit, not to a constant: a host
# that publishes elsewhere must be probed where it publishes.
reset_env; seed_failed_over_healthy
printf '[Container]\nPublishPort=8443:3001\n' >"${QUADLET_DIR}/hive-gateway.container"
run_update rollback >/dev/null
check "the probe reads the published port out of the installed gateway unit" \
  'grep -q "127.0.0.1:8443/api/health" "$CURL_CALL_LOG"'
# THE LYING SUCCESS, verbatim from the issue: hive healthy, dashboard dead.
# The exact state the old script blessed with PASS and exit 0.
reset_env; seed_failed_over_healthy
export FAKE_GATEWAY_CURL_RC=7
out="$(run_update rollback)"; rc=$?
check "a rollback whose gateway never answers exits 78, not 0" '[ "$rc" = "78" ]'
check "and never prints 'and it is serving'" '! printf "%s" "$out" | grep -q "and it is serving"'
check "and says the gateway did not answer" 'printf "%s" "$out" | grep -q "the gateway did not answer"'
check "while the restored digest is still recorded healthy -- hive itself served on it" \
  'grep -q "^# HIVE-PIN healthy .* ${DIGEST_OLD} " "$(dropin)"'
reset_env; seed_failed_over_healthy
export FAKE_GATEWAY_START_RC=1
case_expect "a gateway that will not START is a finding, not a footnote" 78 \
  "nothing an operator can reach" rollback
# A deployment installed without the gateway unit has nothing to restore; the
# script must say the end-to-end check could not run rather than fail or lie.
reset_env; seed_failed_over_healthy
export FAKE_GATEWAY_UNIT_KNOWN=no
case_expect "no gateway unit at all is a warning, not a failure" 0 \
  "no end-to-end check can run" rollback
# pin has the same two obligations: verify end to end on success, and say the
# gateway went down with hive on failure (Requires= took it down too).
reset_env
run_update pin "${REPO}:stable" >/dev/null
check "a successful pin also starts and probes the gateway" \
  'grep -q "systemctl --user start hive-gateway.service" "$SYSTEMCTL_CALL_LOG" \
   && grep -q "127.0.0.1:3001/api/health" "$CURL_CALL_LOG"'
reset_env; export FAKE_GATEWAY_CURL_RC=7
case_expect "a pin whose gateway never answers exits 78" 78 "DEPLOYMENT is not serving" pin "${REPO}:stable"
check "but the digest is still recorded healthy -- the fault is in front of hive" \
  'grep -q "^# HIVE-PIN healthy .* ${DIGEST_NEW} " "$(dropin)"'
reset_env; export FAKE_RESTART_RC=1
case_expect "a failed update says the gateway has stopped with it" 78 \
  "hive-gateway.service has stopped with it" pin "${REPO}:stable"
check "a failed update does not try to start the gateway over a broken hive" \
  '! grep -q "start hive-gateway.service" "$SYSTEMCTL_CALL_LOG"'
reset_env; export FAKE_GATEWAY_CURL_RC=7
seed_dropin "${REPO}@${DIGEST_OLD}" "healthy 2026-08-20T00:00:00Z ${DIGEST_OLD} ${REPO}:stable"
case_expect "unpin whose gateway never answers exits 78" 78 "did not answer" unpin

echo
echo "== history =="
reset_env
hist=()
for i in $(seq 1 14); do
  hist+=("healthy 2026-08-0${i}T00:00:00Z sha256:$(printf '%064d' "$i") ${REPO}:t$i")
done
seed_dropin "${REPO}@${DIGEST_OLD}" "${hist[@]}"
run_update pin "${REPO}:stable" >/dev/null
check "the history is capped rather than growing without bound" \
  '[ "$(grep -cE "^# HIVE-PIN [a-z]+ [0-9]{4}-" "$(dropin)")" -le 10 ]'
check "the newest entry survives the cap" \
  'grep -q "^# HIVE-PIN healthy .* ${DIGEST_NEW} " "$(dropin)"'

check "the header prose is not parsed as a history entry" \
  'run_update status | grep -A3 "Pin history" | grep -qv "newest first; the top"'

echo
echo "== unpin =="
reset_env; seed_dropin "${REPO}@${DIGEST_OLD}" "healthy 2026-08-20T00:00:00Z ${DIGEST_OLD} ${REPO}:stable"
case_expect "unpin warns that the floating tag cannot be rolled back to" 0 "cannot be rolled back to" unpin
check "unpin removes the drop-in" '[ ! -f "$(dropin)" ]'
reset_env
case_expect "unpin with nothing pinned is not an error" 0 "no drop-in to remove" unpin
# unpin removes the pin BEFORE restarting, so a restart that fails leaves the
# unit on the floating tag and down. That must be an exit-78 finding, not a 0.
reset_env; export FAKE_RESTART_RC=1
seed_dropin "${REPO}@${DIGEST_OLD}" "healthy 2026-08-20T00:00:00Z ${DIGEST_OLD} ${REPO}:stable"
case_expect "unpin whose restart fails exits 78" 78 "" unpin

echo
echo "== rootful drives the system manager through sudo =="
reset_env
run_update status --rootful >/dev/null
check "rootful reads the system manager through sudo" 'grep -q "sudo systemctl" "$SUDO_CALL_LOG"'
# The write path, not just the read one: write_dropin and mark_top_outcome must
# go through `sudo install` and still land the same pinned Image=. The drop-in
# directory is pre-created because seed-less writes into a missing directory
# are the BSD-install nuance the pin cases already own; this case is about
# sudo, not about -D.
reset_env
mkdir -p "$(dirname "$(dropin)")"
run_update pin "${REPO}:stable" --rootful >/dev/null
check "rootful pin writes the drop-in through sudo install" \
  'grep -q "sudo install" "$SUDO_CALL_LOG" && grep -q "^Image=${REPO}@${DIGEST_NEW}\$" "$(dropin)"'

echo
echo "== autoupdate (#4411) =="

au_dropin() { printf '%s/hive.container.d/20-autoupdate.conf' "$QUADLET_DIR"; }

reset_env
case_expect "off by default -- #4188 requires opt-in" 0 "auto-update is OFF" autoupdate status
reset_env
check "the tracked opt-in drop-in exists and carries the policy" \
  'grep -q "^AutoUpdate=registry$" "$HIVE_UPDATE_AUTOUPDATE_SRC"'

reset_env
run_update autoupdate on >/dev/null
check "on installs 20-autoupdate.conf beside the pin drop-in" '[ -f "$(au_dropin)" ]'
check "the installed file is the tracked one, comments and all" \
  'diff -q "$HIVE_UPDATE_AUTOUPDATE_SRC" "$(au_dropin)" >/dev/null'
check "on enables the timer" 'grep -q "enable --now podman-auto-update.timer" "${STATE_DIR}/timer.log"'
check "on restarts so the container is recreated with the label" \
  'grep -q "systemctl --user restart hive.service" "$SYSTEMCTL_CALL_LOG"'

reset_env
case_expect "on names the measured detection signal, not ActiveState" 0 \
  "start-job result" autoupdate on
reset_env
case_expect "on states the per-bad-update cost" 0 "one full TimeoutStartSec" autoupdate on
reset_env
case_expect "on states that a bad tag repeats on every firing" 0 "REPEATS on every timer firing" autoupdate on

# THE PIN CONFLICT. Measured: with a digest in Image=, auto-update reports
# UPDATED=false and does nothing, so a pinned host with the timer on looks
# healthy and never updates. Refusing is the only honest outcome.
reset_env; seed_dropin "${REPO}@${DIGEST_OLD}" "healthy 2026-08-20T00:00:00Z ${DIGEST_OLD} ${REPO}:stable"
case_expect "REFUSES to arm auto-update over a digest pin" 78 "would do nothing" autoupdate on
reset_env; seed_dropin "${REPO}@${DIGEST_OLD}" "healthy 2026-08-20T00:00:00Z ${DIGEST_OLD} ${REPO}:stable"
case_expect "the refusal names both ways out" 78 "unpin" autoupdate on
reset_env; seed_dropin "${REPO}@${DIGEST_OLD}" "healthy 2026-08-20T00:00:00Z ${DIGEST_OLD} ${REPO}:stable"
run_update autoupdate on >/dev/null
check "the refusal changes nothing on disk" '[ ! -f "$(au_dropin)" ]'
check "the refusal touches no timer" '[ ! -s "${STATE_DIR}/timer.log" ]'

# Both files present is not something `on` can produce, but an operator can
# reach it by pinning after arming -- which is exactly what a manual rollback
# does. It must be visible without being asked about.
reset_env; seed_dropin "${REPO}@${DIGEST_OLD}" "healthy 2026-08-20T00:00:00Z ${DIGEST_OLD} ${REPO}:stable"
mkdir -p "$(dirname "$(au_dropin)")"; cp "$HIVE_UPDATE_AUTOUPDATE_SRC" "$(au_dropin)"
case_expect "plain status flags pin+autoupdate without being asked" 0 "nothing to poll" status
reset_env; seed_dropin "${REPO}@${DIGEST_OLD}" "healthy 2026-08-20T00:00:00Z ${DIGEST_OLD} ${REPO}:stable"
mkdir -p "$(dirname "$(au_dropin)")"; cp "$HIVE_UPDATE_AUTOUPDATE_SRC" "$(au_dropin)"
case_expect "autoupdate status says it reads as 'up to date'" 0 "already up to date" autoupdate status

reset_env
mkdir -p "$(dirname "$(au_dropin)")"; cp "$HIVE_UPDATE_AUTOUPDATE_SRC" "$(au_dropin)"
run_update autoupdate off >/dev/null
check "off removes the drop-in" '[ ! -f "$(au_dropin)" ]'
check "off disables the timer" 'grep -q "disable --now podman-auto-update.timer" "${STATE_DIR}/timer.log"'
reset_env
mkdir -p "$(dirname "$(au_dropin)")"; cp "$HIVE_UPDATE_AUTOUPDATE_SRC" "$(au_dropin)"
seed_dropin "${REPO}@${DIGEST_OLD}" "healthy 2026-08-20T00:00:00Z ${DIGEST_OLD} ${REPO}:stable"
run_update autoupdate off >/dev/null
check "off leaves the image pin alone" '[ -f "$(dropin)" ]'
reset_env
mkdir -p "$(dirname "$(au_dropin)")"; cp "$HIVE_UPDATE_AUTOUPDATE_SRC" "$(au_dropin)"
seed_dropin "${REPO}@${DIGEST_OLD}" "healthy 2026-08-20T00:00:00Z ${DIGEST_OLD} ${REPO}:stable"
run_update unpin >/dev/null
check "unpin leaves auto-update alone (separate files, separate decisions)" '[ -f "$(au_dropin)" ]'

# A host with no podman systemd units must still get the drop-in, with an
# honest note -- the drop-in is what makes the unit eligible; the timer is only
# one way to fire it.
reset_env
FAKE_TIMER_RC=1 run_update autoupdate on >/dev/null
check "a missing podman-auto-update.timer still installs the drop-in" '[ -f "$(au_dropin)" ]'
reset_env
FAKE_TIMER_RC=1 case_expect "and says how to fire it without the timer" 0 \
  "podman auto-update --rollback=true" autoupdate on

reset_env
case_expect "autoupdate rejects a bad action" 64 "autoupdate takes on, off, or status" autoupdate sideways
reset_env
run_update autoupdate status >/dev/null
check "autoupdate status restarts nothing" '! grep -q "restart" "$SYSTEMCTL_CALL_LOG"'
check "autoupdate status pulls nothing" '! grep -q "^podman pull" "$PODMAN_CALL_LOG"'

reset_env
FAKE_AUTOUPDATE_LABEL=registry FAKE_TIMER_STATE=enabled \
  case_expect "status reports the label the RUNNING container carries" 0 "registry" autoupdate status

echo
echo "== reconcile: the files this host RUNS FROM, not the image (#6078) =="

# Puts the host in the state a fresh install leaves it in: every repo-owned
# file copied out of the checkout. Cases then age exactly one file, so a
# failure names the file that stopped being reconciled rather than reporting
# that "something" drifted.
seed_managed_host() {
  install -Dm644 "${ROOT}/src/deploy/nginx.conf" "${CONF_DIR}/nginx.conf"
  local u
  for u in hive.network hive-data.volume hive.container hive-gateway.container; do
    install -Dm644 "${ROOT}/src/deploy/quadlet/${u}" "${QUADLET_DIR}/${u}"
  done
  for u in hive-boot.target hive-boot-gate.service; do
    install -Dm644 "${ROOT}/src/deploy/systemd/${u}" "${BOOT_UNIT_DIR}/${u}"
  done
}

# The operator files reconcile must never write. Seeded with contents no
# template contains, so "was it preserved" is a content check rather than a
# timestamp one.
seed_operator_files() {
  printf 'dashboard:\n  port: 3002\n# an operator edit\n' >"${CONF_DIR}/hive.yaml"
  printf 'HIVE_DASHBOARD_TOKEN=deadbeefcafe\n' >"${CONF_DIR}/hive.env"
  mkdir -p "${CONF_DIR}/secrets"
  printf 'private-key-material\n' >"${CONF_DIR}/secrets/id_ed25519"
}

reset_env; seed_managed_host
case_expect "a host that matches the checkout reconciles clean" 0 \
  "every repo-owned file on this host matches the checkout" reconcile
reset_env; seed_managed_host
case_expect "check is the default action" 0 "Managed files" reconcile check

# THE REPORTED BUG. An install from 2026-08-22 keeps serving that day's
# nginx.conf while the image rolls forward, so the WebSocket fix in #5200 was
# merged, released, reported present in served_sha, and still dark on the host.
reset_env; seed_managed_host
printf 'user nginx;\n# the 2026-08-22 gateway config\n' >"${CONF_DIR}/nginx.conf"
case_expect "a stale gateway config is reported as stale" 78 \
  "STALE   gateway config" reconcile check
reset_env; seed_managed_host
printf 'user nginx;\n# the 2026-08-22 gateway config\n' >"${CONF_DIR}/nginx.conf"
case_expect "and check exits non-zero so a monitor can read it" 78 \
  "this host is running files older than the checkout" reconcile check

# check must be READ-ONLY. A "reporting" command that quietly fixed things
# would make the drift invisible again, one level up.
reset_env; seed_managed_host
printf 'stale\n' >"${CONF_DIR}/nginx.conf"
run_update reconcile check >/dev/null
check "check writes nothing" '[ "$(cat "${CONF_DIR}/nginx.conf")" = "stale" ]'
check "check restarts nothing" '! grep -qE "systemctl (restart|stop|start)" "$SYSTEMCTL_CALL_LOG"'

reset_env; seed_managed_host
printf 'stale\n' >"${CONF_DIR}/nginx.conf"
case_expect "apply rewrites the stale gateway config" 0 \
  "wrote   gateway config" reconcile apply
check "apply put the checkout's gateway config on the host" \
  'cmp -s "${ROOT}/src/deploy/nginx.conf" "${CONF_DIR}/nginx.conf"'
check "apply restarted the gateway onto it" \
  'grep -qF "restart hive-gateway.service" "$SYSTEMCTL_CALL_LOG"'

# The second instance from the issue thread, and the more dangerous one: a
# stale Image= breaks NOTHING observable. podman auto-update polls the old
# registry path, finds it unchanged, prints UPDATED=false and exits 0 -- which
# is byte-identical to a host that is genuinely current.
stale_image_unit() {
  sed 's|ghcr.io/hivecommons/hive:stable|ghcr.io/kubestellar/hive:stable|' \
    "${ROOT}/src/deploy/quadlet/hive.container" >"${QUADLET_DIR}/hive.container"
}

reset_env; seed_managed_host; stale_image_unit
case_expect "a hive.container pointing at the moved registry org is reported" 78 \
  "STALE   quadlet unit" reconcile check

reset_env; seed_managed_host; stale_image_unit
run_update reconcile apply >/dev/null
check "apply puts the current image reference on the host" \
  'grep -qF "Image=ghcr.io/hivecommons/hive:stable" "${QUADLET_DIR}/hive.container"'
check "a unit rewrite is followed by daemon-reload" \
  'grep -qF "daemon-reload" "$SYSTEMCTL_CALL_LOG"'

reset_env; seed_managed_host; stale_image_unit
case_expect "and apply says the running container has NOT moved yet" 0 \
  "the running container still uses the reference it was created with" reconcile apply

reset_env; seed_managed_host
rm -f "${BOOT_UNIT_DIR}/hive-boot-gate.service"
case_expect "a missing boot unit is reported, not silently ignored" 78 \
  "missing boot unit" reconcile check

# A host whose gateway unit is not installed on this manager: the file still
# belongs on disk, so writing it is right and failing the run is not. This is
# the same judgement ensure_gateway_serving already makes.
reset_env; seed_managed_host
printf 'stale\n' >"${CONF_DIR}/nginx.conf"
FAKE_GATEWAY_UNIT_KNOWN=no \
  case_expect "apply still places the config when the gateway unit is unknown" 0 \
  "nothing was restarted" reconcile apply
check "and the file is on disk even though nothing was restarted" \
  'cmp -s "${ROOT}/src/deploy/nginx.conf" "${CONF_DIR}/nginx.conf"'

# The reason this command exists instead of `setup --force`: that path re-copies
# hive.yaml and hive.env too, and regenerating hive.env takes the dashboard
# token with it. If reconcile ever grows the same reach, these three fail.
reset_env; seed_managed_host; seed_operator_files
printf 'stale\n' >"${CONF_DIR}/nginx.conf"
run_update reconcile apply >/dev/null
check "apply preserves an operator's hive.yaml" \
  'grep -qF "an operator edit" "${CONF_DIR}/hive.yaml"'
check "apply preserves the generated dashboard token" \
  'grep -qF "HIVE_DASHBOARD_TOKEN=deadbeefcafe" "${CONF_DIR}/hive.env"'
check "apply preserves key material under secrets/" \
  'grep -qF "private-key-material" "${CONF_DIR}/secrets/id_ed25519"'
reset_env; seed_managed_host; seed_operator_files
case_expect "and it names what it will not touch before writing" 0 \
  "these hold operator edits" reconcile check

# A host with no checkout must say it did not compare. Reporting "matches"
# there would be the same lie as the served_sha that hid #6078 for two weeks.
reset_env; export HIVE_UPDATE_SRC_ROOT="${TEST_TMP}/not-a-checkout"
case_expect "reconcile refuses without a checkout" 78 \
  "there is nothing to compare the host against" reconcile check
reset_env; export HIVE_UPDATE_SRC_ROOT="${TEST_TMP}/not-a-checkout"
case_expect "and it says how to get one" 78 "git clone" reconcile check

reset_env
case_expect "reconcile rejects a bad action" 64 "reconcile takes check or apply" reconcile sideways

echo
echo "== drift is visible without being asked for (#6078) =="

# The half of the issue that survives whatever fix lands: `podman auto-update`
# is a timer calling podman, so no script can hook it. status is where an
# operator or a monitor finds out the host stopped moving.
reset_env; seed_managed_host
printf 'stale\n' >"${CONF_DIR}/nginx.conf"
case_expect "status flags a stale managed file without being asked" 0 \
  "STALE   gateway config" status
reset_env; seed_managed_host
printf 'stale\n' >"${CONF_DIR}/nginx.conf"
case_expect "status points at the command that fixes it" 0 \
  "reconcile apply" status
reset_env; seed_managed_host
case_expect "a current host says so explicitly" 0 \
  "every repo-owned file on this host matches" status

# "not checked" and "up to date" must not print the same. This is the exact
# confusion the issue is about, so status refuses to imply the second.
reset_env; export HIVE_UPDATE_SRC_ROOT="${TEST_TMP}/not-a-checkout"
case_expect "status distinguishes 'not checked' from 'up to date'" 0 \
  "this is not 'up to date': it is 'not checked'" status

reset_env; seed_managed_host
printf 'stale\n' >"${CONF_DIR}/nginx.conf"
run_update status >/dev/null
check "status still writes nothing while reporting drift" \
  '[ "$(cat "${CONF_DIR}/nginx.conf")" = "stale" ]'

echo
echo "== pin carries the gateway config with the image (#6078) =="

# The issue's second preference, applied to the one path a script can reach:
# an explicit update must not leave the gateway on the config it was installed
# with. The image and the config ship in the same commit; they must land
# together.
reset_env; seed_managed_host
printf 'user nginx;\n# the config this host was installed with\n' >"${CONF_DIR}/nginx.conf"
case_expect "pin refreshes a stale gateway config" 0 \
  "refreshed the gateway config" pin "${REPO}:stable"
check "pin left the checkout's gateway config on the host" \
  'cmp -s "${ROOT}/src/deploy/nginx.conf" "${CONF_DIR}/nginx.conf"'

# The discriminating half: an in-sync host must not be rewritten or have its
# gateway bounced for nothing.
reset_env; seed_managed_host
out="$(run_update pin "${REPO}:stable")"
check "pin says nothing about the gateway config when it already matches" \
  '! printf "%s" "$out" | grep -qF "refreshed the gateway config"'

# Unit files are reported, never rewritten by pin: a unit change needs a
# container recreate to mean anything, and rewriting hive.container under an
# active pin edits the file the pin exists to leave alone.
reset_env; seed_managed_host; stale_image_unit
case_expect "pin reports stale unit files rather than rewriting them" 0 \
  "were NOT rewritten" pin "${REPO}:stable"
check "pin left the stale unit exactly as it found it" \
  'grep -qF "Image=ghcr.io/kubestellar/hive:stable" "${QUADLET_DIR}/hive.container"'

# Operator state is not pin's to touch either.
reset_env; seed_managed_host; seed_operator_files
printf 'stale\n' >"${CONF_DIR}/nginx.conf"
run_update pin "${REPO}:stable" >/dev/null
check "pin preserves the dashboard token while refreshing the gateway" \
  'grep -qF "HIVE_DASHBOARD_TOKEN=deadbeefcafe" "${CONF_DIR}/hive.env"'

# A host with no checkout must still be able to pin. The refresh is a bonus on
# that path, not a precondition for updating an image.
reset_env; export HIVE_UPDATE_SRC_ROOT="${TEST_TMP}/not-a-checkout"
case_expect "pin still works when run from outside a checkout" 0 \
  "serving end to end" pin "${REPO}:stable"

echo
echo "== the two scripts' file lists cannot drift apart (#6078) =="

# bin/hive-podman-setup.sh decides what an install PUTS on the host;
# bin/hive-podman-update.sh decides what reconcile KEEPS current. A unit added
# to the first and forgotten in the second is installed once and then frozen
# forever -- which is #6078 again, for a file that does not exist yet. Compared
# here rather than trusted to review.
array_literal() {
  sed -n 's/^'"$2"'=(\(.*\))$/\1/p' "$1" | head -n1
}
setup_units() { array_literal "${ROOT}/bin/hive-podman-setup.sh" "$1"; }
update_units() { array_literal "$UPDATE" "$1"; }

check "setup's UNITS list was found at all" '[ -n "$(setup_units UNITS)" ]'
check "update's MANAGED_UNITS list was found at all" '[ -n "$(update_units MANAGED_UNITS)" ]'
check "reconcile covers exactly the quadlet units setup installs" \
  '[ "$(setup_units UNITS)" = "$(update_units MANAGED_UNITS)" ]'
check "setup's BOOT_UNITS list was found at all" '[ -n "$(setup_units BOOT_UNITS)" ]'
check "reconcile covers exactly the boot units setup installs" \
  '[ "$(setup_units BOOT_UNITS)" = "$(update_units MANAGED_BOOT_UNITS)" ]'

# The gateway config is the file the issue was filed about, so it must be in
# the managed set; the operator files must never appear there.
reset_env; seed_managed_host
out="$(run_update reconcile check)"
check "nginx.conf is one of the files reconcile manages" \
  'printf "%s" "$out" | grep -qF "match   gateway config"'
check "hive.yaml is never listed as a managed file" \
  '! printf "%s" "$out" | grep -qE "(match|STALE|missing) +[a-z ]*: .*hive[.]yaml"'
check "hive.env is never listed as a managed file" \
  '! printf "%s" "$out" | grep -qE "(match|STALE|missing) +[a-z ]*: .*hive[.]env"'

echo "== invocation =="
reset_env
case_expect "an unknown command is EX_USAGE" 64 "unknown command" nonsense
reset_env
case_expect "an unknown option is EX_USAGE" 64 "unknown option" --nonsense
reset_env
case_expect "two commands are EX_USAGE" 64 "two commands given" status rollback
reset_env
case_expect "pin with no reference is EX_USAGE" 64 "pin needs a reference" pin
reset_env
case_expect "no command at all is EX_USAGE" 64 "Run: bin/hive-podman-update.sh"

echo
printf '%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
