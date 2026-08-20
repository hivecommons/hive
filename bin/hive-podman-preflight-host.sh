#!/usr/bin/env bash
# Podman preflight: SELinux, mounts, secrets, and ports (#4209).
#
# Three of the most common ways a standalone Podman deployment fails on a
# Fedora/RHEL-class host have nothing to do with the engine itself, and all
# three surface as an unhelpful error long after `podman run` has already
# started making changes:
#
#   1. SELinux is enforcing and the bind-mount sources carry a host label the
#      container type cannot read, so the process inside sees EACCES on a file
#      that is plainly mode 0644 on the host.
#   2. The configuration file or the secrets directory is absent, unreadable by
#      the user the engine actually runs as, or readable by everyone on the box.
#   3. Something already holds the port the gateway is about to publish, or the
#      port is below the rootless unprivileged-port floor.
#
# This layer diagnoses exactly those and stops. It is read-only: it starts no
# container, relabels nothing, changes no mode or owner, writes nothing to the
# host, and contacts no registry.
#
# Deliberate non-goals, so the remediation advice stays honest:
#
#   * Disabling SELinux is never offered as the fix. Every remediation here is
#     a labeling or policy action that leaves the kernel enforcing.
#   * Secret permissions are never broadened, and never changed at all. Modes
#     that are too open are reported with the command to NARROW them; the
#     operator runs it.
#   * Host firewall rules, port reservations, and running processes are left
#     exactly as they are.
#
# The engine, connection, root mode, and cgroup layer is #4207; subordinate
# IDs, graphroot, and networking are #4208.
#
# Operator guide, including every remediation this prints:
# src/docs/podman-preflight-host.md
#
# These checks run ONLY when Podman is explicitly selected through
# HIVE_DEPLOY_RUNTIME. Docker is the default and is untouched: with Docker
# selected this reports that Podman is not in use and exits 0 without running
# a single Podman command.
#
# Run: bin/hive-podman-preflight-host.sh
# Exit codes: 0 no failing check, 64 unusable runtime selection,
#             78 at least one failing check (EX_CONFIG).

# Where the standalone deployment assets live. The bind-mount sources in
# src/docker-compose.yaml are relative to this directory.
HIVE_SRC_DIR="${HIVE_SRC_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../src" 2>/dev/null && pwd)}"
HIVE_SRC_DIR="${HIVE_SRC_DIR:-$(dirname "${BASH_SOURCE[0]}")/../src}"

# Host ports the deployment publishes. 3001 is the authenticated gateway and is
# always checked; the raw ttyd port is deliberately NOT published and so is
# deliberately NOT listed here. Space- or comma-separated.
HIVE_PODMAN_PREFLIGHT_PORTS="${HIVE_PODMAN_PREFLIGHT_PORTS:-3001}"

HIVE_PREFLIGHT_PASS=0
HIVE_PREFLIGHT_WARN=0
HIVE_PREFLIGHT_FAIL=0

# Markers match bin/hive-prereq-check.sh so the two read as one report when
# this is invoked from there.
_pfh_pass() { echo "  ✓ $1"; HIVE_PREFLIGHT_PASS=$((HIVE_PREFLIGHT_PASS + 1)); }
_pfh_warn() { echo "  △ $1"; HIVE_PREFLIGHT_WARN=$((HIVE_PREFLIGHT_WARN + 1)); }
_pfh_fail() { echo "  ✗ $1"; HIVE_PREFLIGHT_FAIL=$((HIVE_PREFLIGHT_FAIL + 1)); }
_pfh_hint() { echo "    → $1"; }

# One `podman info` field. A template an older Podman does not know must
# degrade to a caller-visible failure rather than abort the preflight.
_pfh_info() {
  local template="$1" value
  value="$(podman info --format "$template" 2>/dev/null)" || return 1
  [[ -n "$value" && "$value" != "<no value>" ]] || return 1
  printf '%s\n' "$value"
}

# true when the engine is rootless, false when rootful, empty when unknown.
# Several checks below mean different things in each mode, so an unknown mode
# downgrades them to a report rather than a verdict. Asked once: the answer
# cannot change mid-run and several checks want it.
_PFH_ROOTLESS_CACHE=""
_pfh_rootless() {
  local rootless

  if [[ -n "$_PFH_ROOTLESS_CACHE" ]]; then
    [[ "$_PFH_ROOTLESS_CACHE" == "unknown" ]] || printf '%s\n' "$_PFH_ROOTLESS_CACHE"
    return 0
  fi

  _PFH_ROOTLESS_CACHE="unknown"
  rootless="$(_pfh_info '{{.Host.Security.Rootless}}')" || return 0
  case "$rootless" in
    true|false)
      _PFH_ROOTLESS_CACHE="$rootless"
      printf '%s\n' "$rootless"
      ;;
  esac
}

# --- 1. SELinux state --------------------------------------------------------
#
# Two independent facts matter and they can disagree. The kernel's mode says
# whether a denial stops the process; Podman's own view says whether it will
# apply container labels and honour :z/:Z at all. Enforcing-kernel with
# labeling-off is the genuinely broken combination, and it is invisible from
# either fact alone.

# Enforcing | Permissive | Disabled | unknown
_pfh_selinux_mode() {
  local mode

  if command -v getenforce >/dev/null 2>&1 && mode="$(getenforce 2>/dev/null)" && [[ -n "$mode" ]]; then
    printf '%s\n' "$mode"
    return 0
  fi

  # No policy utilities installed: read the filesystem the utilities read.
  # Deliberately a bash builtin rather than `cat` — a preflight that cannot
  # report the SELinux mode because coreutils is missing from a trimmed PATH
  # is worse than no preflight at all.
  local raw=""
  if [[ -r /sys/fs/selinux/enforce ]]; then
    # The node holds a single byte with no trailing newline, so `read` reports
    # EOF-without-delimiter and still assigns. Judge the value, not the status.
    read -r raw </sys/fs/selinux/enforce 2>/dev/null || true
    case "$raw" in
      1) printf 'Enforcing\n'; return 0 ;;
      0) printf 'Permissive\n'; return 0 ;;
    esac
  fi

  # An absent selinuxfs mount is how a kernel without SELinux looks, which is
  # the normal state on Debian/Ubuntu hosts and is not a problem.
  if [[ ! -d /sys/fs/selinux ]]; then
    printf 'Disabled\n'
    return 0
  fi

  printf 'unknown\n'
}

hive_podman_check_selinux() {
  local mode podman_selinux
  mode="$(_pfh_selinux_mode)"
  podman_selinux="$(_pfh_info '{{.Host.Security.SELinuxEnabled}}')" || podman_selinux="unknown"

  case "$mode" in
    Enforcing)
      if [[ "$podman_selinux" == "true" ]]; then
        _pfh_pass "SELinux: Enforcing, Podman labeling enabled"
      elif [[ "$podman_selinux" == "false" ]]; then
        # The kernel denies on label mismatch and Podman will not set a label
        # the container type can read, so every bind mount is a denial and no
        # :z/:Z in any lifecycle asset will fix it.
        _pfh_fail "SELinux: Enforcing, but Podman reports labeling DISABLED"
        _pfh_hint "Bind mounts will be denied and :z/:Z will do nothing. Do not turn SELinux off to work around this."
        _pfh_hint "Install the container policy (dnf install container-selinux) and check for label=false in containers.conf."
        return 1
      else
        _pfh_warn "SELinux: Enforcing, but Podman did not report its labeling state"
        _pfh_hint "Run 'podman info --format {{.Host.Security.SELinuxEnabled}}' to confirm labeling is on."
      fi
      ;;
    Permissive)
      # Everything appears to work here and then fails on the first host that
      # enforces, which is usually production.
      _pfh_warn "SELinux: Permissive — denials are logged, not enforced"
      _pfh_hint "Label problems will not surface here but will on an enforcing host. Qualify the deployment with 'setenforce 1'."
      _pfh_hint "Check for accumulated denials: ausearch -m AVC -ts recent"
      ;;
    Disabled)
      _pfh_pass "SELinux: not enabled on this host — mount labeling does not apply"
      ;;
    *)
      _pfh_warn "SELinux: state could not be determined"
      _pfh_hint "Run 'getenforce', or read /sys/fs/selinux/enforce, to establish the mode before deploying."
      ;;
  esac

  return 0
}

# --- 2. Mount labeling -------------------------------------------------------
#
# The three bind-mount sources in src/docker-compose.yaml. Each is consumed by
# exactly one container, so the private (:Z) form is always available; the
# read-only configuration files are called out as shared (:z) candidates
# because :Z stamps a per-container MCS category onto a file that is part of
# the operator's checkout and is likely read by other tooling.
#
# Format: relative-path|description|recommended-option
HIVE_PODMAN_BIND_SOURCES=(
  "hive.yaml|Hive configuration|z"
  "deploy/nginx.conf|gateway configuration|z"
  "secrets|secrets directory|Z"
)

# Labels a container process can read without a relabel. container_file_t is
# what :Z/:z and the container_file_t fcontext produce; container_share_t is
# the read-only shared variant.
_pfh_label_is_container_readable() {
  case "$1" in
    *:container_file_t:*|*:container_share_t:*|*:container_ro_file_t:*) return 0 ;;
  esac
  return 1
}

# Escapes a path for the POSIX extended regular expression semanage expects.
# Without it the dot in "hive.yaml" matches any character and the fcontext rule
# is wider than the operator wrote.
_pfh_regex_escape() {
  local out="$1"
  out="${out//\\/\\\\}"
  local meta
  for meta in '.' '[' ']' '^' '$' '*' '+' '?' '(' ')' '{' '}' '|'; do
    out="${out//"$meta"/\\$meta}"
  done
  printf '%s\n' "$out"
}

hive_podman_check_mount_labels() {
  local selinux_mode entry rel desc opt path label stat_out
  local unlabeled=0 checked=0

  selinux_mode="$(_pfh_selinux_mode)"

  if [[ "$selinux_mode" == "Disabled" ]]; then
    _pfh_pass "Mount labeling: not applicable — SELinux is not enabled"
    return 0
  fi

  for entry in "${HIVE_PODMAN_BIND_SOURCES[@]}"; do
    IFS='|' read -r rel desc opt <<<"$entry"
    path="${HIVE_SRC_DIR}/${rel}"

    # Existence and readability are the config/secrets check's job; a path that
    # is not there yet has no label to judge.
    [[ -e "$path" ]] || continue

    checked=$((checked + 1))
    stat_out="$(stat -c '%C' "$path" 2>/dev/null)" || stat_out=""
    label="$stat_out"

    if [[ -z "$label" || "$label" == "?" ]]; then
      _pfh_warn "Mount label: ${rel} (${desc}) carries no SELinux label"
      _pfh_hint "The filesystem holding it may not support security xattrs. Move the deployment onto one that does."
      continue
    fi

    if _pfh_label_is_container_readable "$label"; then
      _pfh_pass "Mount label: ${rel} is ${label} — already container-readable"
      continue
    fi

    unlabeled=$((unlabeled + 1))
    _pfh_warn "Mount label: ${rel} (${desc}) is ${label} — a container cannot read it as-is"
    if [[ "$opt" == "Z" ]]; then
      _pfh_hint "Mount it with :Z so Podman relabels it private to the one container that uses it."
    else
      _pfh_hint "Mount it with :z so Podman relabels it shared; :Z would stamp a private category onto a checked-out file."
    fi
    _pfh_hint "To label it once, up front, instead of on every run:"
    if [[ -d "$path" ]]; then
      _pfh_hint "  semanage fcontext -a -t container_file_t '$(_pfh_regex_escape "$path")(/.*)?' && restorecon -R '${path}'"
    else
      _pfh_hint "  semanage fcontext -a -t container_file_t '$(_pfh_regex_escape "$path")' && restorecon '${path}'"
    fi
  done

  if [[ "$checked" -eq 0 ]]; then
    _pfh_warn "Mount labeling: no bind-mount source found under ${HIVE_SRC_DIR}"
    _pfh_hint "Set HIVE_SRC_DIR to the directory holding hive.yaml, deploy/, and secrets/."
    return 0
  fi

  if [[ "$unlabeled" -gt 0 ]]; then
    _pfh_hint "None of this requires weakening SELinux; the kernel stays enforcing either way."
  fi

  return 0
}

# --- 3. Configuration and secrets readability --------------------------------
#
# Readable by the process that needs it, and by nobody else. Both halves are
# reported; neither is repaired here. Broadening a secret's permissions to make
# a check pass would defeat the point of running the check.

# Reports on one path. Usage: _pfh_check_readable <path> <label> <max-mode> <required>
#   max-mode: octal permission bits that must not be exceeded, empty to skip
#   required: "required" to fail when absent, anything else to warn
_pfh_check_readable() {
  local path="$1" desc="$2" max_mode="$3" required="$4"
  local mode owner_uid owner_name stat_out rootless

  if [[ ! -e "$path" ]]; then
    if [[ "$required" == "required" ]]; then
      _pfh_fail "${desc}: ${path} does not exist"
      return 1
    fi
    _pfh_warn "${desc}: ${path} does not exist"
    return 0
  fi

  if ! stat_out="$(stat -c '%a %u %U' "$path" 2>/dev/null)"; then
    _pfh_warn "${desc}: ${path} could not be inspected"
    return 0
  fi
  read -r mode owner_uid owner_name <<<"$stat_out"

  if [[ ! -r "$path" ]]; then
    _pfh_fail "${desc}: ${path} is not readable by $(id -un) (mode ${mode}, owner ${owner_name})"
    _pfh_hint "Rootless Podman opens bind mounts as the invoking user, so an unreadable source is an unreadable mount."
    _pfh_hint "Give that user access deliberately — do not widen the mode to make this pass."
    return 1
  fi

  # In rootless mode the invoking user maps to root inside the user namespace
  # and every other host owner maps to nobody, so an accessible-by-luck file
  # owned by someone else is a latent failure even when it reads fine today.
  rootless="$(_pfh_rootless)"
  if [[ "$rootless" == "true" && "$owner_uid" != "$(id -u)" ]]; then
    _pfh_warn "${desc}: ${path} is owned by ${owner_name}, not $(id -un)"
    _pfh_hint "Under rootless Podman it appears inside the container as 'nobody'; only the invoking user maps to container root."
  fi

  # Too-open modes are reported, never corrected, and the suggestion only ever
  # narrows. 0640 and 0600 both satisfy a 0640 maximum.
  if [[ -n "$max_mode" ]] && (( 8#$mode & ~8#$max_mode )); then
    _pfh_warn "${desc}: ${path} is mode ${mode} — wider than ${max_mode}"
    _pfh_hint "Narrow it when you are ready: chmod ${max_mode} '${path}'"
    _pfh_hint "Preflight does not change it; loosening host permissions is never the remedy."
    return 0
  fi

  _pfh_pass "${desc}: ${path} readable (mode ${mode}, owner ${owner_name})"
  return 0
}

hive_podman_check_config_secrets() {
  local secrets_dir="${HIVE_SRC_DIR}/secrets" key
  local rc=0

  _pfh_check_readable "${HIVE_SRC_DIR}/hive.yaml" "Hive config" "" "required" || rc=1
  _pfh_check_readable "${HIVE_SRC_DIR}/deploy/nginx.conf" "Gateway config" "" "required" || rc=1

  if [[ ! -d "$secrets_dir" ]]; then
    # Token-only deployments never create it; a GitHub App deployment must.
    _pfh_warn "Secrets: ${secrets_dir} does not exist"
    _pfh_hint "Only needed for GitHub App key auth. Create it narrow: mkdir -m 700 -p '${secrets_dir}'"
    return "$rc"
  fi

  # 0700: the directory is bind-mounted read-only, but the host copy is the
  # thing an unrelated local account would read.
  _pfh_check_readable "$secrets_dir" "Secrets dir" "700" "required" || rc=1

  local had_nullglob=0
  shopt -q nullglob && had_nullglob=1
  shopt -s nullglob
  for key in "$secrets_dir"/*; do
    [[ -f "$key" ]] || continue
    _pfh_check_readable "$key" "Secret" "600" "required" || rc=1
  done
  (( had_nullglob )) || shopt -u nullglob

  return "$rc"
}

# --- 4. Host port availability -----------------------------------------------

# Prints the listening sockets bound to $1, one per line, or returns 1 when no
# tool on this host can enumerate them.
_pfh_listeners() {
  local port="$1"

  if command -v ss >/dev/null 2>&1; then
    ss -Hltn 2>/dev/null | awk -v p=":${port}" '$4 ~ (p "$") { print }'
    return 0
  fi

  if command -v netstat >/dev/null 2>&1; then
    netstat -ltn 2>/dev/null | awk -v p=":${port}" '$4 ~ (p "$") { print }'
    return 0
  fi

  return 1
}

# The lowest port an unprivileged process may bind. Only meaningful rootless:
# rootful Podman binds anything. Empty when the value cannot be read, which
# turns the floor check off rather than guessing at it.
_pfh_unprivileged_port_start() {
  local floor=""

  if command -v sysctl >/dev/null 2>&1; then
    floor="$(sysctl -n net.ipv4.ip_unprivileged_port_start 2>/dev/null)"
  fi

  if [[ ! "$floor" =~ ^[0-9]+$ ]]; then
    # As above: assignment can succeed while the status reports EOF.
    read -r floor </proc/sys/net/ipv4/ip_unprivileged_port_start 2>/dev/null || true
  fi

  [[ "$floor" =~ ^[0-9]+$ ]] || return 0
  printf '%s\n' "$floor"
}

hive_podman_check_ports() {
  local rootless floor ports port listeners
  local rc=0

  # Accept either separator so the variable can be written the way it reads.
  read -r -a ports <<<"${HIVE_PODMAN_PREFLIGHT_PORTS//,/ }"

  if [[ "${#ports[@]}" -eq 0 ]]; then
    _pfh_warn "Ports: HIVE_PODMAN_PREFLIGHT_PORTS is empty — nothing checked"
    return 0
  fi

  rootless="$(_pfh_rootless)"
  floor=""
  if [[ "$rootless" == "true" ]]; then
    floor="$(_pfh_unprivileged_port_start)"
  fi

  for port in "${ports[@]}"; do
    if [[ ! "$port" =~ ^[0-9]+$ ]] || (( port < 1 || port > 65535 )); then
      _pfh_fail "Ports: ${port} is not a valid TCP port"
      rc=1
      continue
    fi

    if [[ -n "$floor" ]] && (( port < floor )); then
      # Rootless cannot bind below the floor at all; the container never starts
      # and the message from the engine does not mention the sysctl.
      _pfh_fail "Port ${port}: below the rootless unprivileged floor (${floor}) — rootless Podman cannot publish it"
      _pfh_hint "Raise the host's allowance: sysctl -w net.ipv4.ip_unprivileged_port_start=${port}"
      _pfh_hint "Or publish a port at or above ${floor} and put the privileged listener in front of it."
      rc=1
      continue
    fi

    if ! listeners="$(_pfh_listeners "$port")"; then
      _pfh_warn "Port ${port}: neither ss nor netstat is installed — availability unverified"
      _pfh_hint "Install iproute2 so preflight can see conflicts before startup."
      continue
    fi

    if [[ -n "$listeners" ]]; then
      _pfh_fail "Port ${port}: already in use on this host"
      printf '%s\n' "$listeners" | while IFS= read -r line; do
        [[ -n "$line" ]] && _pfh_hint "held by: ${line}"
      done
      _pfh_hint "Stop whatever holds it, or publish Hive's gateway on a different host port. Preflight changes nothing."
      rc=1
      continue
    fi

    _pfh_pass "Port ${port}: free"
  done

  return "$rc"
}

# --- Runner ------------------------------------------------------------------

hive_podman_preflight_host_selected_runtime() {
  local selector="${HIVE_PODMAN_PREFLIGHT_SELECTOR:-$(dirname "${BASH_SOURCE[0]}")/hive-standalone-runtime.sh}"

  if [[ -r "$selector" ]]; then
    # shellcheck source=bin/hive-standalone-runtime.sh disable=SC1090,SC1091
    source "$selector"
    hive_deploy_runtime_select
    return
  fi

  printf '%s\n' "${HIVE_DEPLOY_RUNTIME:-docker}"
}

hive_podman_preflight_host_main() {
  local runtime

  if ! runtime="$(hive_podman_preflight_host_selected_runtime)"; then
    return 64
  fi

  # The whole point of this gate: with Docker selected, not one Podman command
  # runs and Docker's own prerequisite checks are left exactly as they were.
  if [[ "$runtime" != "podman" ]]; then
    echo "Podman preflight: skipped — HIVE_DEPLOY_RUNTIME selects ${runtime}"
    return 0
  fi

  echo "Podman preflight (SELinux, mounts, secrets, ports)"

  # Each check reports independently: an operator fixing a host wants the whole
  # list, not the first item on it.
  hive_podman_check_selinux
  hive_podman_check_mount_labels
  hive_podman_check_config_secrets
  hive_podman_check_ports

  printf 'SUMMARY: pass=%d warn=%d fail=%d\n' \
    "$HIVE_PREFLIGHT_PASS" "$HIVE_PREFLIGHT_WARN" "$HIVE_PREFLIGHT_FAIL"

  [[ "$HIVE_PREFLIGHT_FAIL" -eq 0 ]] || return 78
  return 0
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  set -uo pipefail
  case "${1:-check}" in
    check) hive_podman_preflight_host_main ;;
    -h|--help|help)
      cat <<'EOF'
Usage: hive-podman-preflight-host.sh [check]

Reports the SELinux mode and Podman's labeling state, the SELinux labels on
every bind-mount source, whether the Hive configuration and secrets are
readable by the right user and nobody else, and whether the published host
ports are free.

Read-only. It starts no container, relabels nothing, changes no permission or
owner, and contacts no registry. It never proposes disabling SELinux and never
proposes widening access to a secret.

Runs only when HIVE_DEPLOY_RUNTIME selects podman; with the Docker default it
reports that it was skipped and exits 0.

Environment:
  HIVE_SRC_DIR                    directory holding hive.yaml, deploy/, secrets/
  HIVE_PODMAN_PREFLIGHT_PORTS     published host ports to check (default 3001)

Exit codes: 0 clean, 64 unusable runtime selection, 78 at least one failure.
EOF
      ;;
    *)
      printf 'ERROR: unknown action %q\n' "$1" >&2
      exit 64
      ;;
  esac
fi
