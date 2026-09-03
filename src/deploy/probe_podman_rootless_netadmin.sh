#!/usr/bin/env bash
# Rootless Podman startup and exit-77 probe (#4199).
#
# Runs the three-case rootless matrix against a Hive image and reports, for
# each case, whether the container started AND whether the forced-proxy egress
# gate was actually installed. Those are different questions: a container that
# starts under `--cap-add NET_ADMIN` proves nothing about the redirect, and a
# container that fails may have failed downstream of a gate that installed
# fine.
#
#   default    rootless, no added capability  -> expect exit 77, no gate
#   netadmin   rootless + --cap-add NET_ADMIN -> expect gate installed
#   advisory   rootless + HIVE_PROXY_ADVISORY_OK=true -> expect advisory warning
#
# By default every container, image, and volume goes into a throwaway store
# created for this run, so the probe never adds to or removes from the
# operator's own Podman storage. Cleanup targets only the containers this
# script created, by name.
#
# Usage:
#   src/deploy/probe_podman_rootless_netadmin.sh [options]
#
#   --image REF     image to probe (default: the hive ref in standalone-images.sh)
#   --store DIR     reuse a probe store instead of creating one (kept on exit)
#   --shared-store  deliberately use the caller's default Podman store
#   --bypass        additionally probe interception from an agent UID (needs egress)
#   --timeout SEC   per-case run cap (default 90)
#
# Exit codes: 0 the matrix matched the documented contract, 1 it did not,
# 78 a prerequisite is missing (EX_CONFIG).

set -uo pipefail

# The image default comes from the #4206 source of truth, so the probe
# measures the same reference the deployment assets run (#4486). IMAGE in the
# environment or --image stays the deliberate override.
# shellcheck source=src/deploy/standalone-images.sh disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/standalone-images.sh"

IMAGE="${IMAGE:-$HIVE_STANDALONE_IMAGE_HIVE}"
STORE=""
SHARED_STORE="false"
BYPASS="false"
CASE_TIMEOUT="90"
OWN_STORE="false"

PROBE_PREFIX="hive-rootless-probe"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --image) IMAGE="${2:?--image needs a value}"; shift 2 ;;
    --store) STORE="${2:?--store needs a value}"; shift 2 ;;
    --shared-store) SHARED_STORE="true"; shift ;;
    --bypass) BYPASS="true"; shift ;;
    --timeout) CASE_TIMEOUT="${2:?--timeout needs a value}"; shift 2 ;;
    -h|--help)  sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) printf 'ERROR: unknown argument %q\n' "$1" >&2; exit 78 ;;
  esac
done

fail_prereq() {
  printf 'PREREQUISITE MISSING: %s\n' "$1" >&2
  exit 78
}

command -v podman >/dev/null 2>&1 || fail_prereq "podman is not installed or not on PATH"

# This probe is only meaningful rootless. Rootful Podman is #4200.
rootless="$(podman info --format '{{.Host.Security.Rootless}}' 2>/dev/null)"
[[ "$rootless" == "true" ]] || \
  fail_prereq "podman reports rootless=${rootless:-unknown}; this probe requires rootless Podman (rootful is #4200)"

WORK="$(mktemp -d)"
# shellcheck disable=SC2329 # invoked through the EXIT trap below
cleanup() {
  local name
  for name in "${PROBE_PREFIX}-default" "${PROBE_PREFIX}-netadmin" "${PROBE_PREFIX}-advisory" "${PROBE_PREFIX}-live"; do
    podman rm -f "$name" >/dev/null 2>&1
  done
  rm -rf "$WORK"

  # Files the containers wrote are owned by mapped subordinate UIDs, so a plain
  # rm -rf cannot remove a store this script created. Delete it from inside the
  # user namespace instead, and fall back to rm for the pre-container case.
  if [[ "$OWN_STORE" == "true" && -n "$STORE" ]]; then
    podman unshare rm -rf "$STORE" 2>/dev/null || rm -rf "$STORE" 2>/dev/null
  fi
  return 0
}
trap cleanup EXIT

if [[ "$SHARED_STORE" != "true" ]]; then
  if [[ -z "$STORE" ]]; then
    STORE="$(mktemp -d -t hive-rootless-probe-store-XXXXXX)"
    OWN_STORE="true"
  fi
  mkdir -p "${STORE}/graph" "${STORE}/run" "${STORE}/tmp"
  cat >"${STORE}/storage.conf" <<EOF
[storage]
driver = "overlay"
graphroot = "${STORE}/graph"
runroot = "${STORE}/run"
EOF
  export CONTAINERS_STORAGE_CONF="${STORE}/storage.conf"
  export TMPDIR="${STORE}/tmp"
fi

# The forced-egress gate lives behind "at least one agent is configured", so a
# config with no agents skips it entirely and the container starts clean while
# proving nothing. This is the smallest config that reaches the gate.
cat >"${WORK}/hive.yaml" <<'EOF'
project:
  org: hivecommons
  repos:
    - hivecommons/hive
github:
  token: "ghp_probe_not_a_real_token"
agents:
  probe:
    backend: claude
EOF

printf '== Hive rootless Podman startup / exit-77 probe ==\n'
# Field-by-field, each tolerating absence. A single combined Go template
# hard-fails ENTIRELY (exit 125) the moment any one field leaves Podman's
# info schema — newer Podman dropped .Host.RootlessNetworkCmd and this
# report line, which is diagnostics rather than a result, lost every field
# at once. Each read degrades to "unknown" on its own instead.
pinfo() { podman info --format "{{$1}}" 2>/dev/null || printf 'unknown'; }
rootlessnet="$(pinfo .Host.RootlessNetworkCmd)"
if [[ "$rootlessnet" == "unknown" || -z "$rootlessnet" ]]; then
  # The summary field is gone; name the provider from the tool entries.
  if [[ -n "$(podman info --format '{{.Host.Pasta.Executable}}' 2>/dev/null)" ]]; then
    rootlessnet="pasta"
  elif [[ -n "$(podman info --format '{{.Host.Slirp4NetNS.Executable}}' 2>/dev/null)" ]]; then
    rootlessnet="slirp4netns"
  else
    rootlessnet="unknown"
  fi
fi
printf 'podman=%s rootless=%s cgroups=%s netbackend=%s rootlessnet=%s runtime=%s\n' \
  "$(pinfo .Version.Version)" "$(pinfo .Host.Security.Rootless)" \
  "$(pinfo .Host.CgroupsVersion)" "$(pinfo .Host.NetworkBackend)" \
  "$rootlessnet" "$(pinfo .Host.OCIRuntime.Name)"
printf 'store=%s\nimage=%s\n\n' "${CONTAINERS_STORAGE_CONF:-<caller default>}" "$IMAGE"

podman pull -q "$IMAGE" >/dev/null 2>&1 || fail_prereq "cannot pull ${IMAGE}"

failures=0
note_fail() { printf '  RESULT: UNEXPECTED — %s\n' "$1"; failures=$((failures + 1)); }
note_ok() { printf '  RESULT: as documented — %s\n' "$1"; }

# CapBnd bit 12 (0x1000) is CAP_NET_ADMIN, the same bit the entrypoint tests.
capbnd_of() {
  podman run --rm "$@" --entrypoint /bin/sh "$IMAGE" \
    -c 'grep -m1 ^CapBnd: /proc/self/status | awk "{print \$2}"' 2>/dev/null
}

has_net_admin() {
  local hex="$1"
  [[ -n "$hex" ]] || return 1
  (( 0x${hex} & 0x1000 ))
}

run_case() {
  local name="$1"; shift
  local logfile="${WORK}/${name}.log"
  local status

  timeout "$CASE_TIMEOUT" podman run --rm --name "${PROBE_PREFIX}-${name}" \
    -v "${WORK}/hive.yaml:/etc/hive/hive.yaml:ro,Z" "$@" "$IMAGE" >"$logfile" 2>&1
  status=$?

  CASE_STATUS="$status"
  CASE_LOG="$logfile"

  if [[ "$status" -eq 124 ]]; then
    printf '  exit: still running at the %ss cap (started)\n' "$CASE_TIMEOUT"
  else
    printf '  exit: %s\n' "$status"
  fi

  # Started is not installed. Report both, separately, always.
  if grep -q 'outbound :443 -> :18443' "$logfile"; then
    printf '  gate: INSTALLED — %s\n' "$(grep -m1 'outbound :443 -> :18443' "$logfile" | sed 's/^\[entrypoint\] //')"
    CASE_GATE="installed"
  elif grep -q 'ADVISORY-ONLY' "$logfile"; then
    printf '  gate: NOT installed, advisory mode accepted\n'
    CASE_GATE="advisory"
  else
    printf '  gate: NOT installed\n'
    CASE_GATE="absent"
  fi
}

# ── Capability bounding sets ────────────────────────────────────────────────
printf -- '--- capability bounding set ---\n'
cap_default="$(capbnd_of)"
cap_netadmin="$(capbnd_of --cap-add NET_ADMIN)"
printf '  default          CapBnd=%s NET_ADMIN=%s\n' \
  "${cap_default:-?}" "$(has_net_admin "$cap_default" && echo yes || echo no)"
printf '  --cap-add        CapBnd=%s NET_ADMIN=%s\n' \
  "${cap_netadmin:-?}" "$(has_net_admin "$cap_netadmin" && echo yes || echo no)"

has_net_admin "$cap_default" && note_fail "rootless default already carries CAP_NET_ADMIN"
has_net_admin "$cap_netadmin" || note_fail "--cap-add NET_ADMIN did not put CAP_NET_ADMIN in the bounding set"

# ── Case 1: default rootless ────────────────────────────────────────────────
printf -- '\n--- case: default (rootless, no added capability) ---\n'
run_case default
if [[ "$CASE_STATUS" -eq 77 ]] && grep -q 'exiting 77' "$CASE_LOG"; then
  note_ok "fail-closed, exit 77 (EX_NOPERM) with the bounding-set explanation"
else
  note_fail "expected exit 77 with the CAP_NET_ADMIN bounding-set message"
fi

# ── Case 2: rootless + NET_ADMIN ────────────────────────────────────────────
printf -- '\n--- case: netadmin (rootless + --cap-add NET_ADMIN) ---\n'
run_case netadmin --cap-add NET_ADMIN
if [[ "$CASE_GATE" == "installed" ]]; then
  if grep -q 'ambient CAP_NET_ADMIN granted' "$CASE_LOG"; then
    note_ok "redirect installed and ambient CAP_NET_ADMIN survived the privilege drop"
  else
    note_fail "redirect installed but the ambient CAP_NET_ADMIN raise did not survive"
  fi
else
  note_fail "the forced-egress redirect did not install under --cap-add NET_ADMIN"
fi

# ── Case 3: deliberate advisory ─────────────────────────────────────────────
printf -- '\n--- case: advisory (rootless, HIVE_PROXY_ADVISORY_OK=true) ---\n'
run_case advisory -e HIVE_PROXY_ADVISORY_OK=true
if grep -q 'ADVISORY-ONLY' "$CASE_LOG"; then
  note_ok "started with the explicit advisory-only warning"
  grep -m1 'ADVISORY-ONLY' "$CASE_LOG" | sed 's/^/  log: /'
  grep -m1 'NOTICE: CAP_NET_ADMIN is not in the bounding set' "$CASE_LOG" | sed 's/^/  log: /'
else
  note_fail "advisory mode did not produce the ADVISORY-ONLY warning"
fi

# ── Optional: does the redirect actually intercept an agent? ────────────────
if [[ "$BYPASS" == "true" ]]; then
  printf -- '\n--- bypass resistance (needs outbound network) ---\n'
  podman rm -f "${PROBE_PREFIX}-live" >/dev/null 2>&1
  podman run -d --name "${PROBE_PREFIX}-live" --cap-add NET_ADMIN \
    -v "${WORK}/hive.yaml:/etc/hive/hive.yaml:ro,Z" "$IMAGE" >/dev/null 2>&1

  for _ in $(seq 1 45); do
    podman logs "${PROBE_PREFIX}-live" 2>&1 | grep -q 'Dropping to dev' && break
    sleep 2
  done

  printf '  installed nat rules:\n'
  podman exec "${PROBE_PREFIX}-live" iptables-nft -t nat -S HIVE_PROXY 2>&1 | sed 's/^/    /'

  agent_uid="$(podman exec "${PROBE_PREFIX}-live" \
    sh -c 'sed -n "s/.*\"probe\": \([0-9]*\).*/\1/p" /var/run/hive/uid-map.json' 2>/dev/null | head -1)"
  agent_uid="${agent_uid:-2001}"

  printf '  agent uid %s issuer: %s\n' "$agent_uid" \
    "$(podman exec -u "$agent_uid" "${PROBE_PREFIX}-live" \
        sh -c 'echo | openssl s_client -connect api.github.com:443 -servername api.github.com 2>/dev/null | grep -m1 ^issuer' 2>&1)"
  printf '  proxy uid 1001 dial: %s\n' \
    "$(podman exec -u 1001 "${PROBE_PREFIX}-live" \
        sh -c 'curl -sS -m 12 -o /dev/null -w "http_code=%{http_code} remote=%{remote_ip}:%{remote_port}" https://api.github.com' 2>&1 | tail -1)"

  podman rm -f "${PROBE_PREFIX}-live" >/dev/null 2>&1
fi

printf '\nSUMMARY: %d unexpected result(s)\n' "$failures"
[[ "$failures" -eq 0 ]] || exit 1
exit 0
