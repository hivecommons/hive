#!/usr/bin/env bash
# Deliberate manual update and rollback for the Hive Quadlet unit (#4378).
#
# ADR-0017 chose Quadlet because `Notify=healthy` means a unit that reports
# started is actually serving. That property only pays off if there is a
# defined way to move to a new image AND to get back when the new one is bad.
# #4354 shipped the unit and #4377 exercised its lifecycle; neither said how to
# change the image it runs. This is that, and it is MANUAL on purpose --
# health-aware auto-update is a separate later slice, and nothing here polls a
# registry or acts on its own.
#
# WHAT IT MANAGES, and why it is not an edit to hive.container.
#
# The shipped unit names a floating tag, `ghcr.io/hivecommons/hive:stable`,
# because src/deploy/standalone-images.sh is the one image-reference source of
# truth and src/deploy/test_standalone_image_refs.sh fails the build when an
# asset stops agreeing with it. A floating tag cannot be rolled back to: today's
# `:stable` and last week's `:stable` are different images with the same name.
#
# So the pin lives in a Quadlet DROP-IN instead:
#
#     <quadlet dir>/hive.container.d/10-image.conf
#
# Quadlet merges `foo.container.d/*.conf` into `foo.container` at
# `daemon-reload`, exactly as systemd does for .service drop-ins, so this file
# replaces Image= without touching the repo's unit. Update and rollback are
# then the same two-line operation on one file, and the file records the pin
# history so `rollback` knows where back is.
#
# COMMANDS
#
#   status          what is pinned, what is RUNNING, and what rollback would do.
#                   Read-only: starts nothing, stops nothing, pulls nothing.
#   resolve <ref>   print the digest <ref> resolves to right now. Read-only.
#   pin <ref>       pin to <ref> BY DIGEST and restart hive.service. <ref> may
#                   be a tag (resolved to a digest first) or a digest.
#                   THIS RESTARTS HIVE: ~11s of downtime measured, and rather
#                   more if the new image never becomes healthy.
#   rollback        return to the newest pin below the current one that this
#                   script watched become HEALTHY, and restart. Skipping the
#                   failed entries is the point -- rollback is for use after a
#                   bad update, when the top of the history is the bad pin.
#   unpin           remove the drop-in, returning the unit to the floating tag
#                   in hive.container, and restart.
#   autoupdate <on|off|status>
#                   OPT-IN health-aware auto-update (#4411). `on` installs
#                   20-autoupdate.conf (AutoUpdate=registry) beside the pin
#                   drop-in, restarts so the container carries the label, and
#                   enables podman-auto-update.timer. `off` reverses it.
#                   `status` is read-only.
#                   REFUSES to turn on while a digest pin is in place: measured,
#                   auto-update then reports UPDATED=false and does nothing,
#                   which is indistinguishable from "up to date". See
#                   src/docs/podman-auto-update.md.
#
# Rootless by default; --rootful drives the system manager through sudo.
#
# Procedure, with the measured update and rollback runs:
# src/docs/podman-quadlet-update-rollback.md
#
# Run: bin/hive-podman-update.sh <command> [args] [--rootful]
# Exit codes: 0 success, 78 the operation did not end healthy (EX_CONFIG),
#             64 unusable invocation (EX_USAGE)

set -uo pipefail

EX_USAGE=64
EX_CONFIG=78

UNIT="hive.service"
# The other half of the deployment (#4493). hive-gateway.service has
# Requires=hive.service, so a failed update takes it down WITH the failing
# unit -- and a later `systemctl restart hive.service` does not bring it back,
# because a restart transaction only cycles dependents that are ACTIVE when
# the job runs. Every path here that restarts hive must therefore treat the
# gateway as part of the deployment, not as something systemd will handle.
GATEWAY_UNIT="hive-gateway.service"
# Published-port fallback when the installed hive-gateway.container cannot be
# read. 3001 is what the shipped unit publishes.
GATEWAY_PORT_DEFAULT=3001
# End-to-end confirmation budget, sized like bin/hive-podman-setup.sh's.
GATEWAY_HEALTH_RETRIES="${HIVE_UPDATE_GATEWAY_RETRIES:-30}"
GATEWAY_HEALTH_DELAY="${HIVE_UPDATE_GATEWAY_DELAY:-2}"
CONTAINER="hive"
DROPIN_NAME="10-image.conf"
# The opt-in auto-update drop-in (#4411). A SEPARATE file from the pin so the
# two can be reasoned about, and turned on and off, independently: `unpin`
# must not silently disable auto-update, and `autoupdate off` must not drop a
# pin someone is relying on.
AUTOUPDATE_NAME="20-autoupdate.conf"
AUTOUPDATE_TIMER="podman-auto-update.timer"
# The repo's unit names this. `unpin` returns to it, and `status` says so.
BASE_IMAGE_REPO="ghcr.io/hivecommons/hive"
HISTORY_KEEP=10

ROOTFUL=0
CMD=""
REF=""

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
    status|resolve|pin|rollback|unpin|autoupdate)
      # `autoupdate status` is a command plus an ACTION, and the action happens
      # to be spelled the same as another command. Only the argument-taking
      # commands may absorb a second one of these words; `status rollback` is
      # still two commands and still an error.
      if [ -z "$CMD" ]; then
        CMD="$1"
      elif [ -z "$REF" ] && case "$CMD" in autoupdate|pin|resolve) true ;; *) false ;; esac; then
        REF="$1"
      else
        printf 'two commands given: %s and %s\n' "$CMD" "$1" >&2; usage
      fi ;;
    --rootful)  ROOTFUL=1 ;;
    --rootless) ROOTFUL=0 ;;
    -h|--help)  usage ;;
    -*) printf 'unknown option: %s\n' "$1" >&2; usage ;;
    *)
      [ -z "$CMD" ] && { printf 'unknown command: %s\n' "$1" >&2; usage; }
      [ -z "$REF" ] && REF="$1" || { printf 'unexpected argument: %s\n' "$1" >&2; usage; } ;;
  esac
  shift
done
[ -n "$CMD" ] || usage

# Every systemd, podman, and file write goes through these, so the rootful and
# rootless paths are one code path with a different prefix rather than two
# transcriptions that drift.
if [ "$ROOTFUL" -eq 1 ]; then
  MODE_LABEL="rootful (system manager)"
  MODE_FLAG=" --rootful"
  SCTL_LABEL="sudo systemctl"
  QUADLET_DIR="${HIVE_UPDATE_QUADLET_DIR:-/etc/containers/systemd}"
  sctl() { sudo systemctl "$@"; }
  pod()  { sudo podman "$@"; }
  as_owner() { sudo "$@"; }
else
  MODE_LABEL="rootless (user manager, uid $(id -u))"
  MODE_FLAG=""
  SCTL_LABEL="systemctl --user"
  QUADLET_DIR="${HIVE_UPDATE_QUADLET_DIR:-$HOME/.config/containers/systemd}"
  sctl() { systemctl --user "$@"; }
  pod()  { podman "$@"; }
  as_owner() { "$@"; }
fi

DROPIN_DIR="${QUADLET_DIR}/hive.container.d"
DROPIN="${DROPIN_DIR}/${DROPIN_NAME}"
AUTOUPDATE_DROPIN="${DROPIN_DIR}/${AUTOUPDATE_NAME}"
# The tracked source of the opt-in file. Copied rather than generated so the
# rationale in its header travels to the host with it.
AUTOUPDATE_SRC="${HIVE_UPDATE_AUTOUPDATE_SRC:-$(cd "$(dirname "$0")/.." 2>/dev/null && pwd)/src/deploy/quadlet/optional/hive-autoupdate.conf}"

show()  { sctl show "$UNIT" -p "$1" --value 2>/dev/null; }
state() { printf '%s/%s/%s' "$(show ActiveState)" "$(show SubState)" "$(show Result)"; }
now()   { date -u +%Y-%m-%dT%H:%M:%SZ; }

require_unit() {
  if ! sctl cat "$UNIT" >/dev/null 2>&1; then
    bad "$UNIT is not known to this manager -- install the units and run daemon-reload"
    info "see src/docs/podman-standalone-quadlet.md"
    exit "$EX_CONFIG"
  fi
}

# --- what the unit and the container actually name --------------------------

# The digest the GENERATED unit would run on its next start. This is the merged
# result of hive.container plus the drop-in, which is the only place the two
# are ever combined -- `systemctl cat` prints that result and never names the
# drop-in file that produced it.
unit_image() {
  sctl cat "$UNIT" 2>/dev/null \
    | grep -m1 '^ExecStart=' \
    | grep -oE '[A-Za-z0-9._/-]+(:[A-Za-z0-9._-]+)?(@sha256:[0-9a-f]{64})?$'
}

# The image the RUNNING container was created from. It does not follow the unit
# until a restart, and the gap between the two is the whole reason `status`
# prints both.
running_image() {
  pod inspect "$CONTAINER" --format '{{.ImageName}}' 2>/dev/null
}

running_version() {
  pod exec "$CONTAINER" hive --version 2>/dev/null | tail -n 1
}

# --- digest resolution ------------------------------------------------------

# TAG -> DIGEST, and the obvious way to do it is wrong.
#
#   podman image inspect <tag> --format '{{index .RepoDigests 0}}'
#
# returns the digest of the per-ARCHITECTURE manifest, not of the manifest list
# the tag points at. Measured: `ghcr.io/hivecommons/hive:stable` reported
# sha256:5d3f442f... that way and sha256:ec8e69bc... from the registry, and
# only the second one is a pin that still resolves on arm64. So resolve from
# the registry with skopeo when it is there, and otherwise from the DIGEST
# column of `podman images --digests`, which is the tag's own digest.
resolve_digest() {
  local ref="$1" repo digest
  case "$ref" in
    *@sha256:*) printf '%s\n' "${ref#*@}"; return 0 ;;
  esac
  # HIVE_UPDATE_SKOPEO is a test seam: it lets the suite force the podman
  # fallback below on a host that has skopeo installed.
  local skopeo_bin="${HIVE_UPDATE_SKOPEO:-skopeo}"
  if command -v "$skopeo_bin" >/dev/null 2>&1; then
    digest="$("$skopeo_bin" inspect --format '{{.Digest}}' "docker://${ref}" 2>/dev/null)"
    if [ -n "$digest" ]; then printf '%s\n' "$digest"; return 0; fi
  fi
  pod pull -q "$ref" >/dev/null 2>&1 || return 1
  repo="$ref"
  digest="$(pod images --digests --format '{{.Repository}}:{{.Tag}} {{.Digest}}' 2>/dev/null \
            | awk -v r="$repo" '$1 == r { print $2; exit }')"
  [ -n "$digest" ] || return 1
  printf '%s\n' "$digest"
}

# repo/name out of a reference, dropping any @digest and any :tag. The tag
# test is deliberately not `${ref%:*}`: a registry may carry a port, and
# `localhost:5000/hive` would lose everything after `localhost` that way.
image_repo() {
  local ref="$1" tail
  ref="${ref%@*}"
  tail="${ref##*:}"
  case "$tail" in
    "$ref") ;;                       # no colon at all
    */*)    ;;                       # the colon was a registry port
    *)      ref="${ref%:*}" ;;
  esac
  printf '%s\n' "$ref"
}

# --- the drop-in, and the pin history inside it -----------------------------
#
# History lines are comments, so Quadlet ignores them and one file holds both
# the current pin and everything `rollback` needs. Fields:
#
#   # HIVE-PIN <outcome> <utc timestamp> <digest> <source ref>
#
# newest first, so line 1 describes the Image= below it. <outcome> is `healthy`
# once this script has watched the unit reach active on that digest, `failed`
# when it did not, and `pending` while a pin is in flight or was interrupted.
# `rollback` returns to the newest `healthy` entry that is not the current one,
# which is what makes it usable from a FAILED update: the entry at the top is
# then the pin that broke, and skipping it is the whole job.

# An entry is `# HIVE-PIN <outcome> <ISO timestamp> <digest> <ref>` and the
# timestamp is part of the match on purpose: without it the prose in the header
# above, which has to mention the marker to explain it, parses as the newest
# entry and `rollback` reads a sentence as a digest.
HIVE_PIN_RE='^# HIVE-PIN [a-z]+ [0-9]{4}-[0-9]{2}-[0-9]{2}T'

history_lines() {
  [ -f "$DROPIN" ] || return 0
  grep -E "$HIVE_PIN_RE" "$DROPIN" 2>/dev/null | sed 's/^# HIVE-PIN //'
}

current_digest() {
  [ -f "$DROPIN" ] || return 0
  grep -m1 '^Image=' "$DROPIN" 2>/dev/null | sed 's/^Image=//' | sed 's/.*@//'
}

current_ref() {
  [ -f "$DROPIN" ] || return 0
  grep -m1 '^Image=' "$DROPIN" 2>/dev/null | sed 's/^Image=//'
}

# newest healthy entry that is not the digest currently pinned
rollback_target() {
  local cur; cur="$(current_digest)"
  history_lines | awk -v cur="$cur" '$1 == "healthy" && $3 != cur { print $3, $4; exit }'
}

write_dropin() {
  local image="$1"; shift
  local tmp; tmp="$(mktemp)"
  {
    cat <<'HEADER'
# The digest pin for hive.service. Written by bin/hive-podman-update.sh (#4378).
#
# Quadlet merges hive.container.d/*.conf into hive.container on every
# daemon-reload, so the Image= below replaces the floating `:stable` tag in the
# shipped unit without editing a file the repo owns -- src/deploy/quadlet is
# checked against src/deploy/standalone-images.sh and must keep naming the tag.
#
# `systemctl cat hive.service` shows the MERGED result and never names this
# file, so this is the only place the pin and its history are written down.
# Changing Image= here takes effect on `daemon-reload` + `systemctl restart
# hive.service`, and not before: a reload alone leaves the running container on
# the old digest.
#
# The pin history is the HIVE-PIN comment lines below, newest first; the top
# one describes the Image= at the bottom. `rollback` returns to the newest
# `healthy` entry, so do not hand-edit them -- use the script.
HEADER
    printf '#\n'
    local n=0 line
    while IFS= read -r line; do
      [ -n "$line" ] || continue
      n=$((n + 1)); [ "$n" -le "$HISTORY_KEEP" ] || break
      printf '# HIVE-PIN %s\n' "$line"
    done
    printf '\n[Container]\nImage=%s\n' "$image"
  } >"$tmp"
  as_owner install -Dm644 "$tmp" "$DROPIN"
  rm -f "$tmp"
}

# Replaces the outcome word of the newest history entry in place.
mark_top_outcome() {
  local outcome="$1" tmp
  [ -f "$DROPIN" ] || return 0
  tmp="$(mktemp)"
  as_owner cat "$DROPIN" | awk -v o="$outcome" '
    /^# HIVE-PIN [a-z]+ [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T/ && !done {
      sub(/^# HIVE-PIN [a-z]+/, "# HIVE-PIN " o); done = 1
    }
    { print }
  ' >"$tmp"
  as_owner install -Dm644 "$tmp" "$DROPIN"
  rm -f "$tmp"
}

# --- restart, and reporting what it did -------------------------------------

# `systemctl restart` on this unit returns only once the healthcheck has passed,
# because Notify=healthy holds it in `activating` until then. A restart that
# returns 0 therefore means Hive is SERVING the new digest, and a restart that
# fails has already spent TimeoutStartSec finding that out -- 300s in the
# shipped unit. Both are reported as measured seconds rather than as "done".
restart_and_report() {
  local t0 t1 rc elapsed
  t0=$(date +%s%N)
  sctl restart "$UNIT"; rc=$?
  t1=$(date +%s%N)
  elapsed=$(( (t1 - t0) / 1000000000 ))
  if [ "$rc" -eq 0 ] && [ "$(show ActiveState)" = "active" ]; then
    ok "restart returned $rc after ${elapsed}s, state $(state)"
    info "with Notify=healthy that return means both in-container listeners answered (#4476),"
    info "which says nothing yet about the PUBLISHED port -- the gateway check below owns that"
    info "running  $(running_image)"
    local v; v="$(running_version)"
    [ -n "$v" ] && info "version  $v"
    return 0
  fi
  bad "restart returned $rc after ${elapsed}s, state $(state)"
  local run; run="$(running_image)"
  info "running  ${run:-<no container>}"
  return 1
}

# --- the gateway, which a hive.service restart does NOT take care of --------

# The published port, read from the INSTALLED Quadlet unit so the check probes
# what this host actually publishes rather than what the checkout says.
gateway_port() {
  local unit_file="${QUADLET_DIR}/hive-gateway.container" port=""
  if [ -f "$unit_file" ]; then
    port="$(sed -n 's|^PublishPort=\([0-9]\{1,5\}\):.*|\1|p' "$unit_file" | head -n1)"
  fi
  printf '%s\n' "${port:-$GATEWAY_PORT_DEFAULT}"
}

# Start the gateway and confirm the deployment answers END TO END before any
# caller claims "serving" (#4493). Two facts make this necessary:
#
#   1. After a failed update, Requires=hive.service has already STOPPED the
#      gateway, and `systemctl restart hive.service` re-starts only dependents
#      that are active when the job runs -- an inactive gateway is not in the
#      transaction, so nothing ever starts it again. Measured: rollback left
#      `hive active / gateway inactive / dashboard :3001 DEAD` while reporting
#      success. `start` is idempotent -- a no-op on a healthy stack, the fix
#      on a stranded one -- so this needs no conditional logic.
#   2. Notify=healthy on hive.service proves Hive's own listeners answered
#      INSIDE the container (#4476 probes both 3002 and 3001 there). It says
#      nothing about the published port an operator actually uses. The
#      installer already refuses to claim healthy without curling the gateway
#      from the host; this is the same assertion, for the same reason.
ensure_gateway_serving() {
  local port; port="$(gateway_port)"
  if ! sctl cat "$GATEWAY_UNIT" >/dev/null 2>&1; then
    warn "$GATEWAY_UNIT is not known to this manager -- no end-to-end check can run"
    info "Hive itself is serving, but nothing here publishes port ${port} for an operator"
    return 0
  fi
  if ! sctl start "$GATEWAY_UNIT" >/dev/null 2>&1; then
    bad "$GATEWAY_UNIT did not start -- Hive is serving, but nothing an operator can reach is"
    info "journalctl -u $GATEWAY_UNIT has the reason (add --user for a rootless install)"
    return 1
  fi
  ok "started $GATEWAY_UNIT (a no-op when it was already running)"
  if ! command -v curl >/dev/null 2>&1; then
    bad "curl is not installed, so the end-to-end check cannot run -- this script will not claim serving on evidence it does not have"
    return 1
  fi
  info "waiting for the gateway on http://127.0.0.1:${port}/api/health"
  local attempt=0
  while [ "$attempt" -lt "$GATEWAY_HEALTH_RETRIES" ]; do
    if curl -sf "http://127.0.0.1:${port}/api/health" >/dev/null 2>&1; then
      ok "gateway answered on ${port} -- healthy end to end"
      return 0
    fi
    attempt=$((attempt + 1))
    sleep "$GATEWAY_HEALTH_DELAY"
  done
  bad "the gateway did not answer on ${port} within $((GATEWAY_HEALTH_RETRIES * GATEWAY_HEALTH_DELAY))s"
  info "Hive itself is healthy, so the gap is in front of it -- check $GATEWAY_UNIT"
  return 1
}

# --- commands ---------------------------------------------------------------

do_status() {
  require_unit
  head1 "Image pin -- $MODE_LABEL"
  local cur run
  cur="$(current_ref)"; run="$(running_image)"
  if [ -f "$DROPIN" ] && [ -n "$cur" ]; then
    ok "pinned by drop-in: $cur"
    info "drop-in  $DROPIN"
  else
    warn "no drop-in: the unit runs the floating tag in hive.container"
    info "a floating tag cannot be rolled back to -- today's and last week's are different images"
    info "pin it: $0 pin ${BASE_IMAGE_REPO}:stable"
  fi
  info "unit ExecStart names   $(unit_image)"
  info "running container      ${run:-<none>}"
  if [ -n "$run" ] && [ -n "$cur" ] && [ "$run" != "$cur" ]; then
    warn "the running container is NOT on the pinned image -- the pin takes effect on the next restart"
  fi
  local v; v="$(running_version)"
  [ -n "$v" ] && info "version                $v"
  info "unit state             $(state)"

  head1 "Pin history (newest first)"
  local lines; lines="$(history_lines)"
  if [ -z "$lines" ]; then
    info "none recorded"
  else
    printf '%s\n' "$lines" | sed 's/^/        /'
  fi

  head1 "Rollback"
  local target; target="$(rollback_target)"
  if [ -n "$target" ]; then
    ok "rollback would return to ${target%% *}  (${target#* })"
  else
    warn "no earlier HEALTHY pin recorded -- rollback has nowhere to go"
    info "the first pin this script makes has no predecessor; that is expected"
  fi

  # #4411. Reported here as well as under `autoupdate status` because the
  # interaction with the pin above is the thing an operator needs to see
  # WITHOUT knowing to ask: a pinned host with the timer on looks healthy and
  # updates nothing.
  head1 "Auto-update (#4411, opt-in)"
  if autoupdate_on_host; then
    ok "enabled by drop-in: $AUTOUPDATE_DROPIN"
    info "container label        $(autoupdate_label)"
    info "$AUTOUPDATE_TIMER  $(timer_state 2>/dev/null || echo unknown)"
    if [ -f "$DROPIN" ]; then
      warn "a digest pin is ALSO in place -- auto-update has nothing to poll and will report"
      info "UPDATED=false while changing nothing. Unpin, or turn auto-update off."
    fi
  else
    info "off (the default) -- turn it on with: $0 autoupdate on${MODE_FLAG}"
    info "read what it costs first: src/docs/podman-auto-update.md"
  fi
}

do_resolve() {
  [ -n "$REF" ] || { printf 'resolve needs a reference\n' >&2; usage; }
  local digest; digest="$(resolve_digest "$REF")"
  if [ -z "$digest" ]; then
    bad "could not resolve $REF to a digest"
    exit "$EX_CONFIG"
  fi
  printf '%s@%s\n' "$(image_repo "$REF")" "$digest"
}

do_pin() {
  [ -n "$REF" ] || { printf 'pin needs a reference\n' >&2; usage; }
  require_unit
  head1 "Pin -- $MODE_LABEL"
  say "  This restarts Hive. Ctrl-C now if that is not what you want."

  local digest repo image
  digest="$(resolve_digest "$REF")"
  if [ -z "$digest" ]; then
    bad "could not resolve $REF to a digest"
    exit "$EX_CONFIG"
  fi
  repo="$(image_repo "$REF")"
  image="${repo}@${digest}"
  ok "$REF resolves to $digest"

  # PULL BEFORE RESTARTING, deliberately. The generated ExecStart pulls a
  # missing image itself, and that pull is spent inside TimeoutStartSec: the
  # Hive image measured 3.8GB, so a cold pull can consume the whole start
  # budget and fail a perfectly good update. Pulling here also means a
  # registry that is down fails BEFORE anything stops.
  head1 "Pull"
  if pod pull -q "$image" >/dev/null 2>&1; then
    ok "$image is present locally"
  else
    bad "could not pull $image -- nothing has been changed"
    exit "$EX_CONFIG"
  fi

  head1 "Write the pin"
  local new_history
  new_history="$( printf 'pending %s %s %s\n' "$(now)" "$digest" "$REF"; history_lines )"
  write_dropin "$image" <<<"$new_history"
  ok "wrote $DROPIN"
  info "Image=$image"
  sctl daemon-reload
  ok "daemon-reload: the generated unit now names $(unit_image)"
  local before_restart; before_restart="$(running_image)"
  info "the running container is still ${before_restart:-<none>} -- a reload does not restart"

  head1 "Restart"
  if restart_and_report; then
    # `healthy` is a statement about this DIGEST as a rollback target -- Hive
    # itself served on it -- so it is recorded before the gateway check, whose
    # failure is a fault in front of Hive, not in the image.
    mark_top_outcome healthy
    if ensure_gateway_serving; then
      head1 "Result"
      ok "updated to $digest and it is serving end to end"
      info "roll it back with: $0 rollback${MODE_FLAG}"
      exit 0
    fi
    head1 "Result"
    bad "updated to $digest and Hive is healthy, but the DEPLOYMENT is not serving"
    info "the dashboard stays dead until the gateway is up: $SCTL_LABEL start $GATEWAY_UNIT"
    exit "$EX_CONFIG"
  fi
  mark_top_outcome failed
  head1 "Result"
  bad "the new image did not become healthy -- Hive is DOWN on this host"
  info "the unit will keep retrying it: Restart=always, one TimeoutStartSec per attempt"
  info "$GATEWAY_UNIT has stopped with it (Requires=), so the published dashboard port is down too"
  info "roll back now, deliberately:"
  info "    $0 rollback${MODE_FLAG}"
  exit "$EX_CONFIG"
}

do_rollback() {
  require_unit
  head1 "Rollback -- $MODE_LABEL"
  local target digest src repo image
  target="$(rollback_target)"
  if [ -z "$target" ]; then
    bad "no earlier HEALTHY pin is recorded in $DROPIN -- there is nowhere to roll back to"
    info "pin an explicit digest instead: $0 pin ${BASE_IMAGE_REPO}@sha256:<digest>"
    exit "$EX_CONFIG"
  fi
  digest="${target%% *}"; src="${target#* }"
  repo="$(image_repo "$src")"
  [ -n "$repo" ] || repo="$BASE_IMAGE_REPO"
  image="${repo}@${digest}"
  ok "returning to $digest  (first pinned from $src)"

  head1 "Pull"
  if pod pull -q "$image" >/dev/null 2>&1; then
    ok "$image is present locally"
  else
    bad "could not pull $image -- nothing has been changed"
    info "a rollback that needs a registry it cannot reach is not a rollback; keep the previous"
    info "image on the host, or roll back to one that is still in local storage"
    exit "$EX_CONFIG"
  fi

  head1 "Write the pin"
  local new_history
  new_history="$( printf 'pending %s %s %s\n' "$(now)" "$digest" "$src"; history_lines )"
  write_dropin "$image" <<<"$new_history"
  ok "wrote $DROPIN"
  sctl daemon-reload
  ok "daemon-reload: the generated unit now names $(unit_image)"

  # STOP FIRST, then start, rather than `restart`. After a failed update the
  # unit is not sitting still: it is in Restart=always's loop, each attempt
  # holding `activating` for TimeoutStartSec. A stop job cancels the start job
  # in flight and returns in under a second; a restart queued behind it would
  # otherwise wait out the bad attempt.
  head1 "Restart"
  sctl stop "$UNIT" >/dev/null 2>&1
  info "stopped the failing unit first: $(state)"
  if restart_and_report; then
    mark_top_outcome healthy
    # THE REASON THIS CALL EXISTS (#4493): the stop above -- and the failed
    # update before it -- left the gateway inactive, and the restart of
    # hive.service did not put it back. Without this, "rolled back and it is
    # serving" was printed over a dead published port.
    if ensure_gateway_serving; then
      head1 "Result"
      ok "rolled back to $digest and it is serving end to end"
      exit 0
    fi
    head1 "Result"
    bad "rolled back to $digest and Hive is healthy, but the DEPLOYMENT is not serving"
    info "the dashboard stays dead until the gateway is up: $SCTL_LABEL start $GATEWAY_UNIT"
    exit "$EX_CONFIG"
  fi
  mark_top_outcome failed
  head1 "Result"
  bad "the rollback target did not become healthy either -- state $(state)"
  info "journalctl -u $UNIT has the reason (add --user for a rootless install)"
  exit "$EX_CONFIG"
}

do_unpin() {
  require_unit
  head1 "Unpin -- $MODE_LABEL"
  if [ ! -f "$DROPIN" ]; then
    warn "no drop-in to remove: $DROPIN"
    exit 0
  fi
  as_owner rm -f "$DROPIN"
  as_owner rmdir "$DROPIN_DIR" 2>/dev/null
  ok "removed $DROPIN"
  sctl daemon-reload
  warn "the unit is back on the floating tag: $(unit_image)"
  info "a floating tag cannot be rolled back to; this is a teardown step, not an update one"
  head1 "Restart"
  if restart_and_report && ensure_gateway_serving; then exit 0; fi
  exit "$EX_CONFIG"
}

# --- auto-update (#4411) ----------------------------------------------------

autoupdate_on_host() { [ -f "$AUTOUPDATE_DROPIN" ]; }

# Whether the RUNNING container actually carries the policy label. The drop-in
# only takes effect on the next recreate, so a host can have the file and a
# container that auto-update will not touch -- the same gap `status` already
# prints for the image pin.
autoupdate_label() {
  pod inspect "$CONTAINER" --format '{{index .Config.Labels "io.containers.autoupdate"}}' 2>/dev/null
}

timer_state() { sctl is-enabled "$AUTOUPDATE_TIMER" 2>/dev/null; }

do_autoupdate() {
  local action="${REF:-status}"
  case "$action" in
    on|off|status) : ;;
    *) printf 'autoupdate takes on, off, or status (got %s)\n' "$action" >&2; usage ;;
  esac

  require_unit
  head1 "Auto-update -- $MODE_LABEL"

  if [ "$action" = "status" ]; then
    if autoupdate_on_host; then
      ok "opt-in drop-in present: $AUTOUPDATE_DROPIN"
    else
      warn "auto-update is OFF -- no $AUTOUPDATE_NAME (this is the default, per #4188)"
    fi
    info "container label        $(autoupdate_label 2>/dev/null || true)"
    info "$AUTOUPDATE_TIMER  $(timer_state 2>/dev/null || echo unknown)"
    info "unit ExecStart names   $(unit_image)"
    if autoupdate_on_host && [ -f "$DROPIN" ]; then
      # The measured trap. Both files present is not an error, but it means the
      # timer runs daily, reports success, and changes nothing.
      warn "a digest pin is ALSO in place -- auto-update has nothing to poll"
      info "measured: it reports UPDATED=false, exits 0, and does not touch the unit,"
      info "which reads exactly like 'already up to date' in its output"
      info "unpin to let auto-update follow the tag again: $0 unpin${MODE_FLAG}"
    fi
    return 0
  fi

  if [ "$action" = "off" ]; then
    if ! autoupdate_on_host; then
      warn "already off: no $AUTOUPDATE_DROPIN"
    else
      as_owner rm -f "$AUTOUPDATE_DROPIN"
      as_owner rmdir "$DROPIN_DIR" 2>/dev/null
      ok "removed $AUTOUPDATE_DROPIN"
      sctl daemon-reload
    fi
    if sctl disable --now "$AUTOUPDATE_TIMER" >/dev/null 2>&1; then
      ok "disabled $AUTOUPDATE_TIMER"
    else
      info "$AUTOUPDATE_TIMER was not enabled for this manager"
    fi
    info "the running container keeps its label until the next restart:"
    info "    $0 pin <ref>${MODE_FLAG}   or   systemctl${MODE_FLAG:+ } restart $UNIT"
    info "the manual path (pin/rollback) is unaffected"
    return 0
  fi

  # --- on ---
  # REFUSE rather than warn. Turning auto-update on over a pin produces a host
  # that reports a healthy daily timer and never updates; that is worse than
  # either state on its own, and it is silent.
  if [ -f "$DROPIN" ]; then
    bad "a digest pin is in place -- turning auto-update on here would do nothing"
    info "pin      $(current_ref)"
    info "measured: with a digest in Image=, auto-update reports UPDATED=false and"
    info "exits 0. The timer would run daily, report success, and update nothing."
    info "worse, if that digest stops resolving in the registry the whole"
    info "auto-update run exits 125 -- for every other container on the host too."
    info ""
    info "decide which mechanism owns the image on this host:"
    info "    $0 unpin${MODE_FLAG}      then   $0 autoupdate on${MODE_FLAG}"
    info "or keep the pin and drive updates by hand, as #4378 does."
    exit "$EX_CONFIG"
  fi

  if [ ! -f "$AUTOUPDATE_SRC" ]; then
    bad "cannot find the opt-in drop-in to install: $AUTOUPDATE_SRC"
    info "run this from a checkout, or set HIVE_UPDATE_AUTOUPDATE_SRC to the file"
    exit "$EX_CONFIG"
  fi

  as_owner mkdir -p "$DROPIN_DIR"
  as_owner cp "$AUTOUPDATE_SRC" "$AUTOUPDATE_DROPIN"
  ok "installed $AUTOUPDATE_DROPIN"
  sctl daemon-reload
  ok "daemon-reload: the unit now carries AutoUpdate=registry"

  if sctl enable --now "$AUTOUPDATE_TIMER" >/dev/null 2>&1; then
    ok "enabled $AUTOUPDATE_TIMER"
  else
    warn "could not enable $AUTOUPDATE_TIMER -- install podman's systemd units, or run"
    info "    podman auto-update --rollback=true"
    info "from your own timer. The drop-in above is what makes this unit eligible."
  fi

  # The label lands on the CONTAINER at create time, so a running container
  # predating the drop-in is invisible to auto-update until it is recreated.
  head1 "Restart"
  info "the label applies at container-create time, so the running container"
  info "is not eligible until it is recreated"
  if restart_and_report && ensure_gateway_serving; then
    ok "container label: $(autoupdate_label)"
  else
    bad "the restart did not come up serving -- auto-update is armed on a deployment that is not"
    exit "$EX_CONFIG"
  fi

  head1 "What this now does, and what it costs"
  info "a bad-but-startable image is detected and rolled back automatically"
  info "(measured: podman reads the start-job result, which is 'timeout' -- NOT"
  info "ActiveState, which never reaches 'failed' on this unit)"
  info "each bad update costs one full TimeoutStartSec of downtime (300s here)"
  info "and REPEATS on every timer firing until the bad tag is replaced upstream"
  info "monitoring: watch the UPDATED column for 'rolled back'; the unit's own"
  info "state and podman-auto-update.service's exit code both stay green"
  info "full measurement: src/docs/podman-auto-update.md"
}

case "$CMD" in
  status)     do_status ;;
  autoupdate) do_autoupdate ;;
  resolve)    do_resolve ;;
  pin)        do_pin ;;
  rollback)   do_rollback ;;
  unpin)      do_unpin ;;
esac
