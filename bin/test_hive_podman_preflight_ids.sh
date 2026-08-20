#!/usr/bin/env bash
# Contract tests for bin/hive-podman-preflight-ids.sh (#4208).
# Run: bash bin/test_hive_podman_preflight_ids.sh
#
# Every input is mocked. A stub `podman` answers the info templates from the
# environment, a stub `stat` answers the graphroot filesystem type, and the
# subid tables are ordinary files the case writes. The whole matrix — delegated,
# missing, short, multi-range, NFS, tmpfs, missing helper, rootful — therefore
# runs on a host with no Podman, no NFS mount, and no privileges, and never
# executes a container command or touches a real /etc file.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PREFLIGHT="${ROOT}/bin/hive-podman-preflight-ids.sh"
BASH_BIN="$(command -v bash)"
TEST_TMP="$(mktemp -d)"
trap 'rm -rf "$TEST_TMP"' EXIT

FAKE_BIN="${TEST_TMP}/bin"
CALL_LOG="${TEST_TMP}/podman-calls.log"
mkdir -p "$FAKE_BIN"

REAL_STAT="$(command -v stat)"

cat >"${FAKE_BIN}/podman" <<'EOF'
#!/bin/sh
printf 'podman %s\n' "$*" >>"$PODMAN_CALL_LOG"
case "$*" in
  "info --format {{.Host.Security.Rootless}}")
    [ "${FAKE_ROOTLESS:-}" = "__ERR__" ] && exit 125
    printf '%s\n' "${FAKE_ROOTLESS:-true}"
    ;;
  "info --format {{.Store.GraphRoot}}")
    [ "${FAKE_GRAPHROOT:-}" = "__ERR__" ] && exit 125
    printf '%s\n' "${FAKE_GRAPHROOT:-/home/u/.local/share/containers/storage}"
    ;;
  "info --format {{.Store.GraphDriverName}}")
    [ "${FAKE_GRAPHDRIVER:-}" = "__ERR__" ] && exit 125
    printf '%s\n' "${FAKE_GRAPHDRIVER:-overlay}"
    ;;
  "info --format {{.Host.NetworkBackend}}")
    [ "${FAKE_BACKEND:-}" = "__ERR__" ] && exit 125
    printf '%s\n' "${FAKE_BACKEND:-netavark}"
    ;;
  "info --format {{.Host.RootlessNetworkCmd}}")
    [ "${FAKE_HELPER:-}" = "__ERR__" ] && exit 125
    printf '%s\n' "${FAKE_HELPER:-pasta}"
    ;;
  *) exit 1 ;;
esac
EOF

# Answers `stat -f -c %T <path>` from FAKE_FSTYPE and delegates everything else
# to the real stat, so a case can put the graphroot "on NFS" without needing
# one.
cat >"${FAKE_BIN}/stat" <<EOF
#!/bin/sh
if [ "\$1" = "-f" ] && [ "\$2" = "-c" ] && [ "\$3" = "%T" ]; then
  [ "\${FAKE_FSTYPE:-}" = "__ERR__" ] && exit 1
  printf '%s\n' "\${FAKE_FSTYPE:-btrfs}"
  exit 0
fi
exec "${REAL_STAT}" "\$@"
EOF

# A stand-in for the default rootless network helper. It exists so the "helper
# present" case is decided by THIS file rather than by whether the machine
# running the tests happens to have passt installed — both pasta and
# slirp4netns are present on a typical Fedora workstation, which would make the
# absent-helper case silently untestable.
cat >"${FAKE_BIN}/pasta" <<'EOF'
#!/bin/sh
exit 0
EOF

chmod +x "${FAKE_BIN}"/*

# PATH is the fake bin ALONE. The preflight needs only podman, stat and shell
# builtins, so nothing real leaks in — which is what makes "slirp4netns is not
# installed" a fact of the fixture instead of a fact of the host.
TEST_PATH="${FAKE_BIN}"

SUBUID="${TEST_TMP}/subuid"
SUBGID="${TEST_TMP}/subgid"

pass_count=0
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
assert_contains() {
  [[ "$1" == *"$2"* ]] || fail "$3: missing '$2' in: $1"
  pass_count=$((pass_count + 1))
}
assert_not_contains() {
  [[ "$1" != *"$2"* ]] || fail "$3: unexpectedly found '$2' in: $1"
  pass_count=$((pass_count + 1))
}
assert_eq() {
  [[ "$2" == "$1" ]] || fail "$3: got '$2', want '$1'"
  pass_count=$((pass_count + 1))
}

# Runs the preflight with Podman selected and the mocks in place. Extra
# KEY=VALUE arguments become environment for that run only.
run_preflight() {
  local -a env=(
    "PATH=${TEST_PATH}"
    "PODMAN_CALL_LOG=${CALL_LOG}"
    "HIVE_DEPLOY_RUNTIME=podman"
    "HIVE_PODMAN_PREFLIGHT_SELECTOR=/nonexistent"
    "HIVE_PODMAN_SUBUID_FILE=${SUBUID}"
    "HIVE_PODMAN_SUBGID_FILE=${SUBGID}"
    "USER=testuser"
  )
  env+=("$@")
  : >"$CALL_LOG"
  set +e
  PREFLIGHT_OUT="$(env -i "${env[@]}" "$BASH_BIN" "$PREFLIGHT" check 2>&1)"
  PREFLIGHT_RC=$?
  set -e
}

# A correctly prepared host: one 65536-wide range in each table.
delegated_ranges() {
  printf 'testuser:100000:65536\n' >"$SUBUID"
  printf 'testuser:100000:65536\n' >"$SUBGID"
}

# ── SUPPORTED ────────────────────────────────────────────────────────────────

delegated_ranges
run_preflight
assert_eq 0 "$PREFLIGHT_RC" "healthy host exits 0"
assert_contains "$PREFLIGHT_OUT" "✓ Subordinate IDs: testuser has subuid=65536 subgid=65536" "healthy subid"
assert_contains "$PREFLIGHT_OUT" "✓ Graphroot:" "healthy graphroot"
assert_contains "$PREFLIGHT_OUT" "on btrfs" "graphroot fstype reported"
assert_contains "$PREFLIGHT_OUT" "✓ Network backend: netavark" "healthy backend"
assert_contains "$PREFLIGHT_OUT" "✓ Rootless network helper: pasta present" "helper present"
assert_contains "$PREFLIGHT_OUT" "SUMMARY: pass=4 warn=0 fail=0" "healthy summary"

# Several delegated ranges add up rather than only the first counting.
printf 'testuser:100000:32768\ntestuser:200000:32768\n' >"$SUBUID"
printf 'testuser:100000:65536\n' >"$SUBGID"
run_preflight
assert_contains "$PREFLIGHT_OUT" "subuid=65536" "multiple ranges sum"
assert_eq 0 "$PREFLIGHT_RC" "summed ranges are sufficient"

# Comments and blank lines are table syntax, not entries.
printf '# a comment\n\ntestuser:100000:65536\n' >"$SUBUID"
printf 'testuser:100000:65536\n' >"$SUBGID"
run_preflight
assert_contains "$PREFLIGHT_OUT" "✓ Subordinate IDs" "comments ignored"

# ── MISSING ──────────────────────────────────────────────────────────────────

# No entry at all: rootless containers cannot start. This is the hard failure.
printf 'someoneelse:100000:65536\n' >"$SUBUID"
printf 'someoneelse:100000:65536\n' >"$SUBGID"
run_preflight
assert_eq 78 "$PREFLIGHT_RC" "missing subid mappings fail with EX_CONFIG"
assert_contains "$PREFLIGHT_OUT" "✗ Subordinate IDs: no range delegated to testuser" "missing subid reported"
assert_contains "$PREFLIGHT_OUT" "usermod --add-subuids" "missing subid remediation"
assert_contains "$PREFLIGHT_OUT" "will not edit /etc/subuid" "states it will not edit the tables"

# A short range starts and then breaks mid-extraction — warn, do not fail.
printf 'testuser:100000:1000\n' >"$SUBUID"
printf 'testuser:100000:1000\n' >"$SUBGID"
run_preflight
assert_eq 0 "$PREFLIGHT_RC" "short range warns rather than fails"
assert_contains "$PREFLIGHT_OUT" "△ Subordinate IDs: testuser has subuid=1000 subgid=1000" "short range reported"
assert_contains "$PREFLIGHT_OUT" "below the usual 65536" "short range names the threshold"

# An unreadable table is a report, not a verdict.
delegated_ranges
run_preflight "HIVE_PODMAN_SUBUID_FILE=${TEST_TMP}/absent-subuid"
assert_eq 0 "$PREFLIGHT_RC" "unreadable subuid does not fail the run"
assert_contains "$PREFLIGHT_OUT" "△ Subordinate IDs:" "unreadable subuid warns"
assert_contains "$PREFLIGHT_OUT" "not readable" "unreadable subuid explained"

# The helper the engine names is not installed: containers get no network.
delegated_ranges
run_preflight "FAKE_HELPER=slirp4netns"
assert_eq 78 "$PREFLIGHT_RC" "absent helper fails"
assert_contains "$PREFLIGHT_OUT" "✗ Rootless network helper: podman selects slirp4netns, but it is not installed" "absent helper reported"
assert_contains "$PREFLIGHT_OUT" "slirp4netns" "absent helper remediation names the package"

# Podman reporting no helper at all is unknown, not absent.
run_preflight "FAKE_HELPER=__ERR__"
assert_eq 0 "$PREFLIGHT_RC" "unknown helper does not fail"
assert_contains "$PREFLIGHT_OUT" "△ Rootless network helper: podman did not report one" "unknown helper warns"

# ── UNSUPPORTED ──────────────────────────────────────────────────────────────

# The acceptance case: NFS graphroot is rejected with remediation.
delegated_ranges
run_preflight "FAKE_FSTYPE=nfs" "FAKE_GRAPHROOT=/net/home/u/.local/share/containers/storage"
assert_eq 78 "$PREFLIGHT_RC" "NFS graphroot fails"
assert_contains "$PREFLIGHT_OUT" "✗ Graphroot:" "NFS graphroot reported as a failure"
assert_contains "$PREFLIGHT_OUT" "is on nfs, which container storage does not support" "NFS named explicitly"
assert_contains "$PREFLIGHT_OUT" "storage.conf" "NFS remediation names the config to change"
assert_contains "$PREFLIGHT_OUT" "podman system reset" "NFS remediation names the follow-up"

# The other distributed filesystems are rejected the same way.
for fs in nfs4 cifs fuse.sshfs 9p ceph glusterfs; do
  run_preflight "FAKE_FSTYPE=${fs}"
  assert_eq 78 "$PREFLIGHT_RC" "${fs} graphroot fails"
  assert_contains "$PREFLIGHT_OUT" "is on ${fs}, which container storage does not support" "${fs} named explicitly"
done

# tmpfs works but loses everything on reboot: a warning about the cost.
run_preflight "FAKE_FSTYPE=tmpfs"
assert_eq 0 "$PREFLIGHT_RC" "tmpfs graphroot warns rather than fails"
assert_contains "$PREFLIGHT_OUT" "△ Graphroot:" "tmpfs warns"
assert_contains "$PREFLIGHT_OUT" "lost on reboot" "tmpfs cost stated"

# An undeterminable filesystem type is a report, not a verdict.
run_preflight "FAKE_FSTYPE=__ERR__"
assert_eq 0 "$PREFLIGHT_RC" "unknown fstype does not fail"
assert_contains "$PREFLIGHT_OUT" "filesystem type could not be determined" "unknown fstype warns"

# Retired CNI backend: report the deprecation, do not fail a working host.
run_preflight "FAKE_BACKEND=cni"
assert_eq 0 "$PREFLIGHT_RC" "cni backend does not fail"
assert_contains "$PREFLIGHT_OUT" "△ Network backend: cni" "cni reported"
assert_contains "$PREFLIGHT_OUT" "netavark" "cni remediation names netavark"

# ── ROOTFUL: the rootless-only checks must not fire ──────────────────────────

delegated_ranges
printf 'someoneelse:100000:65536\n' >"$SUBUID"
run_preflight "FAKE_ROOTLESS=false" "FAKE_HELPER=slirp4netns"
assert_eq 0 "$PREFLIGHT_RC" "rootful host is clean despite no subid range and no helper"
assert_contains "$PREFLIGHT_OUT" "✓ Subordinate IDs: not required (rootful engine)" "rootful skips subid"
assert_contains "$PREFLIGHT_OUT" "✓ Rootless network helper: not required (rootful engine)" "rootful skips helper"

# ── DOCKER: not one Podman command may run ──────────────────────────────────

delegated_ranges
: >"$CALL_LOG"
set +e
DOCKER_OUT="$(env -i "PATH=${TEST_PATH}" "PODMAN_CALL_LOG=${CALL_LOG}" \
  "HIVE_DEPLOY_RUNTIME=docker" "HIVE_PODMAN_PREFLIGHT_SELECTOR=/nonexistent" \
  "$BASH_BIN" "$PREFLIGHT" check 2>&1)"
DOCKER_RC=$?
set -e
assert_eq 0 "$DOCKER_RC" "docker selection exits 0"
assert_contains "$DOCKER_OUT" "skipped" "docker selection reports the skip"
assert_eq "" "$(cat "$CALL_LOG")" "docker selection runs NO podman command"
assert_not_contains "$DOCKER_OUT" "Subordinate IDs" "docker selection runs no check"

# ── READ-ONLY: the subid tables must be byte-identical afterwards ────────────

delegated_ranges
BEFORE_UID="$(cat "$SUBUID")"
BEFORE_GID="$(cat "$SUBGID")"
run_preflight "FAKE_FSTYPE=nfs" "FAKE_HELPER=slirp4netns"
assert_eq "$BEFORE_UID" "$(cat "$SUBUID")" "subuid table unchanged by a failing run"
assert_eq "$BEFORE_GID" "$(cat "$SUBGID")" "subgid table unchanged by a failing run"

# ── Unknown action ──────────────────────────────────────────────────────────

set +e
BAD_OUT="$(env -i "PATH=${TEST_PATH}" "HIVE_DEPLOY_RUNTIME=podman" \
  "$BASH_BIN" "$PREFLIGHT" bogus 2>&1)"
BAD_RC=$?
set -e
assert_eq 64 "$BAD_RC" "unknown action exits 64"
assert_contains "$BAD_OUT" "unknown action" "unknown action explained"

printf 'PASS: %d Podman ID/storage/network preflight assertions\n' "$pass_count"
