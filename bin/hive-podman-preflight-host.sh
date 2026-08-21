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
# TWO LAYOUTS, AND THE DIFFERENCE IS NOT COSMETIC (#4422).
#
# This check knew only the source-tree layout — the one the Compose stack mounts
# out of a checkout, where the gateway config sits at `deploy/nginx.conf`. The
# Quadlet install (src/docs/podman-standalone-quadlet.md) puts the operator's
# files in `%E/hive` with `nginx.conf` FLAT, so pointing this script at that
# directory reported
#
#   ✗ Gateway config: <dir>/deploy/nginx.conf does not exist
#
# and exited 78 with the file sitting right there, one level up. That landed on
# the one install path where this check is most useful: SELinux labelling of
# those binds is exactly the class of problem that otherwise surfaces as a
# 300-second start timeout with the container already `--rm`'d away. A non-zero
# exit also invites an operator to stop reading a report whose other rows —
# mount labels, the secrets group-traverse check, port 3001 — they still need.
#
# The recommended relabel option differs by layout too, and getting THAT
# backwards would be worse than the path bug:
#
#   source-tree   the config files are part of the operator's CHECKOUT and are
#                 likely read by other tooling, so `:z` (shared) is right and
#                 `:Z` would stamp a per-container MCS category onto a tracked
#                 file.
#   quadlet       the config files are operator-owned files in %E/hive read by
#                 exactly ONE container, and the shipped units mount all three
#                 `:ro,Z`. Recommending `:z` there would contradict the units.
#
# Format: relative-path|description|recommended-option
HIVE_PODMAN_BIND_SOURCES_SOURCE=(
  "hive.yaml|Hive configuration|z"
  "deploy/nginx.conf|gateway configuration|z"
  "secrets|secrets directory|Z"
)
HIVE_PODMAN_BIND_SOURCES_QUADLET=(
  "hive.yaml|Hive configuration|Z"
  "nginx.conf|gateway configuration|Z"
  "secrets|secrets directory|Z"
)

# Which layout HIVE_SRC_DIR holds: auto | source | quadlet. `auto` decides from
# what is on disk; the explicit values are for a directory mid-install that has
# neither gateway config in place yet.
HIVE_PODMAN_LAYOUT="${HIVE_PODMAN_LAYOUT:-auto}"

# Resolved once by _pfh_resolve_layout, read by every check below.
HIVE_PFH_LAYOUT=""
HIVE_PFH_NGINX_REL=""
HIVE_PODMAN_BIND_SOURCES=()

# Decides the layout and populates the three globals above. Detection keys on
# the gateway config, the only file whose PATH differs between the two layouts;
# `hive.env` is the tiebreak for a Quadlet directory that has not had nginx.conf
# copied in yet, since nothing in the source tree carries that name.
_pfh_resolve_layout() {
  local dir="$HIVE_SRC_DIR" chosen="$HIVE_PODMAN_LAYOUT"

  if [[ "$chosen" == "auto" ]]; then
    if [[ -e "${dir}/deploy/nginx.conf" ]]; then
      chosen="source"
    elif [[ -e "${dir}/nginx.conf" ]]; then
      chosen="quadlet"
    elif [[ -e "${dir}/hive.env" ]]; then
      chosen="quadlet"
    else
      # Neither shape is on disk. Stay on the historical default rather than
      # guess; the config check reports the missing files by name either way.
      chosen="source"
    fi
  fi

  case "$chosen" in
    source)
      HIVE_PFH_LAYOUT="source"
      HIVE_PFH_NGINX_REL="deploy/nginx.conf"
      HIVE_PODMAN_BIND_SOURCES=("${HIVE_PODMAN_BIND_SOURCES_SOURCE[@]}")
      ;;
    quadlet)
      HIVE_PFH_LAYOUT="quadlet"
      HIVE_PFH_NGINX_REL="nginx.conf"
      HIVE_PODMAN_BIND_SOURCES=("${HIVE_PODMAN_BIND_SOURCES_QUADLET[@]}")
      ;;
    *)
      printf 'ERROR: HIVE_PODMAN_LAYOUT must be auto, source, or quadlet (got %q)\n' \
        "$HIVE_PODMAN_LAYOUT" >&2
      exit 64
      ;;
  esac
}

# --- Reading an SELinux label ------------------------------------------------
#
# `stat -c '%C'` is not a reliable reader. Under uutils coreutils — the default
# on the Fedora-atomic hosts this lane targets — `stat` has no %C and answers
# "unsupported for this operating system" on STDOUT with exit status 0. A
# `|| fallback` therefore never fires and the sentence flows onward as if it
# were a context, so no path can ever match a container type and the check
# warns even about a path the operator has already labelled correctly (#4359).
#
# So the reader is RESOLVED, not assumed: each candidate is tried against a
# real file and its output must look like a context before it is trusted. When
# none does, "cannot read a label" is reported as its own outcome rather than
# being mistaken for "unlabelled" — the check says what it does not know.
#
# Overridable only so the contract test can present a host carrying just one of
# them; operators have no reason to set it.
HIVE_PFH_LABEL_READERS="${HIVE_PFH_LABEL_READERS:-stat /usr/bin/stat getfattr}"

# Resolved once per run: a reader command, or "none".
HIVE_PFH_LABEL_READER=""

# An SELinux context is user:role:type:level. Requiring the role field is what
# makes this a validator rather than a non-empty test — it is exactly the check
# the uutils sentence fails.
_pfh_looks_like_context() {
  case "$1" in
    *:object_r:*) return 0 ;;
  esac
  return 1
}

# Runs one candidate reader against one path. getfattr spells the request
# differently from stat; everything else takes stat's format flag.
_pfh_run_label_reader() {
  local reader="$1" path="$2"

  case "${reader##*/}" in
    getfattr) "$reader" -n security.selinux --only-values --absolute-names "$path" 2>/dev/null ;;
    *) "$reader" -c '%C' "$path" 2>/dev/null ;;
  esac
}

# Where the reader is probed. Deliberately NOT a deployment path: probing on
# one would conflate the two outcomes this whole change exists to separate — a
# reader that does not work, and a path that genuinely has no label. On an
# SELinux host these always carry a context, whatever the deployment sits on.
HIVE_PFH_LABEL_PROBES="${HIVE_PFH_LABEL_PROBES:-/etc /}"

# Picks the first candidate that returns something context-shaped. Returns 1
# when none does, and remembers that so later paths do not re-probe.
_pfh_resolve_label_reader() {
  local reader probe out

  if [[ -n "$HIVE_PFH_LABEL_READER" ]]; then
    [[ "$HIVE_PFH_LABEL_READER" != "none" ]]
    return
  fi

  for reader in $HIVE_PFH_LABEL_READERS; do
    command -v "$reader" >/dev/null 2>&1 || continue
    for probe in $HIVE_PFH_LABEL_PROBES; do
      [[ -e "$probe" ]] || continue
      out="$(_pfh_run_label_reader "$reader" "$probe")" || continue
      _pfh_looks_like_context "$out" || continue
      HIVE_PFH_LABEL_READER="$reader"
      return 0
    done
  done

  HIVE_PFH_LABEL_READER="none"
  return 1
}

# The label of $1, or empty when this path has none. A reader that succeeded on
# the probe can still return nothing useful here — a filesystem without security
# xattrs is the usual reason — so the shape is re-checked every time rather than
# assumed from the probe.
_pfh_read_label() {
  local out
  out="$(_pfh_run_label_reader "$HIVE_PFH_LABEL_READER" "$1")" || return 1
  _pfh_looks_like_context "$out" || return 1
  printf '%s\n' "$out"
}

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
  local selinux_mode entry rel desc opt path label
  local unlabeled=0 checked=0
  local -a present=()

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
    [[ -e "$path" ]] && present+=("$entry")
  done

  # Resolved once, before any path is judged, so a host with no working reader
  # says so once instead of repeating a complaint per bind source.
  if [[ "${#present[@]}" -gt 0 ]]; then
    if ! _pfh_resolve_label_reader; then
      _pfh_warn "Mount labeling: no tool on this host can read an SELinux label"
      _pfh_hint "Tried: ${HIVE_PFH_LABEL_READERS}. Under uutils coreutils, 'stat -c %C' prints an error to stdout and exits 0, so a label read that way is not a label."
      _pfh_hint "Install GNU coreutils or attr (getfattr), then re-run. Mount labeling is UNCHECKED until then — this is not a pass."
      _pfh_hint "Nothing here requires weakening SELinux; the kernel stays enforcing either way."
      return 0
    fi
  fi

  for entry in "${present[@]}"; do
    IFS='|' read -r rel desc opt <<<"$entry"
    path="${HIVE_SRC_DIR}/${rel}"

    checked=$((checked + 1))
    label="$(_pfh_read_label "$path")" || label=""

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
    _pfh_warn "Mount labeling: no bind-mount source found under ${HIVE_SRC_DIR} (layout: ${HIVE_PFH_LAYOUT})"
    _pfh_hint "Set HIVE_SRC_DIR to the directory holding the bind sources: hive.yaml, ${HIVE_PFH_NGINX_REL}, and secrets/."
    _pfh_hint "A source tree (Compose) has deploy/nginx.conf; a Quadlet install has nginx.conf flat in %E/hive. Force either with HIVE_PODMAN_LAYOUT=source|quadlet."
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

# --- Secrets the container can actually reach --------------------------------
#
# The image reads GitHub App keys as `dev` (UID 1001) through the `hive-launch`
# supplementary group, GID 1002 — pinned in src/Dockerfile. The Kubernetes path
# spells the same requirement as `fsGroup: 1002` in
# src/deploy/k8s/deployment.yaml; the standalone path had no equivalent, so its
# advice ("mkdir -m 700") produced a directory the container cannot traverse
# (#4359). A 0700 directory owned by the operator grants `dev` nothing — not
# even the traverse bit — and no amount of relabeling changes that, because the
# refusal is DAC and never reaches SELinux. It presents as Permission denied on
# a key file on an enforcing host, which reads as a labeling problem and is not
# one.
HIVE_PFH_LAUNCH_GID="${HIVE_PFH_LAUNCH_GID:-1002}"

# Where the subordinate ID ranges live. Overridable for the contract test.
HIVE_PFH_SUBGID_FILE="${HIVE_PFH_SUBGID_FILE:-/etc/subgid}"

# The HOST gid that a given CONTAINER gid maps to.
#
# Rootful maps identity, so 1002 is 1002. Rootless does not: the invoking user
# maps to container root and everything above it comes out of the subordinate
# range, so container GID 1002 is subgid_start + 1001 on the host. That is why
# a plain `chgrp 1002` is both wrong and — for an unprivileged user who is not
# in group 1002 — not even permitted. `podman unshare` exists to do this
# translation, and is what the remediation below uses.
_pfh_mapped_gid() {
  local cgid="$1" line start count

  if [[ "$(_pfh_rootless)" != "true" ]]; then
    printf '%s\n' "$cgid"
    return 0
  fi

  line="$(awk -F: -v u="$(id -un)" '$1 == u { print $2 ":" $3; exit }' \
    "$HIVE_PFH_SUBGID_FILE" 2>/dev/null)"
  [[ -n "$line" ]] || return 1

  start="${line%%:*}"
  count="${line##*:}"
  [[ "$start" =~ ^[0-9]+$ && "$count" =~ ^[0-9]+$ ]] || return 1
  (( cgid >= 1 && cgid <= count )) || return 1

  printf '%s\n' "$(( start + cgid - 1 ))"
}

# Reports whether the container's launch group can traverse the secrets
# directory and read what is in it. Read-only: it computes the mapping and
# compares ownership, and starts no container.
_pfh_check_secrets_reachable() {
  local dir="$1" want_gid dir_gid dir_mode rootless

  rootless="$(_pfh_rootless)"

  if ! want_gid="$(_pfh_mapped_gid "$HIVE_PFH_LAUNCH_GID")"; then
    _pfh_warn "Secrets reach: cannot tell which host group maps to container GID ${HIVE_PFH_LAUNCH_GID}"
    _pfh_hint "No usable range for $(id -un) in ${HIVE_PFH_SUBGID_FILE}; subordinate IDs are #4208's check."
    return 0
  fi

  dir_gid="$(stat -c '%g' "$dir" 2>/dev/null)" || return 0
  dir_mode="$(stat -c '%a' "$dir" 2>/dev/null)" || return 0

  # Group-execute on a directory is the traverse bit. Without it the group
  # ownership is decoration.
  if [[ "$dir_gid" == "$want_gid" ]] && (( 8#$dir_mode & 8#010 )); then
    _pfh_pass "Secrets reach: ${dir} is traversable by the container's hive-launch group (host gid ${want_gid})"
    return 0
  fi

  _pfh_warn "Secrets reach: ${dir} is group ${dir_gid} mode ${dir_mode} — the container reads keys as gid ${HIVE_PFH_LAUNCH_GID}, which is host gid ${want_gid} here"
  _pfh_hint "A 0700 directory owned by you grants the container's 'dev' user nothing, not even traverse, so the key is unreadable however it is labeled."
  if [[ "$rootless" == "true" ]]; then
    _pfh_hint "Rootless, the translation needs podman (a plain chgrp ${HIVE_PFH_LAUNCH_GID} is the wrong gid, and is not permitted unless you are in that group):"
    _pfh_hint "  chmod 0750 '${dir}' && podman unshare chown -R 0:${HIVE_PFH_LAUNCH_GID} '${dir}'"
  else
    _pfh_hint "  chgrp -R ${HIVE_PFH_LAUNCH_GID} '${dir}' && chmod 0750 '${dir}'"
  fi
  _pfh_hint "Key files stay 0640; this widens nothing to the world and is the standalone analogue of fsGroup: ${HIVE_PFH_LAUNCH_GID} in src/deploy/k8s/deployment.yaml."
  return 0
}

hive_podman_check_config_secrets() {
  local secrets_dir="${HIVE_SRC_DIR}/secrets" key
  local rc=0

  _pfh_check_readable "${HIVE_SRC_DIR}/hive.yaml" "Hive config" "" "required" || rc=1
  _pfh_check_readable "${HIVE_SRC_DIR}/${HIVE_PFH_NGINX_REL}" "Gateway config" "" "required" || rc=1

  # hive.env is Quadlet-only, and deliberately NOT a row in the bind-source
  # table above: `EnvironmentFile=` is read by systemd ON THE HOST and never
  # bind-mounted, so it has no container label to judge — podman-volume-
  # persistence.md records it staying at its ordinary host label. It still earns
  # a check here, because both ways it goes wrong cost the whole start budget:
  #
  #   missing            `EnvironmentFile=` becomes `podman run --env-file`, and
  #                      `podman run` FAILS outright on a missing file.
  #   no dashboard token Hive refuses to start unless the hive is hub-hosted —
  #                      every mutation endpoint would be unauthenticated — and
  #                      Notify=healthy turns that refusal into a unit stuck in
  #                      `activating` until TimeoutStartSec expires.
  if [[ "$HIVE_PFH_LAYOUT" == "quadlet" ]]; then
    local env_file="${HIVE_SRC_DIR}/hive.env"
    if [[ ! -e "$env_file" ]]; then
      _pfh_fail "Environment file: ${env_file} does not exist"
      _pfh_hint "EnvironmentFile= becomes 'podman run --env-file', which fails on a missing file. Start from the tracked template:"
      _pfh_hint "  cp src/deploy/quadlet/hive.env.example '${env_file}' && chmod 600 '${env_file}'"
      rc=1
    else
      _pfh_check_readable "$env_file" "Environment file" "600" "required" || rc=1
      # Presence of a non-empty assignment only. The value is never read out.
      if grep -qE '^[[:space:]]*HIVE_DASHBOARD_TOKEN[[:space:]]*=[[:space:]]*[^[:space:]]' "$env_file" 2>/dev/null; then
        _pfh_pass "Dashboard token: HIVE_DASHBOARD_TOKEN is set in hive.env"
      else
        _pfh_warn "Dashboard token: HIVE_DASHBOARD_TOKEN is not set in ${env_file}"
        _pfh_hint "Hive REFUSES TO START without it unless the hive is hub-hosted, and Notify=healthy turns that into a unit stuck in 'activating' for the whole TimeoutStartSec."
        _pfh_hint "  printf 'HIVE_DASHBOARD_TOKEN=%s\\n' \"\$(openssl rand -hex 32)\" >> '${env_file}'"
        _pfh_hint "A hub-hosted hive (HIVE_SESSION_KEY / HIVE_HUB_SECRET set) does not need it — that is why this is a warning, not a failure."
      fi
    fi
  fi

  if [[ ! -d "$secrets_dir" ]]; then
    # Token-only deployments never create it; a GitHub App deployment must.
    _pfh_warn "Secrets: ${secrets_dir} does not exist"
    _pfh_hint "Only needed for GitHub App key auth. Create it so the container can traverse it, not merely narrow:"
    if [[ "$(_pfh_rootless)" == "true" ]]; then
      _pfh_hint "  mkdir -m 750 -p '${secrets_dir}' && podman unshare chown -R 0:${HIVE_PFH_LAUNCH_GID} '${secrets_dir}'"
    else
      _pfh_hint "  mkdir -m 750 -p '${secrets_dir}' && chgrp ${HIVE_PFH_LAUNCH_GID} '${secrets_dir}'"
    fi
    _pfh_hint "A 0700 directory looks safer and is not: it also excludes the container, and the failure then reads as an SELinux problem it is not."
    return "$rc"
  fi

  # 0750, not 0700: the container's launch group needs the traverse bit, and
  # 0700 excludes it (#4359). Still nothing for "other" — the host copy is what
  # an unrelated local account would read.
  _pfh_check_readable "$secrets_dir" "Secrets dir" "750" "required" || rc=1
  _pfh_check_secrets_reachable "$secrets_dir"

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

  # Resolve the layout BEFORE any check runs — every path below depends on it.
  # Announced rather than inferred silently: auto-detection that does not say
  # what it picked is indistinguishable from a check looking in the wrong place,
  # which is the bug this replaced (#4422).
  _pfh_resolve_layout
  if [[ "$HIVE_PODMAN_LAYOUT" == "auto" ]]; then
    echo "  layout: ${HIVE_PFH_LAYOUT} (detected) — config under ${HIVE_SRC_DIR}, gateway config at ${HIVE_PFH_NGINX_REL}"
  else
    echo "  layout: ${HIVE_PFH_LAYOUT} (HIVE_PODMAN_LAYOUT) — config under ${HIVE_SRC_DIR}, gateway config at ${HIVE_PFH_NGINX_REL}"
  fi

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
  HIVE_SRC_DIR                    directory holding the deployment's bind sources
  HIVE_PODMAN_LAYOUT              auto (default) | source | quadlet — which shape
                                  HIVE_SRC_DIR has. A source tree (Compose) keeps
                                  the gateway config at deploy/nginx.conf; a
                                  Quadlet install keeps it flat as nginx.conf in
                                  %E/hive, alongside hive.env. auto decides from
                                  what is on disk and prints which it picked.
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
