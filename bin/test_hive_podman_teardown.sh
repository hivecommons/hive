#!/usr/bin/env bash
# Contract tests for bin/hive-podman-teardown.sh (#4326).
# Run: bash bin/test_hive_podman_teardown.sh
#
# The teardown is the first thing that actually removes resources through the
# #4210 ownership contract, so the question this suite has to answer is not
# "does it delete Hive's containers" but "can it reach anything else". It runs
# against a fake Podman holding a store shaped like a real workstation: Hive's
# own resources next to a Distrobox, an unrelated development container, and
# somebody's Postgres volume.
#
# No real engine is contacted. The fake records every call, and `docker` on the
# test PATH is a tripwire that fails the run if the teardown ever speaks to it.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEARDOWN="${ROOT}/bin/hive-podman-teardown.sh"
CONTRACT="${ROOT}/bin/hive-podman-cleanup.sh"
BASH_BIN="$(command -v bash)"
TEST_TMP="$(mktemp -d)"
trap 'rm -rf "$TEST_TMP"' EXIT

OWNER_LABEL="io.kubestellar.hive.owned=true"
OWNED_DEFAULT="${OWNER_LABEL},io.kubestellar.hive.instance=default,io.kubestellar.hive.runtime=podman"
OWNED_STAGING="${OWNER_LABEL},io.kubestellar.hive.instance=staging,io.kubestellar.hive.runtime=podman"

FAKE_BIN="${TEST_TMP}/bin"
CALL_LOG="${TEST_TMP}/podman-calls.log"
DOCKER_LOG="${TEST_TMP}/docker-calls.log"
SYSTEMCTL_LOG="${TEST_TMP}/systemctl-calls.log"
EVENT_LOG="${TEST_TMP}/events.log"
UNIT_STATE="${TEST_TMP}/unit-state"
STORE="${TEST_TMP}/store"
mkdir -p "$FAKE_BIN"

# The systemd scope the teardown selects for THIS test user; assertions must
# not assume the suite never runs as root.
if [[ "$(id -u)" -eq 0 ]]; then
  SCTL_LABEL="systemctl"
else
  SCTL_LABEL="systemctl --user"
fi

pass_count=0

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

assert_eq() {
  local want="$1" got="$2" context="$3"
  [[ "$got" == "$want" ]] || fail "${context}: got '${got}', want '${want}'"
  pass_count=$((pass_count + 1))
}

assert_contains() {
  local haystack="$1" needle="$2" context="$3"
  [[ "$haystack" == *"$needle"* ]] || fail "${context}: missing '${needle}' in '${haystack}'"
  pass_count=$((pass_count + 1))
}

assert_lacks() {
  local haystack="$1" needle="$2" context="$3"
  [[ "$haystack" != *"$needle"* ]] || fail "${context}: unexpectedly found '${needle}'"
  pass_count=$((pass_count + 1))
}

# --- A fake Podman with a workstation-shaped store --------------------------
#
# It implements exactly the surface the teardown uses: the five label-filtered
# listings and the five removals. Filtering is real — a listing returns only
# resources carrying every requested label — because "the unlabelled resource
# survived" is only meaningful if the filter is what excluded it.

cat >"${FAKE_BIN}/podman" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail

printf 'podman %s\n' "$*" >>"$PODMAN_CALL_LOG"
printf 'podman %s\n' "$*" >>"${TEARDOWN_EVENT_LOG:-/dev/null}"

store_kind=""
verb=""
format=""
declare -a wanted=()
declare -a operands=()

args=("$@")
index=0
while (( index < ${#args[@]} )); do
  token="${args[index]}"
  case "$token" in
    --filter)
      wanted+=("${args[index + 1]#label=}")
      index=$((index + 2))
      continue
      ;;
    --format)
      format="${args[index + 1]}"
      index=$((index + 2))
      continue
      ;;
    --all|--force)
      index=$((index + 1))
      continue
      ;;
    pod)
      store_kind="pod"
      index=$((index + 1))
      continue
      ;;
    volume)
      store_kind="volume"
      index=$((index + 1))
      continue
      ;;
    network)
      store_kind="network"
      index=$((index + 1))
      continue
      ;;
    ps|ls|images|rm|rmi|inspect)
      verb="$token"
      [[ -n "$store_kind" ]] || store_kind="container"
      [[ "$token" == "images" ]] && store_kind="image"
      [[ "$token" == "rmi" ]] && store_kind="image"
      index=$((index + 1))
      continue
      ;;
    *)
      operands+=("$token")
      index=$((index + 1))
      continue
      ;;
  esac
done

matches() {
  local labels=",$1,"
  local want
  for want in "${wanted[@]}"; do
    [[ "$labels" == *",${want},"* ]] || return 1
  done
  return 0
}

case "$verb" in
  ps|ls|images)
    while IFS='|' read -r kind name labels; do
      [[ "$kind" == "$store_kind" ]] || continue
      matches "$labels" || continue
      printf '%s\n' "$name"
    done <"$PODMAN_STORE"
    ;;
  inspect)
    # Read of exact names, as the real engine renders {{.Labels}}: a Go map.
    (( ${#operands[@]} > 0 )) || { echo "fake podman: inspect requires a name" >&2; exit 125; }
    for operand in "${operands[@]}"; do
      found="no"
      while IFS='|' read -r kind name labels; do
        if [[ "$kind" == "$store_kind" && "$name" == "$operand" ]]; then
          found="yes"
          printf 'map[%s]\n' "${labels//,/ }" | tr '=' ':'
        fi
      done <"$PODMAN_STORE"
      if [[ "$found" != "yes" ]]; then
        echo "fake podman: no such ${store_kind}: ${operand}" >&2
        exit 125
      fi
    done
    ;;
  rm|rmi)
    (( ${#operands[@]} > 0 )) || { echo "fake podman: refusing an operand-less removal" >&2; exit 125; }
    for operand in "${operands[@]}"; do
      found="no"
      : >"${PODMAN_STORE}.tmp"
      while IFS='|' read -r kind name labels; do
        [[ -n "$kind" ]] || continue
        if [[ "$kind" == "$store_kind" && "$name" == "$operand" ]]; then
          found="yes"
          continue
        fi
        printf '%s|%s|%s\n' "$kind" "$name" "$labels" >>"${PODMAN_STORE}.tmp"
      done <"$PODMAN_STORE"

      if [[ "$found" != "yes" ]]; then
        rm -f "${PODMAN_STORE}.tmp"
        echo "fake podman: no such ${store_kind}: ${operand}" >&2
        exit 125
      fi

      mv "${PODMAN_STORE}.tmp" "$PODMAN_STORE"
      printf '%s\n' "$operand"
    done
    ;;
  *)
    echo "fake podman: unsupported invocation: $*" >&2
    exit 125
    ;;
esac
FAKE
chmod +x "${FAKE_BIN}/podman"

# Docker must never be reached from the Podman teardown.
cat >"${FAKE_BIN}/docker" <<EOF
#!/bin/sh
printf 'docker %s\n' "\$*" >>"${DOCKER_LOG}"
exit 1
EOF
chmod +x "${FAKE_BIN}/docker"

# --- A fake systemctl with per-unit state -----------------------------------
#
# #4484: the teardown must stop the Quadlet-generated services BEFORE it
# removes the resources they own. The fake keeps unit state in a file of
# `unit|LoadState|ActiveState` lines and implements exactly the surface the
# teardown uses (`show -p LoadState --value`, `stop`, `reset-failed`) plus
# `start`, which reproduces the run-once semantics that made the bug: starting
# hive-network.service creates the network ONLY when the unit is not already
# active — an `active (exited)` network unit skips creation, which is exactly
# how a post-teardown install used to hit `network not found`.

cat >"${FAKE_BIN}/systemctl" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail

printf 'systemctl %s\n' "$*" >>"$SYSTEMCTL_CALL_LOG"
printf 'systemctl %s\n' "$*" >>"${TEARDOWN_EVENT_LOG:-/dev/null}"

declare -a args=()
for token in "$@"; do
  [[ "$token" == "--user" ]] && continue
  args+=("$token")
done

verb="${args[0]:-}"
unit="${args[${#args[@]}-1]}"

unit_load=""
unit_active=""
while IFS='|' read -r name load active; do
  [[ "$name" == "$unit" ]] || continue
  unit_load="$load"
  unit_active="$active"
done <"$FAKE_UNIT_STATE"

set_active() {
  : >"${FAKE_UNIT_STATE}.tmp"
  while IFS='|' read -r name load active; do
    [[ -n "$name" ]] || continue
    [[ "$name" == "$unit" ]] && active="$1"
    printf '%s|%s|%s\n' "$name" "$load" "$active" >>"${FAKE_UNIT_STATE}.tmp"
  done <"$FAKE_UNIT_STATE"
  mv "${FAKE_UNIT_STATE}.tmp" "$FAKE_UNIT_STATE"
}

case "$verb" in
  show)
    printf '%s\n' "${unit_load:-not-found}"
    ;;
  is-active)
    printf '%s\n' "${unit_active:-inactive}"
    [[ "${unit_active:-inactive}" == "active" ]] || exit 3
    ;;
  stop)
    if [[ -z "$unit_load" || "$unit_load" == "not-found" ]]; then
      printf 'Failed to stop %s: Unit %s not loaded.\n' "$unit" "$unit" >&2
      exit 5
    fi
    set_active "inactive"
    ;;
  reset-failed)
    exit 0
    ;;
  start)
    if [[ -z "$unit_load" || "$unit_load" == "not-found" ]]; then
      printf 'Failed to start %s: Unit %s not found.\n' "$unit" "$unit" >&2
      exit 5
    fi
    if [[ "$unit" == "hive-network.service" && "$unit_active" != "active" ]]; then
      printf 'network|hive-net|io.kubestellar.hive.owned=true,io.kubestellar.hive.instance=default,io.kubestellar.hive.runtime=podman,io.kubestellar.hive.component=network\n' \
        >>"$PODMAN_STORE"
    fi
    set_active "active"
    ;;
  *)
    printf 'fake systemctl: unsupported invocation: %s\n' "$*" >&2
    exit 125
    ;;
esac
FAKE
chmod +x "${FAKE_BIN}/systemctl"

TEST_PATH="${FAKE_BIN}:/usr/bin:/bin"

reset_store() {
  cat >"$STORE" <<EOF
container|hive|${OWNED_DEFAULT},io.kubestellar.hive.component=hive
container|hive-gateway|${OWNED_DEFAULT},io.kubestellar.hive.component=gateway
container|hive-staging|${OWNED_STAGING},io.kubestellar.hive.component=hive
container|random-dev-box|
container|fedora-toolbox-44|com.github.containers.toolbox=true
pod|hive-pod|${OWNED_DEFAULT},io.kubestellar.hive.component=hive
pod|someones-other-pod|
volume|hive-data|${OWNED_DEFAULT},io.kubestellar.hive.component=hive
volume|my-pgdata|
network|hive-net|${OWNED_DEFAULT},io.kubestellar.hive.component=hive
network|distrobox-net|
image|ghcr.io/hivecommons/hive:stable|${OWNED_DEFAULT},io.kubestellar.hive.component=hive
image|docker.io/library/postgres:16|
EOF
  # The four Quadlet-generated services, loaded and running, as a healthy
  # deployment leaves them.
  cat >"$UNIT_STATE" <<EOF
hive-gateway.service|loaded|active
hive.service|loaded|active
hive-data-volume.service|loaded|active
hive-network.service|loaded|active
EOF
  : >"$CALL_LOG"
  : >"$SYSTEMCTL_LOG"
  : >"$EVENT_LOG"
}

# The resources that must survive every teardown in this suite. If any of these
# disappears, the ownership contract has failed at its actual job.
UNOWNED=(
  "container|random-dev-box"
  "container|fedora-toolbox-44"
  "pod|someones-other-pod"
  "volume|my-pgdata"
  "network|distrobox-net"
  "image|docker.io/library/postgres:16"
)

assert_unowned_survived() {
  local context="$1"
  local entry
  for entry in "${UNOWNED[@]}"; do
    grep -qF "${entry}|" "$STORE" || fail "${context}: teardown removed an unowned resource: ${entry}"
  done
  pass_count=$((pass_count + 1))
}

teardown() {
  local stdout_file="${TEST_TMP}/stdout" stderr_file="${TEST_TMP}/stderr"
  set +e
  env PATH="$TEST_PATH" PODMAN_STORE="$STORE" PODMAN_CALL_LOG="$CALL_LOG" \
    SYSTEMCTL_CALL_LOG="$SYSTEMCTL_LOG" FAKE_UNIT_STATE="$UNIT_STATE" \
    TEARDOWN_EVENT_LOG="$EVENT_LOG" \
    "$@" >"$stdout_file" 2>"$stderr_file"
  RUN_STATUS=$?
  set -e
  RUN_STDOUT="$(cat "$stdout_file")"
  RUN_STDERR="$(cat "$stderr_file")"
}

# --- 1. plan removes nothing ------------------------------------------------

reset_store
store_before="$(cat "$STORE")"
teardown "$BASH_BIN" "$TEARDOWN" plan
assert_eq "0" "$RUN_STATUS" "plan status (stderr: ${RUN_STDERR})"
assert_eq "$store_before" "$(cat "$STORE")" "plan leaves the store byte-identical"
assert_contains "$RUN_STDOUT" "plan only" "plan says it is a plan"
assert_contains "$RUN_STDOUT" "would run: podman rm --force" "plan shows the container removal"
assert_contains "$RUN_STDOUT" "hive-gateway" "plan names the Hive-owned container"
assert_lacks "$RUN_STDOUT" "random-dev-box" "plan cannot see an unlabelled container"
assert_lacks "$RUN_STDOUT" "distrobox-net" "plan cannot see an unlabelled network"
assert_lacks "$(cat "$CALL_LOG")" "podman rm" "plan issues no removal"

# Images are opt-in, so a plan without --images must not offer to remove one.
assert_lacks "$RUN_STDOUT" "podman rmi" "plan omits images by default"

# --- 2. run --yes removes exactly what Hive owns ----------------------------

reset_store
teardown "$BASH_BIN" "$TEARDOWN" run --yes
assert_eq "0" "$RUN_STATUS" "run status (stderr: ${RUN_STDERR})"

store_after="$(cat "$STORE")"
for gone in "container|hive|" "container|hive-gateway|" "pod|hive-pod|" "volume|hive-data|" "network|hive-net|"; do
  assert_lacks "$store_after" "$gone" "run removed ${gone}"
done
assert_unowned_survived "run --yes"

# The image was not requested, so it is still there.
assert_contains "$store_after" "image|ghcr.io/hivecommons/hive:stable|" "images survive without --images"

# A deployment in another instance is not this deployment.
assert_contains "$store_after" "container|hive-staging|" "run is scoped to one instance"

# --- 3. every command the teardown issued passes the #4210 guard ------------
#
# This is the criterion that matters most: not "no prune was written" but "no
# command the path actually emitted could reach past Hive's own labels". The
# guard from #4210 is what renders the verdict — this test only replays the log
# through it.

commands_checked=0
while IFS= read -r logged; do
  [[ -n "$logged" ]] || continue
  commands_checked=$((commands_checked + 1))
  set +e
  # shellcheck disable=SC2086 # the log stores whole command lines on purpose
  env PATH="$TEST_PATH" "$BASH_BIN" "$CONTRACT" check $logged >/dev/null 2>"${TEST_TMP}/guard"
  guard_status=$?
  set -e
  [[ "$guard_status" -eq 0 ]] || fail "teardown issued a command the guard rejects: ${logged}"$'\n'"$(cat "${TEST_TMP}/guard")"

  # Every listing must carry the ownership filter; nothing may carry --all as a
  # removal selector.
  case "$logged" in
    *" ps "*|*" ls "*|*" images "*)
      assert_contains "$logged" "label=${OWNER_LABEL}" "listing is label-scoped: ${logged}"
      ;;
  esac
done <"$CALL_LOG"

[[ "$commands_checked" -ge 8 ]] || fail "too few commands replayed through the guard: ${commands_checked}"
pass_count=$((pass_count + 1))

# --- 4. a store-wide command is rejected, with its reason --------------------
#
# The teardown does not contain these forms; the point here is that the guard it
# runs every command through is the thing that would stop them, and that the
# rejection says why rather than failing silently.

check_guard() {
  local stderr_file="${TEST_TMP}/guard-stderr"
  set +e
  # shellcheck disable=SC2086 # whole command lines on purpose
  env PATH="$TEST_PATH" "$BASH_BIN" "$CONTRACT" check $1 >/dev/null 2>"$stderr_file"
  GUARD_STATUS=$?
  set -e
  GUARD_STDERR="$(cat "$stderr_file")"
}

while IFS='|' read -r command reason; do
  [[ -n "$command" ]] || continue
  check_guard "$command"
  assert_eq "65" "$GUARD_STATUS" "store-wide rejected: ${command}"
  assert_contains "$GUARD_STDERR" "REJECTED" "rejection is explicit: ${command}"
  assert_contains "$GUARD_STDERR" "$reason" "rejection names the reason: ${command}"
done <<'EOF'
podman system prune|whole shared store
podman system prune -a --volumes|whole shared store
podman system reset --force|whole shared store
podman volume prune --force|must carry --filter
podman rm --all --force|--all
podman rmi -af|--all
podman builder prune|build cache
EOF

# The teardown source itself carries no store-wide form. #4210's own test scans
# every tracked file for these; this asserts it for the file that would be the
# most tempting place to write one.
for forbidden in "system prune" "system reset" "builder prune" "--all" "-a "; do
  teardown_body="$(grep -v '^\s*#' "$TEARDOWN" | grep -v 'ps --all' || true)"
  assert_lacks "$teardown_body" "podman ${forbidden}" "teardown source has no 'podman ${forbidden}'"
done

# --- 5. a hostile resource name cannot widen the removal --------------------
#
# The IDs come from the engine, so they are input. If a listing ever returns
# something that would turn `podman rm --force <ids>` into a store-wide
# removal, the guard must reject the constructed command and the teardown must
# abort rather than run it.

reset_store
printf 'container|--all|%s\n' "${OWNED_DEFAULT},io.kubestellar.hive.component=hive" >>"$STORE"
teardown "$BASH_BIN" "$TEARDOWN" run --yes
assert_eq "70" "$RUN_STATUS" "hostile resource name aborts the teardown"
assert_contains "$RUN_STDERR" "REJECTED" "abort quotes the guard's rejection"
assert_contains "$RUN_STDERR" "aborting the teardown" "abort says it aborted"
assert_unowned_survived "hostile resource name"
assert_lacks "$(cat "$CALL_LOG")" "podman rm --force --all" "the rejected command never ran"

# --- 6. instance scoping ----------------------------------------------------

reset_store
teardown env HIVE_DEPLOY_INSTANCE=staging "$BASH_BIN" "$TEARDOWN" run --yes
assert_eq "0" "$RUN_STATUS" "staging teardown status (stderr: ${RUN_STDERR})"
store_after="$(cat "$STORE")"
assert_lacks "$store_after" "container|hive-staging|" "staging teardown removed the staging container"
assert_contains "$store_after" "container|hive|" "staging teardown left the default instance alone"
assert_contains "$store_after" "volume|hive-data|" "staging teardown left the default volume alone"
assert_unowned_survived "instance scoping"

# An instance name cannot smuggle a second filter — the contract validates it,
# and the teardown must refuse rather than run unfiltered.
reset_store
teardown env HIVE_DEPLOY_INSTANCE='x --filter label=other' "$BASH_BIN" "$TEARDOWN" run --yes
assert_eq "64" "$RUN_STATUS" "injected instance is refused"
assert_eq "" "$(cat "$CALL_LOG")" "injected instance runs no command at all"

# --- 6b. an unlabelled hive-data is reported, never removed (#4485) ----------
#
# `podman run -v hive-data:/data` auto-creates a missing named volume with no
# labels, and a volume without the ownership labels is outside the #4210
# selection contract — correctly. What must not happen is the old output: "no
# Hive-owned volumes" over a store where hive-data exists, telling the
# operator the deployment is gone while audit.jsonl survives on disk. The
# well-known name is REPORTED and never removed.

reset_store
sed -i 's/^volume|hive-data|.*/volume|hive-data|/' "$STORE"
store_before="$(cat "$STORE")"
teardown "$BASH_BIN" "$TEARDOWN" plan
assert_eq "0" "$RUN_STATUS" "unlabelled-volume plan status (stderr: ${RUN_STDERR})"
assert_contains "$RUN_STDOUT" "volume hive-data exists but carries no Hive ownership labels" \
  "plan reports the unlabelled well-known volume"
assert_contains "$RUN_STDOUT" "will not touch it" "plan says it will not remove it"
assert_contains "$RUN_STDOUT" "no Hive-owned volumes" "selection itself stays label-scoped"
assert_eq "$store_before" "$(cat "$STORE")" "unlabelled-volume plan removes nothing"

teardown "$BASH_BIN" "$TEARDOWN" run --yes
assert_eq "0" "$RUN_STATUS" "unlabelled-volume run status (stderr: ${RUN_STDERR})"
assert_contains "$RUN_STDOUT" "volume hive-data exists but carries no Hive ownership labels" \
  "run reports the unlabelled well-known volume"
assert_contains "$(cat "$STORE")" "volume|hive-data|" "run never removes the unlabelled volume"
assert_lacks "$(cat "$CALL_LOG")" "volume rm hive-data" "no removal command named the unlabelled volume"
assert_unowned_survived "unlabelled hive-data"

# A labelled hive-data produces no report: it is selected and removed the
# ordinary way.
reset_store
teardown "$BASH_BIN" "$TEARDOWN" run --yes
assert_eq "0" "$RUN_STATUS" "labelled-volume run status (stderr: ${RUN_STDERR})"
assert_lacks "$RUN_STDOUT" "carries no Hive ownership labels" "no report for a labelled volume"
assert_lacks "$(cat "$STORE")" "volume|hive-data|" "the labelled volume was removed"

# hive-data labelled for ANOTHER instance is that deployment's, not an orphan:
# a staging teardown neither removes nor reports the default instance's volume.
reset_store
teardown env HIVE_DEPLOY_INSTANCE=staging "$BASH_BIN" "$TEARDOWN" run --yes
assert_eq "0" "$RUN_STATUS" "staging run status (stderr: ${RUN_STDERR})"
assert_lacks "$RUN_STDOUT" "carries no Hive ownership labels" \
  "another instance's labelled volume is not reported as an orphan"
assert_contains "$(cat "$STORE")" "volume|hive-data|" "another instance's volume survives"

# --- 7. images are removed only when asked -----------------------------------

reset_store
teardown "$BASH_BIN" "$TEARDOWN" run --yes --images
assert_eq "0" "$RUN_STATUS" "run --images status (stderr: ${RUN_STDERR})"
store_after="$(cat "$STORE")"
assert_lacks "$store_after" "image|ghcr.io/hivecommons/hive:stable|" "--images removes the Hive image"
assert_contains "$store_after" "image|docker.io/library/postgres:16|" "--images leaves unowned images"
assert_unowned_survived "run --images"

# --- 8. confirmation is required, and Docker is never touched ---------------

reset_store
store_before="$(cat "$STORE")"
teardown "$BASH_BIN" "$TEARDOWN" run
assert_eq "64" "$RUN_STATUS" "run without --yes is refused"
assert_contains "$RUN_STDERR" "pass --yes" "refusal explains how to confirm"
assert_eq "$store_before" "$(cat "$STORE")" "unconfirmed run changes nothing"

reset_store
teardown env HIVE_DEPLOY_RUNTIME=docker "$BASH_BIN" "$TEARDOWN" run --yes
assert_eq "64" "$RUN_STATUS" "Docker runtime selection is refused"
assert_contains "$RUN_STDERR" "Podman teardown" "refusal names the runtime mismatch"
assert_eq "$store_before" "$(cat "$STORE")" "Docker refusal changes nothing"

reset_store
teardown env HIVE_DEPLOY_RUNTIME=podman "$BASH_BIN" "$TEARDOWN" plan
assert_eq "0" "$RUN_STATUS" "explicit podman selection is accepted"

if [[ -s "$DOCKER_LOG" ]]; then
  fail "the Podman teardown invoked docker:"$'\n'"$(cat "$DOCKER_LOG")"
fi
pass_count=$((pass_count + 1))

# --- 9. units are stopped first, and stay stopped (#4484) --------------------
#
# The Quadlet-generated services OWN the resources this teardown removes.
# hive-network.service is run-once and sits `active (exited)` after creating
# the network; removing the network underneath it leaves systemd convinced the
# network exists, so the next install skips creation and dies with `network
# not found`. The containers carry Restart=always, so a removal that races a
# running unit is undone 30 seconds later. The contract is therefore ordering:
# every unit is stopped (and reset-failed) BEFORE the first removal runs.

reset_store
teardown "$BASH_BIN" "$TEARDOWN" run --yes
assert_eq "0" "$RUN_STATUS" "run with units status (stderr: ${RUN_STDERR})"

stopped_units="$(grep -E "^systemctl (--user )?stop " "$SYSTEMCTL_LOG" | awk '{print $NF}' | tr '\n' ' ')"
assert_eq "hive-gateway.service hive.service hive-data-volume.service hive-network.service " \
  "$stopped_units" "units are stopped in reverse start order"

for unit in hive-gateway.service hive.service hive-data-volume.service hive-network.service; do
  assert_contains "$(grep -E "reset-failed" "$SYSTEMCTL_LOG")" "$unit" "reset-failed clears ${unit}"
  assert_contains "$(grep -F "${unit}|" "$UNIT_STATE")" "|inactive" "${unit} is inactive after teardown"
done

# Ordering: the LAST unit stop precedes the FIRST resource removal.
last_stop="$(grep -nE "^systemctl (--user )?stop " "$EVENT_LOG" | tail -n1 | cut -d: -f1)"
first_removal="$(grep -nE "^podman (rm|pod rm|volume rm|network rm) " "$EVENT_LOG" | head -n1 | cut -d: -f1)"
[[ -n "$last_stop" && -n "$first_removal" && "$last_stop" -lt "$first_removal" ]] \
  || fail "a removal ran before the units were stopped (last stop line ${last_stop:-none}, first removal line ${first_removal:-none})"
pass_count=$((pass_count + 1))

# --- 10. teardown → install cycle succeeds -----------------------------------
#
# The measured failure was: post-teardown, hive-network.service still `active
# (exited)`, so the next start skips network creation. The fake reproduces
# those run-once semantics: `start` creates the network only from a non-active
# unit. After a correct teardown the start must therefore recreate it.

assert_lacks "$(cat "$STORE")" "network|hive-net|" "no Hive network remains after teardown"
env PATH="$TEST_PATH" PODMAN_STORE="$STORE" PODMAN_CALL_LOG="$CALL_LOG" \
  SYSTEMCTL_CALL_LOG="$SYSTEMCTL_LOG" FAKE_UNIT_STATE="$UNIT_STATE" \
  systemctl --user start hive-network.service \
  || fail "starting hive-network.service after teardown failed"
assert_contains "$(cat "$STORE")" "network|hive-net|" "a fresh start recreates the network — the install cycle holds"

# Negative control, documenting the old failure mode: with the unit left
# `active` and the network already gone, the same start creates nothing.
reset_store
grep -v "network|hive-net|" "$STORE" >"${STORE}.tmp" && mv "${STORE}.tmp" "$STORE"
env PATH="$TEST_PATH" PODMAN_STORE="$STORE" PODMAN_CALL_LOG="$CALL_LOG" \
  SYSTEMCTL_CALL_LOG="$SYSTEMCTL_LOG" FAKE_UNIT_STATE="$UNIT_STATE" \
  systemctl --user start hive-network.service || true
assert_lacks "$(cat "$STORE")" "network|hive-net|" "an active-but-skewed unit skips creation — the state teardown must never leave"

# --- 11. re-running the teardown is safe -------------------------------------

reset_store
teardown "$BASH_BIN" "$TEARDOWN" run --yes
assert_eq "0" "$RUN_STATUS" "first teardown status (stderr: ${RUN_STDERR})"
teardown "$BASH_BIN" "$TEARDOWN" run --yes
assert_eq "0" "$RUN_STATUS" "second teardown is idempotent (stderr: ${RUN_STDERR})"
assert_unowned_survived "re-run teardown"

# Units that were never installed — or removed since — are reported, not fatal.
cat >"$UNIT_STATE" <<EOF
hive-gateway.service|not-found|inactive
hive.service|not-found|inactive
hive-data-volume.service|not-found|inactive
hive-network.service|not-found|inactive
EOF
: >"$SYSTEMCTL_LOG"
teardown "$BASH_BIN" "$TEARDOWN" run --yes
assert_eq "0" "$RUN_STATUS" "teardown with no units installed succeeds"
assert_contains "$RUN_STDOUT" "is not loaded" "missing units are reported, not stopped"
assert_lacks "$(grep -E "^systemctl (--user )?stop " "$SYSTEMCTL_LOG" || true)" "stop" \
  "no stop is issued for a unit that is not loaded"

# --- 12. unit handling respects plan mode and instance scope -----------------

reset_store
teardown "$BASH_BIN" "$TEARDOWN" plan
assert_eq "0" "$RUN_STATUS" "plan with units status"
assert_contains "$RUN_STDOUT" "would run: ${SCTL_LABEL} stop hive-network.service" "plan announces the network unit stop"
assert_eq "" "$(cat "$SYSTEMCTL_LOG")" "plan issues no systemctl call at all"
assert_contains "$(grep -F "hive-network.service|" "$UNIT_STATE")" "|active" "plan leaves unit state untouched"

reset_store
teardown env HIVE_DEPLOY_INSTANCE=staging "$BASH_BIN" "$TEARDOWN" run --yes
assert_eq "0" "$RUN_STATUS" "staging teardown with units status"
assert_eq "" "$(cat "$SYSTEMCTL_LOG")" "a non-default instance leaves the default's units alone"
assert_contains "$RUN_STDOUT" "default instance" "the instance-scope skip says why"

# --- 13. deployment assets must label what they create -----------------------
#
# #4326's first requirement is that every container, pod, network, and volume
# the standalone path creates carries the #4210 label set. No standalone Podman
# deployment asset exists yet — the service asset is its own slice — so this
# scan passes vacuously today. It exists so that it stops being vacuous the
# moment one lands: an asset that creates a Podman resource without going
# through the labels seam fails here rather than being discovered by an
# operator whose Distrobox survived teardown only by luck.
#
# Deliberately not scanned: probe_*.sh and test_*.sh. Those are spike
# reproduction material and tests. They create resources in a throwaway store
# with a private graphroot and remove them by exact name, and the commands they
# run are quoted verbatim in the spike write-ups.

if command -v git >/dev/null 2>&1 && git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1; then
  create_pattern='podman[[:space:]]+([a-z]+[[:space:]]+)?(run|create|build)\b'
  assets_scanned=0
  unlabelled=""

  while IFS= read -r tracked; do
    case "$tracked" in
      bin/hive-podman-cleanup.sh|bin/hive-podman-teardown.sh) continue ;;
      */probe_*.sh|probe_*.sh|*/test_*.sh|test_*.sh) continue ;;
    esac

    case "$tracked" in
      *.md) continue ;;
      src/deploy/*|bin/*|systemd/*|*.container|*.pod|*.network|*.volume) ;;
      *) continue ;;
    esac

    [[ -f "${ROOT}/${tracked}" ]] || continue

    # Comment lines are stripped first. Every Podman script in bin/ explains
    # itself by naming `podman run` in its header prose, and a comment creates
    # nothing.
    grep -vE '^[[:space:]]*#' "${ROOT}/${tracked}" 2>/dev/null \
      | grep -qE "$create_pattern" || continue

    assets_scanned=$((assets_scanned + 1))
    if ! grep -qE 'hive-podman-cleanup\.sh labels|hive_podman_labels|io\.kubestellar\.hive\.owned=true' \
      "${ROOT}/${tracked}"; then
      unlabelled+="${tracked}"$'\n'
    fi
  done < <(git -C "$ROOT" ls-files)

  [[ -z "$unlabelled" ]] || fail \
    "deployment asset creates Podman resources without the #4210 ownership labels:"$'\n'"${unlabelled}"
  pass_count=$((pass_count + 1))
  printf 'scanned %d resource-creating deployment asset(s) for ownership labels\n' "$assets_scanned"
fi

printf 'PASS: %d Podman teardown contract assertions\n' "$pass_count"
