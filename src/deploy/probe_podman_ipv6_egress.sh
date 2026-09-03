#!/usr/bin/env bash
# probe_podman_ipv6_egress.sh — does the forced-proxy egress gate hold over
# IPv6? (kubestellar/hive#4319)
#
# The gate's IPv4 redirect lives in the nat table; #4319 asks whether an
# agent-UID process on a dual-stack network can reach a :443 endpoint over
# IPv6 without traversing the proxy. This probe answers it OBSERVATIONALLY:
#
#   1. creates a throwaway dual-stack (IPv4 + ULA IPv6) podman network,
#   2. runs a TARGET container on it whose :443 sends a family-stamped banner
#      on accept and closes,
#   3. runs Hive on the same network with --cap-add NET_ADMIN so the gate
#      installs normally,
#   4. from inside Hive, AS AN AGENT UID, dials the target's IPv4 and IPv6
#      addresses on :443 and reports which family (if either) reached the
#      target directly.
#
# The banner is the family evidence #4319 asks for: the target only speaks
# first, so an agent that READS it reached the target without traversing the
# proxy (the proxy never volunteers data on accept). The IPv4 dial is made in
# the same run, so a negative IPv6 result cannot be confused with a gate that
# failed to install.
#
# A ULA network is sufficient: the redirect/reject decision happens in this
# netns's OUTPUT hook, which cannot tell a ULA destination from a global one —
# no globally routable IPv6 is required.
#
# STORAGE SAFETY. As in probe_podman_rootful_netadmin.sh, this probe NEVER
# touches the host's rootful store: every podman call is pinned to a throwaway
# --root/--runroot. Cleanup removes only the containers and the network this
# script created, by exact name, and only deletes a store it created itself.
#
# Exit codes:
#   0  the gate held on BOTH families (IPv4 redirected, IPv6 blocked)
#   1  a bypass was observed (or the IPv4 gate itself did not install)
#  78  a prerequisite is missing (EX_CONFIG)

set -uo pipefail

# The image default comes from the #4206 source of truth, so the probe
# measures the same reference the deployment assets run (#4486). IMAGE in the
# environment or --image stays the deliberate override.
# shellcheck source=src/deploy/standalone-images.sh disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/standalone-images.sh"

IMAGE="${IMAGE:-$HIVE_STANDALONE_IMAGE_HIVE}"
GATE_TIMEOUT="${GATE_TIMEOUT:-120}"
PROBE_PREFIX="hive-ipv6-probe-$$"
NET_NAME="${PROBE_PREFIX}-net"
SUBNET4="${SUBNET4:-10.89.77.0/24}"
SUBNET6="${SUBNET6:-fd77:4319::/64}"
STORE="${STORE:-}"
OWN_STORE="false"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --store) STORE="$2"; shift 2 ;;
    --image) IMAGE="$2"; shift 2 ;;
    -h|--help) sed -n '2,37p' "$0"; exit 0 ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; exit 64 ;;
  esac
done

fail_prereq() {
  printf 'PREREQUISITE MISSING: %s\n' "$1" >&2
  exit 78
}

command -v podman >/dev/null 2>&1 || fail_prereq "podman is not installed or not on PATH"
[[ "$(id -u)" -eq 0 ]] || fail_prereq "this probe must run as root (it mirrors the rootful baseline of #4200); re-run with sudo"

if [[ -z "$STORE" ]]; then
  STORE="$(mktemp -d -p /var/tmp hive-ipv6-probe-store-XXXXXX)"
  OWN_STORE="true"
fi
mkdir -p "${STORE}/graph" "${STORE}/run"

host_graph="$(podman info --format '{{.Store.GraphRoot}}' 2>/dev/null || true)"
case "$STORE" in
  /var/lib/containers*|/run/containers*)
    fail_prereq "refusing to use ${STORE}: that is the host's rootful store" ;;
esac
if [[ -n "$host_graph" && "${STORE}/graph" == "$host_graph" ]]; then
  fail_prereq "refusing to use ${STORE}/graph: that is the host's graphRoot"
fi

# An ARRAY, not a function: `timeout` execs its argument and cannot run a
# shell function (see probe_podman_rootful_netadmin.sh).
POD=(podman --root "${STORE}/graph" --runroot "${STORE}/run")
pod() { "${POD[@]}" "$@"; }

WORK="$(mktemp -d)"
# shellcheck disable=SC2329 # invoked through the EXIT trap below
cleanup() {
  pod rm -f "${PROBE_PREFIX}-target" "${PROBE_PREFIX}-hive" >/dev/null 2>&1
  pod network rm -f "$NET_NAME" >/dev/null 2>&1
  rm -rf "$WORK"
  # Only a store this script created is removed. A --store passed in is kept.
  if [[ "$OWN_STORE" == "true" && -n "$STORE" ]]; then
    rm -rf "$STORE" 2>/dev/null
  fi
  return 0
}
trap cleanup EXIT

# The gate lives behind "at least one agent is configured"; this config makes
# it install and creates agent UID 2001 (HIVE_UID_BASE) for the dials below.
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

printf '== Hive IPv6 egress-gate probe (#4319) ==\n'
pod info --format 'podman={{.Version.Version}} rootless={{.Host.Security.Rootless}} cgroups={{.Host.CgroupsVersion}} netbackend={{.Host.NetworkBackend}} runtime={{.Host.OCIRuntime.Name}}'
printf 'store=%s (throwaway=%s)\nimage=%s\nnetwork=%s subnets=%s,%s\n\n' \
  "$STORE" "$OWN_STORE" "$IMAGE" "$NET_NAME" "$SUBNET4" "$SUBNET6"

rootless="$(pod info --format '{{.Host.Security.Rootless}}' 2>/dev/null)"
[[ "$rootless" == "false" ]] || \
  fail_prereq "podman reports rootless=${rootless:-unknown}; this probe requires ROOTFUL podman"

pod pull -q "$IMAGE" >/dev/null 2>&1 || fail_prereq "cannot pull ${IMAGE} into the throwaway store"

pod network create --ipv6 --subnet "$SUBNET4" --subnet "$SUBNET6" "$NET_NAME" >/dev/null \
  || fail_prereq "cannot create a dual-stack network (${SUBNET4} + ${SUBNET6})"

failures=0
note_fail() { printf '  RESULT: BYPASS/FAILURE — %s\n' "$1"; failures=$((failures + 1)); }
note_ok()   { printf '  RESULT: gate held — %s\n' "$1"; }

# ── Target: dual-stack :443 banner server ───────────────────────────────────
# Speaks FIRST on accept, stamping the family of the peer address, then
# closes. Reading the banner therefore proves a direct, un-proxied reach.
pod run -d --name "${PROBE_PREFIX}-target" --network "$NET_NAME" \
  --entrypoint python3 "$IMAGE" -u -c '
import socket
s = socket.socket(socket.AF_INET6, socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 0)
s.bind(("::", 443))
s.listen(16)
print("target listening dual-stack :443", flush=True)
while True:
    c, a = s.accept()
    fam = "V4" if a[0].startswith("::ffff:") else "V6"
    try:
        c.sendall(("TARGET-%s\n" % fam).encode())
    finally:
        c.close()
' >/dev/null || fail_prereq "target container failed to start"

target_v4="$(pod inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${PROBE_PREFIX}-target")"
target_v6="$(pod inspect -f '{{range .NetworkSettings.Networks}}{{.GlobalIPv6Address}}{{end}}' "${PROBE_PREFIX}-target")"
printf -- '--- target addresses ---\n  v4=%s\n  v6=%s\n\n' "${target_v4:-NONE}" "${target_v6:-NONE}"
[[ -n "$target_v4" && -n "$target_v6" ]] || \
  fail_prereq "the network did not assign both families to the target; a dual-stack endpoint cannot be provided here"

# ── Hive with NET_ADMIN on the same dual-stack network ──────────────────────
pod run -d --name "${PROBE_PREFIX}-hive" --network "$NET_NAME" \
  --cap-add NET_ADMIN \
  -v "${WORK}/hive.yaml:/etc/hive/hive.yaml:ro,Z" "$IMAGE" >/dev/null \
  || fail_prereq "hive container failed to start"

printf -- '--- waiting for the gate (max %ss) ---\n' "$GATE_TIMEOUT"
gate4=""; gate6=""
for (( i = 0; i < GATE_TIMEOUT; i += 3 )); do
  log="$(pod logs "${PROBE_PREFIX}-hive" 2>&1)"
  gate4="$(grep -m1 'outbound :443 -> :18443' <<<"$log" || true)"
  gate6="$(grep -m1 'outbound IPv6 :443' <<<"$log" || true)"
  [[ -n "$gate4" ]] && break
  pod container exists "${PROBE_PREFIX}-hive" || break
  sleep 3
done
printf '  ipv4: %s\n' "${gate4:-NOT INSTALLED}"
printf '  ipv6: %s\n' "${gate6:-NOT INSTALLED (pre-#4319 entrypoint, or ip6tables failed)}"
if [[ -z "$gate4" ]]; then
  note_fail "the IPv4 gate never installed; nothing below is meaningful"
  printf '\n== VERDICT: cannot evaluate (IPv4 gate absent) — %d failure(s) ==\n' "$failures"
  exit 1
fi

# ── Dials from an agent UID ─────────────────────────────────────────────────
# UID 2001 is the first agent UID (HIVE_UID_BASE) — the exact identity the
# gate exists to confine. The dialer prints BANNER (direct reach), SILENT
# (connected but nothing spoke: the redirect delivered us to the MITM proxy,
# which waits for the client), or BLOCKED (reset/refused/timeout).
dial_as_agent() {
  local addr="$1"
  pod exec -u 2001 "${PROBE_PREFIX}-hive" python3 -c '
import socket, sys
addr = sys.argv[1]
fam = socket.AF_INET6 if ":" in addr else socket.AF_INET
s = socket.socket(fam, socket.SOCK_STREAM)
s.settimeout(8)
try:
    s.connect((addr, 443))
    try:
        data = s.recv(64)
    except OSError as e:
        print("BLOCKED post-connect %s: %s" % (type(e).__name__, e)); sys.exit(0)
    if data:
        print("BANNER %s" % data.decode(errors="replace").strip())
    else:
        print("SILENT (connected, peer sent nothing)")
except OSError as e:
    print("BLOCKED %s: %s" % (type(e).__name__, e))
finally:
    s.close()
' "$addr" 2>&1
}

printf -- '\n--- agent-UID dial: IPv4 %s:443 ---\n' "$target_v4"
r4="$(dial_as_agent "$target_v4")"
printf '  %s\n' "$r4"
case "$r4" in
  *"BANNER TARGET-V4"*) note_fail "agent reached the target DIRECTLY over IPv4 — the redirect did not apply" ;;
  *SILENT*|*BLOCKED*)   note_ok "IPv4 dial did not reach the target (redirected into the proxy, as documented)" ;;
  *)                    note_fail "unexpected IPv4 dial outcome: $r4" ;;
esac

printf -- '\n--- agent-UID dial: IPv6 [%s]:443 ---\n' "$target_v6"
r6="$(dial_as_agent "$target_v6")"
printf '  %s\n' "$r6"
case "$r6" in
  *"BANNER TARGET-V6"*) note_fail "agent reached the target DIRECTLY over IPv6 — the #4319 bypass is REAL on this host" ;;
  *BLOCKED*)            note_ok "IPv6 dial was blocked (ip6tables REJECT held)" ;;
  *SILENT*)             note_fail "IPv6 dial CONNECTED (no redirect target exists on IPv6; a connect is an escape even without the banner)" ;;
  *)                    note_fail "unexpected IPv6 dial outcome: $r6" ;;
esac

printf '\n== VERDICT: %s — %d failure(s) ==\n' \
  "$( ((failures == 0)) && echo 'gate held on both families' || echo 'bypass or gate failure observed' )" \
  "$failures"
(( failures == 0 )) && exit 0 || exit 1
