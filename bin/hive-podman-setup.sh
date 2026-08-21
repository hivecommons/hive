#!/usr/bin/env bash
# One-command standalone install for Hive under Podman (#4470).
#
# The Docker path has bin/hive-setup.sh. The Podman path had README.md's Quick
# Start (Podman) — correct and verified (#4412, #4448), but a MANUAL sequence of
# roughly twenty steps. That asymmetry is the gap this closes, and it is not a
# cosmetic one: two of those steps are the known traps.
#
#   #4367  hive.yaml's `dashboard.port` and the unit's HealthCmd port must
#          agree. The example config ships 3001 for source runs; the unit
#          probes 3002. Install the example unchanged and Notify=healthy
#          correctly holds the unit in `activating` for the whole 300-second
#          TimeoutStartSec, after which --rm deletes the container that held
#          the evidence. This script does not warn about that coupling — it
#          READS the port out of the unit and writes that value into the
#          config, then reads it back and refuses to continue if they differ.
#   #4359  The secrets directory needs mode 750 and group `hive-launch` (GID
#          1002), and the command differs by root mode: `podman unshare chown`
#          rootless, because container GID 1002 is not host GID 1002; a plain
#          `chgrp` rootful, where it is. A 0700 directory owned by the operator
#          grants the container's `dev` user not even the traverse bit, and the
#          resulting EACCES on an enforcing host reads as an SELinux problem it
#          is not.
#
# THIS IS NOT A PORT OF bin/hive-setup.sh, and deliberately does three things
# differently (#4470):
#
#   * It INSTALLS NO PACKAGES. hive-setup.sh runs `apt-get install docker-ce`.
#     Podman's supported hosts include image-based systems — Fedora
#     Silverblue/Bluefin, RHEL image mode — where the package manager cannot do
#     that and /usr is read-only. Missing prerequisites are DETECTED and
#     REPORTED by the existing preflights; the host is never modified to suit
#     us.
#   * It CLONES NOTHING. hive-setup.sh clones v2 into /opt/hive. This runs from
#     the checkout it lives in.
#   * It GENERATES NO DEPLOYMENT DESCRIPTION. The four Quadlet units in
#     src/deploy/quadlet/ are the source of truth (#4404) and are installed
#     verbatim, so the rationale in their headers travels to the host.
#
# It also adds no preflight logic of its own. The three existing preflights are
# invoked and their guidance is surfaced, not restated — one place to fix a
# check, one place to fix its advice.
#
# WHAT IT DOES NOT DO ON FAILURE: roll back. A half-configured host is
# inspectable and a wiped one is not, so a failing step says which step it was
# and stops. Re-running after fixing the cause is safe — see idempotency below.
#
# IDEMPOTENT. A second run never clobbers an existing hive.yaml, hive.env, or
# anything under secrets/: those are kept, and reported as kept. --force
# re-copies the config files from the repository's examples. NOTHING under
# secrets/ is ever overwritten, with or without --force.
#
# Rootless by default; --rootful drives the system manager through sudo, the
# same flag convention as bin/hive-podman-update.sh.
#
# Run: bin/hive-podman-setup.sh [--rootful|--rootless] [--force] [--skip-pull]
# Exit codes: 0 installed and healthy, 64 unusable invocation (EX_USAGE),
#             70 an assumption this script makes about the repo is broken
#             (EX_SOFTWARE), 78 a step failed (EX_CONFIG)

set -uo pipefail

EX_USAGE=64
EX_SOFTWARE=70
EX_CONFIG=78

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Overridable for the contract test, which drives the whole script against
# fakes. Same convention as HIVE_UPDATE_QUADLET_DIR in hive-podman-update.sh.
SRC_DIR="${HIVE_SETUP_SRC_DIR:-${ROOT}/src}"
QUADLET_SRC="${HIVE_SETUP_QUADLET_SRC:-${SRC_DIR}/deploy/quadlet}"
PREFLIGHT_DIR="${HIVE_SETUP_PREFLIGHT_DIR:-${ROOT}/bin}"

# The container-side GID of `hive-launch`, pinned in src/Dockerfile and spelled
# `fsGroup: 1002` on the Kubernetes path. bin/hive-podman-preflight-host.sh
# checks the same number under the same name (#4359).
LAUNCH_GID="${HIVE_SETUP_LAUNCH_GID:-1002}"

# The four units, in the order they are installed. systemd resolves the real
# ordering from the Requires=/After= the generator derives; this order is for a
# reader watching the output.
UNITS=(hive.network hive-data.volume hive.container hive-gateway.container)

# Health confirmation budget. The gateway answering is an END-TO-END check —
# nginx up, DNS resolving `hive`, Hive serving — so it can legitimately trail
# `systemctl start` returning by a moment.
HEALTH_RETRIES="${HIVE_SETUP_HEALTH_RETRIES:-30}"
HEALTH_DELAY="${HIVE_SETUP_HEALTH_DELAY:-2}"

ROOTFUL=0
FORCE=0
SKIP_PULL=0
STEP="startup"

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

step() { STEP="$1"; head1 "$1"; }

# Every exit path that is not success goes through here, so the operator always
# learns WHICH step failed and that the host was left alone.
die() {
  local code="$1"; shift
  bad "$*"
  printf '\n%sFAILED at step: %s%s\n' "$c_red" "$STEP" "$c_reset" >&2
  printf 'The host is left exactly as this step found it. Nothing was rolled back,\n' >&2
  printf 'so the partial state is there to inspect. Fix the cause and re-run:\n' >&2
  printf '  bin/hive-podman-setup.sh%s\n' "$MODE_FLAG" >&2
  exit "$code"
}

usage() {
  cat <<'EOF'
Usage: hive-podman-setup.sh [--rootful|--rootless] [--force] [--skip-pull]

Installs the standalone Hive Podman deployment: runs the existing preflights,
materialises the configuration, installs the four Quadlet units, and confirms
the gateway answers before returning.

  --rootful    system manager, config in /etc/hive, units in
               /etc/containers/systemd, systemctl and podman through sudo
  --rootless   user manager, config in ~/.config/hive (the default)
  --force      re-copy hive.yaml, nginx.conf and hive.env from the repository's
               examples, overwriting what is there. NEVER touches secrets/.
  --skip-pull  do not pre-pull the image. The generated ExecStart will pull it
               instead, inside TimeoutStartSec, on an image of roughly 3.8GB.

Installs no packages, clones nothing, and generates no deployment description.
A failing step stops the run and says which one; nothing is rolled back.
EOF
  exit "$EX_USAGE"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --rootful)   ROOTFUL=1 ;;
    --rootless)  ROOTFUL=0 ;;
    --force)     FORCE=1 ;;
    --skip-pull) SKIP_PULL=1 ;;
    -h|--help)   usage ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; usage ;;
  esac
  shift
done

# --- root mode --------------------------------------------------------------
#
# One code path with a different prefix, rather than two transcriptions that
# drift apart. Same shape as bin/hive-podman-update.sh.
if [ "$ROOTFUL" -eq 1 ]; then
  MODE_LABEL="rootful (system manager)"
  MODE_FLAG=" --rootful"
  CONF_DIR="${HIVE_SETUP_CONF_DIR:-/etc/hive}"
  UNIT_DIR="${HIVE_SETUP_UNIT_DIR:-/etc/containers/systemd}"
  SCTL_LABEL="sudo systemctl"
  sctl() { sudo systemctl "$@"; }
  pod()  { sudo podman "$@"; }
  as_owner() { sudo "$@"; }
else
  MODE_LABEL="rootless (user manager, uid $(id -u))"
  MODE_FLAG=""
  CONF_DIR="${HIVE_SETUP_CONF_DIR:-$HOME/.config/hive}"
  UNIT_DIR="${HIVE_SETUP_UNIT_DIR:-$HOME/.config/containers/systemd}"
  SCTL_LABEL="systemctl --user"
  sctl() { systemctl --user "$@"; }
  pod()  { podman "$@"; }
  as_owner() { "$@"; }
fi

SECRETS_DIR="${CONF_DIR}/secrets"

# --- reading the units, rather than repeating them --------------------------
#
# Every number this script writes or checks is read out of the unit that will
# actually enforce it. A constant copied into this file is a second source of
# truth and would eventually be the #4367 bug again, one level up.

# The FIRST probe's port, and the pattern is written to keep it that way.
# HealthCmd names two listeners since #4476 — the Go API first, the auth proxy
# second — and it is the API port that dashboard.port is coupled to. A leading
# `.*` is greedy and would match through to the LAST URL on the line, so the
# script would write the PROXY's port into hive.yaml: #4367 again, caused by the
# fix for a different bug. `[^:]*://` cannot cross the first scheme colon.
unit_health_port() {
  sed -n 's|^HealthCmd=[^:]*://127\.0\.0\.1:\([0-9]\{1,5\}\)/api/health.*|\1|p' "$1" | head -n1
}

# The SECOND probe's port: the auth proxy nginx dials (#4476). Empty on a unit
# that still probes one listener.
unit_proxy_health_port() {
  grep -E '^HealthCmd=' "$1" \
    | grep -oE '127\.0\.0\.1:[0-9]{1,5}/api/health' \
    | sed -n '2s|127\.0\.0\.1:\([0-9]\{1,5\}\)/api/health|\1|p'
}

unit_publish_port() {
  sed -n 's|^PublishPort=\([0-9]\{1,5\}\):.*|\1|p' "$1" | head -n1
}

unit_image() {
  sed -n 's|^Image=\(.*\)$|\1|p' "$1" | head -n1
}

# hive.yaml's dashboard.port, read only from inside the `dashboard:` block. A
# bare `grep port:` would match the gateway's, the proxy's, or a commented one.
dashboard_port() {
  awk '
    /^[^[:space:]#]/ { in_dash = ($0 ~ /^dashboard:[[:space:]]*$/) }
    in_dash && $0 ~ /^[[:space:]]+port:[[:space:]]*[0-9]+/ {
      value = $0
      sub(/^[[:space:]]+port:[[:space:]]*/, "", value)
      sub(/[^0-9].*$/, "", value)
      print value
      exit
    }
  ' "$1"
}

# Rewrite it in place, within that block only. Exit 3 when there was no
# dashboard.port to rewrite — silence would be the failure mode that matters.
set_dashboard_port() {
  local file="$1" want="$2" tmp
  tmp="$(mktemp)" || return 1
  awk -v want="$want" '
    /^[^[:space:]#]/ { in_dash = ($0 ~ /^dashboard:[[:space:]]*$/) }
    in_dash && $0 ~ /^[[:space:]]+port:[[:space:]]*[0-9]+[[:space:]]*$/ {
      sub(/port:[[:space:]]*[0-9]+[[:space:]]*$/, "port: " want)
      seen = 1
    }
    { print }
    END { exit(seen ? 0 : 3) }
  ' "$file" >"$tmp"
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    rm -f "$tmp"
    return "$rc"
  fi
  # Through as_owner: rootful writes into /etc/hive, which this script reaches
  # with sudo and not directly. cp onto an existing file keeps its mode.
  as_owner cp "$tmp" "$file" || { rm -f "$tmp"; return 1; }
  rm -f "$tmp"
}

# --- socket isolation (#4188) -----------------------------------------------
#
# Nothing in this deployment mounts a container socket, and this script is the
# thing that puts the units on the host — so it is where "we do not do that"
# becomes something the machine checks rather than something a reviewer
# remembers. A unit that acquired a socket mount would otherwise be installed
# by a script whose header says it never installs one.
assert_no_container_socket() {
  local file="$1" hit
  hit="$(grep -nE '(docker|podman)\.sock' "$file" | grep -v '^[0-9]*:#' | head -n1)"
  if [ -n "$hit" ]; then
    die "$EX_SOFTWARE" "$(basename "$file") mounts or names a container socket: ${hit}"
  fi
}

# ---------------------------------------------------------------------------
say ""
say "${c_bold}Hive standalone install — Podman${c_reset}"
say "  mode:    ${MODE_LABEL}"
say "  config:  ${CONF_DIR}"
say "  units:   ${UNIT_DIR}"
say "  source:  ${SRC_DIR}"

# --- step 1: this really is the Podman path ---------------------------------
step "1/7  Runtime selection"

# The standalone runtime selector (#4205) defaults to Docker. Refuse if an
# operator has explicitly selected something else — this script speaks only to
# Podman and must not act on a deployment they did not mean to touch.
if [ -n "${HIVE_DEPLOY_RUNTIME:-}" ] && [ "${HIVE_DEPLOY_RUNTIME}" != "podman" ]; then
  bad "HIVE_DEPLOY_RUNTIME=${HIVE_DEPLOY_RUNTIME} selects a different runtime; this is the Podman installer."
  info "Docker deployments install through bin/hive-setup.sh, which this script does not touch."
  exit "$EX_USAGE"
fi

# EXPORTED, not merely assumed. The three preflights below check nothing and
# exit 0 unless Podman is explicitly selected, which is the single most
# expensive way to be reassured by this script: a green run that checked
# nothing at all. The README's manual sequence opens with the same export and
# warns about exactly this.
export HIVE_DEPLOY_RUNTIME=podman
ok "HIVE_DEPLOY_RUNTIME=podman — the preflights below will actually run"

command -v podman >/dev/null 2>&1 || die "$EX_CONFIG" "podman is not installed or not in PATH"
ok "podman found: $(command -v podman)"

for unit in "${UNITS[@]}"; do
  [ -f "${QUADLET_SRC}/${unit}" ] || die "$EX_SOFTWARE" "missing unit in the checkout: ${QUADLET_SRC}/${unit}"
  assert_no_container_socket "${QUADLET_SRC}/${unit}"
done
ok "four Quadlet units present, none mounting a container socket"

HEALTH_PORT="$(unit_health_port "${QUADLET_SRC}/hive.container")"
PROXY_HEALTH_PORT="$(unit_proxy_health_port "${QUADLET_SRC}/hive.container")"
GATEWAY_PORT="$(unit_publish_port "${QUADLET_SRC}/hive-gateway.container")"
HIVE_IMAGE="$(unit_image "${QUADLET_SRC}/hive.container")"
[ -n "$HEALTH_PORT" ]  || die "$EX_SOFTWARE" "could not read the HealthCmd port from hive.container"
[ -n "$GATEWAY_PORT" ] || die "$EX_SOFTWARE" "could not read PublishPort from hive-gateway.container"
[ -n "$HIVE_IMAGE" ]   || die "$EX_SOFTWARE" "could not read Image= from hive.container"
ok "read from the units: health port ${HEALTH_PORT}, gateway port ${GATEWAY_PORT}"

# Reported because the two are not interchangeable, and step 4 below couples
# dashboard.port to the FIRST one only. The second is the auth proxy the
# gateway dials; it is probed so the unit cannot report healthy while the
# dashboard is dead (#4476), but its port comes from HIVE_PROXY_PORT, not from
# hive.yaml, so there is nothing here for this script to set.
if [ -n "$PROXY_HEALTH_PORT" ]; then
  info "the unit also probes the auth proxy on ${PROXY_HEALTH_PORT} (#4476)"
else
  warn "hive.container probes only ${HEALTH_PORT}: it can report healthy while the dashboard is dead (#4476)"
fi
info "image: ${HIVE_IMAGE}"

# --- step 2: the host preflights, before anything is written ----------------
step "2/7  Host preflight (engine, root mode, cgroups; subordinate IDs, storage, networking)"

# Their output IS the guidance. Restating it here would be a second copy to
# keep in sync, and the copy would be the one the operator reads.
run_preflight() {
  local script="$1" label="$2"
  if [ ! -x "${PREFLIGHT_DIR}/${script}" ]; then
    die "$EX_SOFTWARE" "missing preflight: ${PREFLIGHT_DIR}/${script}"
  fi
  say ""
  say "  -> ${script}"
  if ! "${PREFLIGHT_DIR}/${script}"; then
    say ""
    bad "${label} reported a failure."
    info "The remediation is in that script's output above; this installer does not repeat it,"
    info "and does not attempt to fix the host. Nothing has been written yet."
    printf '\n%sFAILED at step: %s%s\n' "$c_red" "$STEP" "$c_reset" >&2
    exit "$EX_CONFIG"
  fi
}

run_preflight hive-podman-preflight.sh     "Engine/root-mode/cgroup preflight"
run_preflight hive-podman-preflight-ids.sh "Subordinate-ID/graphroot/network preflight"
ok "both pre-write preflights passed"

# --- step 3: configuration ---------------------------------------------------
step "3/7  Configuration in ${CONF_DIR}"

as_owner mkdir -p "$CONF_DIR" || die "$EX_CONFIG" "could not create ${CONF_DIR}"
as_owner mkdir -p "$SECRETS_DIR" || die "$EX_CONFIG" "could not create ${SECRETS_DIR}"

# Copy an example into place unless the operator already has one. `keep` is the
# idempotent path and is reported, not silent: an operator re-running this
# after an edit must be able to see that their file survived.
place() {
  local src="$1" dest="$2" mode="$3" label="$4"
  [ -f "$src" ] || die "$EX_SOFTWARE" "missing template in the checkout: ${src}"
  if [ -e "$dest" ] && [ "$FORCE" -eq 0 ]; then
    ok "keep    ${label}: ${dest} already exists (--force re-copies it)"
    return 0
  fi
  if [ -e "$dest" ]; then
    warn "replace ${label}: ${dest} overwritten by --force"
  fi
  as_owner install -Dm"$mode" "$src" "$dest" || die "$EX_CONFIG" "could not write ${dest}"
  ok "wrote   ${label}: ${dest} (mode ${mode})"
}

place "${SRC_DIR}/hive.yaml.example"        "${CONF_DIR}/hive.yaml"  644 "Hive config"
place "${SRC_DIR}/deploy/nginx.conf"        "${CONF_DIR}/nginx.conf" 644 "Gateway config"
# EnvironmentFile= becomes `podman run --env-file`, which fails on a missing
# file — so this one must exist even if every line in it stays commented out.
place "${QUADLET_SRC}/hive.env.example"     "${CONF_DIR}/hive.env"   600 "Environment file"

# A dashboard token, generated once and then left alone. Appended rather than
# templated so an operator's own value survives a re-run, and so --force
# re-copying the example is the only thing that ever regenerates it.
if grep -qE '^[[:space:]]*HIVE_DASHBOARD_TOKEN=' "${CONF_DIR}/hive.env" 2>/dev/null; then
  ok "keep    dashboard token: HIVE_DASHBOARD_TOKEN already set in hive.env"
elif command -v openssl >/dev/null 2>&1; then
  token="$(openssl rand -hex 32)" || die "$EX_CONFIG" "openssl rand failed"
  printf 'HIVE_DASHBOARD_TOKEN=%s\n' "$token" | as_owner tee -a "${CONF_DIR}/hive.env" >/dev/null \
    || die "$EX_CONFIG" "could not append HIVE_DASHBOARD_TOKEN to ${CONF_DIR}/hive.env"
  ok "wrote   dashboard token: 32 random bytes appended to hive.env"
else
  warn "openssl not found — set HIVE_DASHBOARD_TOKEN in ${CONF_DIR}/hive.env yourself"
fi
info "HIVE_GITHUB_TOKEN is yours to add: see src/docs/github-app-setup.md for the PAT scopes."

# --- step 4: the #4367 coupling, enforced -----------------------------------
step "4/7  dashboard.port must equal the unit's HealthCmd port (#4367)"

current="$(dashboard_port "${CONF_DIR}/hive.yaml")"
if [ -z "$current" ]; then
  die "$EX_CONFIG" "no dashboard.port found in ${CONF_DIR}/hive.yaml — cannot guarantee the healthcheck will ever answer"
fi

if [ "$current" != "$HEALTH_PORT" ]; then
  if ! set_dashboard_port "${CONF_DIR}/hive.yaml" "$HEALTH_PORT"; then
    die "$EX_CONFIG" "could not set dashboard.port to ${HEALTH_PORT} in ${CONF_DIR}/hive.yaml"
  fi
  ok "set     dashboard.port ${current} -> ${HEALTH_PORT}"
else
  ok "already dashboard.port ${current}"
fi

# Read it back from the file that will be mounted, not from a variable this
# script set. This is the check that makes the coupling impossible to get
# wrong rather than merely warned about, and it runs on the re-run path too:
# an operator who edited the port back by hand is stopped here rather than by
# a silent 300-second hang with no container left to inspect.
verified="$(dashboard_port "${CONF_DIR}/hive.yaml")"
if [ "$verified" != "$HEALTH_PORT" ]; then
  die "$EX_CONFIG" "dashboard.port reads back as ${verified:-<unset>}, but the unit probes ${HEALTH_PORT}"
fi
ok "verified from the file that will be mounted: ${verified} == ${HEALTH_PORT}"

# --- step 5: secrets the container can actually reach (#4359) ---------------
step "5/7  Secrets directory mode and group (#4359)"

as_owner chmod 750 "$SECRETS_DIR" || die "$EX_CONFIG" "could not chmod 750 ${SECRETS_DIR}"
ok "mode 750 on ${SECRETS_DIR}"

# Rootful maps identity, so container GID 1002 is host GID 1002 and chgrp is
# right. Rootless does not: the invoking user maps to container root and
# everything above comes out of the subordinate range, so a plain chgrp 1002 is
# both wrong and — for a user not in group 1002 — not even permitted. `podman
# unshare` does that translation. Same reasoning as the preflight's remediation.
if [ "$ROOTFUL" -eq 1 ]; then
  as_owner chgrp -R "$LAUNCH_GID" "$SECRETS_DIR" \
    || die "$EX_CONFIG" "could not chgrp -R ${LAUNCH_GID} ${SECRETS_DIR}"
  ok "group ${LAUNCH_GID} (hive-launch) via chgrp — rootful maps identity"
else
  pod unshare chown -R "0:${LAUNCH_GID}" "$SECRETS_DIR" \
    || die "$EX_CONFIG" "could not podman unshare chown -R 0:${LAUNCH_GID} ${SECRETS_DIR}"
  ok "container 0:${LAUNCH_GID} via podman unshare chown — rootless does not map identity"
fi
info "Nothing under secrets/ is created or overwritten here; put your GitHub App key in it yourself."

# --- step 6: the post-write preflight, then the units -----------------------
step "6/7  Host preflight over what was just written, then install the units"

# This one checks the files the steps above created — SELinux labels on the
# bind sources, secrets reachability, hive.env, the gateway port — so it can
# only run now.
if [ ! -x "${PREFLIGHT_DIR}/hive-podman-preflight-host.sh" ]; then
  die "$EX_SOFTWARE" "missing preflight: ${PREFLIGHT_DIR}/hive-podman-preflight-host.sh"
fi
say ""
say "  -> hive-podman-preflight-host.sh (HIVE_SRC_DIR=${CONF_DIR})"
if ! HIVE_SRC_DIR="$CONF_DIR" "${PREFLIGHT_DIR}/hive-podman-preflight-host.sh"; then
  say ""
  bad "SELinux/mount/secret/port preflight reported a failure."
  info "The remediation is in that script's output above. The configuration written so far"
  info "is left in ${CONF_DIR} for you to inspect; no unit has been installed yet."
  printf '\n%sFAILED at step: %s%s\n' "$c_red" "$STEP" "$c_reset" >&2
  exit "$EX_CONFIG"
fi
ok "host preflight passed against ${CONF_DIR}"

if [ "$SKIP_PULL" -eq 0 ]; then
  say ""
  say "  -> podman pull ${HIVE_IMAGE}"
  info "Pulling now rather than inside TimeoutStartSec: the generated ExecStart would"
  info "otherwise spend the start budget on a ~3.8GB pull."
  pod pull "$HIVE_IMAGE" || die "$EX_CONFIG" "could not pull ${HIVE_IMAGE}"
  ok "image present"
else
  warn "skipping the pull (--skip-pull); the first start pays for it inside TimeoutStartSec"
fi

as_owner mkdir -p "$UNIT_DIR" || die "$EX_CONFIG" "could not create ${UNIT_DIR}"
for unit in "${UNITS[@]}"; do
  as_owner install -Dm644 "${QUADLET_SRC}/${unit}" "${UNIT_DIR}/${unit}" \
    || die "$EX_CONFIG" "could not install ${unit} into ${UNIT_DIR}"
  ok "installed ${unit}"
done
info "All four: the gateway will not generate without the network it names."

sctl daemon-reload || die "$EX_CONFIG" "systemctl daemon-reload failed"
ok "daemon-reload — the Quadlet generator has run"

# --- step 7: started is not the same as healthy -----------------------------
step "7/7  Start, and confirm HEALTHY rather than started"

# Starting the gateway pulls hive, the network and the volume up in order.
# In a logged-in session the stack may already be up: daemon-reload runs the
# generator, which writes the default.target.wants/ symlinks from [Install],
# and systemd starts newly-wanted units of an already-active target. The start
# is then a no-op that returns 0 and is worth running as the confirmation.
sctl start hive-gateway.service || die "$EX_CONFIG" "systemctl start hive-gateway.service failed"
ok "start returned"

# Notify=healthy makes this a real distinction: the unit is held in
# `activating` until HealthCmd passes, so `active` here means Hive answered
# /api/health rather than that conmon came up. Checking it is what makes the
# claim at the end of this script true.
for unit_service in hive.service hive-gateway.service; do
  state="$(sctl is-active "$unit_service" 2>/dev/null)"
  if [ "$state" != "active" ]; then
    bad "${unit_service} is ${state:-unknown}, not active"
    info "${SCTL_LABEL} status ${unit_service} — and journalctl${MODE_FLAG:+} -u ${unit_service}"
    info "Notify=healthy holds a unit in 'activating' until its healthcheck passes, so"
    info "'activating' here means Hive started but has not answered /api/health yet."
    die "$EX_CONFIG" "the deployment is not healthy"
  fi
  ok "${unit_service} is active (Notify=healthy: that means its healthcheck passed)"
done

# The end-to-end check the issue names: the gateway answering on its published
# port. It covers what neither unit state can — nginx up, aardvark-dns
# resolving `hive`, and Hive serving behind it.
if ! command -v curl >/dev/null 2>&1; then
  die "$EX_CONFIG" "curl is not installed, so the gateway check cannot run — both units are active, but this script will not claim healthy on evidence it does not have"
fi

say ""
say "  -> waiting for the gateway on http://127.0.0.1:${GATEWAY_PORT}/api/health"
attempt=0
answered=0
while [ "$attempt" -lt "$HEALTH_RETRIES" ]; do
  if curl -sf "http://127.0.0.1:${GATEWAY_PORT}/api/health" >/dev/null 2>&1; then
    answered=1
    break
  fi
  attempt=$((attempt + 1))
  sleep "$HEALTH_DELAY"
done

if [ "$answered" -ne 1 ]; then
  bad "the gateway did not answer on ${GATEWAY_PORT} within $((HEALTH_RETRIES * HEALTH_DELAY))s"
  info "Both units are active, so Hive itself is healthy and the gap is in front of it."
  info "A 502 here is usually name resolution: podman info --format '{{.Host.NetworkBackend}}'"
  info "should say netavark, and aardvark-dns must be installed, or the gateway cannot"
  info "resolve 'hive'. The deployment is left running for you to inspect."
  die "$EX_CONFIG" "installed, but not confirmed healthy end to end"
fi
ok "gateway answered on ${GATEWAY_PORT} — healthy end to end"

head1 "Hive is running"
say "  Dashboard:  http://localhost:${GATEWAY_PORT}"
say "  Config:     ${CONF_DIR}/hive.yaml   (edit for your project, then: ${SCTL_LABEL} restart hive.service)"
say "  Secrets:    ${SECRETS_DIR}          (mode 750, group ${LAUNCH_GID}; put your GitHub App key here)"
say "  Tokens:     ${CONF_DIR}/hive.env    (HIVE_GITHUB_TOKEN, HIVE_DASHBOARD_TOKEN)"
say ""
say "  Update or roll back:  bin/hive-podman-update.sh status${MODE_FLAG}"
say "  Remove:               bin/hive-podman-teardown.sh plan"
say "  Guide:                src/docs/podman-standalone-quadlet.md"
say ""
exit 0
