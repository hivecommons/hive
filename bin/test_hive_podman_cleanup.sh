#!/usr/bin/env bash
# Contract tests for bin/hive-podman-cleanup.sh (#4210).
# Run: bash bin/test_hive_podman_cleanup.sh
#
# This test never deletes a container, pod, volume, network, or image. The
# guard is pure argument analysis, and every engine binary on the test PATH is
# a tripwire that records the call and fails the run instead of doing anything.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONTRACT="${ROOT}/bin/hive-podman-cleanup.sh"
BASH_BIN="$(command -v bash)"
TEST_TMP="$(mktemp -d)"
trap 'rm -rf "$TEST_TMP"' EXIT

OWNER_LABEL="io.kubestellar.hive.owned=true"

# Tripwire engines: if the contract module ever shells out to a real engine,
# these record it and the run fails.
TRIPWIRE_BIN="${TEST_TMP}/bin"
TRIPWIRE_LOG="${TEST_TMP}/engine-calls.log"
mkdir -p "$TRIPWIRE_BIN"
for engine in podman buildah docker; do
  cat >"${TRIPWIRE_BIN}/${engine}" <<EOF
#!/bin/sh
printf '${engine} %s\n' "\$*" >>"${TRIPWIRE_LOG}"
exit 1
EOF
  chmod +x "${TRIPWIRE_BIN}/${engine}"
done
TEST_PATH="${TRIPWIRE_BIN}:/usr/bin:/bin"

pass_count=0

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

assert_eq() {
  local want="$1"
  local got="$2"
  local context="$3"
  [[ "$got" == "$want" ]] || fail "${context}: got '${got}', want '${want}'"
  pass_count=$((pass_count + 1))
}

assert_contains() {
  local haystack="$1"
  local needle="$2"
  local context="$3"
  [[ "$haystack" == *"$needle"* ]] || fail "${context}: missing '${needle}' in '${haystack}'"
  pass_count=$((pass_count + 1))
}

run_capture() {
  local stdout_file="${TEST_TMP}/stdout"
  local stderr_file="${TEST_TMP}/stderr"
  set +e
  env PATH="$TEST_PATH" "$@" >"$stdout_file" 2>"$stderr_file"
  RUN_STATUS=$?
  set -e
  RUN_STDOUT="$(cat "$stdout_file")"
  RUN_STDERR="$(cat "$stderr_file")"
}

contract() {
  run_capture "$BASH_BIN" "$CONTRACT" "$@"
}

# --- Ownership labels are stable and cover every resource kind --------------

contract labels hive
assert_eq "0" "$RUN_STATUS" "labels status"
assert_contains "$RUN_STDOUT" "$OWNER_LABEL" "labels ownership marker"
assert_contains "$RUN_STDOUT" "io.kubestellar.hive.component=hive" "labels component"
assert_contains "$RUN_STDOUT" "io.kubestellar.hive.instance=default" "labels instance"
assert_contains "$RUN_STDOUT" "io.kubestellar.hive.runtime=podman" "labels runtime"

# The same label set applies to every kind Podman can own, so a component name
# is all a caller ever supplies.
for component in gateway network-hive volume-hive-data image-hive; do
  contract labels "$component"
  assert_eq "0" "$RUN_STATUS" "labels ${component} status"
  assert_contains "$RUN_STDOUT" "io.kubestellar.hive.component=${component}" "labels ${component} value"
done

# A component name cannot smuggle a second label or a shell metacharacter.
contract labels 'hive --label evil=true'
assert_eq "64" "$RUN_STATUS" "labels reject injected component"

contract labels
assert_eq "64" "$RUN_STATUS" "labels require a component"

# --- Selection filters carry the ownership marker ---------------------------

contract filters
assert_eq "0" "$RUN_STATUS" "filters status"
assert_contains "$RUN_STDOUT" "label=${OWNER_LABEL}" "filters ownership marker"
assert_contains "$RUN_STDOUT" "label=io.kubestellar.hive.instance=default" "filters instance"

run_capture env HIVE_DEPLOY_INSTANCE=staging "$BASH_BIN" "$CONTRACT" filters
assert_eq "0" "$RUN_STATUS" "named instance filters status"
assert_contains "$RUN_STDOUT" "label=io.kubestellar.hive.instance=staging" "named instance filters"

run_capture env HIVE_DEPLOY_INSTANCE='x --filter label=other' "$BASH_BIN" "$CONTRACT" filters
assert_eq "64" "$RUN_STATUS" "reject injected instance"
assert_eq "" "$RUN_STDOUT" "injected instance emits no filter"

# --- Broad commands are rejected --------------------------------------------
#
# The four named in #4210 lead, followed by the variants a mechanical Docker
# translation would otherwise produce.

while IFS='|' read -r command expected_reason; do
  [[ -n "$command" ]] || continue
  # shellcheck disable=SC2086 # the table stores whole command lines on purpose
  contract check $command
  assert_eq "65" "$RUN_STATUS" "reject: ${command}"
  assert_contains "$RUN_STDERR" "REJECTED" "reject reason present: ${command}"
  assert_contains "$RUN_STDERR" "$expected_reason" "reject reason text: ${command}"
done <<'EOF'
podman system prune|whole shared store
podman system prune -a --volumes --force|whole shared store
podman image prune -a|--all
podman image prune --all --force|--all
podman rmi -a -f|--all
podman rmi --all|--all
buildah rm --all|--all
buildah rmi --all|--all
podman system reset --force|whole shared store
podman machine reset|whole shared store
podman builder prune --all|build cache
podman container prune|must carry --filter
podman volume prune --force|must carry --filter
podman network prune|must carry --filter
podman pod prune|must carry --filter
podman image prune --filter label=other=1|must carry --filter
podman rm --force|must name Hive-owned resources
podman volume rm --all|--all
podman pod rm -a|--all
buildah rm -a|--all
podman --root /var/tmp/store system prune|whole shared store
podman --connection remote image prune -a|--all
EOF

# --- Scoped commands are allowed --------------------------------------------

while IFS= read -r command; do
  [[ -n "$command" ]] || continue
  # shellcheck disable=SC2086 # the table stores whole command lines on purpose
  contract check $command
  assert_eq "0" "$RUN_STATUS" "allow: ${command} (stderr: ${RUN_STDERR})"
done <<EOF
podman rm --force hive hive-gateway
podman rm --filter label=${OWNER_LABEL}
podman container prune --filter label=${OWNER_LABEL}
podman volume prune --force --filter label=${OWNER_LABEL}
podman network prune --filter label=${OWNER_LABEL}
podman volume rm hive-data
podman network rm hive-net
podman rmi ghcr.io/hivecommons/hive:v4
buildah rm hive-build
podman ps --all --filter label=${OWNER_LABEL}
podman images --filter label=${OWNER_LABEL}
EOF

# --- Docker cleanup behavior is not changed by this contract ----------------
#
# The guard refuses to render a verdict on Docker rather than quietly blessing
# or rewriting the existing Docker cleanup paths.

contract check docker system prune -a
assert_eq "64" "$RUN_STATUS" "docker is out of contract scope"
assert_contains "$RUN_STDERR" "podman and buildah only" "docker scope message"

contract check
assert_eq "64" "$RUN_STATUS" "check requires a command"

# --- The plan is scoped, and printing it runs nothing ------------------------

contract plan
assert_eq "0" "$RUN_STATUS" "plan status"
assert_eq "" "$RUN_STDERR" "plan diagnostics"

plan_output="$RUN_STDOUT"
plan_lines=0
while IFS= read -r planned; do
  [[ -n "$planned" ]] || continue
  plan_lines=$((plan_lines + 1))

  # Every selection command must be label-scoped; every removal command must be
  # label-scoped or name explicit Hive-owned operands.
  case "$planned" in
    *ps*|*ls*|*images*)
      assert_contains "$planned" "label=${OWNER_LABEL}" "plan selection is label-scoped"
      ;;
  esac

  # shellcheck disable=SC2086 # planned lines are whole commands
  contract check $planned
  assert_eq "0" "$RUN_STATUS" "plan line passes the guard: ${planned}"
done <<<"$plan_output"

[[ "$plan_lines" -ge 10 ]] || fail "plan covers too few resource kinds: ${plan_lines}"
pass_count=$((pass_count + 1))

for kind in "podman rm" "podman pod rm" "podman volume rm" "podman network rm" "podman rmi"; do
  assert_contains "$plan_output" "$kind" "plan covers ${kind}"
done

# --- No Hive tooling ships a broad Podman or Buildah cleanup ----------------

broad_patterns=(
  'podman system prune'
  'podman system reset'
  'podman machine reset'
  'podman image prune -a'
  'podman image prune --all'
  'podman rmi -a'
  'podman rmi --all'
  'podman builder prune'
  'buildah rm --all'
  'buildah rm -a'
  'buildah rmi --all'
  'buildah rmi -a'
)

# The contract module, this test, the teardown's own test, and the contract
# documentation quote those commands in order to reject and explain them.
exempt=(
  'bin/hive-podman-cleanup.sh'
  'bin/test_hive_podman_cleanup.sh'
  'bin/test_hive_podman_teardown.sh'
  'src/docs/podman-ownership-cleanup.md'
)

if command -v git >/dev/null 2>&1 && git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1; then
  offenders=""
  while IFS= read -r tracked; do
    skip=""
    for exempt_path in "${exempt[@]}"; do
      [[ "$tracked" == "$exempt_path" ]] && skip="yes"
    done
    [[ -n "$skip" ]] && continue
    [[ -f "${ROOT}/${tracked}" ]] || continue

    for pattern in "${broad_patterns[@]}"; do
      if grep -qF -- "$pattern" "${ROOT}/${tracked}" 2>/dev/null; then
        offenders+="${tracked}: ${pattern}"$'\n'
      fi
    done
  done < <(git -C "$ROOT" ls-files)

  [[ -z "$offenders" ]] || fail "broad Podman/Buildah cleanup found:"$'\n'"${offenders}"
  pass_count=$((pass_count + 1))
fi

# --- The whole test ran without touching a container engine ------------------

if [[ -s "$TRIPWIRE_LOG" ]]; then
  fail "the contract invoked a container engine:"$'\n'"$(cat "$TRIPWIRE_LOG")"
fi
pass_count=$((pass_count + 1))

printf 'PASS: %d Podman ownership and cleanup contract assertions\n' "$pass_count"
