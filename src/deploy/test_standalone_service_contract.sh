#!/usr/bin/env bash
# Snapshots the standalone deployment invariants the Podman work must preserve
# (kubestellar/hive#4204).
#
# The existing deployment guards cover the auto-update profile
# (test_watchtower_socket_contract.sh: socket containment, profile gating, the
# proxy's ports/networks/API sections) and the build inputs
# (test_supply_chain_pins.sh: digest pins). Nothing asserted anything about the
# two services an operator actually runs. This covers a focused subset for
# `hive` and `gateway` -- exposure boundary, capabilities, persistence,
# readiness, and read-only config/secret mounts -- and deliberately does not
# re-test what those two already own.
#
# Why these: each is invisible at runtime when it breaks. A Podman asset that
# publishes 7681 still starts and still serves the dashboard; it just also
# serves an unauthenticated root shell. A `/data` bind mount instead of a named
# volume still runs; it just loses the agent state on the next recreate. A
# missing NET_ADMIN still builds; the container then exits 77 at deploy time
# (see src/docs/podman-rootless-startup-spike.md). None of these fail a review
# by looking wrong, which is why they need a guard.
#
# This does NOT claim Docker/Podman parity. It snapshots one subset; further
# invariants belong in separate small issues.
#
# Runs without starting a container.
# Run: bash src/deploy/test_standalone_service_contract.sh
set -euo pipefail

PASS=0
FAIL=0

SRC_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMPOSE="$SRC_DIR/docker-compose.yaml"

pass() {
  echo "  PASS: $1"
  PASS=$((PASS + 1))
}

fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && echo "        $2"
  FAIL=$((FAIL + 1))
}

echo "=== standalone service deployment contract (#4204) ==="

if [ ! -f "$COMPOSE" ]; then
  echo "  FAIL: docker-compose.yaml exists — not found at $COMPOSE"
  exit 1
fi

# Prefer a real compose render so the assertions run against the RESOLVED
# config, with the short port syntax already normalised. Fall back to a plain
# YAML parse so this still guards on a machine without Docker -- including the
# Podman-only hosts this contract exists to protect.
RENDER=""
if command -v docker >/dev/null 2>&1 && docker compose -f "$COMPOSE" --profile auto-update config >/dev/null 2>&1; then
  RENDER="$(docker compose -f "$COMPOSE" --profile auto-update config 2>/dev/null)"
  pass "compose file renders (docker compose config)"
else
  echo "  note: docker CLI unavailable — falling back to raw YAML parse"
fi

if ! command -v python3 >/dev/null 2>&1; then
  echo "  SKIP: python3 unavailable — cannot make structural assertions"
  exit 0
fi
if ! python3 -c 'import yaml' >/dev/null 2>&1; then
  echo "  SKIP: PyYAML unavailable — cannot make structural assertions"
  exit 0
fi

RESULTS="$(RENDER="$RENDER" COMPOSE="$COMPOSE" python3 <<'PY'
import os, yaml

render = os.environ.get("RENDER") or ""
if render.strip():
    doc = yaml.safe_load(render)
else:
    doc = yaml.safe_load(open(os.environ["COMPOSE"]))

services = doc.get("services") or {}
top_volumes = doc.get("volumes") or {}
out = []
def ok(msg): out.append(("PASS", msg, ""))
def bad(msg, detail=""): out.append(("FAIL", msg, detail))

# The raw file uses the short "host:container" strings; `docker compose config`
# renders the long form. Normalise both to a list of (published, target).
def published_ports(svc):
    spec = services.get(svc, {}).get("ports") or []
    pairs = []
    for p in spec:
        if isinstance(p, dict):
            target = p.get("target")
            pub = p.get("published")
            pairs.append((str(pub) if pub is not None else "", str(target)))
        else:
            bits = str(p).split(":")
            if len(bits) == 1:
                pairs.append(("", bits[0]))
            else:
                # host:container, or ip:host:container
                pairs.append((bits[-2], bits[-1]))
    return [(h, c.split("/")[0]) for h, c in pairs]

def exposed_ports(svc):
    return [str(p).split("/")[0] for p in (services.get(svc, {}).get("expose") or [])]

# Volumes have two spellings too: the raw file uses "source:target:opts"
# strings, `docker compose config` renders dicts. Normalise to one shape.
def volumes_of(svc):
    mounts = []
    for v in (services.get(svc, {}).get("volumes") or []):
        if isinstance(v, dict):
            src = v.get("source") or ""
            mounts.append({
                "source": src,
                "target": v.get("target") or "",
                "ro": bool(v.get("read_only")),
                "bind": (v.get("type") == "bind"),
                "raw": v,
            })
        else:
            bits = str(v).split(":")
            src = bits[0] if len(bits) > 1 else ""
            target = bits[1] if len(bits) > 1 else bits[0]
            opts = bits[2].split(",") if len(bits) > 2 else []
            mounts.append({
                "source": src,
                "target": target,
                "ro": ("ro" in opts),
                "bind": src.startswith(".") or src.startswith("/"),
                "raw": str(v),
            })
    return mounts

def mounts_at(svc, target):
    return [m for m in volumes_of(svc) if m["target"] == target]

for required in ("hive", "gateway"):
    if required not in services:
        bad(f"{required} service exists", "the standalone stack is hive + gateway")

# --- Exposure boundary -------------------------------------------------------
#
# The whole point of the gateway is that the only way in is an authenticated
# port. 7681 is the raw writable ttyd terminal inside the credential-holding
# container; publishing it is a remote root shell, and it once was published.

gw = published_ports("gateway")
if [c for _, c in gw] == ["3001"]:
    ok("gateway publishes exactly one port, container 3001")
else:
    bad("gateway publishes exactly one port, container 3001",
        f"got {gw!r} — the authenticated proxy port is the only way in")

hive_pub = published_ports("hive")
if not hive_pub:
    ok("hive publishes no host ports")
else:
    bad("hive publishes no host ports",
        f"got {hive_pub!r} — hive is reached through the gateway, not directly")

# Swept across EVERY service, profile services included: no configuration of
# this stack may put 7681 on a host port.
offenders = {s: [p for p in published_ports(s) if p[1] == "7681" or p[0] == "7681"]
             for s in services}
offenders = {s: p for s, p in offenders.items() if p}
if not offenders:
    ok("no service publishes the raw ttyd port 7681")
else:
    bad("no service publishes the raw ttyd port 7681",
        f"got {offenders!r} — this publishes an unauthenticated shell into the "
        "credential-holding container")

if "7681" in exposed_ports("hive"):
    ok("hive still exposes 7681 internally (reachable by the gateway only)")
else:
    bad("hive still exposes 7681 internally (reachable by the gateway only)",
        f"got expose={exposed_ports('hive')!r} — the terminal must stay "
        "reachable in-network without being published")

# --- Capabilities ------------------------------------------------------------
#
# NET_ADMIN is what lets the entrypoint install the forced-proxy egress
# redirect. Without it the container fails closed with exit 77 rather than
# running with an unenforced capability model.

caps = [str(c).upper() for c in (services.get("hive", {}).get("cap_add") or [])]
if "NET_ADMIN" in caps:
    ok("hive requests NET_ADMIN for the forced-proxy egress gate")
else:
    bad("hive requests NET_ADMIN for the forced-proxy egress gate",
        f"got cap_add={caps!r} — without it the entrypoint exits 77 (EX_NOPERM)")

# --- Persistence -------------------------------------------------------------
#
# /data must be a NAMED volume declared at the top level. An anonymous volume
# or a bind mount still runs and still looks fine; it just does not survive a
# recreate the way the operator docs promise.

data_mounts = mounts_at("hive", "/data")
if len(data_mounts) == 1:
    m = data_mounts[0]
    if m["bind"]:
        bad("hive persists /data on a named volume",
            f"got {m['raw']!r} — a bind mount is not the documented "
            "persistence contract")
    elif m["source"] in top_volumes:
        ok(f"hive persists /data on the named volume '{m['source']}'")
    else:
        bad("hive persists /data on a named volume",
            f"'{m['source']}' is not declared in the top-level volumes: block")
else:
    bad("hive persists /data on a named volume",
        f"got {[m['raw'] for m in data_mounts]!r} — expected exactly one /data mount")

# --- Readiness ---------------------------------------------------------------
#
# The gateway proxies to hive. Starting it before hive is healthy serves errors
# on the port the operator was told to trust.

for svc in ("hive", "gateway"):
    if (services.get(svc, {}).get("healthcheck") or {}).get("test"):
        ok(f"{svc} defines a healthcheck")
    else:
        bad(f"{svc} defines a healthcheck",
            "readiness is what the gateway ordering and any Podman "
            "Notify=healthy unit depend on")

dep = services.get("gateway", {}).get("depends_on") or {}
cond = dep.get("hive", {}).get("condition") if isinstance(dep, dict) else None
if cond == "service_healthy":
    ok("gateway waits for hive to be healthy, not merely started")
else:
    bad("gateway waits for hive to be healthy, not merely started",
        f"got {cond!r} — service_started lets the gateway serve errors on :3001")

# --- Read-only config and secret mounts --------------------------------------
#
# The container must not be able to rewrite the nginx config it serves from or
# the secrets it reads. Podman assets carry the same mounts with an added
# SELinux relabel suffix; the :ro must survive that edit.

gw_conf = mounts_at("gateway", "/etc/nginx/nginx.conf")
if gw_conf and all(m["ro"] for m in gw_conf):
    ok("gateway mounts nginx.conf read-only")
else:
    bad("gateway mounts nginx.conf read-only",
        f"got {[m['raw'] for m in gw_conf]!r} — the container must not be able "
        "to rewrite the config it serves from")

secrets = mounts_at("hive", "/secrets")
if secrets and all(m["ro"] for m in secrets):
    ok("hive mounts /secrets read-only")
else:
    bad("hive mounts /secrets read-only",
        f"got {[m['raw'] for m in secrets]!r}")

for status, msg, detail in out:
    print(f"{status}\t{msg}\t{detail}")
PY
)"

while IFS=$'\t' read -r status msg detail; do
  [ -n "$status" ] || continue
  if [ "$status" = "PASS" ]; then
    pass "$msg"
  else
    fail "$msg" "$detail"
  fi
done <<<"$RESULTS"

echo
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
