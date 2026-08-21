#!/usr/bin/env bash
# The Podman half of the published-port boundary (kubestellar/hive#4375).
# Run: bash src/deploy/test_quadlet_port_boundary.sh
#
# src/deploy/test_standalone_service_contract.sh (#4204) asserts this boundary
# for the Docker Compose stack and says why it needs a guard at all:
#
#     A Podman asset that publishes 7681 still starts and still serves the
#     dashboard; it just also serves an unauthenticated root shell.
#
# That test parses docker-compose.yaml and cannot see a Quadlet unit. This is
# the same set of invariants for src/deploy/quadlet/, so the two runtimes are
# guarded rather than one being guarded and the other reviewed.
#
# What is asserted, and why each one is invisible at runtime when it breaks:
#
#   - The gateway publishes host 3001 -> container 3001 and nothing else. A
#     second PublishPort= is the edit that turns this deployment into a remote
#     root shell, and the dashboard keeps working while it does.
#   - NOTHING under quadlet/ names 7681 on either side of a PublishPort=.
#     Swept across every unit, not just the two that exist today.
#   - hive.container publishes nothing at all. Hive is reached through the
#     gateway; a PublishPort= there bypasses authentication without breaking
#     anything visible.
#   - Hive and the gateway share one network, referenced by unit name, and the
#     gateway is ordered after Hive. Without the shared network nginx cannot
#     resolve `hive`; without the ordering it serves 502s on the port the
#     operator was told to trust.
#   - hive.network leaves DisableDNS/Internal/IPv6 at their defaults. Each has
#     its own failure mode and IPv6 is the dangerous one: the forced-proxy
#     egress gate is IPv4-only (src/docs/podman-ipv6-egress-bypass.md), so an
#     IPv6-enabled network is an egress bypass that nothing reports.
#   - The gateway's nginx.conf mount stays read-only.
#
# Pure text analysis of the unit files: nothing is started, no image is pulled,
# and podman does not have to be installed. Unit syntax is separately gated by
# src/deploy/test_quadlet_generator_gate.sh (#4338), which runs the real
# generator; this asserts the semantics that generator exits 0 on.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
UNIT_DIR="${ROOT}/src/deploy/quadlet"

PASS=0
FAIL=0

pass() { printf '  PASS: %s\n' "$1"; PASS=$((PASS + 1)); }
fail() {
  printf '  FAIL: %s\n' "$1"
  [[ $# -gt 1 ]] && printf '        %s\n' "$2"
  FAIL=$((FAIL + 1))
}

# systemd treats a line whose first non-blank character is `#` or `;` as a
# comment, and nothing else. Stripping them matters here because the units
# discuss `PublishPort=` in prose at length — a grep over the raw file would
# match the warning not to add one.
unit_values() {
  local file="$1" key="$2"
  sed -e 's/\r$//' "$file" \
    | grep -v '^[[:space:]]*[#;]' \
    | grep -E "^[[:space:]]*${key}[[:space:]]*=" \
    | sed -E "s/^[[:space:]]*${key}[[:space:]]*=[[:space:]]*//" \
    | sed -e 's/[[:space:]]*$//'
}

printf '=== Quadlet published-port boundary (#4375) ===\n\n'

if [[ ! -d "$UNIT_DIR" ]]; then
  printf '  FAIL: src/deploy/quadlet/ exists — not found at %s\n' "$UNIT_DIR"
  exit 1
fi

mapfile -t UNITS < <(find "$UNIT_DIR" -maxdepth 1 -type f \
  \( -name '*.container' -o -name '*.pod' -o -name '*.kube' \
     -o -name '*.network' -o -name '*.volume' \) | sort)

if [[ "${#UNITS[@]}" -eq 0 ]]; then
  printf '  FAIL: at least one Quadlet unit exists — none found under %s\n' "$UNIT_DIR"
  exit 1
fi

HIVE_UNIT="${UNIT_DIR}/hive.container"
GW_UNIT="${UNIT_DIR}/hive-gateway.container"
NET_UNIT="${UNIT_DIR}/hive.network"

for required in "$HIVE_UNIT" "$GW_UNIT" "$NET_UNIT"; do
  if [[ -f "$required" ]]; then
    pass "${required#"${ROOT}/"} exists"
  else
    fail "${required#"${ROOT}/"} exists" \
         "the standalone Podman stack is hive + gateway on a shared network"
  fi
done

# Bail out rather than emit a cascade of confusing failures against files that
# are not there.
[[ -f "$HIVE_UNIT" && -f "$GW_UNIT" && -f "$NET_UNIT" ]] || {
  printf '\n=== Results: %d passed, %d failed ===\n' "$PASS" "$FAIL"
  exit 1
}

# --- Exposure boundary -------------------------------------------------------

printf '\n--- exposure boundary ---\n'

# PublishPort is [[ip:][hostPort]:]containerPort[/protocol]. Prints
# "<host>\t<container>" with the protocol and any bind address dropped, so an
# assertion does not have to care which spelling a unit used.
publish_pairs() {
  local file="$1" spec host container
  while IFS= read -r spec; do
    [[ -n "$spec" ]] || continue
    spec="${spec%%/*}"
    container="${spec##*:}"
    if [[ "$spec" == *:* ]]; then
      host="${spec%:*}"
      host="${host##*:}"
    else
      host=""
    fi
    printf '%s\t%s\n' "$host" "$container"
  done < <(unit_values "$file" "PublishPort")
}

gw_pairs="$(publish_pairs "$GW_UNIT")"
if [[ "$gw_pairs" == $'3001\t3001' ]]; then
  pass "the gateway publishes exactly one port, host 3001 -> container 3001"
else
  fail "the gateway publishes exactly one port, host 3001 -> container 3001" \
       "got $(printf '%q' "$gw_pairs") — the authenticated proxy port is the only way in"
fi

hive_pairs="$(publish_pairs "$HIVE_UNIT")"
if [[ -z "$hive_pairs" ]]; then
  pass "hive.container publishes no host ports"
else
  fail "hive.container publishes no host ports" \
       "got $(printf '%q' "$hive_pairs") — Hive is reached through the gateway, not directly"
fi

# Swept over every unit in the directory, including any that land later: no
# Quadlet asset in this repository may put 7681 on either side of a publish.
offenders=""
for unit in "${UNITS[@]}"; do
  while IFS=$'\t' read -r host container; do
    [[ -n "${host}${container}" ]] || continue
    if [[ "$host" == "7681" || "$container" == "7681" ]]; then
      offenders+="${unit#"${ROOT}/"}:${host}:${container} "
    fi
  done < <(publish_pairs "$unit")
done
if [[ -z "$offenders" ]]; then
  pass "no Quadlet unit publishes the raw ttyd port 7681"
else
  fail "no Quadlet unit publishes the raw ttyd port 7681" \
       "got ${offenders}— this publishes an unauthenticated shell into the credential-holding container"
fi

# Same sweep for the whole directory: 3001 on the gateway is the only publish
# any unit here is allowed to carry.
extra=""
for unit in "${UNITS[@]}"; do
  [[ "$unit" == "$GW_UNIT" ]] && continue
  pairs="$(publish_pairs "$unit")"
  [[ -n "$pairs" ]] && extra+="${unit#"${ROOT}/"} "
done
if [[ -z "$extra" ]]; then
  pass "the gateway holds the only PublishPort= in src/deploy/quadlet/"
else
  fail "the gateway holds the only PublishPort= in src/deploy/quadlet/" \
       "also published by: ${extra}"
fi

# --- The shared network ------------------------------------------------------

printf '\n--- shared network ---\n'

hive_nets="$(unit_values "$HIVE_UNIT" "Network")"
gw_nets="$(unit_values "$GW_UNIT" "Network")"
if [[ "$hive_nets" == "hive.network" && "$gw_nets" == "hive.network" ]]; then
  pass "hive and the gateway both join hive.network, by unit name"
else
  fail "hive and the gateway both join hive.network, by unit name" \
       "hive=$(printf '%q' "$hive_nets") gateway=$(printf '%q' "$gw_nets") — referencing the unit is what makes the generated service order itself after hive-network.service, and the shared network is what makes nginx's 'server hive:3001' resolve"
fi

net_name="$(unit_values "$NET_UNIT" "NetworkName")"
if [[ "$net_name" == "hive" ]]; then
  pass "hive.network sets NetworkName=hive rather than taking the systemd- prefix"
else
  fail "hive.network sets NetworkName=hive rather than taking the systemd- prefix" \
       "got $(printf '%q' "$net_name")"
fi

# Three defaults that are load-bearing. Each is asserted as "not turned on"
# rather than "absent", so writing the default out explicitly still passes.
check_off() {
  local key="$1" why="$2" value
  value="$(unit_values "$NET_UNIT" "$key" | tail -n 1)"
  case "${value,,}" in
    ""|false|no|0)
      pass "hive.network leaves ${key} off"
      ;;
    *)
      fail "hive.network leaves ${key} off" "got ${key}=${value} — ${why}"
      ;;
  esac
}
check_off "DisableDNS" "aardvark-dns is what makes 'hive' resolve for the gateway; with DNS off nginx cannot find its upstream and :3001 serves 502s"
check_off "Internal" "Hive's agents have to reach GitHub and the model APIs; an internal network has no route off the host"
check_off "IPv6" "the forced-proxy egress gate in src/deploy/entrypoint.sh is iptables-nft only and has zero ip6tables rules, so an IPv6-enabled network is a measured egress bypass — see src/docs/podman-ipv6-egress-bypass.md"

# --- Readiness ordering ------------------------------------------------------

printf '\n--- readiness ordering ---\n'

gw_requires="$(unit_values "$GW_UNIT" "Requires")"
gw_after="$(unit_values "$GW_UNIT" "After")"
if grep -qx 'hive.service' <<<"$gw_requires"; then
  pass "the gateway Requires=hive.service"
else
  fail "the gateway Requires=hive.service" \
       "got $(printf '%q' "$gw_requires") — a gateway answering on :3001 with Hive absent is worse than a port that refuses connections"
fi
if grep -qx 'hive.service' <<<"$gw_after"; then
  pass "the gateway is ordered After=hive.service"
else
  fail "the gateway is ordered After=hive.service" \
       "got $(printf '%q' "$gw_after") — without it the gateway serves errors on the port operators were told to trust"
fi

# After=hive.service only means "after Hive is HEALTHY" because hive.container
# is a notify unit. Drop Notify=healthy there and the ordering silently weakens
# to "after conmon started" without this file changing at all.
hive_notify="$(unit_values "$HIVE_UNIT" "Notify" | tail -n 1)"
if [[ "$hive_notify" == "healthy" ]]; then
  pass "hive.container still sets Notify=healthy, which is what makes After= mean 'after healthy'"
else
  fail "hive.container still sets Notify=healthy, which is what makes After= mean 'after healthy'" \
       "got $(printf '%q' "$hive_notify") — the gateway's ordering degrades to 'after the process spawned' with no edit to hive-gateway.container"
fi

for unit_path in "$HIVE_UNIT" "$GW_UNIT"; do
  unit_rel="${unit_path#"${ROOT}/"}"
  if [[ -n "$(unit_values "$unit_path" "HealthCmd")" ]]; then
    pass "${unit_rel} defines a HealthCmd"
  else
    fail "${unit_rel} defines a HealthCmd" \
         "the Quadlet generator accepts Notify=healthy with no healthcheck and exits 0, producing a unit that reports started the moment conmon is up"
  fi
done

# --- Read-only config mount --------------------------------------------------

printf '\n--- read-only config mount ---\n'

gw_conf_mounts="$(unit_values "$GW_UNIT" "Volume" | grep ':/etc/nginx/nginx.conf' || true)"
if [[ -z "$gw_conf_mounts" ]]; then
  fail "the gateway mounts nginx.conf read-only" \
       "no /etc/nginx/nginx.conf mount found — the gateway would serve the image's default config, which proxies nothing"
elif grep -q ':/etc/nginx/nginx.conf:[^:]*\bro\b' <<<"$gw_conf_mounts" \
  || grep -qE ':/etc/nginx/nginx\.conf:([^:]*,)?ro(,[^:]*)?$' <<<"$gw_conf_mounts"; then
  pass "the gateway mounts nginx.conf read-only"
else
  fail "the gateway mounts nginx.conf read-only" \
       "got $(printf '%q' "$gw_conf_mounts") — the container must not be able to rewrite the config it serves from"
fi

printf '\n=== Results: %d passed, %d failed ===\n' "$PASS" "$FAIL"
[[ "$FAIL" -eq 0 ]]
