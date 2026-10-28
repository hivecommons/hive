#!/usr/bin/env bash
# Local-mode contributor helpers, sourced by the Justfile's contribute-hive
# (local branch) and contribute-stop recipes. Kept in one file so the logic is
# exercised by tests (src/pkg/dashboard/contribute_local_relay_preflight_test.go)
# instead of living only inside recipe bodies.
#
#   hive_relay_pid_file             path of the relay's pid record
#   hive_live_local_relay [file]    "PID SESSION" of a live local relay, or rc 1
#   hive_refuse_if_local_relay_running [file]
#                                   rc 1 + message when a local relay is live
#   hive_stop_local_relay [file]    stop the live local relay; rc 1 if none
#   hive_local_sandbox_probe        run `true` through a bubblewrap sandbox
#   hive_local_sandbox_preflight BACKEND
#                                   rc 1 + remediation when the probe fails

# The same path bin/contributor-relay.js writes (RELAY_PID_FILE) and hivectl
# reads (relayPIDFileName in src/pkg/hivectl/profiles.go).
hive_relay_pid_file() {
  printf '%s\n' "${HIVE_RELAY_PID_FILE:-${HIVE_CONTRIBUTOR_CONFIG_DIR:-${HOME}/.config/hive}/contributor-relay.pid}"
}

# _hive_relay_pid_field FILE KEY prints one scalar field of the JSON record the
# relay writes (writeRelayPidFile: one key per line). No jq dependency: the
# record's shape is ours.
_hive_relay_pid_field() {
  sed -n "s/^[[:space:]]*\"$2\":[[:space:]]*\"\{0,1\}\([^\",]*\)\"\{0,1\},\{0,1\}[[:space:]]*\$/\1/p" "$1" 2>/dev/null | head -n 1
}

# hive_live_local_relay prints "PID SESSION" (SESSION may be empty) when the pid
# record names a LIVE local relay, and returns 1 otherwise. Same liveness rules
# as hivectl's SignalRunningRelay: a missing or stale record is "not running",
# and a container-mode record (container_name set) is not a local relay — the
# container cleanup owns it. The process must also still be a
# contributor-relay, so a recycled PID is never signalled.
hive_live_local_relay() {
  local file pid container session args
  file="${1:-}"
  [[ -n "$file" ]] || file="$(hive_relay_pid_file)"
  [[ -f "$file" ]] || return 1
  container="$(_hive_relay_pid_field "$file" container_name)"
  [[ -z "$container" ]] || return 1
  pid="$(_hive_relay_pid_field "$file" pid)"
  [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 1
  kill -0 "$pid" 2>/dev/null || return 1
  args="$(ps -o args= -p "$pid" 2>/dev/null || true)"
  [[ "$args" == *contributor-relay* ]] || return 1
  session="$(_hive_relay_pid_field "$file" agent_session)"
  if [[ -z "$session" && -r "/proc/${pid}/environ" ]]; then
    session="$(tr '\0' '\n' < "/proc/${pid}/environ" 2>/dev/null | sed -n 's/^HIVE_AGENT_SESSION=//p' | head -n 1)"
  fi
  printf '%s %s\n' "$pid" "$session"
}

# hive_refuse_if_local_relay_running returns 1, with the message an operator
# needs, when a local relay is already running. Starting a second one would
# connect a second relay under the same contributor ID and overwrite the only
# record of the first (hivecommons/hive#14833).
hive_refuse_if_local_relay_running() {
  local info pid session
  info="$(hive_live_local_relay "${1:-}")" || return 0
  pid="${info%% *}"
  session="${info#* }"
  echo "A local relay is already running (PID ${pid}, tmux session ${session:-unknown}). Run \`just contribute-stop\` first."
  return 1
}

# hive_stop_local_relay stops the live local relay named by the pid record:
# SIGTERM (the relay's own cleanup removes the record), SIGKILL after a grace
# period, then its tmux session, then the record if it survived. Returns 1 when
# no live local relay was recorded.
hive_stop_local_relay() {
  local file info pid session i
  file="${1:-}"
  [[ -n "$file" ]] || file="$(hive_relay_pid_file)"
  info="$(hive_live_local_relay "$file")" || return 1
  pid="${info%% *}"
  session="${info#* }"
  kill -TERM "$pid" 2>/dev/null || true
  for ((i = 0; i < 20; i++)); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.5
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill -KILL "$pid" 2>/dev/null || true
  fi
  if [[ -n "$session" ]] && command -v tmux >/dev/null 2>&1; then
    tmux kill-session -t "$session" 2>/dev/null || true
  fi
  rm -f "$file"
  echo "Stopped local relay (PID ${pid}${session:+, tmux session ${session}})."
}

# hive_local_sandbox_probe runs one no-op through bubblewrap the way Claude
# Code's Linux sandbox runs every agent command: bwrap in a new user namespace,
# and inside it a NESTED user namespace whose setgroups/uid_map are written —
# the step Claude's apply-seccomp helper performs before each command. Ubuntu's
# /etc/apparmor.d/bwrap-userns-restrict confines bwrap's child with
# `audit deny capability`, so that nested write fails there even with
# kernel.apparmor_restrict_unprivileged_userns=0; a sysctl check alone cannot
# see it (hivecommons/hive#14832). Prints the failure output on rc != 0.
hive_local_sandbox_probe() {
  local out nested=(true)
  if command -v unshare >/dev/null 2>&1; then
    nested=(unshare --user --map-root-user true)
  fi
  if out="$(bwrap --ro-bind / / --dev /dev --unshare-user --unshare-pid --proc /proc --die-with-parent -- "${nested[@]}" 2>&1)"; then
    return 0
  fi
  printf '%s\n' "${out:-bwrap exited non-zero with no output}"
  return 1
}

# hive_local_sandbox_preflight BACKEND fails local-mode startup, before the
# relay advertises readiness, when a sandboxed `true` cannot run.
hive_local_sandbox_preflight() {
  local backend="${1:-claude}" out
  if out="$(hive_local_sandbox_probe)"; then
    return 0
  fi
  echo "ERROR: ${backend}'s OS sandbox cannot run commands on this host."
  echo "       A sandboxed no-op ('true' under bubblewrap, in the nested user"
  echo "       namespace the sandbox creates for every command) failed:"
  printf '%s\n' "$out" | sed 's/^/         /'
  echo "       On Ubuntu this is the AppArmor user-namespace restriction: the"
  echo "       /etc/apparmor.d/bwrap-userns-restrict profile (it denies this even"
  echo "       with kernel.apparmor_restrict_unprivileged_userns=0) and/or"
  echo "       kernel.apparmor_restrict_unprivileged_userns=1."
  echo "       Local mode would start and then every agent command would fail."
  echo "       Use container mode instead:"
  echo "         just contribute-hive ${backend}"
  return 1
}
