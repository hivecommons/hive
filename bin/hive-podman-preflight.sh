#!/usr/bin/env bash
# Podman preflight: engine, root mode, and cgroup checks (#4207).
#
# Operators need actionable diagnostics BEFORE Hive invokes a Podman lifecycle
# implementation. Without them the first sign of an unusable host is a
# half-created deployment failing somewhere inside `podman run`.
#
# This layer answers four questions and nothing else:
#
#   1. Is there a Podman engine, and is it new enough?
#   2. Which connection is it actually talking to? (local, or a remote/Machine
#      service, which changes what every later check even means)
#   3. Rootless or rootful?
#   4. Is the host on cgroup v2?
#
# Storage, SELinux, networking, secrets, and ports are separate issues (#4208,
# #4209). Nothing here writes to the host, starts a container, or contacts a
# registry.
#
# These checks run ONLY when Podman is explicitly selected through
# HIVE_DEPLOY_RUNTIME. Docker is the default and is not touched: with Docker
# selected this script reports that Podman is not in use and exits 0 without
# running a single Podman command.
#
# Run: bin/hive-podman-preflight.sh
# Exit codes: 0 no failing check, 78 at least one failing check (EX_CONFIG).

# Quadlet arrived in Podman 4.4; below that none of the lifecycle options under
# evaluation exist. Override for a host deliberately running something older.
HIVE_PODMAN_MIN_VERSION="${HIVE_PODMAN_MIN_VERSION:-4.4}"

# .pod units, the Pod= key, and Notify=healthy are Podman 5 features. Below 5
# the engine works but the Quadlet shape recorded in
# src/docs/podman-quadlet-container-pod-spike.md does not.
HIVE_PODMAN_QUADLET_POD_VERSION="${HIVE_PODMAN_QUADLET_POD_VERSION:-5.0}"

HIVE_PREFLIGHT_PASS=0
HIVE_PREFLIGHT_WARN=0
HIVE_PREFLIGHT_FAIL=0

# Markers match bin/hive-prereq-check.sh so the two read as one report when
# this is invoked from there.
_pf_pass() { echo "  ✓ $1"; HIVE_PREFLIGHT_PASS=$((HIVE_PREFLIGHT_PASS + 1)); }
_pf_warn() { echo "  △ $1"; HIVE_PREFLIGHT_WARN=$((HIVE_PREFLIGHT_WARN + 1)); }
_pf_fail() { echo "  ✗ $1"; HIVE_PREFLIGHT_FAIL=$((HIVE_PREFLIGHT_FAIL + 1)); }
_pf_hint() { echo "    → $1"; }

# Compares dotted versions. Returns 0 when $1 >= $2.
_pf_version_ge() {
  local have="$1" want="$2"
  local -a h w
  IFS='.' read -r -a h <<<"${have%%-*}"
  IFS='.' read -r -a w <<<"${want%%-*}"

  local i hv wv
  for i in 0 1 2; do
    hv="${h[i]:-0}"; wv="${w[i]:-0}"
    hv="${hv//[!0-9]/}"; wv="${wv//[!0-9]/}"
    hv="${hv:-0}"; wv="${wv:-0}"
    (( hv > wv )) && return 0
    (( hv < wv )) && return 1
  done
  return 0
}

# One `podman info` field. A template an older Podman does not know must
# degrade to "unknown" rather than abort the preflight.
_pf_info() {
  local template="$1" value
  value="$(podman info --format "$template" 2>/dev/null)" || return 1
  [[ -n "$value" && "$value" != "<no value>" ]] || return 1
  printf '%s\n' "$value"
}

# --- 1. Engine executable and version ---------------------------------------

hive_podman_check_engine() {
  local version

  if ! command -v podman >/dev/null 2>&1; then
    _pf_fail "Podman: not installed or not on PATH"
    _pf_hint "Install podman, or unset HIVE_DEPLOY_RUNTIME to use the Docker default."
    return 1
  fi

  if ! version="$(podman version --format '{{.Client.Version}}' 2>/dev/null)" || [[ -z "$version" ]]; then
    _pf_fail "Podman: $(command -v podman) did not report a client version"
    _pf_hint "Run 'podman version' directly; the install is not usable until it does."
    return 1
  fi

  if ! _pf_version_ge "$version" "$HIVE_PODMAN_MIN_VERSION"; then
    _pf_fail "Podman ${version} at $(command -v podman) — Hive needs ${HIVE_PODMAN_MIN_VERSION} or newer"
    _pf_hint "Quadlet arrived in 4.4; below that none of the supported lifecycle options exist."
    return 1
  fi

  _pf_pass "Podman ${version} ($(command -v podman))"

  if ! _pf_version_ge "$version" "$HIVE_PODMAN_QUADLET_POD_VERSION"; then
    _pf_warn "Podman ${version} predates ${HIVE_PODMAN_QUADLET_POD_VERSION} — .pod units, Pod=, and Notify=healthy are unavailable"
    _pf_hint "The Compose lifecycle still works; the Quadlet pod layout does not."
  fi

  return 0
}

# --- 2. Connection identity --------------------------------------------------
#
# Reported, not judged. A remote or Machine connection is legitimate, but every
# later check means something different when the engine is not on this host --
# storage, SELinux, and published ports all belong to the far side.

hive_podman_check_connection() {
  local remote connection

  remote="$(_pf_info '{{.Host.ServiceIsRemote}}')" || remote="unknown"

  connection="$(podman system connection list --format '{{.Name}} {{.URI}} {{.Default}}' 2>/dev/null \
    | awk '$3 == "true" { print $1 " -> " $2 }' | head -1)"

  if [[ "$remote" == "true" ]]; then
    _pf_warn "Podman connection: REMOTE${connection:+ (${connection})}"
    _pf_hint "The engine is not on this host. Storage, SELinux, and port checks apply to the far side, not here."
  elif [[ -n "$connection" ]]; then
    _pf_pass "Podman connection: local, default connection ${connection}"
  elif [[ "$remote" == "false" ]]; then
    _pf_pass "Podman connection: local (no named connection configured)"
  else
    _pf_warn "Podman connection: could not be determined"
    _pf_hint "Run 'podman system connection list' and 'podman info' to see what this install is talking to."
  fi

  return 0
}

# --- 3. Root mode ------------------------------------------------------------
#
# Reported explicitly rather than inferred, because the supported
# rootful/rootless matrix in #4188 turns on it and because the effective
# security mode differs between them.

hive_podman_check_root_mode() {
  local rootless

  if ! rootless="$(_pf_info '{{.Host.Security.Rootless}}')"; then
    _pf_fail "Podman root mode: podman info did not report it"
    _pf_hint "Run 'podman info' directly; the engine is not answering."
    return 1
  fi

  case "$rootless" in
    true)  _pf_pass "Podman root mode: rootless (uid $(id -u))" ;;
    false) _pf_pass "Podman root mode: rootful" ;;
    *)
      _pf_warn "Podman root mode: unrecognized value ${rootless}"
      return 0
      ;;
  esac

  return 0
}

# --- 4. cgroup version -------------------------------------------------------

hive_podman_check_cgroups() {
  local version manager

  if ! version="$(_pf_info '{{.Host.CgroupsVersion}}')"; then
    _pf_fail "cgroups: podman info did not report a version"
    return 1
  fi

  manager="$(_pf_info '{{.Host.CgroupManager}}')" || manager="unknown"

  if [[ "$version" != "v2" ]]; then
    _pf_fail "cgroups: ${version} — Hive's Podman path requires cgroup v2"
    _pf_hint "Quadlet requires v2, and rootless resource control is unavailable on v1."
    _pf_hint "Boot with systemd.unified_cgroup_hierarchy=1, then re-run."
    return 1
  fi

  _pf_pass "cgroups: v2 (manager: ${manager})"
  return 0
}

# --- Runner ------------------------------------------------------------------

hive_podman_preflight_selected_runtime() {
  local selector="${HIVE_PODMAN_PREFLIGHT_SELECTOR:-$(dirname "${BASH_SOURCE[0]}")/hive-standalone-runtime.sh}"

  if [[ -r "$selector" ]]; then
    # shellcheck source=bin/hive-standalone-runtime.sh disable=SC1090,SC1091
    source "$selector"
    hive_deploy_runtime_select
    return
  fi

  printf '%s\n' "${HIVE_DEPLOY_RUNTIME:-docker}"
}

hive_podman_preflight_main() {
  local runtime

  if ! runtime="$(hive_podman_preflight_selected_runtime)"; then
    return 64
  fi

  # The whole point of this gate: with Docker selected, not one Podman command
  # runs and Docker's own prerequisite checks are left exactly as they were.
  if [[ "$runtime" != "podman" ]]; then
    echo "Podman preflight: skipped — HIVE_DEPLOY_RUNTIME selects ${runtime}"
    return 0
  fi

  echo "Podman preflight (engine, root mode, cgroups)"

  if hive_podman_check_engine; then
    hive_podman_check_connection
    hive_podman_check_root_mode
    hive_podman_check_cgroups
  else
    # Every remaining check shells out to podman, so without a usable engine
    # they would all report the same missing binary in four different ways.
    _pf_hint "Connection, root mode, and cgroup checks skipped — no usable engine."
  fi

  printf 'SUMMARY: pass=%d warn=%d fail=%d\n' \
    "$HIVE_PREFLIGHT_PASS" "$HIVE_PREFLIGHT_WARN" "$HIVE_PREFLIGHT_FAIL"

  [[ "$HIVE_PREFLIGHT_FAIL" -eq 0 ]] || return 78
  return 0
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  set -uo pipefail
  case "${1:-check}" in
    check) hive_podman_preflight_main ;;
    -h|--help|help)
      cat <<'EOF'
Usage: hive-podman-preflight.sh [check]

Reports the Podman engine and version, the connection it is talking to, the
root mode, and the cgroup version. Read-only: it starts no container, writes
nothing to the host, and contacts no registry.

Runs only when HIVE_DEPLOY_RUNTIME selects podman; with the Docker default it
reports that it was skipped and exits 0.

Exit codes: 0 clean, 78 at least one failing check.
EOF
      ;;
    *)
      printf 'ERROR: unknown action %q\n' "$1" >&2
      exit 64
      ;;
  esac
fi
