#!/usr/bin/env bash
# Asserts the Docker-socket containment contract for the auto-update profile.
#
# kubestellar/hive#3865: watchtower bind-mounted /var/run/docker.sock, which is
# the full daemon API — including /exec, which reads the GitHub App private key
# out of the hive container with no escalation at all. The socket now sits
# behind a deny-by-default docker-socket-proxy on an internal-only network.
#
# This tests the INVARIANT, not behaviour. Every property below is one edit away
# from silently reverting: re-adding the bind mount "to debug something",
# publishing 2375, adding EXEC=1 to make some other tool work, or attaching the
# proxy to the default network where the hive container runs semi-trusted agent
# code. None of those break anything at runtime, which is exactly why they need
# a guard rather than a code review.
#
# What this canNOT assert, and the docs say so plainly: the proxy does not make
# auto-update safe. CONTAINERS+POST is host-root-equivalent on its own
# (POST /containers/create with HostConfig.Privileged, and
# GET /containers/{id}/archive to read /secrets). See
# src/docs/auto-update-profile.md.
#
# Run: bash src/deploy/test_watchtower_socket_contract.sh
set -euo pipefail

PASS=0
FAIL=0

V2_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMPOSE="$V2_DIR/docker-compose.yaml"
DOC="$V2_DIR/docs/auto-update-profile.md"

pass() {
  echo "  PASS: $1"
  PASS=$((PASS + 1))
}

fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && echo "        $2"
  FAIL=$((FAIL + 1))
}

echo "=== watchtower / docker-socket containment contract (#3865) ==="

if [ ! -f "$COMPOSE" ]; then
  echo "  FAIL: docker-compose.yaml exists — not found at $COMPOSE"
  exit 1
fi

# Prefer a real compose render so the checks run against the RESOLVED config
# (implicit `default` network membership included) rather than against text that
# merely looks right. Fall back to a YAML parse when the docker CLI is absent,
# so this still guards on a machine without Docker.
RENDER=""
if command -v docker >/dev/null 2>&1 && docker compose -f "$COMPOSE" --profile auto-update config >/dev/null 2>&1; then
  RENDER="$(docker compose -f "$COMPOSE" --profile auto-update config 2>/dev/null)"
  pass "compose file renders (docker compose config)"
else
  echo "  note: docker CLI unavailable — falling back to raw YAML parse"
fi

# python3 with PyYAML does the structural assertions; a raw grep would pass on a
# commented-out line and fail on a reordered key.
if ! command -v python3 >/dev/null 2>&1; then
  echo "  SKIP: python3 unavailable — cannot make structural assertions"
  exit 0
fi
if ! python3 -c 'import yaml' >/dev/null 2>&1; then
  echo "  SKIP: PyYAML unavailable — cannot make structural assertions"
  exit 0
fi

RESULTS="$(RENDER="$RENDER" COMPOSE="$COMPOSE" python3 <<'PY'
import os, sys, yaml

render = os.environ.get("RENDER") or ""
if render.strip():
    doc = yaml.safe_load(render)
    resolved = True
else:
    doc = yaml.safe_load(open(os.environ["COMPOSE"]))
    resolved = False

services = doc.get("services") or {}
out = []
def ok(msg):   out.append(("PASS", msg, ""))
def bad(msg, detail=""): out.append(("FAIL", msg, detail))

def nets(svc):
    n = services.get(svc, {}).get("networks")
    if n is None:
        # Unrendered compose: a service with no `networks:` key is implicitly on
        # `default`. Say so rather than reporting "no networks".
        return ["default"] if not resolved else []
    return sorted(n.keys()) if isinstance(n, dict) else sorted(n)

def env(svc):
    e = services.get(svc, {}).get("environment") or []
    if isinstance(e, dict):
        return {k: ("" if v is None else str(v)) for k, v in e.items()}
    d = {}
    for item in e:
        k, _, v = str(item).partition("=")
        d[k] = v
    return d

# 1. Both services must exist and stay behind the opt-in profile.
for svc in ("watchtower", "docker-socket-proxy"):
    if svc not in services:
        bad(f"{svc} service exists", "not found in docker-compose.yaml")
        continue
    profiles = services[svc].get("profiles") or []
    if "auto-update" in profiles:
        ok(f"{svc} stays behind the opt-in auto-update profile")
    else:
        bad(f"{svc} stays behind the auto-update profile",
            f"profiles={profiles!r} — this must never run by default")

# 2. THE finding: watchtower must not hold the socket.
wt_vols = services.get("watchtower", {}).get("volumes") or []
sock = [v for v in wt_vols if "docker.sock" in str(v)]
if sock:
    bad("watchtower does NOT bind the Docker socket",
        f"found {sock!r} — this is #3865 itself; watchtower must reach the daemon "
        f"only through docker-socket-proxy")
else:
    ok("watchtower does NOT bind the Docker socket")

# 3. ...and it must actually be pointed at the proxy, or it silently falls back
#    to the default /var/run/docker.sock and the mount removal means nothing.
host = env("watchtower").get("DOCKER_HOST", "")
if "docker-socket-proxy" in host:
    ok(f"watchtower targets the proxy (DOCKER_HOST={host})")
else:
    bad("watchtower targets the proxy via DOCKER_HOST",
        f"got {host!r} — without this watchtower falls back to the local socket")

# 4. Exactly one service may hold the socket, and it is the proxy.
holders = sorted(s for s, v in services.items()
                 if any("docker.sock" in str(x) for x in (v.get("volumes") or [])))
if holders == ["docker-socket-proxy"]:
    ok("docker-socket-proxy is the ONLY service mounting the socket")
else:
    bad("docker-socket-proxy is the only service mounting the socket",
        f"socket holders: {holders!r}")

# 5. The proxy speaks an unauthenticated Docker API — it must never be published.
ports = services.get("docker-socket-proxy", {}).get("ports") or []
if ports:
    bad("docker-socket-proxy publishes NO ports",
        f"ports={ports!r} — publishing 2375 exposes an unauthenticated Docker API")
else:
    ok("docker-socket-proxy publishes no ports")

# 6. Network isolation: the proxy must not be reachable from the hive container,
#    which runs semi-trusted agent code.
proxy_nets = set(nets("docker-socket-proxy"))
hive_nets = set(nets("hive"))
shared = proxy_nets & hive_nets
if shared:
    bad("the hive container cannot reach the Docker API",
        f"hive and docker-socket-proxy share network(s) {sorted(shared)!r} — "
        f"agent code could then create privileged containers")
else:
    ok("hive shares no network with docker-socket-proxy")

if "default" in proxy_nets:
    bad("docker-socket-proxy is off the default network",
        "the default network carries the hive and gateway containers")
else:
    ok("docker-socket-proxy is off the default network")

# 7. The isolating network must actually be internal.
netdefs = doc.get("networks") or {}
proxy_only = [n for n in proxy_nets if n != "default"]
for n in proxy_only:
    spec = netdefs.get(n) or {}
    if spec.get("internal") is True:
        ok(f"network {n!r} is internal")
    else:
        bad(f"network {n!r} is internal",
            f"got {spec!r} — a non-internal network gives the proxy outbound reach")

# 8. The allowlist: EXEC and friends must stay off.
pe = env("docker-socket-proxy")
DENY = ["EXEC", "AUTH", "BUILD", "COMMIT", "CONFIGS", "DISTRIBUTION", "INFO",
        "NETWORKS", "NODES", "PLUGINS", "SECRETS", "SERVICES", "SESSION",
        "SWARM", "SYSTEM", "TASKS", "VOLUMES"]
enabled = [k for k in DENY if pe.get(k, "0") not in ("0", "", "false", "False")]
if enabled:
    bad("no dangerous Docker API section is enabled on the proxy",
        f"enabled: {enabled!r} — EXEC in particular reads /secrets out of the hive container")
else:
    ok("no dangerous Docker API section is enabled on the proxy")

# The four watchtower genuinely needs. Asserted so a "tightening" that breaks
# updates fails here instead of silently in production at 03:00.
for k in ("CONTAINERS", "IMAGES", "POST", "DELETE"):
    if pe.get(k) == "1":
        ok(f"proxy grants {k}=1 (required by watchtower)")
    else:
        bad(f"proxy grants {k}=1", f"got {pe.get(k)!r} — watchtower cannot update containers without it")

# 9. Digest pins. The proxy is now the socket holder, so its pin matters at
#    least as much as watchtower's.
for svc, repo in (("docker-socket-proxy", "tecnativa/docker-socket-proxy"),
                  ("watchtower", "containrrr/watchtower")):
    img = services.get(svc, {}).get("image", "")
    if not img.startswith(repo):
        bad(f"{svc} uses the expected image", f"got {img!r}")
    elif "@sha256:" in img and len(img.split("@sha256:")[1]) >= 64:
        ok(f"{svc} is pinned by @sha256 digest")
    else:
        bad(f"{svc} is pinned by @sha256 digest", f"got {img!r} — a tag is mutable")

for status, msg, detail in out:
    print(f"{status}\t{msg}\t{detail}")
PY
)"

while IFS=$'\t' read -r status msg detail; do
  [ -z "$status" ] && continue
  if [ "$status" = "PASS" ]; then pass "$msg"; else fail "$msg" "$detail"; fi
done <<< "$RESULTS"

# 10. The residual risk must stay documented. #3865 asked for the risk to be
#     documented prominently, and the proxy does NOT close the finding — a
#     future reader who believes it does will enable the profile on a
#     credential-holding host. Deleting the doc must fail.
if [ ! -f "$DOC" ]; then
  fail "src/docs/auto-update-profile.md exists" \
       "the residual risk is only acceptable while it is written down (#3865)"
else
  pass "src/docs/auto-update-profile.md exists"
  for needle in "What the proxy does NOT fix" "host root" "not a mitigation"; do
    if grep -qF "$needle" "$DOC"; then
      pass "auto-update doc still states: '$needle'"
    else
      fail "auto-update doc still states: '$needle'" \
           "the doc must keep naming the residual, not just the mitigation"
    fi
  done
fi

echo
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
