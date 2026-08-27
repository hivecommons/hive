#!/usr/bin/env bash
# Contract tests for bin/hive-podman-preflight.sh (#4207).
# Run: bash bin/test_hive_podman_preflight.sh
#
# Every case drives a stub `podman` whose output is set from the environment,
# so the whole matrix — old versions, rootful, cgroup v1, a remote connection,
# a missing engine — runs on a host that has no Podman at all and never
# executes a real container command.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PREFLIGHT="${ROOT}/bin/hive-podman-preflight.sh"
BASH_BIN="$(command -v bash)"
TEST_TMP="$(mktemp -d)"
trap 'rm -rf "$TEST_TMP"' EXIT

FAKE_BIN="${TEST_TMP}/bin"
CALL_LOG="${TEST_TMP}/podman-calls.log"
mkdir -p "$FAKE_BIN"

cat >"${FAKE_BIN}/podman" <<'EOF'
#!/bin/sh
# Stub Podman. Records every invocation, then answers from the environment.
printf 'podman %s\n' "$*" >>"$PODMAN_CALL_LOG"

case "$*" in
  "version --format {{.Client.Version}}")
    [ "${FAKE_VERSION:-}" = "__ERR__" ] && exit 125
    printf '%s\n' "${FAKE_VERSION:-5.8.4}"
    ;;
  "info --format {{.Host.ServiceIsRemote}}")
    [ "${FAKE_REMOTE:-}" = "__ERR__" ] && exit 125
    printf '%s\n' "${FAKE_REMOTE:-false}"
    ;;
  "info --format {{.Host.Security.Rootless}}")
    [ "${FAKE_ROOTLESS:-}" = "__ERR__" ] && exit 125
    printf '%s\n' "${FAKE_ROOTLESS:-true}"
    ;;
  "info --format {{.Host.CgroupsVersion}}")
    [ "${FAKE_CGROUPS:-}" = "__ERR__" ] && exit 125
    printf '%s\n' "${FAKE_CGROUPS:-v2}"
    ;;
  "info --format {{.Host.CgroupManager}}")
    printf '%s\n' "${FAKE_CGROUP_MANAGER:-systemd}"
    ;;
  "system connection list --format {{.Name}} {{.URI}} {{.Default}}")
    [ -n "${FAKE_CONNECTIONS:-}" ] && printf '%s\n' "$FAKE_CONNECTIONS"
    ;;
  *)
    exit 1
    ;;
esac
EOF
chmod +x "${FAKE_BIN}/podman"
TEST_PATH="${FAKE_BIN}:/usr/bin:/bin"

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

run_preflight() {
  : >"$CALL_LOG"
  set +e
  RUN_OUT="$(env PATH="$TEST_PATH" PODMAN_CALL_LOG="$CALL_LOG" "$@" \
    "$BASH_BIN" "$PREFLIGHT" 2>&1)"
  RUN_STATUS=$?
  set -e
  RUN_CALLS="$(cat "$CALL_LOG")"
}

# --- Docker stays the default and stays untouched ---------------------------
#
# The strongest form of "Docker behavior is unchanged": with Docker selected,
# the stub Podman is never executed at all.

run_preflight env -u HIVE_DEPLOY_RUNTIME
assert_eq "0" "$RUN_STATUS" "default runtime exit"
assert_contains "$RUN_OUT" "skipped" "default runtime skips"
assert_contains "$RUN_OUT" "selects docker" "default runtime names docker"
assert_eq "" "$RUN_CALLS" "default runtime runs no podman command"

run_preflight env HIVE_DEPLOY_RUNTIME=docker
assert_eq "0" "$RUN_STATUS" "explicit docker exit"
assert_contains "$RUN_OUT" "skipped" "explicit docker skips"
assert_eq "" "$RUN_CALLS" "explicit docker runs no podman command"

# An unusable selection fails before any check runs.
run_preflight env HIVE_DEPLOY_RUNTIME=containerd
assert_eq "64" "$RUN_STATUS" "invalid runtime exit"
assert_eq "" "$RUN_CALLS" "invalid runtime runs no podman command"

# --- A healthy Podman host ---------------------------------------------------

run_preflight env HIVE_DEPLOY_RUNTIME=podman
assert_eq "0" "$RUN_STATUS" "healthy host exit"
assert_contains "$RUN_OUT" "Podman 5.8.4" "healthy host reports the version"
assert_contains "$RUN_OUT" "root mode: rootless" "healthy host reports root mode"
assert_contains "$RUN_OUT" "cgroups: v2" "healthy host reports cgroups"
assert_contains "$RUN_OUT" "SUMMARY: pass=4 warn=0 fail=0" "healthy host summary"

# --- Engine presence and version --------------------------------------------

# A PATH that really has no podman on it. The test host may well have a real
# Podman installed, so this cannot be done by prepending a directory — the slim
# PATH carries only the handful of tools the preflight shells out to.
SLIM_BIN="${TEST_TMP}/slim"
mkdir -p "$SLIM_BIN"
for tool in dirname id awk head; do
  ln -sf "$(command -v "$tool")" "${SLIM_BIN}/${tool}"
done
[[ -x "${SLIM_BIN}/podman" ]] && fail "slim PATH must not contain podman"

set +e
RUN_OUT="$(env PATH="$SLIM_BIN" HIVE_DEPLOY_RUNTIME=podman \
  PODMAN_CALL_LOG="$CALL_LOG" "$BASH_BIN" "$PREFLIGHT" 2>&1)"
RUN_STATUS=$?
set -e
assert_eq "78" "$RUN_STATUS" "missing engine exit"
assert_contains "$RUN_OUT" "not installed or not on PATH" "missing engine message"
assert_contains "$RUN_OUT" "skipped — no usable engine" "missing engine skips later checks"
assert_not_contains "$RUN_OUT" "root mode:" "missing engine reports no root mode"

run_preflight env HIVE_DEPLOY_RUNTIME=podman FAKE_VERSION=4.0.3
assert_eq "78" "$RUN_STATUS" "old version exit"
assert_contains "$RUN_OUT" "Hive needs 4.4 or newer" "old version message"
assert_contains "$RUN_OUT" "Quadlet arrived in 4.4" "old version remediation"

# Exactly at the floor: usable, but warned about the Podman 5 Quadlet shape.
run_preflight env HIVE_DEPLOY_RUNTIME=podman FAKE_VERSION=4.4.0
assert_eq "0" "$RUN_STATUS" "floor version exit"
assert_contains "$RUN_OUT" "✓ Podman 4.4.0" "floor version passes"
assert_contains "$RUN_OUT" "predates 5.0" "floor version warns about pod units"

run_preflight env HIVE_DEPLOY_RUNTIME=podman FAKE_VERSION=5.0.0
assert_eq "0" "$RUN_STATUS" "pod-capable version exit"
assert_not_contains "$RUN_OUT" "predates 5.0" "pod-capable version does not warn"

run_preflight env HIVE_DEPLOY_RUNTIME=podman FAKE_VERSION=__ERR__
assert_eq "78" "$RUN_STATUS" "unusable engine exit"
assert_contains "$RUN_OUT" "did not report a client version" "unusable engine message"

# --- Connection identity ------------------------------------------------------

run_preflight env HIVE_DEPLOY_RUNTIME=podman FAKE_REMOTE=true
assert_eq "0" "$RUN_STATUS" "remote connection exit"
assert_contains "$RUN_OUT" "connection: REMOTE" "remote connection reported"
assert_contains "$RUN_OUT" "not on this host" "remote connection caveat"

run_preflight env HIVE_DEPLOY_RUNTIME=podman \
  FAKE_CONNECTIONS="prod ssh://hive@example.invalid/run/podman.sock true"
assert_eq "0" "$RUN_STATUS" "named connection exit"
assert_contains "$RUN_OUT" "prod -> ssh://hive@example.invalid/run/podman.sock" \
  "named connection reports name and URI"

# A non-default connection must not be reported as the active one.
run_preflight env HIVE_DEPLOY_RUNTIME=podman \
  FAKE_CONNECTIONS="staging ssh://elsewhere.invalid/run/podman.sock false"
assert_not_contains "$RUN_OUT" "staging" "non-default connection is not reported as active"

# --- Root mode ----------------------------------------------------------------

run_preflight env HIVE_DEPLOY_RUNTIME=podman FAKE_ROOTLESS=false
assert_eq "0" "$RUN_STATUS" "rootful exit"
assert_contains "$RUN_OUT" "root mode: rootful" "rootful reported"

run_preflight env HIVE_DEPLOY_RUNTIME=podman FAKE_ROOTLESS=__ERR__
assert_eq "78" "$RUN_STATUS" "unreported root mode exit"
assert_contains "$RUN_OUT" "did not report it" "unreported root mode message"

# --- cgroups ------------------------------------------------------------------

run_preflight env HIVE_DEPLOY_RUNTIME=podman FAKE_CGROUPS=v1
assert_eq "78" "$RUN_STATUS" "cgroup v1 exit"
assert_contains "$RUN_OUT" "requires cgroup v2" "cgroup v1 message"
assert_contains "$RUN_OUT" "systemd.unified_cgroup_hierarchy=1" "cgroup v1 remediation is actionable"

run_preflight env HIVE_DEPLOY_RUNTIME=podman FAKE_CGROUP_MANAGER=cgroupfs
assert_contains "$RUN_OUT" "manager: cgroupfs" "cgroup manager reported"

# --- Failures accumulate rather than short-circuiting -------------------------

run_preflight env HIVE_DEPLOY_RUNTIME=podman FAKE_CGROUPS=v1 FAKE_ROOTLESS=__ERR__
assert_eq "78" "$RUN_STATUS" "multiple failures exit"
assert_contains "$RUN_OUT" "fail=2" "both failures counted"

printf 'PASS: %d Podman preflight assertions\n' "$pass_count"
