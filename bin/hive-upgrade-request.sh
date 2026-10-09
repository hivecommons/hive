#!/usr/bin/env bash
# The host-side half of the dashboard upgrade bridge (#10344, option A).
#
# WHY THIS EXISTS. A Hive under Quadlet runs in its own mount, PID and user
# namespace. The dashboard upgrade button used to exec a host-side helper from
# INSIDE that namespace (src/pkg/dashboard/deployment_upgrade.go), which can
# never work on a rootless Podman/Quadlet host: the helper is not in the image,
# and the lifecycle it must drive — systemctl, podman, the Quadlet drop-ins
# under %E/hive — is not reachable from a container that deliberately mounts no
# podman, docker or systemd socket. The button was explained by #10346, not
# fixed.
#
# The container therefore does the only thing it safely can across that
# boundary: it writes a REQUEST FILE into a bind-mounted directory. A host-side
# hive-upgrade.path unit notices and starts hive-upgrade.service, which runs
# this script OUTSIDE every container namespace.
#
# THE TRUST BOUNDARY IS THIS FILE. Everything in a request was written by the
# container and is therefore untrusted input, no matter how privileged the Hive
# process inside that container is. So:
#
#   * the image reference is matched against the SAME closed allow-list
#     bin/hive-dashboard-upgrade-helper.sh uses — ghcr.io/hivecommons/hive by
#     tag or by sha256 digest, and nothing else;
#   * every value is passed to bin/hive-podman-update.sh as ARGV, never
#     interpolated into shell text, and this script never eval/source/`$(...)`s
#     anything it read;
#   * the root mode is taken from the HOST (this script's own manager), never
#     from the request: a container must not be able to ask for the system
#     manager;
#   * a request older than the staleness budget is archived unapplied, so a
#     request file left behind by a host that was down for a week cannot
#     silently downgrade a deployment when the host returns;
#   * a handled request is MOVED out of the watched directory before its result
#     is recorded, so neither success nor failure can be replayed by the path
#     unit re-triggering;
#   * the request directory and its done/ and failed/ archives must be real
#     directories, and a result is only ever written to a path that does not
#     exist yet. The container can write anything into the bind mount,
#     including symlinks; following one would let it steer this script's
#     writes — which run as the HOST user, root on a rootful install — onto
#     any host path it names.
#
# REQUEST FORMAT. One JSON object per file, written atomically (temp name in
# the same directory, then rename(2)) so the path unit can never observe a
# partial file:
#
#     {"target_ref":"ghcr.io/hivecommons/hive:a1b2c3d",
#      "requester":"owner-login",
#      "requested_at":"2026-10-03T11:04:05Z"}
#
# `target_ref` is required; `requester` and `requested_at` are advisory and are
# only recorded. The legacy spellings `ref` and `requestedAt` are accepted as
# fallbacks. Unknown fields are ignored. The full contract, including the
# dashboard side, is src/docs/dashboard-standalone-upgrades.md.
#
# COMMANDS
#
#   apply     drain the request directory oldest-first, applying each request.
#             This is what hive-upgrade.service runs.
#   status    what is pending, and what the last handled request did.
#             Read-only: applies nothing, starts nothing.
#
# Rootless by default; --rootful drives the system manager, and under systemd
# the mode is derived from the manager that started this unit rather than from
# a flag. Same shape as bin/hive-podman-update.sh and bin/hive-podman-setup.sh.
#
# Run: bin/hive-upgrade-request.sh <apply|status> [--rootful|--rootless]
# Exit codes: 0 success (including "nothing to do"),
#             78 a request was rejected or its upgrade did not end healthy,
#             64 unusable invocation

set -uo pipefail

EX_USAGE=64
EX_CONFIG=78

# A request older than this is archived unapplied. Long enough that a host
# rebooting mid-request still honours it, short enough that a week-old ref
# cannot resurface as an "upgrade" that is really a downgrade.
MAX_REQUEST_AGE_SECONDS="${HIVE_UPGRADE_REQUEST_MAX_AGE:-3600}"

# Kept in step with HISTORY_KEEP in bin/hive-podman-update.sh: enough handled
# requests to explain what happened, not an unbounded log.
ARCHIVE_KEEP="${HIVE_UPGRADE_REQUEST_ARCHIVE_KEEP:-20}"

CMD=""
ROOTFUL=-1

c_reset=""; c_bold=""; c_red=""; c_green=""; c_yellow=""
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  c_reset=$'\033[0m'; c_bold=$'\033[1m'; c_red=$'\033[31m'
  c_green=$'\033[32m'; c_yellow=$'\033[33m'
fi

say()   { printf '%s\n' "$*"; }
head1() { printf '\n%s%s%s\n' "$c_bold" "$*" "$c_reset"; }
ok()    { printf '  %sPASS%s  %s\n' "$c_green" "$c_reset" "$*"; }
warn()  { printf '  %sWARN%s  %s\n' "$c_yellow" "$c_reset" "$*"; }
bad()   { printf '  %sFAIL%s  %s\n' "$c_red" "$c_reset" "$*"; }
info()  { printf '        %s\n' "$*"; }

usage() {
  sed -n '2,/^set -uo/p' "$0" | sed 's/^# \{0,1\}//; $d'
  exit "$EX_USAGE"
}

while [ $# -gt 0 ]; do
  case "$1" in
    apply|status)
      [ -z "$CMD" ] || { printf 'two commands given: %s and %s\n' "$CMD" "$1" >&2; usage; }
      CMD="$1" ;;
    --rootful)  ROOTFUL=1 ;;
    --rootless) ROOTFUL=0 ;;
    -h|--help)  usage ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; usage ;;
  esac
  shift
done
[ -n "$CMD" ] || usage

# --- root mode, taken from the host and never from a request ----------------
#
# $MANAGERPID is set for a process spawned by a systemd USER instance and never
# by the system instance — the same discriminator src/deploy/systemd/
# hive-boot-gate.service uses. Under hive-upgrade.service that makes the mode
# an unforgeable property of which manager owns the deployment. An explicit
# flag still wins, for running this by hand.
if [ "$ROOTFUL" -eq -1 ]; then
  if [ -n "${MANAGERPID:-}" ]; then ROOTFUL=0; elif [ "$(id -u)" = "0" ]; then ROOTFUL=1; else ROOTFUL=0; fi
fi

if [ "$ROOTFUL" -eq 1 ]; then
  MODE_LABEL="rootful (system manager)"
  MODE_FLAG="--rootful"
  CONF_DIR="${HIVE_UPGRADE_CONF_DIR:-/etc/hive}"
else
  MODE_LABEL="rootless (user manager, uid $(id -u))"
  MODE_FLAG="--rootless"
  CONF_DIR="${HIVE_UPGRADE_CONF_DIR:-$HOME/.config/hive}"
fi

# Overridable for the contract test, which drives this against a fixture
# directory. Same convention as HIVE_UPDATE_QUADLET_DIR in
# bin/hive-podman-update.sh.
REQUEST_DIR="${HIVE_UPGRADE_REQUEST_DIR:-${CONF_DIR}/upgrade-requests}"
DONE_DIR="${REQUEST_DIR}/done"
FAILED_DIR="${REQUEST_DIR}/failed"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UPDATE_SCRIPT="${HIVE_PODMAN_UPDATE_SCRIPT:-${ROOT}/bin/hive-podman-update.sh}"

# The one allow-list, character for character the one in
# bin/hive-dashboard-upgrade-helper.sh. A ref that does not match is never
# passed to podman, and never reaches a shell.
REF_RE='^ghcr\.io/hivecommons/hive(:[A-Za-z0-9._-]{1,128}|@sha256:[0-9a-f]{64})$'

# --- reading a request ------------------------------------------------------
#
# The request is read ONCE into REQUEST_BODY, and only after the same checks
# the drain loop relies on: a symlink or anything but a regular file is never
# read. A read error is reported as such instead of surfacing later as an
# empty `<missing>` ref. On a rootless host the container uid maps to a host
# subuid, so a request the drain user cannot read is retried through
# `podman unshare`, whose userns root can read what the container wrote.
read_request() {
  local file="$1" owner reason
  REQUEST_BODY=""
  if [ -L "$file" ] || [ ! -f "$file" ]; then
    bad "request is not a regular file; refusing to read it"
    return 1
  fi
  if [ -r "$file" ] && REQUEST_BODY="$(cat -- "$file" 2>/dev/null)"; then
    return 0
  fi
  if [ "$ROOTFUL" -eq 0 ] && command -v podman >/dev/null 2>&1 \
     && REQUEST_BODY="$(podman unshare cat -- "$file" 2>/dev/null)"; then
    info "read through podman unshare (the request is owned by a container subuid)"
    return 0
  fi
  REQUEST_BODY=""
  owner="$(stat -c %u -- "$file" 2>/dev/null)" || owner="?"
  reason="read error"
  [ -r "$file" ] || reason="permission denied"
  bad "cannot read request: ${reason} (owner uid ${owner}, drain uid $(id -u))"
  info "The dashboard writes requests mode 0644; a 0600 request from an older"
  info "image is unreadable to the host user on a rootless install."
  return 1
}

# Deliberately NOT `jq -r` piped into eval, and deliberately not sourced: the
# request is attacker-shaped input. This extracts one string field from
# REQUEST_BODY with a single sed and takes the FIRST match, so a duplicated key
# cannot smuggle a second value past the validation of the first.
request_field() {
  printf '%s\n' "$REQUEST_BODY" \
    | sed -n 's/.*"'"$1"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1
}

# The advisory fields are only ever echoed, but they are echoed into the
# journal: strip control bytes (ESC included) so a request cannot smuggle
# terminal escapes or fake lines into `systemctl status` / `journalctl`, and
# bound the length so one request cannot flood the log.
printable() {
  printf '%s' "$1" | tr -d '\000-\037\177' | cut -c1-200
}

# Seconds since the file was last modified. mtime rather than the request's own
# `requested_at`, because the timestamp in the file is written by the container
# and the staleness guard must not be something the container can set.
request_age_seconds() {
  local mtime now
  mtime="$(stat -c %Y "$1" 2>/dev/null)" || return 1
  now="$(date +%s)"
  printf '%s\n' "$(( now - mtime ))"
}

# A directory under the bind mount is only used if it is a real directory,
# not a symlink the container planted. Symlinks are rejected with -L BEFORE
# -d, since -d follows them. Creation is `mkdir` without -p: -p treats a
# symlink to an existing directory as success. Archives are created 0700: the
# requests they keep are 0644 and carry the requester login.
real_dir() {
  local dir="$1"
  [ -L "$dir" ] && return 1
  [ -d "$dir" ] && return 0
  mkdir -m 0700 "$dir" 2>/dev/null || return 1
  [ ! -L "$dir" ] && [ -d "$dir" ]
}

# Create the result file with O_EXCL (noclobber): the open fails if ANYTHING
# already sits at that path, a pre-planted symlink included, instead of
# following it and truncating whatever host file it points at.
write_result() {
  local path="$1" result="$2"
  [ -e "$path" ] || [ -L "$path" ] && return 1
  ( set -o noclobber; printf '%s\n' "$result" > "$path" ) 2>/dev/null
}

# Archive, then record. In this order: the file leaves the watched directory
# before any result is written, so hive-upgrade.path cannot re-trigger on a
# request that has already been handled.
archive_request() {
  local file="$1" dest_dir="$2" result="$3" stamp base dest
  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  base="$(basename "$file")"
  if ! real_dir "$dest_dir"; then
    bad "archive directory is not a real directory: ${dest_dir}; deleting the request instead"
    rm -f "$file" 2>/dev/null
    return 1
  fi
  dest="${dest_dir}/${stamp}-${base}"
  if [ -e "$dest" ] || [ -L "$dest" ]; then
    # Same second, same name: a collision is more likely a planted entry than
    # a double-click, and either way nothing of ours is ever written over it.
    dest="${dest_dir}/${stamp}-$$-${base}"
  fi
  if [ ! -e "$dest" ] && [ ! -L "$dest" ] && mv -f "$file" "$dest" 2>/dev/null; then
    write_result "${dest}.result" "$result" \
      || warn "could not record the result for $(basename "$dest"); something already occupied ${dest}.result"
  else
    # If it cannot be moved it must at least not be replayed.
    rm -f "$file" 2>/dev/null
  fi
  prune_archive "$dest_dir"
}

prune_archive() {
  local dir="$1" entry count=0
  [ -d "$dir" ] || return 0
  # Newest first; everything past the keep budget goes, result file included.
  while IFS= read -r entry; do
    count=$(( count + 1 ))
    [ "$count" -gt "$ARCHIVE_KEEP" ] && rm -f "$entry" "${entry}.result" 2>/dev/null
  done < <(find "$dir" -maxdepth 1 -type f -name '*.json' -printf '%T@ %p\n' 2>/dev/null \
             | sort -rn | cut -d' ' -f2-)
}

apply_one() {
  local file="$1" ref requester requested_at age
  say ""
  say "  request: $(basename "$file")"

  age="$(request_age_seconds "$file")" || age=""
  if [ -z "$age" ]; then
    bad "cannot stat the request; archiving it unapplied"
    archive_request "$file" "$FAILED_DIR" "rejected: unreadable request"
    return 1
  fi
  if [ "$age" -gt "$MAX_REQUEST_AGE_SECONDS" ]; then
    bad "request is ${age}s old (budget ${MAX_REQUEST_AGE_SECONDS}s); archiving it UNAPPLIED"
    info "A stale request is a ref someone chose under conditions that no longer hold."
    info "Click Upgrade again if it is still what you want."
    archive_request "$file" "$FAILED_DIR" "rejected: stale (${age}s)"
    return 1
  fi

  if ! read_request "$file"; then
    archive_request "$file" "$FAILED_DIR" "rejected: unreadable request"
    return 1
  fi

  # The dashboard writes target_ref/requested_at (src/pkg/dashboard); the
  # camelCase and bare spellings are accepted for hand-written requests.
  ref="$(request_field target_ref)"
  [ -n "$ref" ] || ref="$(request_field ref)"
  requester="$(printable "$(request_field requester)")"
  requested_at="$(printable "$(request_field requested_at)")"
  [ -n "$requested_at" ] || requested_at="$(printable "$(request_field requestedAt)")"
  [ -n "$requester" ] || requester="unknown"
  [ -n "$requested_at" ] || requested_at="unknown"

  if ! [[ "$ref" =~ $REF_RE ]]; then
    bad "ref is not an allowed Hive image reference; refusing to run podman"
    info "got: $(printable "${ref:-<missing>}")"
    info "allowed: ghcr.io/hivecommons/hive:<tag> or ghcr.io/hivecommons/hive@sha256:<64 hex>"
    archive_request "$file" "$FAILED_DIR" "rejected: disallowed ref"
    return 1
  fi

  ok "ref ${ref} (requested by ${requester} at ${requested_at})"
  if [ ! -x "$UPDATE_SCRIPT" ]; then
    bad "podman update script is not executable: ${UPDATE_SCRIPT}"
    archive_request "$file" "$FAILED_DIR" "failed: missing ${UPDATE_SCRIPT}"
    return 1
  fi

  # ARGV, not a command line. $ref has already been proved to match the
  # allow-list, and it is still passed as a separate word rather than
  # interpolated — the validation is the first guard, not the only one.
  #
  # `upgrade` is what bin/hive-dashboard-upgrade-helper.sh now delegates to as
  # well (#10344 gap 3): it is `pin` when the host is not on registry
  # tracking, unchanged, and `podman auto-update` semantics instead of a
  # digest pin when it is — so a request routed through the bridge gets the
  # same posture-preserving behaviour as one run by hand.
  say ""
  if "$UPDATE_SCRIPT" upgrade "$ref" "$MODE_FLAG"; then
    ok "upgrade to ${ref} completed and ended healthy"
    archive_request "$file" "$DONE_DIR" "ok: upgraded to ${ref}"
    return 0
  fi
  bad "upgrade to ${ref} did not end healthy; see the output above"
  info "The deployment's own rollback is ${UPDATE_SCRIPT} rollback ${MODE_FLAG}"
  archive_request "$file" "$FAILED_DIR" "failed: ${ref}"
  return 1
}

cmd_apply() {
  head1 "Hive upgrade requests — ${MODE_LABEL}"
  say "  directory: ${REQUEST_DIR}"

  if [ ! -d "$REQUEST_DIR" ]; then
    say ""
    info "no request directory; nothing to do"
    return 0
  fi
  if [ -L "$REQUEST_DIR" ]; then
    say ""
    bad "request directory is a symlink; refusing to drain it"
    info "bin/hive-podman-setup.sh creates ${REQUEST_DIR} as a real directory; replace the link and retry."
    return "$EX_CONFIG"
  fi

  local rc=0 handled=0 file
  # Oldest first: requests are applied in the order they were made, so a
  # double-click lands on the newer ref last rather than first.
  while IFS= read -r file; do
    [ -n "$file" ] || continue
    handled=$(( handled + 1 ))
    apply_one "$file" || rc="$EX_CONFIG"
  done < <(find "$REQUEST_DIR" -maxdepth 1 -type f -name '*.json' -printf '%T@ %p\n' 2>/dev/null \
             | sort -n | cut -d' ' -f2-)

  say ""
  if [ "$handled" -eq 0 ]; then
    info "no pending requests"
  else
    info "handled ${handled} request(s)"
  fi
  return "$rc"
}

cmd_status() {
  head1 "Hive upgrade requests — ${MODE_LABEL}"
  say "  directory: ${REQUEST_DIR}"
  say ""

  if [ ! -d "$REQUEST_DIR" ]; then
    warn "the request directory does not exist"
    info "The dashboard Upgrade button stays disabled until bin/hive-podman-setup.sh"
    info "creates it and installs hive-upgrade.path (src/docs/dashboard-standalone-upgrades.md)."
    return 0
  fi

  local pending
  pending="$(find "$REQUEST_DIR" -maxdepth 1 -type f -name '*.json' 2>/dev/null | wc -l)"
  say "  pending: ${pending}"

  local dir entry
  for dir in "$DONE_DIR" "$FAILED_DIR"; do
    [ -d "$dir" ] || continue
    say ""
    say "  $(basename "$dir")/ (newest first)"
    while IFS= read -r entry; do
      [ -n "$entry" ] || continue
      printf '        %s  %s\n' "$(basename "$entry")" "$(cat "${entry}.result" 2>/dev/null)"
    done < <(find "$dir" -maxdepth 1 -type f -name '*.json' -printf '%T@ %p\n' 2>/dev/null \
               | sort -rn | cut -d' ' -f2- | head -n5)
  done
  return 0
}

case "$CMD" in
  apply)  cmd_apply ;;
  status) cmd_status ;;
  *)      usage ;;
esac
