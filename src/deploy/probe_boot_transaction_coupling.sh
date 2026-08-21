#!/usr/bin/env bash
# probe_boot_transaction_coupling.sh — does a WantedBy=default.target unit that
# never becomes ready hold the SYSTEM manager's boot for its TimeoutStartSec?
# (kubestellar/hive#4478)
#
# WHY THIS EXISTS
# ---------------
# #4413 measured two real reboots of one host and found the sharpest behavioural
# difference between the root modes: on the ROOTFUL boot systemd's own
# FinishTimestampMonotonic was 549us after hive-gateway.service became active --
# the boot was not declared finished until Hive was serving -- while the ROOTLESS
# boot finished at 9.2s with Hive not healthy until 18.5s.
#
# That established the coupling on a GOOD boot. What #4478 could not establish is
# the consequence, and it says so in as many words:
#
#     Inferred, not measured: that the same 120s would be spent inside the boot
#     transaction had it happened at boot rather than at an interactive
#     `systemctl start`. The mechanism above says it would [...] but a
#     deliberately-broken rootful boot was not executed.
#
# Executing that on the measuring host means rebooting it into a deliberately
# broken state, which is why it stayed inferred. This probe executes it somewhere
# that can be thrown away: a systemd container, whose PID 1 runs the same system
# manager and the same job transaction.
#
# WHAT IT DOES AND DOES NOT ESTABLISH
# -----------------------------------
# ESTABLISHES: the MANAGER behaviour. A Type=notify unit reached through
# default.target.wants/ that never sends READY holds the default-target job, and
# therefore the boot-finished timestamp, for its full TimeoutStartSec. That is
# precisely the link #4478 left inferred.
#
# DOES NOT: re-measure Hive, and does not claim a container boot is a bare-metal
# boot. There is no initrd and no firmware here, and the stand-in unit is a
# `sleep`, not a Hive. Every number below is about the manager, not about the
# image -- which is the point, because the manager is the half that was inferred.
#
# THE CONTROL IS THE ARGUMENT. Three cases differ in ONE thing: whether the unit
# is in default.target.wants/. Without the `unwired` case, `broken` would only
# show that a failing unit takes TimeoutStartSec to fail, which nobody doubted.
#
#   control   Type=notify, wired, sends READY at once
#             -> boot finishes microseconds after the unit goes active
#   broken    Type=notify, wired, never sends READY
#             -> boot finishes a full TimeoutStartSec later
#   unwired   the SAME never-ready unit, NOT wired into default.target
#             -> boot finishes at once; the timeout is paid by whoever starts it
#
# The Quadlet generator installs default.target.wants/hive.service in BOTH root
# modes -- that was measured in #4377 and again in #4413 -- so `broken` is the
# rootful shape and `unwired` is the shape rootless gets by having its
# default.target reached by logind after the system boot is already over.
#
# SAFETY. Every object is named hive-boottx-<pid>-*; the probe REFUSES TO START
# if any of those names already exists, and cleanup removes exactly the names it
# created. Nothing on the host is reconfigured, nothing is rebooted, no existing
# container, image or volume is read or removed.
#
# Run: bash src/deploy/probe_boot_transaction_coupling.sh
#      TIMEOUT_SEC=20 BASE_IMAGE=... bash src/deploy/probe_boot_transaction_coupling.sh
set -uo pipefail

TIMEOUT_SEC="${TIMEOUT_SEC:-20}"
BASE_IMAGE="${BASE_IMAGE:-registry.access.redhat.com/ubi9/ubi-init:latest}"
# How close "finished right after the unit" has to be, in microseconds. The real
# rootful boot measured 549us; a container has less running in parallel, so this
# is deliberately loose -- it is asserting coupling, not a performance figure.
COUPLING_MAX_US="${COUPLING_MAX_US:-500000}"

NAME="hive-boottx-$$"
CASES=(control broken unwired)

findings=0
c_green=""; c_red=""; c_yellow=""; c_reset=""
if [ -t 1 ]; then
  c_green="$(printf '\033[32m')"; c_red="$(printf '\033[31m')"
  c_yellow="$(printf '\033[33m')"; c_reset="$(printf '\033[0m')"
fi
ok()   { printf '  %sPASS%s  %s\n' "$c_green" "$c_reset" "$*"; }
bad()  { printf '  %sFAIL%s  %s\n' "$c_red" "$c_reset" "$*"; findings=$((findings + 1)); }
warn() { printf '  %sWARN%s  %s\n' "$c_yellow" "$c_reset" "$*"; }
info() { printf '        %s\n' "$*"; }
head1() { printf '\n%s\n' "$*"; }

cleanup() {
  for c in "${CASES[@]}"; do
    podman rm -f "${NAME}-${c}" >/dev/null 2>&1
    podman rmi -f "${NAME}-${c}" >/dev/null 2>&1
  done
  [ -n "${WORKDIR:-}" ] && rm -rf "$WORKDIR"
}
trap cleanup EXIT

command -v podman >/dev/null 2>&1 || { echo "SKIP: podman not installed"; exit 0; }

# Name discipline: refuse rather than reuse or remove anything pre-existing.
for c in "${CASES[@]}"; do
  if podman container exists "${NAME}-${c}" 2>/dev/null || podman image exists "${NAME}-${c}" 2>/dev/null; then
    echo "REFUSING TO START: ${NAME}-${c} already exists" >&2
    exit 1
  fi
done

WORKDIR="$(mktemp -d)"

cat >"${WORKDIR}/ready.service" <<EOF
[Unit]
Description=hive-boottx control: notifies READY at once
[Service]
Type=notify
NotifyAccess=all
TimeoutStartSec=${TIMEOUT_SEC}
ExecStart=/bin/sh -c 'systemd-notify --ready; exec sleep infinity'
[Install]
WantedBy=default.target
EOF

# The stand-in for a Hive that never goes healthy. Under Quadlet the READY that
# never arrives comes from --sdnotify=healthy waiting on a healthcheck that never
# passes; here it is simply never sent. The manager cannot tell the difference,
# and the manager is what is under test.
cat >"${WORKDIR}/neverready.service" <<EOF
[Unit]
Description=hive-boottx stand-in for a Hive that never goes healthy
[Service]
Type=notify
NotifyAccess=all
TimeoutStartSec=${TIMEOUT_SEC}
ExecStart=/bin/sh -c 'exec sleep infinity'
[Install]
WantedBy=default.target
EOF

build_case() {
  local case_name="$1" containerfile="${WORKDIR}/Containerfile.$1"
  case "$case_name" in
    control) cat >"$containerfile" <<EOF
FROM ${BASE_IMAGE}
COPY ready.service /etc/systemd/system/
RUN systemctl enable ready.service
EOF
      ;;
    broken) cat >"$containerfile" <<EOF
FROM ${BASE_IMAGE}
COPY neverready.service /etc/systemd/system/
RUN systemctl enable neverready.service
EOF
      ;;
    unwired) cat >"$containerfile" <<EOF
FROM ${BASE_IMAGE}
COPY neverready.service /etc/systemd/system/
EOF
      ;;
  esac
  podman build -q -f "$containerfile" -t "${NAME}-${case_name}" "$WORKDIR" >/dev/null 2>&1
}

# systemctl show inside a case, one property, --value.
cshow() { podman exec "${NAME}-$1" systemctl show "${@:3}" -p "$2" --value 2>/dev/null | tr -d '\r'; }

head1 "Boot-transaction coupling -- base ${BASE_IMAGE}, TimeoutStartSec=${TIMEOUT_SEC}s"

for c in "${CASES[@]}"; do
  if ! build_case "$c"; then
    warn "could not build the ${c} image -- no registry access?"
    echo "SKIP: cannot build from ${BASE_IMAGE}"
    exit 0
  fi
done
ok "three images built, differing only in which unit is enabled"

for c in "${CASES[@]}"; do
  podman run -d --name "${NAME}-${c}" --systemd=always "${NAME}-${c}" /sbin/init >/dev/null 2>&1 \
    || { bad "could not start the ${c} container"; exit 1; }
done
ok "three containers booted"

# Wait for each manager to declare the boot finished. `broken` cannot until its
# unit times out, which is the whole result, so the budget is the timeout plus
# generous slack.
deadline=$(( TIMEOUT_SEC * 2 + 60 ))
for c in "${CASES[@]}"; do
  waited=0
  while [ "$(cshow "$c" FinishTimestampMonotonic)" = "0" ] && [ "$waited" -lt "$deadline" ]; do
    sleep 1; waited=$((waited + 1))
  done
  if [ "$(cshow "$c" FinishTimestampMonotonic)" = "0" ]; then
    bad "${c}: boot never finished within ${deadline}s"
    exit 1
  fi
done

declare -A FINISH_REL
head1 "Observed"
printf '        %-9s %-14s %-16s %s\n' "case" "wired?" "boot finished" "state"
for c in "${CASES[@]}"; do
  u="$(cshow "$c" UserspaceTimestampMonotonic)"
  f="$(cshow "$c" FinishTimestampMonotonic)"
  rel="$(awk -v a="$f" -v b="$u" 'BEGIN{printf "%.3f", (a-b)/1000000}')"
  FINISH_REL[$c]="$rel"
  wired="yes"; [ "$c" = "unwired" ] && wired="no"
  printf '        %-9s %-14s %-16s %s\n' "$c" "$wired" "+${rel}s" \
    "$(podman exec "${NAME}-${c}" systemctl is-system-running 2>/dev/null | tr -d '\r')"
done

head1 "Findings"

# 1. The coupling itself, on a good boot: the same shape as the 549us measured
#    on the real rootful reboot in #4413.
u="$(cshow control UserspaceTimestampMonotonic)"
f="$(cshow control FinishTimestampMonotonic)"
a="$(cshow control ActiveEnterTimestampMonotonic ready.service)"
if [ -z "$a" ] || [ "$a" = "0" ]; then
  bad "control: ready.service never went active -- the control is not a control"
else
  gap="$(( f - a ))"
  if [ "$gap" -ge 0 ] && [ "$gap" -le "$COUPLING_MAX_US" ]; then
    ok "control: boot finished $(awk -v g="$gap" 'BEGIN{printf "%d", g}')us after the unit went active"
    info "the boot transaction waits for a WantedBy=default.target Type=notify unit"
  else
    bad "control: boot finished ${gap}us after the unit went active, outside the ${COUPLING_MAX_US}us window"
  fi
fi

# 2. The half #4478 left inferred: a unit that never becomes ready holds the
#    boot for its whole TimeoutStartSec.
u="$(cshow broken UserspaceTimestampMonotonic)"
f="$(cshow broken FinishTimestampMonotonic)"
x="$(cshow broken InactiveExitTimestampMonotonic neverready.service)"
res="$(cshow broken Result neverready.service)"
if [ -z "$x" ] || [ "$x" = "0" ]; then
  bad "broken: neverready.service never started"
else
  held_us=$(( f - x ))
  held="$(awk -v h="$held_us" 'BEGIN{printf "%.3f", h/1000000}')"
  if awk -v h="$held_us" -v t="$TIMEOUT_SEC" 'BEGIN{exit !(h >= t*1000000*0.9)}'; then
    ok "broken: the boot was held ${held}s -- the unit's whole TimeoutStartSec (${TIMEOUT_SEC}s)"
    info "Result=${res}; the timeout was spent INSIDE the boot transaction"
  else
    bad "broken: the boot was held only ${held}s of a ${TIMEOUT_SEC}s timeout"
  fi
fi

# 3. The control that makes 2 mean what it says: the same unit, same timeout,
#    not in default.target.wants/.
if awk -v b="${FINISH_REL[broken]}" -v w="${FINISH_REL[unwired]}" -v t="$TIMEOUT_SEC" \
     'BEGIN{exit !(b - w >= t*0.9)}'; then
  ok "unwired: boot finished +${FINISH_REL[unwired]}s against broken's +${FINISH_REL[broken]}s"
  info "membership of default.target.wants/ is the only difference, and it is the whole cost"
else
  bad "unwired finished +${FINISH_REL[unwired]}s, not meaningfully before broken's +${FINISH_REL[broken]}s"
fi

# 4. And the same failure outside the transaction still costs the timeout -- to
#    whoever asked for it, not to the boot. This is the rootless shape.
start_s="$(date +%s)"
podman exec "${NAME}-unwired" systemctl start neverready.service >/dev/null 2>&1
end_s="$(date +%s)"
blocked=$(( end_s - start_s ))
if [ "$blocked" -ge $(( TIMEOUT_SEC * 9 / 10 )) ]; then
  ok "unwired: an interactive start of the same unit blocked ${blocked}s"
  info "the cost does not vanish, it moves off the boot and onto the caller"
else
  warn "unwired: the interactive start returned in ${blocked}s, under the ${TIMEOUT_SEC}s timeout"
fi

head1 "Result"
if [ "$findings" -eq 0 ]; then
  printf '  no findings -- the boot transaction waits, and default.target.wants/ is why\n\n'
  exit 0
fi
printf '  %d finding(s)\n\n' "$findings"
exit 1
