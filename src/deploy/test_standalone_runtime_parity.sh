#!/usr/bin/env bash
# Cross-runtime manifest/contract parity (kubestellar/hive#4404).
# Run: bash src/deploy/test_standalone_runtime_parity.sh
#
# WHY
# ---
# #4188 asked for this in as many words:
#
#     Avoid maintaining two unrelated deployment descriptions by convention
#     alone. [...] A change to one runtime must fail CI if the other runtime's
#     contract was unintentionally left stale.
#
# Both descriptions exist -- src/docker-compose.yaml and src/deploy/quadlet/* --
# and until now nothing compared them. Exactly one axis was guarded: image
# references, via standalone-images.sh and test_standalone_image_refs.sh. Every
# other axis was convention. The sibling guards each look at ONE runtime:
# test_standalone_service_contract.sh (#4204) parses only the Compose file and
# says so in its own header ("This does NOT claim Docker/Podman parity"), and
# test_quadlet_port_boundary.sh (#4375) parses only the units. A property can
# hold in both of those and still differ between them.
#
# WHAT THIS IS AND IS NOT
# -----------------------
# A STATIC comparison of the two deployment descriptions on a named list of
# runtime-invariant axes. It is not a claim of behavioural equivalence at
# runtime -- that is what the live CI lanes (#4334, #4335) cover -- and it does
# not look at the Kubernetes manifest, whose capability set #4379 covers.
#
# THE EXCEPTION TABLE IS THE POINT. Some divergences are correct. Encoding them
# as entries with a stated reason, rather than by not looking at the axis, is
# what keeps the unexamined cases from hiding among the examined ones. Two kinds
# exist and they are not the same claim:
#
#   deliberate     the two runtimes SHOULD differ here, and why.
#   defect-tracked they should not, this is a real divergence, and here is the
#                  issue. It does not fail the build today so that the check can
#                  land; it is a debt marker with a number on it, not a pass.
#
# STALE EXCEPTIONS FAIL. Every entry is checked in reverse: an exception that no
# longer matches an actual divergence is itself a failure. Fixing a tracked
# defect therefore forces the entry to be removed, so the table cannot rot into
# a list of things that used to be true.
#
# The Compose side is read as raw YAML rather than through `docker compose
# config`. Only variable NAMES, ports, mounts, capabilities and probe fields are
# compared, none of which interpolation changes, and the Podman side has no
# renderer to match — so requiring Docker here would make the parity guard skip
# on exactly the Podman-only hosts it exists to protect.
#
# Runs without starting a container, like its sibling contract tests.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE="${ROOT}/src/docker-compose.yaml"
UNIT_DIR="${ROOT}/src/deploy/quadlet"
ENV_EXAMPLE="${UNIT_DIR}/hive.env.example"
IMAGES_SH="${ROOT}/src/deploy/standalone-images.sh"

PASS=0
FAIL=0
EXCEPTED=0

pass() { printf '  PASS: %s\n' "$1"; PASS=$((PASS + 1)); }
fail() {
  printf '  FAIL: %s\n' "$1"
  [[ $# -gt 1 && -n "${2:-}" ]] && printf '        %s\n' "$2"
  FAIL=$((FAIL + 1))
}
note() { printf '  %s\n' "$1"; }

printf '=== standalone Docker/Podman contract parity (#4404) ===\n\n'

for required in "$COMPOSE" "$UNIT_DIR" "$ENV_EXAMPLE" "$IMAGES_SH"; do
  if [[ ! -e "$required" ]]; then
    printf '  FAIL: %s exists\n' "${required#"${ROOT}/"}"
    printf '\n=== Results: %d passed, %d failed ===\n' "$PASS" "$((FAIL + 1))"
    exit 1
  fi
done

if ! command -v python3 >/dev/null 2>&1; then
  printf '  SKIP: python3 unavailable — cannot compare the two descriptions structurally\n'
  exit 0
fi
if ! python3 -c 'import yaml' >/dev/null 2>&1; then
  printf '  SKIP: PyYAML unavailable — cannot parse docker-compose.yaml\n'
  exit 0
fi

# Image references are normalised by the SOURCE OF TRUTH's own function rather
# than by a second copy of the rules here: Docker may spell an image
# `nginx:alpine` where Podman requires `docker.io/library/nginx:alpine`, and
# hive_standalone_image_normalize is what already knows the two are equal.
# shellcheck source=/dev/null
. "$IMAGES_SH"

canonical_images() {
  local component
  while IFS= read -r component; do
    [[ -n "$component" ]] || continue
    printf '%s\t%s\n' "$component" \
      "$(hive_standalone_image_normalize "$(hive_standalone_image "$component")")"
  done < <(hive_standalone_image_components)
}

NORMALIZED_SOT="$(canonical_images)"

RESULTS="$(
  COMPOSE="$COMPOSE" UNIT_DIR="$UNIT_DIR" ENV_EXAMPLE="$ENV_EXAMPLE" \
  NORMALIZED_SOT="$NORMALIZED_SOT" python3 <<'PY'
import os, re, sys, yaml

compose_path = os.environ["COMPOSE"]
unit_dir = os.environ["UNIT_DIR"]
env_example = os.environ["ENV_EXAMPLE"]

out = []
def ok(msg):                out.append(("PASS", msg, ""))
def bad(msg, detail=""):    out.append(("FAIL", msg, detail))
def info(msg):              out.append(("NOTE", msg, ""))
def excepted(msg, detail):  out.append(("EXCEPT", msg, detail))

# ── The exception table ──────────────────────────────────────────────────────
#
# key -> (kind, reason). `kind` is "deliberate" or "defect-tracked". Every entry
# is verified to still correspond to a real divergence; see the stale-exception
# sweep at the end.
EXCEPTIONS = {
    # --- deliberate -----------------------------------------------------------
    "mount-mode:hive:/etc/hive/hive.yaml": (
        "deliberate",
        "Podman is STRICTER: the unit mounts it ro,Z, Compose mounts it rw. The "
        "entrypoint supports both — its Docker-mode branch reads 'Config path is "
        "read-only — using $HIVE_CONFIG_SOURCE directly' and falls back to "
        "/data/hive.yaml.runtime, which is the layer that survives a recreate "
        "anyway (#3961, pinned by src/pkg/config/save_readonly_test.go). The "
        "asymmetry is allowed in ONE direction only: the check below fails if the "
        "Podman side ever becomes the looser of the two.",
    ),
    "labels:hive:com.centurylinklabs.watchtower.enable": (
        "deliberate",
        "Docker-only by design (#4188 Phase 3). The auto-update profile is built "
        "on the Docker socket and a filtered Docker API proxy and is explicitly "
        "not repointed at the Podman socket, so a Quadlet unit carrying this "
        "label would advertise a manager that will never manage it. Asserted "
        "positively below rather than merely skipped: no unit may carry it.",
    ),
    "labels:*:io.kubestellar.hive.*": (
        "deliberate",
        "Podman-only by design (#4210). bin/hive-podman-teardown.sh (#4343) "
        "selects by this label set and nothing else; the Compose stack is torn "
        "down by project/`container_name` and has no equivalent need.",
    ),
    "expose:hive": (
        "deliberate",
        "Compose lists expose: 3001/3002/7681; the units deliberately carry no "
        "ExposeHostPort=. Both are documentation — every member of a netavark "
        "network can already reach every port of every other member — but "
        "Quadlet's spelling is `--expose`, which becomes a REAL published port "
        "the moment anything adds -P. hive-gateway.container's header records "
        "the decision. Asserted positively below: no unit may carry "
        "ExposeHostPort=.",
    ),
    "healthcheck-start-period:gateway": (
        "deliberate",
        "Compose sets no start_period for the gateway; the unit sets "
        "HealthStartPeriod=30s. The unit needs a start budget because ADR-0017 "
        "requires TimeoutStartSec to exceed it and Notify=healthy gates the "
        "start on it; Compose's gateway has no equivalent gate, it just waits "
        "for hive via depends_on. The probe command and interval/timeout/retries "
        "are compared strictly and must still match.",
    ),
    "build:hive": (
        "deliberate",
        "Compose carries a build: section so `docker compose up --build` can "
        "build from source. Quadlet has no build key for a .container unit at "
        "all (a .build unit is a separate asset), and the Podman path is "
        "install-from-registry. Structural, not a contract divergence.",
    ),

    # --- defect-tracked -------------------------------------------------------
    "restart:gateway": (
        "defect-tracked",
        "Compose says unless-stopped, the unit says Restart=on-failure. "
        "hive.container learned in #4377 that a clean external stop must still "
        "be recovered and uses Restart=always (systemd never restarts a unit a "
        "`systemctl stop` job stopped, so that is the exact equivalent of "
        "unless-stopped); the gateway did not get the same treatment. A gateway "
        "that exits 0 stays down under on-failure — nginx exits 0 on a clean "
        "SIGTERM, so the only published port on the host goes away while "
        "systemctl reports success — and would restart under Compose. Tracked "
        "in #4415; not fixed here, #4404 is the check and not the fixes.",
    ),
}

# Exceptions that fired. Anything left over at the end is stale.
fired = set()

def diverges(key, msg, detail):
    """One divergence. Excepted (with its reason) or a failure."""
    if key in EXCEPTIONS:
        fired.add(key)
        kind, reason = EXCEPTIONS[key]
        excepted(f"[{kind}] {msg}", reason)
        return
    bad(msg, detail)

# ── Parsing: Compose ─────────────────────────────────────────────────────────
doc = yaml.safe_load(open(compose_path)) or {}
services = doc.get("services") or {}
top_volumes = doc.get("volumes") or {}

# The two services an operator actually runs. The auto-update profile services
# are Docker-only by design (#4188 Phase 3) and have no Podman counterpart, so
# they are not components of the parity comparison; the watchtower label
# exception above is what records that boundary.
COMPONENTS = [("hive", "hive.container"), ("gateway", "hive-gateway.container")]

def c_list(svc, key):
    return list((services.get(svc) or {}).get(key) or [])

def compose_env_names(svc):
    env = (services.get(svc) or {}).get("environment") or []
    names = set()
    if isinstance(env, dict):
        names.update(str(k) for k in env)
    else:
        for item in env:
            names.add(str(item).split("=", 1)[0])
    return names

def compose_published(svc):
    pairs = []
    for p in c_list(svc, "ports"):
        if isinstance(p, dict):
            pairs.append((str(p.get("published") or ""), str(p.get("target") or "")))
        else:
            bits = str(p).split("/")[0].split(":")
            pairs.append((bits[-2] if len(bits) > 1 else "", bits[-1]))
    return sorted(pairs)

def compose_mounts(svc):
    """target -> {'ro': bool, 'named': bool, 'source': str, 'raw': str}"""
    mounts = {}
    for v in c_list(svc, "volumes"):
        if isinstance(v, dict):
            src, target = v.get("source") or "", v.get("target") or ""
            mounts[target] = {"ro": bool(v.get("read_only")), "source": src,
                              "named": v.get("type") == "volume", "raw": str(v)}
        else:
            bits = str(v).split(":")
            src = bits[0] if len(bits) > 1 else ""
            target = bits[1] if len(bits) > 1 else bits[0]
            opts = bits[2].split(",") if len(bits) > 2 else []
            mounts[target] = {
                "ro": "ro" in opts, "source": src,
                "named": not (src.startswith(".") or src.startswith("/")),
                "raw": str(v),
            }
    return mounts

def compose_health(svc):
    hc = (services.get(svc) or {}).get("healthcheck") or {}
    test = hc.get("test") or []
    if isinstance(test, str):
        cmd = test
    else:
        cmd = " ".join(str(t) for t in test if str(t) not in ("CMD", "CMD-SHELL"))
    return {
        "cmd": cmd.strip(),
        "interval": str(hc.get("interval") or ""),
        "timeout": str(hc.get("timeout") or ""),
        "retries": str(hc.get("retries") or ""),
        "start_period": str(hc.get("start_period") or ""),
    }

# ── Parsing: Quadlet ─────────────────────────────────────────────────────────
#
# systemd treats a line whose first non-blank character is `#` or `;` as a
# comment and nothing else. Stripping them matters: these units discuss keys
# like PublishPort= in prose at length, so a grep over the raw text matches the
# warning not to add one.
def unit_parse(path):
    values = {}
    if not os.path.isfile(path):
        return values
    for line in open(path, encoding="utf-8"):
        line = line.rstrip("\r\n").strip()
        if not line or line[0] in "#;" or line.startswith("["):
            continue
        if "=" not in line:
            continue
        k, v = line.split("=", 1)
        values.setdefault(k.strip(), []).append(v.strip())
    return values

def u(units, key):
    return list(units.get(key) or [])

def u1(units, key, default=""):
    vals = u(units, key)
    return vals[0] if vals else default

UNITS = {name: unit_parse(os.path.join(unit_dir, name))
         for name in os.listdir(unit_dir) if not name.endswith(".example")}

def quadlet_mounts(units):
    """Volume=SOURCE:TARGET[:OPTS] -> target -> {...}"""
    mounts = {}
    for spec in u(units, "Volume"):
        bits = spec.split(":")
        if len(bits) < 2:
            continue
        src, target = bits[0], bits[1]
        opts = bits[2].split(",") if len(bits) > 2 else []
        mounts[target] = {
            "ro": "ro" in opts, "source": src,
            # A Volume= source naming a .volume unit is the named-volume form;
            # anything starting with / or a systemd specifier is a bind mount.
            "named": src.endswith(".volume"),
            "raw": spec,
        }
    return mounts

def quadlet_published(units):
    pairs = []
    for spec in u(units, "PublishPort"):
        spec = spec.split("/")[0]
        if ":" in spec:
            host, container = spec.rsplit(":", 1)
            host = host.rsplit(":", 1)[-1]
        else:
            host, container = "", spec
        pairs.append((host, container))
    return sorted(pairs)

def quadlet_health(units):
    return {
        "cmd": u1(units, "HealthCmd"),
        "interval": u1(units, "HealthInterval"),
        "timeout": u1(units, "HealthTimeout"),
        "retries": u1(units, "HealthRetries"),
        "start_period": u1(units, "HealthStartPeriod"),
    }

def duration_eq(a, b):
    """`10s` and `10s` — and Compose's `1m30s` vs a unit's `90s`."""
    def secs(s):
        s = str(s).strip()
        if not s:
            return None
        total, seen = 0, False
        for value, unit in re.findall(r"(\d+)\s*(h|m|s|ms)?", s):
            if not value:
                continue
            seen = True
            total += int(value) * {"h": 3600, "m": 60, "s": 1, "ms": 0, "": 1}[unit or ""]
        return total if seen else None
    return secs(a) == secs(b)

missing_units = [unit for _, unit in COMPONENTS if not UNITS.get(unit)]
if missing_units:
    bad("both Quadlet component units parse", f"missing or empty: {missing_units!r}")
    for status, msg, detail in out:
        print(f"{status}\t{msg}\t{detail}")
    sys.exit(0)

# ── Axis: image references ───────────────────────────────────────────────────
info("--- axis: image references ---")
sot = {}
for line in os.environ["NORMALIZED_SOT"].splitlines():
    if "\t" in line:
        comp, ref = line.split("\t", 1)
        sot[comp.strip()] = ref.strip()

def normalize_ref(ref):
    """Mirrors hive_standalone_image_normalize for the assets' own spellings."""
    if not ref:
        return ""
    first = ref.split("/")[0]
    if "/" in ref and ("." in first or ":" in first or first == "localhost"):
        return ref
    return f"docker.io/{ref}" if "/" in ref else f"docker.io/library/{ref}"

for svc, unit in COMPONENTS:
    c_ref = normalize_ref(str((services.get(svc) or {}).get("image") or ""))
    q_ref = normalize_ref(u1(UNITS[unit], "Image"))
    if c_ref and c_ref == q_ref:
        ok(f"{svc}: both runtimes reference the same image ({c_ref})")
        if sot.get(svc) and sot[svc] != c_ref:
            bad(f"{svc}: that shared reference is the one in standalone-images.sh",
                f"assets agree on {c_ref} but the source of truth says {sot[svc]}")
    else:
        diverges(f"image:{svc}", f"{svc}: both runtimes reference the same image",
                 f"compose={c_ref!r} quadlet={q_ref!r} — one runtime is pulling "
                 "different code than the other")

# ── Axis: environment variables ──────────────────────────────────────────────
#
# EnvironmentFile= names a path and says nothing about what belongs in it, so
# there is no Podman-side artifact to compare against unless one is tracked.
# hive.env.example is that artifact; without it this axis is uncheckable and a
# seventh Compose variable reaches Docker and silently never reaches Podman.
info("--- axis: environment variables ---")
env_file_keys = set()
for line in open(env_example, encoding="utf-8"):
    line = line.strip()
    if line.startswith("#"):
        line = line.lstrip("#").strip()
    if not line or "=" not in line:
        continue
    name = line.split("=", 1)[0].strip()
    if re.fullmatch(r"[A-Z_][A-Z0-9_]*", name):
        env_file_keys.add(name)

if not u(UNITS["hive.container"], "EnvironmentFile"):
    bad("hive.container reads its environment from an EnvironmentFile",
        "without it hive.env.example describes nothing and this axis is vacuous")
else:
    ok("hive.container reads its environment from an EnvironmentFile")

c_env = compose_env_names("hive")
if not c_env:
    bad("the compose hive service declares environment variables",
        "nothing to compare — the parser found no environment: block")
elif not env_file_keys:
    bad("hive.env.example enumerates the environment contract",
        f"parsed no variable names out of {os.path.basename(env_example)}")
elif c_env == env_file_keys:
    ok(f"both runtimes declare the same {len(c_env)} environment variables "
       f"({', '.join(sorted(c_env))})")
else:
    only_compose = sorted(c_env - env_file_keys)
    only_podman = sorted(env_file_keys - c_env)
    diverges("env:hive",
             "both runtimes declare the same environment variables",
             f"only in docker-compose.yaml: {only_compose!r}; only in "
             f"hive.env.example: {only_podman!r} — a variable added to one "
             "description reaches that runtime and silently never reaches the "
             "other")

# ── Axis: mounts and their modes ─────────────────────────────────────────────
info("--- axis: mounts and their modes ---")
for svc, unit in COMPONENTS:
    cm, qm = compose_mounts(svc), quadlet_mounts(UNITS[unit])
    for target in sorted(set(cm) | set(qm)):
        in_c, in_q = target in cm, target in qm
        if in_c and not in_q:
            diverges(f"mount-target:{svc}:{target}",
                     f"{svc}: {target} is mounted by both runtimes",
                     f"docker-compose.yaml mounts it ({cm[target]['raw']!r}); no "
                     f"Volume= in {unit} does")
        elif in_q and not in_c:
            diverges(f"mount-target:{svc}:{target}",
                     f"{svc}: {target} is mounted by both runtimes",
                     f"{unit} mounts it ({qm[target]['raw']!r}); no volume in "
                     "docker-compose.yaml does")
        elif cm[target]["ro"] != qm[target]["ro"]:
            # Direction matters. Podman being stricter is a posture choice the
            # table can accept; Podman being LOOSER is never acceptable, because
            # the Compose :ro entries are the ones the security guards assert.
            if qm[target]["ro"] and not cm[target]["ro"]:
                diverges(f"mount-mode:{svc}:{target}",
                         f"{svc}: {target} has the same mode in both runtimes",
                         f"compose rw, quadlet ro")
            else:
                bad(f"{svc}: {target} has the same mode in both runtimes",
                    f"compose ro, quadlet RW — the Podman asset is the LOOSER of "
                    "the two, which no exception permits: this container must not "
                    "be able to rewrite what it reads")
        else:
            mode = "ro" if cm[target]["ro"] else "rw"
            ok(f"{svc}: {target} mounted {mode} by both runtimes")

# ── Axis: published vs exposed ports ─────────────────────────────────────────
#
# The #4375 invariant, in both assets at once: 3001 published, 7681 never.
info("--- axis: published vs exposed ports ---")
for svc, unit in COMPONENTS:
    cp, qp = compose_published(svc), quadlet_published(UNITS[unit])
    if cp == qp:
        ok(f"{svc}: both runtimes publish {cp!r}")
    else:
        diverges(f"publish:{svc}", f"{svc}: both runtimes publish the same ports",
                 f"compose={cp!r} quadlet={qp!r}")

gw_c, gw_q = compose_published("gateway"), quadlet_published(UNITS["hive-gateway.container"])
if gw_c == [("3001", "3001")] == gw_q:
    ok("the gateway publishes host 3001 -> container 3001 and nothing else, in both")
else:
    bad("the gateway publishes host 3001 -> container 3001 and nothing else, in both",
        f"compose={gw_c!r} quadlet={gw_q!r}")

# Swept across EVERY service and EVERY unit, not just the two components.
offenders = []
for svc in services:
    offenders += [f"compose:{svc}:{p!r}" for p in compose_published(svc)
                  if "7681" in p]
for name, units in UNITS.items():
    offenders += [f"{name}:{p!r}" for p in quadlet_published(units) if "7681" in p]
if not offenders:
    ok("no service and no unit publishes the raw ttyd port 7681")
else:
    bad("no service and no unit publishes the raw ttyd port 7681",
        f"{offenders!r} — this publishes an unauthenticated shell into the "
        "credential-holding container")

c_expose = [str(p).split("/")[0] for p in c_list("hive", "expose")]
q_expose = sorted({v for units in UNITS.values() for v in u(units, "ExposeHostPort")})
if c_expose and not q_expose:
    diverges("expose:hive", "both runtimes declare the same in-network ports",
             f"compose expose={c_expose!r}, no ExposeHostPort= in any unit")
elif c_expose == q_expose:
    ok(f"both runtimes declare the same in-network ports ({c_expose!r})")
else:
    bad("both runtimes declare the same in-network ports",
        f"compose={c_expose!r} quadlet={q_expose!r}")
if q_expose:
    bad("no unit carries ExposeHostPort=",
        f"{q_expose!r} — Quadlet's spelling of expose is `--expose`, which "
        "becomes a real published port as soon as anything adds -P")
else:
    ok("no unit carries ExposeHostPort= (it turns into a published port under -P)")

# ── Axis: capabilities ───────────────────────────────────────────────────────
info("--- axis: capabilities ---")
for svc, unit in COMPONENTS:
    c_caps = sorted({str(c).upper() for c in c_list(svc, "cap_add")})
    q_caps = sorted({v.upper() for v in u(UNITS[unit], "AddCapability")})
    if c_caps == q_caps:
        ok(f"{svc}: both runtimes add {c_caps or ['<none>']!r}")
    else:
        diverges(f"caps:{svc}", f"{svc}: both runtimes add the same capabilities",
                 f"compose cap_add={c_caps!r} quadlet AddCapability={q_caps!r} — "
                 "without NET_ADMIN the entrypoint fails closed with exit 77")

# ── Axis: health checks ──────────────────────────────────────────────────────
info("--- axis: health checks ---")
for svc, unit in COMPONENTS:
    ch, qh = compose_health(svc), quadlet_health(UNITS[unit])
    if not ch["cmd"] or not qh["cmd"]:
        bad(f"{svc}: both runtimes define a health probe",
            f"compose={ch['cmd']!r} quadlet={qh['cmd']!r} — readiness is what "
            "the gateway ordering and Notify=healthy both depend on")
        continue
    if ch["cmd"] == qh["cmd"]:
        ok(f"{svc}: both runtimes probe with `{ch['cmd']}`")
    else:
        diverges(f"healthcheck-cmd:{svc}",
                 f"{svc}: both runtimes probe the same endpoint",
                 f"compose={ch['cmd']!r} quadlet={qh['cmd']!r}")
    for field, label in (("interval", "interval"), ("timeout", "timeout"),
                         ("retries", "retries"), ("start_period", "start period")):
        a, b = ch[field], qh[field]
        same = (a == b) if field == "retries" else duration_eq(a, b)
        if same:
            ok(f"{svc}: health {label} matches ({a or '<unset>'})")
        else:
            key = ("healthcheck-start-period" if field == "start_period"
                   else f"healthcheck-{field}")
            diverges(f"{key}:{svc}", f"{svc}: health {label} matches",
                     f"compose={a or '<unset>'!r} quadlet={b or '<unset>'!r}")

# ── Axis: persistence ────────────────────────────────────────────────────────
info("--- axis: persistence ---")
c_data = compose_mounts("hive").get("/data")
q_data = quadlet_mounts(UNITS["hive.container"]).get("/data")
if not c_data or not q_data:
    bad("/data persists on a named volume in both runtimes",
        f"compose={c_data!r} quadlet={q_data!r}")
else:
    if c_data["named"] and q_data["named"]:
        ok("/data persists on a named volume in both runtimes")
    else:
        bad("/data persists on a named volume in both runtimes",
            f"compose named={c_data['named']} ({c_data['raw']!r}), quadlet named="
            f"{q_data['named']} ({q_data['raw']!r}) — a bind mount still runs and "
            "just loses agent state on the next recreate")
    # The volume NAME is part of the contract: the operator docs, the backup
    # procedure and bin/hive-podman-teardown.sh all name it.
    c_name = c_data["source"]
    q_unit = os.path.join(unit_dir, q_data["source"])
    q_name = u1(unit_parse(q_unit), "VolumeName") if os.path.isfile(q_unit) else ""
    if c_name and c_name == q_name:
        ok(f"both runtimes call the state volume '{c_name}'")
    else:
        diverges("volume-name:hive-data",
                 "both runtimes call the state volume the same thing",
                 f"compose={c_name!r} quadlet VolumeName={q_name!r} — the backup "
                 "and teardown docs name one volume for both runtimes")
    if c_name and c_name not in top_volumes:
        bad("the compose state volume is declared in the top-level volumes: block",
            f"'{c_name}' is not")

# ── Axis: network exposure ───────────────────────────────────────────────────
info("--- axis: network exposure ---")
# Both components must sit on ONE shared network on each side, and the gateway
# must be ordered after hive is HEALTHY rather than merely started -- otherwise
# it serves 502s on the port operators were told to trust.
q_nets = {unit: set(u(UNITS[unit], "Network")) for _, unit in COMPONENTS}
shared = set.intersection(*q_nets.values()) if q_nets else set()
if len(shared) == 1:
    ok(f"both units join one shared network ({next(iter(shared))})")
else:
    bad("both units join one shared network", f"got {q_nets!r}")

for net_unit_name in sorted(shared):
    net_units = UNITS.get(net_unit_name, {})
    internal = u1(net_units, "Internal", "false").lower()
    if internal in ("", "false", "no", "0"):
        ok(f"{net_unit_name} is not Internal= (agents must reach GitHub and the model APIs)")
    else:
        bad(f"{net_unit_name} is not Internal=",
            f"Internal={internal} — an internal network has no route off the host")

dep = (services.get("gateway") or {}).get("depends_on") or {}
cond = dep.get("hive", {}).get("condition") if isinstance(dep, dict) else None
q_after = set(u(UNITS["hive-gateway.container"], "After"))
q_requires = set(u(UNITS["hive-gateway.container"], "Requires"))
q_notify = u1(UNITS["hive.container"], "Notify").lower()
compose_ready = cond == "service_healthy"
podman_ready = ("hive.service" in q_after and "hive.service" in q_requires
                and q_notify == "healthy")
if compose_ready and podman_ready:
    ok("both runtimes hold the gateway until hive is HEALTHY, not merely started")
else:
    diverges("readiness-ordering:gateway",
             "both runtimes hold the gateway until hive is healthy",
             f"compose depends_on condition={cond!r}; gateway unit "
             f"Requires={sorted(q_requires)!r} After={sorted(q_after)!r}; "
             f"hive.container Notify={q_notify!r}")

# The internal-only auto-update network is the Docker-only counterpart and is
# asserted here rather than left unexamined: if it ever stops being internal,
# the unauthenticated Docker API on :2375 becomes routable from the network the
# hive runs semi-trusted agent code on.
proxy_net = (doc.get("networks") or {}).get("docker-proxy") or {}
if proxy_net.get("internal") is True:
    ok("the Docker-only auto-update network stays internal: true (no Podman counterpart)")
else:
    bad("the Docker-only auto-update network stays internal: true",
        f"got {proxy_net!r} — the filtered Docker API must stay unroutable")

# ── Axis: restart policy ─────────────────────────────────────────────────────
#
# Compose `unless-stopped` and systemd `Restart=always` are the same statement:
# systemd never restarts a unit that a `systemctl stop` job stopped, so "always"
# means "unless stopped". `on-failure` is a strictly weaker claim.
info("--- axis: restart policy ---")
EQUIVALENT = {"unless-stopped": "always", "always": "always"}
for svc, unit in COMPONENTS:
    c_restart = str((services.get(svc) or {}).get("restart") or "")
    q_restart = u1(UNITS[unit], "Restart")
    want = EQUIVALENT.get(c_restart)
    if want is None:
        bad(f"{svc}: the compose restart policy is one this check knows how to map",
            f"got {c_restart!r} — extend EQUIVALENT before trusting this axis")
    elif q_restart == want:
        ok(f"{svc}: restart policies agree (compose {c_restart} == systemd Restart={q_restart})")
    else:
        diverges(f"restart:{svc}", f"{svc}: restart policies agree",
                 f"compose={c_restart!r} maps to Restart={want!r}, unit says "
                 f"Restart={q_restart!r}")

# ── Axis: labels ─────────────────────────────────────────────────────────────
info("--- axis: labels ---")
def compose_labels(svc):
    labels = (services.get(svc) or {}).get("labels") or []
    if isinstance(labels, dict):
        return {str(k): str(v) for k, v in labels.items()}
    parsed = {}
    for item in labels:
        k, _, v = str(item).partition("=")
        parsed[k.strip().strip('"')] = v.strip().strip('"')
    return parsed

WATCHTOWER_LABEL = "com.centurylinklabs.watchtower.enable"
OWNERSHIP_PREFIX = "io.kubestellar.hive."

for svc, unit in COMPONENTS:
    c_labels = set(compose_labels(svc))
    q_labels = {v.split("=", 1)[0] for v in u(UNITS[unit], "Label")}
    for name in sorted(c_labels - q_labels):
        diverges(f"labels:{svc}:{name}",
                 f"{svc}: label {name} is set by both runtimes",
                 "set in docker-compose.yaml, absent from the unit")
    podman_only = sorted(n for n in (q_labels - c_labels)
                         if not n.startswith(OWNERSHIP_PREFIX))
    for name in podman_only:
        diverges(f"labels:{svc}:{name}", f"{svc}: label {name} is set by both runtimes",
                 "set in the unit, absent from docker-compose.yaml")
    if q_labels - c_labels and not podman_only:
        fired.add("labels:*:io.kubestellar.hive.*")
        kind, reason = EXCEPTIONS["labels:*:io.kubestellar.hive.*"]
        excepted(f"[{kind}] {svc}: the unit carries only ownership labels Compose does not",
                 reason)

# The Docker-only exception, asserted rather than assumed: a unit carrying the
# Watchtower label would advertise a manager that cannot manage it.
wt_units = [name for name, units in UNITS.items()
            if any(v.startswith(WATCHTOWER_LABEL) for v in u(units, "Label"))]
if not wt_units:
    ok("no Quadlet unit carries the Watchtower enable label (Docker-only by design)")
else:
    bad("no Quadlet unit carries the Watchtower enable label",
        f"{wt_units!r} — auto-update is Docker-socket-based and is not repointed "
        "at Podman (#4188 Phase 3)")

# ── Structural: the compose build: section ───────────────────────────────────
if (services.get("hive") or {}).get("build"):
    diverges("build:hive", "hive: both runtimes describe the same build inputs",
             "compose carries build:, the unit has no equivalent key")

# ── Stale exceptions ─────────────────────────────────────────────────────────
#
# The table may only describe divergences that are actually there. An entry that
# stops firing means the underlying difference was resolved, and the entry has to
# go with it -- otherwise fixing the gateway restart policy would leave a
# permanent note claiming it is still broken.
info("--- exception table: every entry still describes a real divergence ---")
stale = sorted(set(EXCEPTIONS) - fired)
if not stale:
    ok(f"all {len(EXCEPTIONS)} exceptions still correspond to a live divergence")
else:
    bad("every exception still corresponds to a live divergence",
        f"stale: {stale!r} — the divergence is gone, so delete the entry (if a "
        "tracked defect was fixed, close its issue too)")

for status, msg, detail in out:
    print(f"{status}\t{msg}\t{detail}")
PY
)"

while IFS=$'\t' read -r status msg detail; do
  [[ -n "$status" ]] || continue
  case "$status" in
    PASS)   pass "$msg" ;;
    FAIL)   fail "$msg" "$detail" ;;
    NOTE)   printf '\n%s\n' "$msg" ;;
    EXCEPT)
      printf '  EXCEPTION: %s\n' "$msg"
      printf '             %s\n' "$detail"
      EXCEPTED=$((EXCEPTED + 1))
      ;;
    *)      note "$status $msg" ;;
  esac
done <<<"$RESULTS"

printf '\n=== Results: %d passed, %d failed, %d accepted exceptions ===\n' \
  "$PASS" "$FAIL" "$EXCEPTED"
[[ "$FAIL" -eq 0 ]]
