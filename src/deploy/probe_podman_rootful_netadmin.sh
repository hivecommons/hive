#!/usr/bin/env bash
# probe_podman_rootful_netadmin.sh — rootful Podman baseline for Hive's
# forced-proxy egress gate (kubestellar/hive#4200).
#
# The rootless counterpart is probe_podman_rootless_netadmin.sh (#4199). This
# script asks the same questions of ROOTFUL Podman, so the rootless result has a
# known-good baseline to be compared against, and adds the one test the rootless
# spike could not do: isolating the SO_MARK exemption from the owner-UID one.
#
# Cases:
#   default    rootful, no added capability   -> expect fail-closed, exit 77
#   netadmin   rootful + --cap-add NET_ADMIN  -> expect gate installed
#   advisory   rootful + HIVE_PROXY_ADVISORY_OK=true -> expect advisory warning
#   somark     (--somark) delete the owner-UID RETURNs from a live HIVE_PROXY
#              chain and re-dial, so the packet-mark exemption is exercised on
#              its own rather than shadowed by the owner match.
#
# STORAGE SAFETY. Rootful Podman on a workstation is usually where the long-
# lived services live. This probe therefore NEVER touches the host's rootful
# store: every podman call goes through pod(), which pins --root/--runroot to a
# throwaway directory. Cleanup removes only the containers this script created,
# by exact name, and only deletes a store this script created itself.
#
# Exit codes:
#   0  every case matched what src/docs/podman-rootful-egress-baseline.md records
#   1  at least one case did not match
#  78  a prerequisite is missing (EX_CONFIG)

set -uo pipefail

# The image default comes from the #4206 source of truth, so the probe
# measures the same reference the deployment assets run (#4486). IMAGE in the
# environment or --image stays the deliberate override.
# shellcheck source=src/deploy/standalone-images.sh disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/standalone-images.sh"

IMAGE="${IMAGE:-$HIVE_STANDALONE_IMAGE_HIVE}"
CASE_TIMEOUT="${CASE_TIMEOUT:-90}"
PROBE_PREFIX="hive-rootful-probe-$$"
STORE="${STORE:-}"
OWN_STORE="false"
SOMARK="false"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --somark) SOMARK="true"; shift ;;
    --store)  STORE="$2"; shift 2 ;;
    --image)  IMAGE="$2"; shift 2 ;;
    -h|--help) sed -n '2,27p' "$0"; exit 0 ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; exit 64 ;;
  esac
done

fail_prereq() {
  printf 'PREREQUISITE MISSING: %s\n' "$1" >&2
  exit 78
}

command -v podman >/dev/null 2>&1 || fail_prereq "podman is not installed or not on PATH"
[[ "$(id -u)" -eq 0 ]] || fail_prereq "this probe must run as root (rootless Podman is #4199); re-run with sudo"

# A throwaway store, on disk rather than tmpfs: the image is measured in GB and
# a tmpfs store would spend RAM for it.
if [[ -z "$STORE" ]]; then
  STORE="$(mktemp -d -p /var/tmp hive-rootful-probe-store-XXXXXX)"
  OWN_STORE="true"
fi
mkdir -p "${STORE}/graph" "${STORE}/run"

# Refuse to address the host's own rootful store, whatever was passed in. On a
# workstation that store holds long-lived services, and a probe has no business
# writing to it.
host_graph="$(podman info --format '{{.Store.GraphRoot}}' 2>/dev/null || true)"
case "$STORE" in
  /var/lib/containers*|/run/containers*)
    fail_prereq "refusing to use ${STORE}: that is the host's rootful store" ;;
esac
if [[ -n "$host_graph" && "${STORE}/graph" == "$host_graph" ]]; then
  fail_prereq "refusing to use ${STORE}/graph: that is the host's graphRoot"
fi

# Every podman call in this script goes through here. Nothing addresses the
# host's rootful store.
# An ARRAY, not a function: `timeout` execs its argument, and cannot run a shell
# function — a function here makes every timed case exit 127 instead of running.
POD=(podman --root "${STORE}/graph" --runroot "${STORE}/run")
pod() { "${POD[@]}" "$@"; }

WORK="$(mktemp -d)"
# shellcheck disable=SC2329 # invoked through the EXIT trap below
cleanup() {
  local name
  for name in "${PROBE_PREFIX}-default" "${PROBE_PREFIX}-netadmin" \
              "${PROBE_PREFIX}-advisory" "${PROBE_PREFIX}-live"; do
    pod rm -f "$name" >/dev/null 2>&1
  done
  rm -rf "$WORK"
  # Only a store this script created is removed. A --store passed in is kept.
  if [[ "$OWN_STORE" == "true" && -n "$STORE" ]]; then
    rm -rf "$STORE" 2>/dev/null
  fi
  return 0
}
trap cleanup EXIT

# The gate lives behind "at least one agent is configured", so a config with no
# agents skips it and the container starts clean while proving nothing.
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

printf '== Hive rootful Podman egress-gate baseline (#4200) ==\n'
pod info --format 'podman={{.Version.Version}} rootless={{.Host.Security.Rootless}} cgroups={{.Host.CgroupsVersion}} netbackend={{.Host.NetworkBackend}} runtime={{.Host.OCIRuntime.Name}}'
printf 'store=%s (throwaway=%s)\nimage=%s\n\n' "$STORE" "$OWN_STORE" "$IMAGE"

rootless="$(pod info --format '{{.Host.Security.Rootless}}' 2>/dev/null)"
[[ "$rootless" == "false" ]] || \
  fail_prereq "podman reports rootless=${rootless:-unknown}; this probe requires ROOTFUL podman (rootless is #4199)"

pod pull -q "$IMAGE" >/dev/null 2>&1 || fail_prereq "cannot pull ${IMAGE} into the throwaway store"

failures=0
note_fail() { printf '  RESULT: UNEXPECTED — %s\n' "$1"; failures=$((failures + 1)); }
note_ok()   { printf '  RESULT: as documented — %s\n' "$1"; }

# CapBnd bit 12 (0x1000) is CAP_NET_ADMIN, the bit the entrypoint tests.
capbnd_of() {
  # shellcheck disable=SC2016 # $2 is awk's field, expanded in the container, not here
  pod run --rm "$@" --entrypoint /bin/sh "$IMAGE" \
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

  timeout "$CASE_TIMEOUT" "${POD[@]}" run --rm --name "${PROBE_PREFIX}-${name}" \
    -v "${WORK}/hive.yaml:/etc/hive/hive.yaml:ro,Z" "$@" "$IMAGE" >"$logfile" 2>&1
  status=$?

  CASE_STATUS="$status"
  CASE_LOG="$logfile"

  if [[ "$status" -eq 124 ]]; then
    printf '  exit: still running at the %ss cap (started)\n' "$CASE_TIMEOUT"
  else
    printf '  exit: %s\n' "$status"
  fi

  # Started and enforcing are different questions. Report both, separately.
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

has_net_admin "$cap_default" && note_fail "rootful default already carries CAP_NET_ADMIN"
has_net_admin "$cap_netadmin" || note_fail "--cap-add NET_ADMIN did not put CAP_NET_ADMIN in the bounding set"

# ── Case 1: default rootful ─────────────────────────────────────────────────
printf -- '\n--- case: default (rootful, no added capability) ---\n'
run_case default
if [[ "$CASE_STATUS" -eq 77 ]] && grep -q 'exiting 77' "$CASE_LOG"; then
  note_ok "fail-closed, exit 77 (EX_NOPERM) with the bounding-set explanation"
else
  note_fail "expected exit 77 with the CAP_NET_ADMIN bounding-set message"
fi

# ── Case 2: rootful + NET_ADMIN ─────────────────────────────────────────────
printf -- '\n--- case: netadmin (rootful + --cap-add NET_ADMIN) ---\n'
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
printf -- '\n--- case: advisory (rootful, HIVE_PROXY_ADVISORY_OK=true) ---\n'
run_case advisory -e HIVE_PROXY_ADVISORY_OK=true
if grep -q 'ADVISORY-ONLY' "$CASE_LOG"; then
  note_ok "started with the explicit advisory-only warning"
  grep -m1 'ADVISORY-ONLY' "$CASE_LOG" | sed 's/^/  log: /'
else
  note_fail "advisory mode did not produce the ADVISORY-ONLY warning"
fi

# ── Optional: isolate the SO_MARK exemption ─────────────────────────────────
# The rootless spike could not tell which RETURN matched, because the owner-UID
# rule and the mark rule were both in the chain. Deleting the owner rules leaves
# the mark as the only exemption, which is the OpenShift/OVN configuration.
if [[ "$SOMARK" == "true" ]]; then
  printf -- '\n--- SO_MARK isolation (needs outbound network) ---\n'
  pod rm -f "${PROBE_PREFIX}-live" >/dev/null 2>&1
  pod run -d --name "${PROBE_PREFIX}-live" --cap-add NET_ADMIN \
    -v "${WORK}/hive.yaml:/etc/hive/hive.yaml:ro,Z" "$IMAGE" >/dev/null 2>&1

  for _ in $(seq 1 45); do
    pod logs "${PROBE_PREFIX}-live" 2>&1 | grep -q 'Dropping to dev' && break
    sleep 2
  done

  printf '  chain as installed:\n'
  pod exec "${PROBE_PREFIX}-live" iptables-nft -t nat -S HIVE_PROXY 2>&1 | sed 's/^/    /'

  # CONTROL. curl exec'd as the proxy UID carries NO packet mark: SO_MARK is set
  # by the proxy's own dialer (pkg/proxy/somark_linux.go), and an exec'd process
  # gets neither that code path nor the ambient CAP_NET_ADMIN that setting a mark
  # requires. So this line measures the OWNER rule only, never the mark.
  printf '  control, unmarked uid-1001 dial, owner rules present: %s\n' \
    "$(pod exec -u 1001 "${PROBE_PREFIX}-live" \
        sh -c 'curl -sS -m 12 -o /dev/null -w "http_code=%{http_code}" https://api.github.com' 2>&1 | tail -1)"

  # THE REAL TEST. An AGENT-uid request is redirected into the proxy, which then
  # makes its own upstream dial — and that dial is the one carrying SO_MARK. If it
  # still completes once the owner RETURNs are gone, the mark path carried it.
  agent_uid="$(pod exec "${PROBE_PREFIX}-live" \
    sh -c 'sed -n "s/.*\"probe\": \([0-9]*\).*/\1/p" /var/run/hive/uid-map.json' 2>/dev/null | head -1)"
  agent_uid="${agent_uid:-2001}"
  printf '  agent uid: %s\n' "$agent_uid"

  agent_before="$(pod exec -u "$agent_uid" "${PROBE_PREFIX}-live" \
      sh -c 'curl -sS -k -m 15 -o /dev/null -w "http_code=%{http_code}" https://api.github.com' 2>&1 | tail -1)"
  printf '  agent through proxy, owner rules present:            %s\n' "$agent_before"

  # Remove BOTH owner-UID RETURNs. The mark RETURN is then the only exemption --
  # the OpenShift/OVN configuration, where xt_owner is absent.
  pod exec "${PROBE_PREFIX}-live" \
    sh -c 'iptables-nft -t nat -D HIVE_PROXY -m owner --uid-owner 0 -j RETURN 2>/dev/null;
           iptables-nft -t nat -D HIVE_PROXY -m owner --uid-owner 1001 -j RETURN 2>/dev/null; true' >/dev/null 2>&1

  printf '  chain with owner RETURNs removed:\n'
  pod exec "${PROBE_PREFIX}-live" iptables-nft -t nat -S HIVE_PROXY 2>&1 | sed 's/^/    /'

  printf '  control, unmarked uid-1001 dial, owner rules gone:   %s\n' \
    "$(pod exec -u 1001 "${PROBE_PREFIX}-live" \
        sh -c 'curl -sS -m 12 -o /dev/null -w "http_code=%{http_code}" https://api.github.com' 2>&1 | tail -1)"

  agent_after="$(pod exec -u "$agent_uid" "${PROBE_PREFIX}-live" \
      sh -c 'curl -sS -k -m 15 -o /dev/null -w "http_code=%{http_code}" https://api.github.com' 2>&1 | tail -1)"
  printf '  agent through proxy, MARK path alone:                %s\n' "$agent_after"

  if printf '%s' "$agent_before" | grep -q 'http_code=[23]'; then
    if printf '%s' "$agent_after" | grep -q 'http_code=[23]'; then
      note_ok "SO_MARK alone carries the proxy's upstream dial with no owner rule present"
    else
      note_fail "proxy upstream broke once only the mark exemption remained (the #2678 shape)"
    fi
  else
    printf '  RESULT: INCONCLUSIVE — the agent path did not work even WITH the owner rules,\n'
    printf '          so this run cannot say anything about the mark path.\n'
  fi

  pod rm -f "${PROBE_PREFIX}-live" >/dev/null 2>&1
fi

printf '\nSUMMARY: %d unexpected result(s)\n' "$failures"
[[ "$failures" -eq 0 ]] || exit 1
exit 0
