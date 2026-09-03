#!/usr/bin/env bash
# Contract tests for bin/hive-podman-preflight-host.sh (#4209).
# Run: bash bin/test_hive_podman_preflight_host.sh
#
# Every input is mocked. A stub `podman`, `getenforce`, `sysctl`, and `ss`
# answer from the environment, and a stub `stat` answers from a lookup table
# for the paths a case wants to control and delegates to the real one for
# everything else. The whole matrix — enforcing, permissive, disabled,
# labeling off, wrong labels, unreadable and over-permissive secrets, occupied
# and sub-floor ports — therefore runs on a host with no SELinux, no Podman,
# and no privileges, and never executes a container command.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PREFLIGHT="${ROOT}/bin/hive-podman-preflight-host.sh"
BASH_BIN="$(command -v bash)"
TEST_TMP="$(mktemp -d)"
trap 'rm -rf "$TEST_TMP"' EXIT

FAKE_BIN="${TEST_TMP}/bin"
CALL_LOG="${TEST_TMP}/podman-calls.log"
STAT_MAP="${TEST_TMP}/stat-map"
mkdir -p "$FAKE_BIN"
: >"$STAT_MAP"

REAL_STAT="$(command -v stat)"

cat >"${FAKE_BIN}/podman" <<'EOF'
#!/bin/sh
printf 'podman %s\n' "$*" >>"$PODMAN_CALL_LOG"
case "$*" in
  "info --format {{.Host.Security.SELinuxEnabled}}")
    [ "${FAKE_SELINUX_ENABLED:-}" = "__ERR__" ] && exit 125
    printf '%s\n' "${FAKE_SELINUX_ENABLED:-true}"
    ;;
  "info --format {{.Host.Security.Rootless}}")
    [ "${FAKE_ROOTLESS:-}" = "__ERR__" ] && exit 125
    printf '%s\n' "${FAKE_ROOTLESS:-true}"
    ;;
  *) exit 1 ;;
esac
EOF

# uutils coreutils has no %C: it prints an error to STDOUT and exits 0, so a
# `|| fallback` never fires and the sentence flows on as if it were a context
# (#4359). Everything else delegates, because uutils implements the rest.
cat >"${FAKE_BIN}/uustat" <<EOF
#!${BASH_BIN}
case "\$*" in
  *%C*) printf 'unsupported for this operating system\n'; exit 0 ;;
esac
exec "${FAKE_BIN}/stat" "\$@"
EOF
chmod +x "${FAKE_BIN}/uustat"

cat >"${FAKE_BIN}/getenforce" <<'EOF'
#!/bin/sh
[ "${FAKE_ENFORCE:-}" = "__ERR__" ] && exit 1
printf '%s\n' "${FAKE_ENFORCE:-Enforcing}"
EOF

cat >"${FAKE_BIN}/sysctl" <<'EOF'
#!/bin/sh
# Only the unprivileged-port floor is consulted. -n <key>
[ "${FAKE_PORT_FLOOR:-}" = "__ERR__" ] && exit 1
printf '%s\n' "${FAKE_PORT_FLOOR:-1024}"
EOF

cat >"${FAKE_BIN}/ss" <<'EOF'
#!/bin/sh
# `ss -Hltn`. FAKE_LISTENERS holds whole listener lines; the script filters
# them by the local-address column exactly as it would filter real output.
printf '%s' "${FAKE_LISTENERS:-}"
[ -n "${FAKE_LISTENERS:-}" ] && printf '\n'
exit 0
EOF

# Answers `stat -c FMT PATH` field by field. STAT_MAP lines are
# <path>|<mode>|<uid>|<user>|<label>, and any field left empty falls through to
# the real stat — so a case can fake an SELinux label on a host that has none
# without also freezing the permissions the case is actually manipulating.
# Absolute interpreter: the stub has to survive the trimmed-PATH cases below,
# where neither `env` nor `bash` is reachable by name.
cat >"${FAKE_BIN}/stat" <<EOF
#!${BASH_BIN}
fmt=""
path=""
while [ \$# -gt 0 ]; do
  case "\$1" in
    -c) fmt="\$2"; shift 2 ;;
    -c*) fmt="\${1#-c}"; shift ;;
    *) path="\$1"; shift ;;
  esac
done

if [ -n "\${FAKE_LABEL_PROBE:-}" ] && [ "\$path" = "\$FAKE_LABEL_PROBE" ]; then
  case "\$fmt" in
    *%C*) printf '%s\\n' "\${FAKE_LABEL_PROBE_CONTEXT:-system_u:object_r:etc_t:s0}"; exit 0 ;;
  esac
fi

line="\$(grep -F -- "\${path}|" "\$FAKE_STAT_MAP" 2>/dev/null | head -1)"
IFS='|' read -r _p mode uid user label gid <<<"\$line"

real() { "${REAL_STAT}" -c "\$1" "\$path" 2>/dev/null; }
[ -n "\$mode" ]  || mode="\$(real %a)"
[ -n "\$uid" ]   || uid="\$(real %u)"
[ -n "\$user" ]  || user="\$(real %U)"
[ -n "\$label" ] || label="\$(real %C)"
[ -n "\$gid" ]   || gid="\$(real %g)"

out="\$fmt"
out="\${out//%a/\$mode}"
out="\${out//%u/\$uid}"
out="\${out//%U/\$user}"
out="\${out//%C/\$label}"
out="\${out//%g/\$gid}"
printf '%s\n' "\$out"
EOF

chmod +x "${FAKE_BIN}"/*
TEST_PATH="${FAKE_BIN}:/usr/bin:/bin"

# --- Deployment fixture -------------------------------------------------------
#
# A stand-in for src/: the three bind-mount sources src/docker-compose.yaml
# declares, at permissions a correctly prepared host would have.

SRC_FIXTURE="${TEST_TMP}/src"
mkdir -p "${SRC_FIXTURE}/deploy" "${SRC_FIXTURE}/secrets"
echo "levels: []" >"${SRC_FIXTURE}/hive.yaml"
echo "events {}" >"${SRC_FIXTURE}/deploy/nginx.conf"
echo "-----BEGIN PRIVATE KEY-----" >"${SRC_FIXTURE}/secrets/gh-app-key.pem"
# 0750, not 0700: the container's hive-launch group needs the traverse bit
# (#4359). Nothing for "other" either way.
chmod 750 "${SRC_FIXTURE}/secrets"
chmod 600 "${SRC_FIXTURE}/secrets/gh-app-key.pem"

# The rootless mapping under test: container GID 1002 comes out of the
# subordinate range, so it is SUBGID_START + 1001 on the host — 525289 for a
# range starting at 524288, which is what podman itself computes. chgrp to that
# group is not available to an unprivileged test, so the stub stat reports the
# ownership instead and the arithmetic is exercised for real.
# Resolving "can this host read a label at all" must not be answered by a
# deployment path: that is exactly the conflation #4359 is about. The probe is
# its own file, and the stub answers it regardless of the label map in force.
LABEL_PROBE="${TEST_TMP}/label-probe"
: >"$LABEL_PROBE"

SUBGID_START=524288
SUBGID_FIXTURE="${TEST_TMP}/subgid"
printf '%s:%s:65536\n' "$(id -un)" "$SUBGID_START" >"$SUBGID_FIXTURE"
MAPPED_LAUNCH_GID=$(( SUBGID_START + 1001 ))

# Default map: every bind source already carries a container-readable label, so
# a case that says nothing about labels gets a clean labeling report.
label_all_container_readable() {
  cat >"$STAT_MAP" <<EOF
${SRC_FIXTURE}/hive.yaml||||system_u:object_r:container_file_t:s0
${SRC_FIXTURE}/deploy/nginx.conf||||system_u:object_r:container_file_t:s0
${SRC_FIXTURE}/secrets||||system_u:object_r:container_file_t:s0|${MAPPED_LAUNCH_GID}
${SRC_FIXTURE}/secrets/gh-app-key.pem||||system_u:object_r:container_file_t:s0
EOF
}
label_all_container_readable

pass_count=0

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

assert_eq() {
  [[ "$2" == "$1" ]] || fail "$3: got '$2', want '$1'"
  pass_count=$((pass_count + 1))
}

assert_contains() {
  [[ "$1" == *"$2"* ]] || fail "$3: missing '$2' in: $1"
  pass_count=$((pass_count + 1))
}

assert_not_contains() {
  [[ "$1" != *"$2"* ]] || fail "$3: unexpectedly found '$2' in: $1"
  pass_count=$((pass_count + 1))
}

# Snapshot of the fixture's ownership and permissions, taken with the REAL stat
# so the stub cannot hide a change. Used to prove the preflight is read-only.
fixture_snapshot() {
  find "$SRC_FIXTURE" -mindepth 0 -print0 \
    | sort -z \
    | xargs -0 "$REAL_STAT" -c '%n %a %u %g %s'
}

run_preflight() {
  : >"$CALL_LOG"
  local before after
  before="$(fixture_snapshot)"
  set +e
  RUN_OUT="$(env PATH="$TEST_PATH" PODMAN_CALL_LOG="$CALL_LOG" FAKE_STAT_MAP="$STAT_MAP" \
    HIVE_SRC_DIR="$SRC_FIXTURE" HIVE_PFH_SUBGID_FILE="$SUBGID_FIXTURE" \
    HIVE_PFH_LABEL_PROBES="$LABEL_PROBE" FAKE_LABEL_PROBE="$LABEL_PROBE" \
    "$@" "$BASH_BIN" "$PREFLIGHT" 2>&1)"
  RUN_STATUS=$?
  set -e
  RUN_CALLS="$(cat "$CALL_LOG")"
  after="$(fixture_snapshot)"
  [[ "$before" == "$after" ]] || fail "preflight modified the deployment fixture"
  pass_count=$((pass_count + 1))
}

# --- Docker stays the default and stays untouched -----------------------------
#
# The strongest form of "Docker behavior is unchanged": with Docker selected,
# the stub Podman is never executed at all and no host state is even read.

run_preflight env -u HIVE_DEPLOY_RUNTIME
assert_eq "0" "$RUN_STATUS" "default runtime exit"
assert_contains "$RUN_OUT" "skipped" "default runtime skips"
assert_contains "$RUN_OUT" "selects docker" "default runtime names docker"
assert_eq "" "$RUN_CALLS" "default runtime runs no podman command"
assert_not_contains "$RUN_OUT" "SELinux" "default runtime checks no SELinux state"
assert_not_contains "$RUN_OUT" "Port" "default runtime checks no port"

run_preflight env HIVE_DEPLOY_RUNTIME=docker
assert_eq "0" "$RUN_STATUS" "explicit docker exit"
assert_contains "$RUN_OUT" "skipped" "explicit docker skips"
assert_eq "" "$RUN_CALLS" "explicit docker runs no podman command"

run_preflight env HIVE_DEPLOY_RUNTIME=containerd
assert_eq "64" "$RUN_STATUS" "invalid runtime exit"
assert_eq "" "$RUN_CALLS" "invalid runtime runs no podman command"

# --- A correctly prepared enforcing host --------------------------------------

run_preflight env HIVE_DEPLOY_RUNTIME=podman
assert_eq "0" "$RUN_STATUS" "prepared host exit"
assert_contains "$RUN_OUT" "SELinux: Enforcing, Podman labeling enabled" "prepared host SELinux"
assert_contains "$RUN_OUT" "already container-readable" "prepared host labels"
assert_contains "$RUN_OUT" "Port 3001: free" "prepared host port"
assert_contains "$RUN_OUT" "fail=0" "prepared host has no failure"
assert_not_contains "$RUN_OUT" "△" "prepared host has no warning"

# --- SELinux state: enforcing, permissive, disabled ---------------------------

# Enforcing with Podman labeling off is the combination that breaks every bind
# mount while both facts look fine in isolation.
run_preflight env HIVE_DEPLOY_RUNTIME=podman FAKE_SELINUX_ENABLED=false
assert_eq "78" "$RUN_STATUS" "labeling-off exit"
assert_contains "$RUN_OUT" "labeling DISABLED" "labeling-off message"
assert_contains "$RUN_OUT" "container-selinux" "labeling-off names the policy package"
assert_contains "$RUN_OUT" "containers.conf" "labeling-off names the config to check"

run_preflight env HIVE_DEPLOY_RUNTIME=podman FAKE_SELINUX_ENABLED=__ERR__
assert_eq "0" "$RUN_STATUS" "unreported labeling exit"
assert_contains "$RUN_OUT" "did not report its labeling state" "unreported labeling message"

run_preflight env HIVE_DEPLOY_RUNTIME=podman FAKE_ENFORCE=Permissive
assert_eq "0" "$RUN_STATUS" "permissive exit"
assert_contains "$RUN_OUT" "SELinux: Permissive" "permissive reported"
assert_contains "$RUN_OUT" "will on an enforcing host" "permissive warns about the enforcing case"
assert_contains "$RUN_OUT" "ausearch" "permissive points at the denial log"

# Disabled is a legitimate host configuration, not a finding, and it takes the
# labeling checks off the table rather than leaving them noisy.
run_preflight env HIVE_DEPLOY_RUNTIME=podman FAKE_ENFORCE=Disabled
assert_eq "0" "$RUN_STATUS" "disabled exit"
assert_contains "$RUN_OUT" "not enabled on this host" "disabled reported"
assert_contains "$RUN_OUT" "Mount labeling: not applicable" "disabled skips labeling"
assert_not_contains "$RUN_OUT" "Mount label:" "disabled judges no individual label"

# No policy utilities on PATH at all. The mode must still be resolved from the
# kernel rather than degrading to "unknown", because a trimmed PATH is exactly
# what a minimal deployment host looks like.
SLIM_BIN="${TEST_TMP}/slim"
mkdir -p "$SLIM_BIN"
for tool in dirname id awk head grep; do
  ln -sf "$(command -v "$tool")" "${SLIM_BIN}/${tool}"
done
ln -sf "${FAKE_BIN}/podman" "${SLIM_BIN}/podman"
ln -sf "${FAKE_BIN}/stat" "${SLIM_BIN}/stat"
[[ -x "${SLIM_BIN}/getenforce" ]] && fail "slim PATH must not contain getenforce"

set +e
RUN_OUT="$(env PATH="$SLIM_BIN" HIVE_DEPLOY_RUNTIME=podman PODMAN_CALL_LOG="$CALL_LOG" \
  FAKE_STAT_MAP="$STAT_MAP" HIVE_SRC_DIR="$SRC_FIXTURE" "$BASH_BIN" "$PREFLIGHT" 2>&1)"
RUN_STATUS=$?
set -e
assert_eq "0" "$RUN_STATUS" "no-utilities host exit"
if [[ -r /sys/fs/selinux/enforce ]]; then
  # This host really runs SELinux; selinuxfs must supply the mode unaided.
  case "$(cat /sys/fs/selinux/enforce)" in
    1) assert_contains "$RUN_OUT" "SELinux: Enforcing" "selinuxfs supplies Enforcing without getenforce" ;;
    *) assert_contains "$RUN_OUT" "SELinux: Permissive" "selinuxfs supplies Permissive without getenforce" ;;
  esac
  assert_not_contains "$RUN_OUT" "could not be determined" "no-utilities host still resolves a state"
elif [[ ! -d /sys/fs/selinux ]]; then
  assert_contains "$RUN_OUT" "not enabled on this host" "absent selinuxfs reads as not enabled"
  assert_not_contains "$RUN_OUT" "could not be determined" "no-utilities host still resolves a state"
else
  # selinuxfs is mounted but enforce is unreadable (e.g. a container on an
  # SELinux host that masks /sys/fs/selinux/enforce): the preflight
  # deliberately reports an indeterminate state rather than guessing.
  assert_contains "$RUN_OUT" "could not be determined" "masked selinuxfs reads as indeterminate"
fi

# --- Mount labeling -----------------------------------------------------------

# The everyday Fedora case: a checkout under $HOME, so every source is
# user_home_t and no container type can read it without a relabel.
cat >"$STAT_MAP" <<EOF
${SRC_FIXTURE}/hive.yaml||||unconfined_u:object_r:user_home_t:s0
${SRC_FIXTURE}/deploy/nginx.conf||||unconfined_u:object_r:user_home_t:s0
${SRC_FIXTURE}/secrets||||unconfined_u:object_r:user_home_t:s0
${SRC_FIXTURE}/secrets/gh-app-key.pem||||system_u:object_r:container_file_t:s0
EOF

run_preflight env HIVE_DEPLOY_RUNTIME=podman
assert_eq "0" "$RUN_STATUS" "wrong labels are a warning, not a failure"
assert_contains "$RUN_OUT" "a container cannot read it as-is" "wrong label reported"
# Read-only configuration out of a checkout gets the shared option; the secrets
# directory gets the private one.
assert_contains "$RUN_OUT" "Mount it with :z" "config gets the shared relabel"
assert_contains "$RUN_OUT" "Mount it with :Z" "secrets get the private relabel"
assert_contains "$RUN_OUT" "semanage fcontext -a -t container_file_t" "persistent labeling offered"
# A directory rule needs the recursive suffix; a single file must not get one.
assert_contains "$RUN_OUT" "/src/secrets(/.*)?' && restorecon -R " "directory fcontext is recursive"
assert_contains "$RUN_OUT" "hive\\.yaml' &&" "file fcontext is exact and escapes the dot"
assert_contains "$RUN_OUT" "kernel stays enforcing" "relabeling is framed as SELinux-preserving"

# A filesystem with no security xattrs reports as unlabeled rather than wrong.
cat >"$STAT_MAP" <<EOF
${SRC_FIXTURE}/hive.yaml||||?
EOF
run_preflight env HIVE_DEPLOY_RUNTIME=podman
assert_contains "$RUN_OUT" "carries no SELinux label" "unlabeled source reported"
assert_contains "$RUN_OUT" "security xattrs" "unlabeled source explains why"

label_all_container_readable

# --- #4359: the label reader is resolved, never assumed ------------------------
#
# Both cases run with no SELinux on the host: the defect is in how the label is
# READ, so it reproduces anywhere uutils coreutils shadows GNU. #4211 measured
# that hosted runners have no SELinux at all, which is exactly why this half is
# the half that can be covered in CI.

label_all_container_readable

# The Bluefin/Universal Blue shape: uutils first, GNU still reachable. The
# fallback has to be taken, and correctly-labelled paths reported as correct.
run_preflight env HIVE_DEPLOY_RUNTIME=podman HIVE_PFH_LABEL_READERS="uustat stat"
assert_eq "0" "$RUN_STATUS" "uutils-with-fallback exit"
assert_contains "$RUN_OUT" "already container-readable" "uutils falls back and reads the real label"
assert_not_contains "$RUN_OUT" "unsupported for this operating system" \
  "uutils error string never reaches the report"
assert_not_contains "$RUN_OUT" "a container cannot read it as-is" \
  "a correctly labelled path is not warned about under uutils"

# uutils alone. There is no label to be had, and saying so is the only honest
# answer — "unlabelled" would be a different claim, and a pass would be a lie.
run_preflight env HIVE_DEPLOY_RUNTIME=podman HIVE_PFH_LABEL_READERS="uustat"
assert_eq "0" "$RUN_STATUS" "no-reader exit"
assert_contains "$RUN_OUT" "no tool on this host can read an SELinux label" \
  "no working reader is reported as its own outcome"
assert_contains "$RUN_OUT" "UNCHECKED" "no working reader does not read as a pass"
assert_not_contains "$RUN_OUT" "unsupported for this operating system" \
  "no-reader case still never prints the error string as a label"
assert_not_contains "$RUN_OUT" "already container-readable" \
  "no working reader cannot claim a path is fine"
assert_not_contains "$RUN_OUT" "carries no SELinux label" \
  "cannot-read is not reported as unlabelled"

# --- #4359: the secrets directory the container can actually traverse ---------

label_all_container_readable

# Group-owned by the mapped launch gid, with the traverse bit: the state the
# remediation reaches, verified on an enforcing host to be readable by dev.
run_preflight env HIVE_DEPLOY_RUNTIME=podman
assert_contains "$RUN_OUT" "traversable by the container's hive-launch group" \
  "correctly prepared secrets dir passes the reach check"

# Owned by the operator's own group: the state `mkdir -m 700` produces, where
# SELinux is satisfied and the read still fails, with no AVC to explain it.
cat >"$STAT_MAP" <<EOF
${SRC_FIXTURE}/hive.yaml||||system_u:object_r:container_file_t:s0
${SRC_FIXTURE}/deploy/nginx.conf||||system_u:object_r:container_file_t:s0
${SRC_FIXTURE}/secrets|700|||system_u:object_r:container_file_t:s0|$(id -g)
${SRC_FIXTURE}/secrets/gh-app-key.pem||||system_u:object_r:container_file_t:s0
EOF
run_preflight env HIVE_DEPLOY_RUNTIME=podman
assert_contains "$RUN_OUT" "Secrets reach:" "unreachable secrets dir is reported"
assert_contains "$RUN_OUT" "podman unshare chown" "rootless remedy uses the id translation"
assert_not_contains "$RUN_OUT" "chmod 0755" "remedy never widens the secrets directory"
assert_not_contains "$RUN_OUT" "setenforce" "remedy never disables SELinux"
label_all_container_readable

# --- Configuration and secrets ------------------------------------------------

# Config that is not there yet fails: the bind mount has nothing to bind.
mv "${SRC_FIXTURE}/hive.yaml" "${TEST_TMP}/hive.yaml.stash"
run_preflight env HIVE_DEPLOY_RUNTIME=podman
assert_eq "78" "$RUN_STATUS" "missing config exit"
assert_contains "$RUN_OUT" "Hive config: ${SRC_FIXTURE}/hive.yaml does not exist" "missing config reported"
mv "${TEST_TMP}/hive.yaml.stash" "${SRC_FIXTURE}/hive.yaml"

mv "${SRC_FIXTURE}/deploy/nginx.conf" "${TEST_TMP}/nginx.conf.stash"
run_preflight env HIVE_DEPLOY_RUNTIME=podman
assert_eq "78" "$RUN_STATUS" "missing gateway config exit"
assert_contains "$RUN_OUT" "Gateway config" "missing gateway config reported"
mv "${TEST_TMP}/nginx.conf.stash" "${SRC_FIXTURE}/deploy/nginx.conf"

# A token-only deployment never creates secrets/, so its absence is a warning
# with a narrow-by-construction remedy — not a failure and not a wide mkdir.
mv "${SRC_FIXTURE}/secrets" "${TEST_TMP}/secrets.stash"
run_preflight env HIVE_DEPLOY_RUNTIME=podman
assert_eq "0" "$RUN_STATUS" "absent secrets dir is not a failure"
assert_contains "$RUN_OUT" "does not exist" "absent secrets dir reported"
assert_contains "$RUN_OUT" "mkdir -m 750" "absent secrets dir remedy creates it traversable"
assert_not_contains "$RUN_OUT" "mkdir -m 700" "absent secrets dir remedy no longer excludes the container"
mv "${TEST_TMP}/secrets.stash" "${SRC_FIXTURE}/secrets"

# An over-permissive secret is reported and left alone. The only command
# offered narrows it, and run_preflight has already asserted the mode on disk
# did not move.
chmod 644 "${SRC_FIXTURE}/secrets/gh-app-key.pem"
run_preflight env HIVE_DEPLOY_RUNTIME=podman
assert_eq "0" "$RUN_STATUS" "over-permissive secret is a warning"
assert_contains "$RUN_OUT" "wider than 600" "over-permissive secret reported"
assert_contains "$RUN_OUT" "chmod 600" "over-permissive secret remedy narrows"
assert_contains "$RUN_OUT" "loosening host permissions is never the remedy" "no broadening offered"
assert_eq "644" "$("$REAL_STAT" -c '%a' "${SRC_FIXTURE}/secrets/gh-app-key.pem")" \
  "secret mode untouched by preflight"
chmod 600 "${SRC_FIXTURE}/secrets/gh-app-key.pem"

chmod 775 "${SRC_FIXTURE}/secrets"
run_preflight env HIVE_DEPLOY_RUNTIME=podman
assert_contains "$RUN_OUT" "wider than 750" "over-permissive secrets dir reported"
chmod 750 "${SRC_FIXTURE}/secrets"

# 0400 is narrower than the 0600 maximum and must not be flagged.
chmod 400 "${SRC_FIXTURE}/secrets/gh-app-key.pem"
run_preflight env HIVE_DEPLOY_RUNTIME=podman
assert_not_contains "$RUN_OUT" "wider than 600" "0400 secret is not flagged as too wide"
chmod 600 "${SRC_FIXTURE}/secrets/gh-app-key.pem"

# Rootless maps only the invoking user to container root; anything else lands
# as nobody, so foreign ownership is a latent failure even when it reads today.
cat >"$STAT_MAP" <<EOF
${SRC_FIXTURE}/secrets/gh-app-key.pem||4242|otheruser|system_u:object_r:container_file_t:s0
EOF
run_preflight env HIVE_DEPLOY_RUNTIME=podman FAKE_ROOTLESS=true
assert_contains "$RUN_OUT" "is owned by otheruser" "foreign ownership reported rootless"
assert_contains "$RUN_OUT" "'nobody'" "foreign ownership explains the mapping"

# Rootful Podman does not map UIDs that way, so the same file is unremarkable.
run_preflight env HIVE_DEPLOY_RUNTIME=podman FAKE_ROOTLESS=false
assert_not_contains "$RUN_OUT" "is owned by otheruser" "foreign ownership is rootless-only"

label_all_container_readable

if [[ "$EUID" -ne 0 ]]; then
  # root can read anything, so this case only means something unprivileged.
  chmod 000 "${SRC_FIXTURE}/hive.yaml"
  run_preflight env HIVE_DEPLOY_RUNTIME=podman
  assert_eq "78" "$RUN_STATUS" "unreadable config exit"
  assert_contains "$RUN_OUT" "is not readable by" "unreadable config reported"
  assert_contains "$RUN_OUT" "do not widen the mode to make this pass" "no broadening offered"
  chmod 644 "${SRC_FIXTURE}/hive.yaml"
else
  printf 'SKIP: running as root — the unreadable-source case cannot be exercised\n'
fi

# --- Ports --------------------------------------------------------------------

# 3001 is the authenticated gateway and is checked whether or not the operator
# names it. 7681, the raw writable ttyd port, is deliberately never published
# and so is deliberately never checked.
run_preflight env HIVE_DEPLOY_RUNTIME=podman
assert_contains "$RUN_OUT" "Port 3001" "3001 checked by default"
assert_not_contains "$RUN_OUT" "Port 7681" "the unpublished ttyd port is not checked"

run_preflight env HIVE_DEPLOY_RUNTIME=podman \
  FAKE_LISTENERS="LISTEN 0 4096 0.0.0.0:3001 0.0.0.0:*"
assert_eq "78" "$RUN_STATUS" "occupied port exit"
assert_contains "$RUN_OUT" "Port 3001: already in use" "occupied port reported"
assert_contains "$RUN_OUT" "held by: LISTEN 0 4096 0.0.0.0:3001" "occupied port names the listener"
assert_contains "$RUN_OUT" "Preflight changes nothing" "occupied port is not resolved for the operator"

# A listener on a different port must not be mistaken for one on 3001, and a
# port that merely ends in the same digits must not match either.
run_preflight env HIVE_DEPLOY_RUNTIME=podman \
  FAKE_LISTENERS="LISTEN 0 4096 0.0.0.0:13001 0.0.0.0:*"
assert_eq "0" "$RUN_STATUS" "13001 is not 3001"
assert_contains "$RUN_OUT" "Port 3001: free" "suffix match does not occupy the port"

# Additional published ports are checked, in either separator style.
run_preflight env HIVE_DEPLOY_RUNTIME=podman HIVE_PODMAN_PREFLIGHT_PORTS="3001,8443"
assert_contains "$RUN_OUT" "Port 8443: free" "comma-separated extra port checked"
run_preflight env HIVE_DEPLOY_RUNTIME=podman HIVE_PODMAN_PREFLIGHT_PORTS="3001 8443"
assert_contains "$RUN_OUT" "Port 8443: free" "space-separated extra port checked"

run_preflight env HIVE_DEPLOY_RUNTIME=podman HIVE_PODMAN_PREFLIGHT_PORTS="https"
assert_eq "78" "$RUN_STATUS" "non-numeric port exit"
assert_contains "$RUN_OUT" "is not a valid TCP port" "non-numeric port reported"
run_preflight env HIVE_DEPLOY_RUNTIME=podman HIVE_PODMAN_PREFLIGHT_PORTS="70000"
assert_eq "78" "$RUN_STATUS" "out-of-range port exit"

# Rootless cannot bind below the unprivileged floor at all. The remedy raises
# the host's allowance or moves the port; it never says "run Hive as root".
run_preflight env HIVE_DEPLOY_RUNTIME=podman FAKE_ROOTLESS=true \
  HIVE_PODMAN_PREFLIGHT_PORTS="443"
assert_eq "78" "$RUN_STATUS" "sub-floor rootless port exit"
assert_contains "$RUN_OUT" "below the rootless unprivileged floor (1024)" "sub-floor port reported"
assert_contains "$RUN_OUT" "net.ipv4.ip_unprivileged_port_start=443" "sub-floor remedy is the sysctl"
assert_not_contains "$RUN_OUT" "as root" "sub-floor remedy does not propose running as root"

# Rootful publishes any port, so the floor does not apply.
run_preflight env HIVE_DEPLOY_RUNTIME=podman FAKE_ROOTLESS=false \
  HIVE_PODMAN_PREFLIGHT_PORTS="443"
assert_eq "0" "$RUN_STATUS" "rootful sub-floor port exit"
assert_contains "$RUN_OUT" "Port 443: free" "rootful ignores the unprivileged floor"

# A host that already lowered the floor can publish below 1024 rootless.
run_preflight env HIVE_DEPLOY_RUNTIME=podman FAKE_ROOTLESS=true FAKE_PORT_FLOOR=80 \
  HIVE_PODMAN_PREFLIGHT_PORTS="443"
assert_eq "0" "$RUN_STATUS" "lowered floor exit"
assert_contains "$RUN_OUT" "Port 443: free" "lowered floor permits the port"

# No enumeration tool: say so rather than reporting the port as free.
NOSS_BIN="${TEST_TMP}/noss"
mkdir -p "$NOSS_BIN"
for tool in dirname id awk head grep cat find sort xargs sed; do
  ln -sf "$(command -v "$tool")" "${NOSS_BIN}/${tool}"
done
for tool in podman getenforce stat sysctl; do
  ln -sf "${FAKE_BIN}/${tool}" "${NOSS_BIN}/${tool}"
done
[[ -x "${NOSS_BIN}/ss" || -x "${NOSS_BIN}/netstat" ]] && fail "no-ss PATH must have neither tool"

set +e
RUN_OUT="$(env PATH="$NOSS_BIN" HIVE_DEPLOY_RUNTIME=podman PODMAN_CALL_LOG="$CALL_LOG" \
  FAKE_STAT_MAP="$STAT_MAP" HIVE_SRC_DIR="$SRC_FIXTURE" "$BASH_BIN" "$PREFLIGHT" 2>&1)"
RUN_STATUS=$?
set -e
assert_eq "0" "$RUN_STATUS" "no port tool exit"
assert_contains "$RUN_OUT" "availability unverified" "no port tool reported"
assert_contains "$RUN_OUT" "iproute2" "no port tool remedy"
assert_not_contains "$RUN_OUT" "Port 3001: free" "no port tool does not claim the port is free"

# --- Remediation never weakens the host ---------------------------------------
#
# Swept across every case above rather than asserted once: no output path may
# offer to turn SELinux off, opt a container out of labeling, or widen access.

FORBIDDEN=(
  "setenforce 0"
  "SELINUX=disabled"
  "selinux=0"
  "label=disable"
  "label:disable"
  "--privileged"
  "chmod 777"
  "chmod 666"
  "chmod a+r"
  "chmod o+r"
  "chmod -R 777"
)

sweep_out=""
for case_env in \
  "HIVE_DEPLOY_RUNTIME=podman" \
  "HIVE_DEPLOY_RUNTIME=podman FAKE_SELINUX_ENABLED=false" \
  "HIVE_DEPLOY_RUNTIME=podman FAKE_ENFORCE=Permissive" \
  "HIVE_DEPLOY_RUNTIME=podman FAKE_ENFORCE=Disabled" \
  "HIVE_DEPLOY_RUNTIME=podman FAKE_ROOTLESS=true HIVE_PODMAN_PREFLIGHT_PORTS=443" \
  "HIVE_DEPLOY_RUNTIME=podman FAKE_LISTENERS=LISTEN_0_4096_0.0.0.0:3001"
do
  # shellcheck disable=SC2086 # the case string is a deliberate word-split env list
  run_preflight env $case_env
  sweep_out+="${RUN_OUT}"$'\n'
done

# The user_home_t case carries the densest remediation text, so sweep it too.
cat >"$STAT_MAP" <<EOF
${SRC_FIXTURE}/hive.yaml||||unconfined_u:object_r:user_home_t:s0
${SRC_FIXTURE}/deploy/nginx.conf||||unconfined_u:object_r:user_home_t:s0
${SRC_FIXTURE}/secrets||||unconfined_u:object_r:user_home_t:s0
EOF
run_preflight env HIVE_DEPLOY_RUNTIME=podman
sweep_out+="${RUN_OUT}"$'\n'
chmod 644 "${SRC_FIXTURE}/secrets/gh-app-key.pem"
run_preflight env HIVE_DEPLOY_RUNTIME=podman
sweep_out+="${RUN_OUT}"$'\n'
chmod 600 "${SRC_FIXTURE}/secrets/gh-app-key.pem"

for forbidden in "${FORBIDDEN[@]}"; do
  assert_not_contains "$sweep_out" "$forbidden" "no case ever recommends '${forbidden}'"
done

# --- Layout detection (#4422) -------------------------------------------------
#
# The bug: this check knew only the source-tree layout, so a Quadlet install —
# where nginx.conf sits FLAT in %E/hive rather than under deploy/ — reported
# `✗ Gateway config: <dir>/deploy/nginx.conf does not exist` and exited 78 with
# the file present one level up. These cases pin both layouts, the forced
# override, and the fact that the two recommend DIFFERENT relabel options.

QUADLET_FIXTURE="${TEST_TMP}/quadlet"
mkdir -p "${QUADLET_FIXTURE}/secrets"
echo "levels: []" >"${QUADLET_FIXTURE}/hive.yaml"
echo "events {}" >"${QUADLET_FIXTURE}/nginx.conf"
echo "-----BEGIN PRIVATE KEY-----" >"${QUADLET_FIXTURE}/secrets/gh-app-key.pem"
printf '#HIVE_DASHBOARD_TOKEN=\n' >"${QUADLET_FIXTURE}/hive.env"
chmod 750 "${QUADLET_FIXTURE}/secrets"
chmod 600 "${QUADLET_FIXTURE}/secrets/gh-app-key.pem" "${QUADLET_FIXTURE}/hive.env"

label_quadlet_container_readable() {
  cat >"$STAT_MAP" <<EOF
${QUADLET_FIXTURE}/hive.yaml||||system_u:object_r:container_file_t:s0
${QUADLET_FIXTURE}/nginx.conf||||system_u:object_r:container_file_t:s0
${QUADLET_FIXTURE}/secrets||||system_u:object_r:container_file_t:s0|${MAPPED_LAUNCH_GID}
${QUADLET_FIXTURE}/secrets/gh-app-key.pem||||system_u:object_r:container_file_t:s0
EOF
}

# Same runner, pointed at a chosen HIVE_SRC_DIR. The fixture-immutability
# assertion in run_preflight only covers SRC_FIXTURE, so this snapshots the
# quadlet fixture itself to keep "read-only" proven on this path too.
run_preflight_dir() { # $1 = HIVE_SRC_DIR, rest = extra env
  local dir="$1"; shift
  local before after
  before="$(find "$dir" -mindepth 0 -print0 | sort -z | xargs -0 "$REAL_STAT" -c '%n %a %u %g %s')"
  : >"$CALL_LOG"
  set +e
  RUN_OUT="$(env PATH="$TEST_PATH" PODMAN_CALL_LOG="$CALL_LOG" FAKE_STAT_MAP="$STAT_MAP" \
    HIVE_SRC_DIR="$dir" HIVE_PFH_SUBGID_FILE="$SUBGID_FIXTURE" \
    HIVE_PFH_LABEL_PROBES="$LABEL_PROBE" FAKE_LABEL_PROBE="$LABEL_PROBE" \
    HIVE_DEPLOY_RUNTIME=podman "$@" "$BASH_BIN" "$PREFLIGHT" 2>&1)"
  RUN_STATUS=$?
  set -e
  after="$(find "$dir" -mindepth 0 -print0 | sort -z | xargs -0 "$REAL_STAT" -c '%n %a %u %g %s')"
  [[ "$before" == "$after" ]] || fail "preflight modified ${dir}"
  pass_count=$((pass_count + 1))
}

# THE BUG, as a test: a Quadlet layout must not report a missing gateway config.
label_quadlet_container_readable
run_preflight_dir "$QUADLET_FIXTURE"
assert_contains "$RUN_OUT" "layout: quadlet (detected)" "quadlet layout is auto-detected"
assert_contains "$RUN_OUT" "Gateway config: ${QUADLET_FIXTURE}/nginx.conf readable" \
  "quadlet: the flat nginx.conf is found"
assert_not_contains "$RUN_OUT" "deploy/nginx.conf does not exist" \
  "quadlet: no false failure about deploy/nginx.conf"
assert_eq "$RUN_STATUS" 0 "quadlet: a correctly prepared install exits 0, not 78"

# The source tree keeps working exactly as before.
label_all_container_readable
run_preflight_dir "$SRC_FIXTURE"
assert_contains "$RUN_OUT" "layout: source (detected)" "source layout is auto-detected"
assert_contains "$RUN_OUT" "Gateway config: ${SRC_FIXTURE}/deploy/nginx.conf readable" \
  "source: deploy/nginx.conf is still where it looks"
assert_eq "$RUN_STATUS" 0 "source: unchanged, still exits 0"

# Detection keys on the gateway config; hive.env is the tiebreak for a Quadlet
# directory that has not had nginx.conf copied in yet.
TIEBREAK_FIXTURE="${TEST_TMP}/tiebreak"
mkdir -p "$TIEBREAK_FIXTURE"
printf '#HIVE_DASHBOARD_TOKEN=\n' >"${TIEBREAK_FIXTURE}/hive.env"
chmod 600 "${TIEBREAK_FIXTURE}/hive.env"
run_preflight_dir "$TIEBREAK_FIXTURE"
assert_contains "$RUN_OUT" "layout: quadlet (detected)" \
  "hive.env alone is enough to identify a Quadlet directory"

# An empty directory stays on the historical default rather than guessing.
EMPTY_FIXTURE="${TEST_TMP}/empty-layout"
mkdir -p "$EMPTY_FIXTURE"
run_preflight_dir "$EMPTY_FIXTURE"
assert_contains "$RUN_OUT" "layout: source (detected)" \
  "a directory with neither shape falls back to the historical default"

# The explicit override, for a directory mid-install.
run_preflight_dir "$EMPTY_FIXTURE" HIVE_PODMAN_LAYOUT=quadlet
assert_contains "$RUN_OUT" "layout: quadlet (HIVE_PODMAN_LAYOUT)" \
  "HIVE_PODMAN_LAYOUT=quadlet forces the layout and says so"
run_preflight_dir "$QUADLET_FIXTURE" HIVE_PODMAN_LAYOUT=source
assert_contains "$RUN_OUT" "layout: source (HIVE_PODMAN_LAYOUT)" \
  "HIVE_PODMAN_LAYOUT=source forces the layout even where quadlet would be detected"
run_preflight_dir "$QUADLET_FIXTURE" HIVE_PODMAN_LAYOUT=sideways
assert_eq "$RUN_STATUS" 64 "an unknown HIVE_PODMAN_LAYOUT is EX_USAGE"
assert_contains "$RUN_OUT" "must be auto, source, or quadlet" "and says what the valid values are"

# THE RELABEL RECOMMENDATION FLIPS WITH THE LAYOUT, and getting it backwards
# would contradict the shipped units, which mount all three :ro,Z.
cat >"$STAT_MAP" <<EOF
${QUADLET_FIXTURE}/hive.yaml||||unconfined_u:object_r:config_home_t:s0
${QUADLET_FIXTURE}/nginx.conf||||unconfined_u:object_r:config_home_t:s0
${QUADLET_FIXTURE}/secrets||||system_u:object_r:container_file_t:s0|${MAPPED_LAUNCH_GID}
${QUADLET_FIXTURE}/secrets/gh-app-key.pem||||system_u:object_r:container_file_t:s0
EOF
run_preflight_dir "$QUADLET_FIXTURE"
assert_contains "$RUN_OUT" "Mount it with :Z" "quadlet: config files are recommended :Z, matching the units"
assert_not_contains "$RUN_OUT" "Mount it with :z so Podman relabels it shared" \
  "quadlet: never recommends the shared form for an operator-owned config file"
cat >"$STAT_MAP" <<EOF
${SRC_FIXTURE}/hive.yaml||||unconfined_u:object_r:user_home_t:s0
${SRC_FIXTURE}/deploy/nginx.conf||||unconfined_u:object_r:user_home_t:s0
${SRC_FIXTURE}/secrets||||system_u:object_r:container_file_t:s0|${MAPPED_LAUNCH_GID}
${SRC_FIXTURE}/secrets/gh-app-key.pem||||system_u:object_r:container_file_t:s0
EOF
run_preflight_dir "$SRC_FIXTURE"
assert_contains "$RUN_OUT" "Mount it with :z so Podman relabels it shared" \
  "source: a checked-out config file is still recommended :z, not :Z"

# --- hive.env, Quadlet only ---------------------------------------------------
#
# Not a bind source — EnvironmentFile= is read by systemd on the host and never
# mounted, so it has no container label. It is checked because both ways it goes
# wrong cost the whole start budget.
label_quadlet_container_readable
mv "${QUADLET_FIXTURE}/hive.env" "${TEST_TMP}/hive.env.stash"
run_preflight_dir "$QUADLET_FIXTURE"
assert_contains "$RUN_OUT" "Environment file: ${QUADLET_FIXTURE}/hive.env does not exist" \
  "quadlet: a missing hive.env is a failure"
assert_contains "$RUN_OUT" "'--env-file' on the container command line" "and says why the container start would fail"
assert_eq "$RUN_STATUS" 78 "quadlet: a missing hive.env exits 78"
mv "${TEST_TMP}/hive.env.stash" "${QUADLET_FIXTURE}/hive.env"

run_preflight_dir "$QUADLET_FIXTURE"
assert_contains "$RUN_OUT" "HIVE_DASHBOARD_TOKEN is not set" "quadlet: an unset dashboard token is reported"
assert_eq "$RUN_STATUS" 0 "an unset dashboard token is a WARNING — a hub-hosted hive does not need one"

printf 'HIVE_DASHBOARD_TOKEN=%s\n' "deadbeefcafe" >>"${QUADLET_FIXTURE}/hive.env"
run_preflight_dir "$QUADLET_FIXTURE"
assert_contains "$RUN_OUT" "HIVE_DASHBOARD_TOKEN is set in hive.env" "quadlet: a set dashboard token passes"
assert_not_contains "$RUN_OUT" "deadbeefcafe" "the token VALUE is never printed"

# The source layout has no hive.env and must not grow a check for one.
label_all_container_readable
run_preflight_dir "$SRC_FIXTURE"
assert_not_contains "$RUN_OUT" "Environment file" "source: no hive.env row — it is a Quadlet-only file"
assert_not_contains "$RUN_OUT" "HIVE_DASHBOARD_TOKEN" "source: no dashboard-token row either"

printf 'PASS: %d Podman host preflight assertions\n' "$pass_count"
