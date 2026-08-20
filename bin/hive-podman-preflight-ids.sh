#!/usr/bin/env bash
# Podman preflight: subordinate IDs, graphroot, and networking (#4208).
#
# Rootless Podman needs three things from the host that have nothing to do with
# the engine version and everything to do with how the box was prepared. All
# three fail LATE and confusingly if nobody looks first:
#
#   1. SUBORDINATE IDs. Rootless containers map container UIDs onto a range the
#      host has delegated to the user in /etc/subuid and /etc/subgid. With no
#      range, `podman run` fails with "cannot find UID/GID for user" — after it
#      has already created the container. With a range too small, the container
#      starts and then breaks on the first file owned by a UID past the end.
#   2. GRAPHROOT. Container storage needs a local filesystem that supports the
#      ownership and extended attributes overlayfs relies on. On NFS it does
#      not: layers fail to extract, or extract and then behave incorrectly.
#      This is a well-known unsupported configuration and the failure it
#      produces never mentions the filesystem.
#   3. ROOTLESS NETWORKING. A rootless container reaches the network through a
#      helper (pasta or slirp4netns) driven by a backend (netavark or the
#      retired CNI). If the helper named by the engine is not installed, every
#      container starts and has no network.
#
# This layer diagnoses exactly those and stops. It is read-only. In particular
# it NEVER writes to /etc/subuid or /etc/subgid: delegating a subordinate range
# is a host-administration decision with security consequences, made by an
# administrator who knows which ranges are already spoken for, not by a
# preflight script running as an unprivileged user. Missing mappings are
# reported with the exact usermod command to run.
#
# The engine, connection, root mode, and cgroup layer is #4207; SELinux,
# mounts, secrets, and ports are #4209.
#
# These checks run ONLY when Podman is explicitly selected through
# HIVE_DEPLOY_RUNTIME. Docker is the default and is untouched: with Docker
# selected this reports that Podman is not in use and exits 0 without running
# a single Podman command.
#
# Run: bin/hive-podman-preflight-ids.sh
# Exit codes: 0 no failing check, 64 unusable runtime selection,
#             78 at least one failing check (EX_CONFIG).

# A rootless container image commonly contains files owned by UIDs up to 65535,
# so a delegated range shorter than 65536 breaks on extraction. This is the
# value shadow-utils itself hands out by default; a host that deliberately
# delegates less can lower it.
HIVE_PODMAN_MIN_SUBID_COUNT="${HIVE_PODMAN_MIN_SUBID_COUNT:-65536}"

# The subid tables. Overridable so the contract tests can exercise the missing,
# short, and multi-range cases without touching a real /etc file — the same
# reason the sibling layer parameterises HIVE_SRC_DIR. Nothing here ever writes
# to them, whichever paths they name.
HIVE_PODMAN_SUBUID_FILE="${HIVE_PODMAN_SUBUID_FILE:-/etc/subuid}"
HIVE_PODMAN_SUBGID_FILE="${HIVE_PODMAN_SUBGID_FILE:-/etc/subgid}"

HIVE_PREFLIGHT_PASS=0
HIVE_PREFLIGHT_WARN=0
HIVE_PREFLIGHT_FAIL=0

# Markers match bin/hive-prereq-check.sh so the two read as one report when
# this is invoked from there.
_pfi_pass() { echo "  ✓ $1"; HIVE_PREFLIGHT_PASS=$((HIVE_PREFLIGHT_PASS + 1)); }
_pfi_warn() { echo "  △ $1"; HIVE_PREFLIGHT_WARN=$((HIVE_PREFLIGHT_WARN + 1)); }
_pfi_fail() { echo "  ✗ $1"; HIVE_PREFLIGHT_FAIL=$((HIVE_PREFLIGHT_FAIL + 1)); }
_pfi_hint() { echo "    → $1"; }

# One `podman info` field. A template an older Podman does not know must
# degrade to a caller-visible failure rather than abort the preflight.
_pfi_info() {
  local template="$1" value
  value="$(podman info --format "$template" 2>/dev/null)" || return 1
  [[ -n "$value" && "$value" != "<no value>" ]] || return 1
  printf '%s\n' "$value"
}

# true when the engine is rootless, false when rootful, empty when unknown.
# Every check in this file means something different in each mode — subordinate
# IDs are irrelevant to a rootful engine, and so is the rootless network helper
# — so an unknown mode downgrades a verdict to a report.
_PFI_ROOTLESS_CACHE=""
_pfi_rootless() {
  local rootless

  if [[ -n "$_PFI_ROOTLESS_CACHE" ]]; then
    [[ "$_PFI_ROOTLESS_CACHE" == "unknown" ]] || printf '%s\n' "$_PFI_ROOTLESS_CACHE"
    return 0
  fi

  _PFI_ROOTLESS_CACHE="unknown"
  rootless="$(_pfi_info '{{.Host.Security.Rootless}}')" || return 0
  case "$rootless" in
    true|false)
      _PFI_ROOTLESS_CACHE="$rootless"
      printf '%s\n' "$rootless"
      ;;
  esac
}

# --- 1. Subordinate UID/GID mappings ----------------------------------------
#
# Read from /etc/subuid and /etc/subgid directly rather than from `podman info`.
# The engine reports the mappings it RESOLVED, which on a host using a
# non-file source (SSSD, a directory service) can be correct while the files
# are empty — and can be stale relative to a file edited since the engine last
# started. The files are what an administrator edits and what the remediation
# command changes, so they are what an operator needs reported.

# Sums the count column of every entry for a user in a subid file. Entries are
# `name:start:count`, and a user may legitimately hold several ranges, so the
# counts add. Prints 0 when the user has no entry at all — which is the case
# that matters and must be distinguishable from a small range.
_pfi_subid_count() {
  local file="$1" user="$2" line name count total=0

  [[ -r "$file" ]] || return 1

  while IFS= read -r line || [[ -n "$line" ]]; do
    # Skip comments and blanks; a subid file is a plain colon-separated table.
    line="${line%%#*}"
    [[ -n "${line// /}" ]] || continue
    IFS=':' read -r name _ count <<<"$line"
    [[ "$name" == "$user" ]] || continue
    # A malformed count must not silently read as zero delegation.
    [[ "$count" =~ ^[0-9]+$ ]] || continue
    total=$((total + count))
  done <"$file"

  printf '%d\n' "$total"
}

hive_podman_check_subordinate_ids() {
  local rootless user uid_count gid_count
  rootless="$(_pfi_rootless)"
  user="${USER:-$(id -un 2>/dev/null)}"

  if [[ "$rootless" == "false" ]]; then
    # A rootful engine maps container UIDs straight onto host UIDs; there is no
    # subordinate range in the picture and an absent one is not a defect.
    _pfi_pass "Subordinate IDs: not required (rootful engine)"
    return 0
  fi

  if [[ -z "$user" ]]; then
    _pfi_warn "Subordinate IDs: cannot determine the current user — mappings not checked"
    return 0
  fi

  if ! uid_count="$(_pfi_subid_count "$HIVE_PODMAN_SUBUID_FILE" "$user")"; then
    _pfi_warn "Subordinate IDs: ${HIVE_PODMAN_SUBUID_FILE} is not readable — mappings not checked"
    _pfi_hint "the file is world-readable on a normal host; an unreadable one is itself worth a look"
    return 0
  fi
  if ! gid_count="$(_pfi_subid_count "$HIVE_PODMAN_SUBGID_FILE" "$user")"; then
    _pfi_warn "Subordinate IDs: ${HIVE_PODMAN_SUBGID_FILE} is not readable — mappings not checked"
    return 0
  fi

  # No delegation at all is the hard failure: rootless containers cannot start.
  if [[ "$uid_count" -eq 0 || "$gid_count" -eq 0 ]]; then
    _pfi_fail "Subordinate IDs: no range delegated to ${user} (subuid=${uid_count} subgid=${gid_count})"
    _pfi_hint "an administrator delegates one with: sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 ${user}"
    _pfi_hint "then: podman system migrate"
    _pfi_hint "this preflight will not edit /etc/subuid or /etc/subgid — pick ranges that do not overlap an existing user"
    return 0
  fi

  # A short range starts and then breaks partway through extracting an image,
  # which is far harder to recognise than not starting at all.
  if [[ "$uid_count" -lt "$HIVE_PODMAN_MIN_SUBID_COUNT" || "$gid_count" -lt "$HIVE_PODMAN_MIN_SUBID_COUNT" ]]; then
    _pfi_warn "Subordinate IDs: ${user} has subuid=${uid_count} subgid=${gid_count}, below the usual ${HIVE_PODMAN_MIN_SUBID_COUNT}"
    _pfi_hint "images containing files owned by high UIDs will fail to extract; widen the range with usermod --add-subuids"
    return 0
  fi

  _pfi_pass "Subordinate IDs: ${user} has subuid=${uid_count} subgid=${gid_count}"
}

# --- 2. Graphroot filesystem -------------------------------------------------
#
# Container storage on a distributed or network filesystem is unsupported, and
# the way it fails does not name the filesystem. NFS is the one operators
# actually hit, usually because the home directory is on it and rootless
# storage defaults under $HOME.

# Filesystem type of the path's mount. `stat -f` is the portable reading and
# needs no mount-table parsing; findmnt is the fallback for a stat that does
# not implement %T.
_pfi_fstype() {
  local path="$1" fstype

  if fstype="$(stat -f -c %T "$path" 2>/dev/null)" && [[ -n "$fstype" ]]; then
    printf '%s\n' "$fstype"
    return 0
  fi
  if command -v findmnt >/dev/null 2>&1 &&
    fstype="$(findmnt -n -o FSTYPE --target "$path" 2>/dev/null)" && [[ -n "$fstype" ]]; then
    printf '%s\n' "$fstype"
    return 0
  fi
  return 1
}

# Filesystems container storage must not live on. Every one of these either
# lacks the ownership/xattr semantics overlayfs needs or serves the same tree
# to several hosts, which silently corrupts a store built for one.
_pfi_fstype_unsupported() {
  case "$1" in
    nfs | nfs4 | nfsd | cifs | smb | smb2 | smbfs | fuse.sshfs | fuseblk.sshfs | \
      9p | ceph | fuse.cephfs | glusterfs | fuse.glusterfs | afs | lustre | gpfs)
      return 0
      ;;
  esac
  return 1
}

hive_podman_check_graphroot() {
  local graphroot driver fstype

  if ! graphroot="$(_pfi_info '{{.Store.GraphRoot}}')"; then
    _pfi_warn "Graphroot: podman did not report a storage root — filesystem not checked"
    return 0
  fi
  driver="$(_pfi_info '{{.Store.GraphDriverName}}')" || driver="unknown"

  if ! fstype="$(_pfi_fstype "$graphroot")"; then
    _pfi_warn "Graphroot: ${graphroot} (driver ${driver}) — filesystem type could not be determined"
    return 0
  fi

  if _pfi_fstype_unsupported "$fstype"; then
    _pfi_fail "Graphroot: ${graphroot} is on ${fstype}, which container storage does not support"
    _pfi_hint "move the store to local disk: set graphroot in ~/.config/containers/storage.conf (rootless) or /etc/containers/storage.conf"
    _pfi_hint "or export CONTAINERS_STORAGE_CONF pointing at a config whose graphroot is local"
    _pfi_hint "after moving it: podman system reset (destroys existing local containers and images)"
    return 0
  fi

  # tmpfs works and is genuinely wanted for a throwaway CI store, so this is a
  # warning about what it costs rather than a failure.
  if [[ "$fstype" == "tmpfs" || "$fstype" == "ramfs" ]]; then
    _pfi_warn "Graphroot: ${graphroot} is on ${fstype} — images and containers are lost on reboot and consume RAM"
    return 0
  fi

  _pfi_pass "Graphroot: ${graphroot} on ${fstype} (driver ${driver})"
}

# --- 3. Rootless networking --------------------------------------------------
#
# The engine names the helper it intends to use. Whether that helper is
# actually installed is a separate question, and the answer only shows up as a
# container with no network.

hive_podman_check_rootless_network() {
  local rootless backend helper

  backend="$(_pfi_info '{{.Host.NetworkBackend}}')" || backend=""
  rootless="$(_pfi_rootless)"

  if [[ -n "$backend" ]]; then
    case "$backend" in
      netavark)
        _pfi_pass "Network backend: netavark"
        ;;
      cni)
        # CNI still works on the Podman versions that ship it, so this reports
        # the deprecation rather than failing a host that is running fine.
        _pfi_warn "Network backend: cni — retired upstream in favour of netavark"
        _pfi_hint "install netavark and aardvark-dns, then: podman system reset"
        ;;
      *)
        _pfi_warn "Network backend: ${backend} (unrecognised)"
        ;;
    esac
  else
    _pfi_warn "Network backend: podman did not report one"
  fi

  if [[ "$rootless" == "false" ]]; then
    # A rootful engine puts containers on a bridge directly; no helper process
    # is involved and its absence is not a defect.
    _pfi_pass "Rootless network helper: not required (rootful engine)"
    return 0
  fi

  if ! helper="$(_pfi_info '{{.Host.RootlessNetworkCmd}}')"; then
    _pfi_warn "Rootless network helper: podman did not report one — containers may have no network"
    _pfi_hint "install pasta (passt) or slirp4netns and re-check"
    return 0
  fi

  # The engine names a helper; confirm the binary is actually on the host.
  # `pasta` is shipped by the passt package and is what Podman 5 selects by
  # default; slirp4netns is the older one and is still perfectly serviceable.
  if command -v "$helper" >/dev/null 2>&1; then
    _pfi_pass "Rootless network helper: ${helper} present"
    return 0
  fi

  _pfi_fail "Rootless network helper: podman selects ${helper}, but it is not installed"
  _pfi_hint "containers will start with no network until it is present"
  case "$helper" in
    pasta) _pfi_hint "install it: dnf install passt   (Debian/Ubuntu: apt install passt)" ;;
    slirp4netns) _pfi_hint "install it: dnf install slirp4netns   (Debian/Ubuntu: apt install slirp4netns)" ;;
    *) _pfi_hint "install ${helper} from your distribution" ;;
  esac
}

# --- Runtime gate and entry point -------------------------------------------

hive_podman_preflight_ids_selected_runtime() {
  local selector="${HIVE_PODMAN_PREFLIGHT_SELECTOR:-$(dirname "${BASH_SOURCE[0]}")/hive-standalone-runtime.sh}"

  if [[ -r "$selector" ]]; then
    # shellcheck source=bin/hive-standalone-runtime.sh disable=SC1090,SC1091
    source "$selector"
    hive_deploy_runtime_select
    return
  fi

  printf '%s\n' "${HIVE_DEPLOY_RUNTIME:-docker}"
}

hive_podman_preflight_ids_main() {
  local runtime

  if ! runtime="$(hive_podman_preflight_ids_selected_runtime)"; then
    return 64
  fi

  # The whole point of this gate: with Docker selected, not one Podman command
  # runs and Docker's own prerequisite checks are left exactly as they were.
  if [[ "$runtime" != "podman" ]]; then
    echo "Podman preflight: skipped — HIVE_DEPLOY_RUNTIME selects ${runtime}"
    return 0
  fi

  echo "Podman preflight (subordinate IDs, graphroot, networking)"

  # Each check reports independently: an operator fixing a host wants the whole
  # list, not the first item on it.
  hive_podman_check_subordinate_ids
  hive_podman_check_graphroot
  hive_podman_check_rootless_network

  printf 'SUMMARY: pass=%d warn=%d fail=%d\n' \
    "$HIVE_PREFLIGHT_PASS" "$HIVE_PREFLIGHT_WARN" "$HIVE_PREFLIGHT_FAIL"

  [[ "$HIVE_PREFLIGHT_FAIL" -eq 0 ]] || return 78
  return 0
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  set -uo pipefail
  case "${1:-check}" in
    check) hive_podman_preflight_ids_main ;;
    -h|--help|help)
      cat <<'EOF'
Usage: hive-podman-preflight-ids.sh [check]

Reports whether the current user holds subordinate UID/GID ranges large enough
for rootless containers, whether Podman's storage root sits on a filesystem
container storage supports, and whether the rootless network helper the engine
selects is actually installed.

Read-only. It never writes to /etc/subuid or /etc/subgid — delegating a
subordinate range is an administrator's decision, so a missing mapping is
reported with the usermod command rather than applied. It starts no container,
moves no storage, installs nothing, and contacts no registry.

Runs only when HIVE_DEPLOY_RUNTIME selects podman; with the Docker default it
reports that it was skipped and exits 0.

Environment:
  HIVE_PODMAN_MIN_SUBID_COUNT   smallest acceptable delegated range (default 65536)
  HIVE_PODMAN_SUBUID_FILE       subuid table to read (default /etc/subuid)
  HIVE_PODMAN_SUBGID_FILE       subgid table to read (default /etc/subgid)

Exit codes: 0 clean, 64 unusable runtime selection, 78 at least one failure.
EOF
      ;;
    *)
      printf 'ERROR: unknown action %q\n' "$1" >&2
      exit 64
      ;;
  esac
fi
