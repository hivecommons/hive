#!/usr/bin/env bash
# test_manifest_caps_runtime.sh — RUNTIME regression test for #4379.
#
# THE GAP THIS CLOSES
# -------------------
# `src/deploy/k8s/deployment.yaml` ships an exact capability grant:
#
#     capabilities:
#       drop: [ALL]
#       add:  [NET_ADMIN, SETUID, SETGID, SETPCAP, CHOWN, DAC_OVERRIDE]
#
# and until now NOTHING ever booted a container under it. The one runtime job in
# this area (test_ambient_cap_runtime.sh) uses `docker run --cap-add=NET_ADMIN`,
# i.e. Docker's DEFAULT bounding set plus NET_ADMIN and no `--cap-drop` at all —
# a strictly wider set than the pod gets. Everything else (check-suid-contract.sh)
# is a grep over files, and a grep cannot catch a capability the kernel refuses at
# execve time.
#
# The concrete miss: #3895 proposed removing SETUID/SETGID from the list. That
# would have broken agent launch in the pod on two counts — the entrypoint's
# root->dev `setpriv --reuid` drop, and su-exec's setgroups/setgid/setuid to the
# per-agent UID (2001+). CI was fully green. A human caught it in review.
#
# WHAT THIS DOES
# --------------
#   1. DERIVES the cap set from deployment.yaml. Nothing here restates the list —
#      a test carrying its own copy would stay green while the manifest drifted,
#      which is the exact failure mode being fixed. The OpenShift SCC overlay's
#      `allowedCapabilities` (which must mirror the manifest 1:1 or the pod is
#      rejected at admission) and the hosted-hive template in
#      src/pkg/hub/saas_provision.go are checked against the same derivation.
#   2. BOOTS a container under `--cap-drop=ALL --cap-add=<the derived set>` and
#      exercises each capability's ACTUAL consumer: the entrypoint's own setpriv
#      flags, the gosu fallback, the SUID su-exec hop to a per-agent UID, the
#      per-agent chown, a DAC_OVERRIDE write, and the ACMM iptables egress gate.
#   3. MUTATES: re-runs the whole chain once per capability with THAT capability
#      removed, and requires the chain to break and the report to name it. This is
#      what makes the positive run non-vacuous — and it is machine-checked every
#      run, not verified by hand once.
#   4. Any capability in the manifest whose removal breaks NOTHING is reported as
#      UNEXERCISED and fails the job, unless it is in the documented allowlist
#      below. So a capability added to the manifest without coverage turns this
#      job RED instead of silently widening the grant.
#
# SCOPE (same statement test_ambient_cap_runtime.sh makes): docker's seccomp and
# apparmor defaults are not the pod's `RuntimeDefault`, so this validates the
# CAPABILITY contract only, not the full pod security context.
#
# Run: bash src/deploy/test_manifest_caps_runtime.sh
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
MANIFEST="$REPO/src/deploy/k8s/deployment.yaml"
SCC="$REPO/src/deploy/kustomize/overlays/openshift-netadmin/scc-hive-netadmin.yaml"
SAAS="$REPO/src/pkg/hub/saas_provision.go"
DOCKERFILE="$REPO/src/Dockerfile"
ENTRYPOINT="$HERE/entrypoint.sh"

PASS=0
FAIL=0
ok()  { echo "  PASS: $1"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); }

# ── Capabilities the manifest grants that this suite cannot make fail ─────────
#
# A capability lands here ONLY with a kernel-level reason why no reachable code
# path in this image needs it. It is not a TODO list and it is not permission to
# drop the capability from the manifest — that is a separate, deliberate change
# (#3895 is closed and held); this is a record of what the boot does and does not
# prove.
#
# SETPCAP: raising NET_ADMIN into the inheritable set (the #3874 fix, and the
#   only pI write in the image) does NOT require it. `cap_capset()` in
#   security/commoncap.c admits a new pI when it is a subset of (old pI | pP) and
#   of (old pI | bounding); the container's initial process is UID 0, so the
#   granted NET_ADMIN is already in pP and the raise is legal without CAP_SETPCAP.
#   Verified by boot: the full chain below passes with SETPCAP removed. The
#   CAP_SETPCAP requirement for pI writes is pre-2.6.25 kernel history, which is
#   why it is in the list; it stays until someone deliberately revisits the grant.
UNEXERCISED_ALLOWLIST="SETPCAP"

echo "=== #4379 manifest capability-set runtime tests ==="
echo ""

# ── Derivation ───────────────────────────────────────────────────────────────
#
# Pull `capabilities.{drop,add}` out of a YAML-shaped file. Works on the k8s
# manifest and, unchanged, on the YAML template embedded in saas_provision.go's
# Go raw string. Emits "drop NAME" / "add NAME" lines.
parse_capabilities() {
  awk '
    /^[[:space:]]*capabilities:[[:space:]]*$/ { incap = 1; capind = match($0, /[^ ]/); mode = ""; next }
    incap {
      if ($0 ~ /^[[:space:]]*$/) next
      if ($0 ~ /^[[:space:]]*#/) next
      ind = match($0, /[^ ]/)
      if (ind <= capind) { incap = 0; mode = ""; next }
      if ($0 ~ /^[[:space:]]*drop:/) { mode = "drop"; next }
      if ($0 ~ /^[[:space:]]*add:/)  { mode = "add";  next }
      if (mode != "" && $0 ~ /^[[:space:]]*-[[:space:]]*[A-Za-z_]/) {
        line = $0
        sub(/#.*/, "", line)
        sub(/^[[:space:]]*-[[:space:]]*/, "", line)
        gsub(/[[:space:]]/, "", line)
        if (line != "") print mode " " line
      }
    }
  ' "$1"
}

# Items under a top-level YAML list key, e.g. `allowedCapabilities:` in the SCC.
parse_toplevel_list() {
  awk -v key="$2" '
    $0 ~ "^" key ":[[:space:]]*$" { inlist = 1; next }
    inlist {
      if ($0 ~ /^[[:space:]]*#/) next
      if ($0 !~ /^[[:space:]]*-[[:space:]]*[A-Za-z_]/) { inlist = 0; next }
      line = $0
      sub(/#.*/, "", line)
      sub(/^[[:space:]]*-[[:space:]]*/, "", line)
      gsub(/[[:space:]]/, "", line)
      if (line != "") print line
    }
  ' "$1"
}

sorted_csv() { tr ' ' '\n' | sed '/^$/d' | LC_ALL=C sort -u | paste -sd, -; }

echo "-- derivation: the cap set under test comes from the manifest --"
if [ ! -f "$MANIFEST" ]; then
  bad "deployment.yaml not found at $MANIFEST — the derivation this test is built on is gone"
  echo ""
  echo "=== Results: $PASS passed, $FAIL failed ==="
  exit 1
fi

MANIFEST_CAPS="$(parse_capabilities "$MANIFEST")"
MANIFEST_DROP="$(echo "$MANIFEST_CAPS" | awk '$1 == "drop" { print $2 }' | tr '\n' ' ')"
MANIFEST_ADD="$(echo "$MANIFEST_CAPS" | awk '$1 == "add" { print $2 }' | tr '\n' ' ')"
MANIFEST_DROP="$(echo "$MANIFEST_DROP" | xargs || true)"
MANIFEST_ADD="$(echo "$MANIFEST_ADD" | xargs || true)"

echo "     deployment.yaml drop = [${MANIFEST_DROP}]"
echo "     deployment.yaml add  = [${MANIFEST_ADD}]"

if [ "$MANIFEST_DROP" = "ALL" ]; then
  ok "manifest drops ALL before adding back (the posture this job boots)"
else
  bad "manifest drop list is [${MANIFEST_DROP}], expected [ALL] — either the posture regressed or the parser did; fix before trusting anything below"
fi

if [ -z "$MANIFEST_ADD" ]; then
  bad "no capabilities parsed out of deployment.yaml — the derivation is broken, so this job would boot an empty set and prove nothing"
  echo ""
  echo "=== Results: $PASS passed, $FAIL failed ==="
  exit 1
fi
ok "capability add list derived from the manifest ($(echo "$MANIFEST_ADD" | wc -w) capabilities), not restated in this test"

# ── The OpenShift SCC overlay must mirror the manifest exactly ───────────────
# An SCC admits a pod only if EVERY added capability is in allowedCapabilities.
# Drift here does not fail a grep — it fails admission, on the cluster, at deploy.
echo "-- derivation: the openshift-netadmin SCC mirrors the manifest --"
if [ ! -f "$SCC" ]; then
  bad "SCC overlay not found at $SCC"
else
  SCC_ALLOWED="$(parse_toplevel_list "$SCC" allowedCapabilities | sorted_csv)"
  SCC_DROP="$(parse_toplevel_list "$SCC" requiredDropCapabilities | sorted_csv)"
  MANIFEST_ADD_CSV="$(echo "$MANIFEST_ADD" | sorted_csv)"
  MANIFEST_DROP_CSV="$(echo "$MANIFEST_DROP" | sorted_csv)"
  echo "     SCC allowedCapabilities = [${SCC_ALLOWED}]"
  if [ "$SCC_ALLOWED" = "$MANIFEST_ADD_CSV" ]; then
    ok "SCC allowedCapabilities matches deployment.yaml capabilities.add 1:1"
  else
    bad "SCC allowedCapabilities [${SCC_ALLOWED}] != deployment.yaml add [${MANIFEST_ADD_CSV}] — on OpenShift the pod is REJECTED AT ADMISSION until these agree"
  fi
  if [ "$SCC_DROP" = "$MANIFEST_DROP_CSV" ]; then
    ok "SCC requiredDropCapabilities matches deployment.yaml capabilities.drop"
  else
    bad "SCC requiredDropCapabilities [${SCC_DROP}] != deployment.yaml drop [${MANIFEST_DROP_CSV}]"
  fi
fi

# ── The hosted-hive template in saas_provision.go ────────────────────────────
#
# Acceptance criterion from #4379: the hosted template is covered by the same
# derivation, or the reason it is not is recorded. It IS covered, by containment:
# the hosted template adds capabilities but does NOT drop ALL, so its bounding set
# is the runtime default set UNION its add list. The runtime default set already
# contains CHOWN, DAC_OVERRIDE, SETGID, SETUID and SETPCAP, so as long as the
# hosted add list is a subset of the manifest's, the hosted pod's bounding set is
# a strict SUPERSET of the one booted below — the manifest boot is the tighter
# case and passing it implies the hosted case. Both halves of that argument are
# asserted rather than assumed: if the template ever gains its own `drop:`, or an
# `add:` entry the manifest does not have, the containment breaks and this fails
# with an instruction to give the template its own boot.
echo "-- derivation: the hosted-hive template (saas_provision.go) is covered --"
if [ ! -f "$SAAS" ]; then
  bad "saas_provision.go not found at $SAAS"
else
  SAAS_CAPS="$(parse_capabilities "$SAAS")"
  SAAS_DROP="$(echo "$SAAS_CAPS" | awk '$1 == "drop" { print $2 }' | xargs || true)"
  SAAS_ADD="$(echo "$SAAS_CAPS" | awk '$1 == "add" { print $2 }' | xargs || true)"
  echo "     saas_provision.go drop = [${SAAS_DROP:-<none>}]"
  echo "     saas_provision.go add  = [${SAAS_ADD:-<none>}]"

  if [ -z "$SAAS_ADD" ]; then
    bad "no capabilities parsed out of the hosted template — either it stopped granting any (NET_ADMIN loss would silently disable the ACMM egress gate on every hosted hive) or the parser broke"
  else
    saas_outside=""
    for c in $SAAS_ADD; do
      case " $MANIFEST_ADD " in
        *" $c "*) : ;;
        *) saas_outside="$saas_outside $c" ;;
      esac
    done
    if [ -z "$saas_outside" ]; then
      ok "hosted template's add list is a subset of the manifest's — covered by containment (its bounding set is a superset of the one booted here)"
    else
      bad "hosted template adds [${saas_outside# }] which deployment.yaml does not — containment broken, the hosted pod needs its own boot in this job"
    fi
  fi

  if [ -z "$SAAS_DROP" ]; then
    ok "hosted template declares no drop list, so its bounding set stays the runtime default plus its adds (the containment premise holds)"
  else
    bad "hosted template now declares drop [${SAAS_DROP}] — its bounding set is no longer a superset of the manifest's, so the containment argument is void; boot it explicitly in this job"
  fi
fi

echo ""

# ── Runtime ──────────────────────────────────────────────────────────────────
if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  echo "-- runtime: SKIP --"
  echo "  SKIP: docker unavailable — the capability boot requires it"
  echo "  (CI runs this on ubuntu-latest where docker is present)"
  echo ""
  echo "=== Results: $PASS passed, $FAIL failed ==="
  [ "$FAIL" -eq 0 ]
  exit $?
fi

# Everything the fixture needs is READ OUT OF src/Dockerfile, so the container the
# privilege chain runs in is built from the shipped image's own parameters: same
# runtime base, same su-exec commit + SHA256 pin, same hive-launch GID, same dev
# UID and primary group. A hand-copied fixture would drift exactly like a
# hand-copied cap list.
dockerfile_arg() { sed -n "s/^ARG $1=\(.*\)$/\1/p" "$DOCKERFILE" | head -1; }

RUNTIME_BASE="$(sed -n 's/^FROM \(.*\) AS runtime$/\1/p' "$DOCKERFILE" | head -1)"
SU_EXEC_COMMIT="$(dockerfile_arg SU_EXEC_COMMIT)"
SU_EXEC_SHA256="$(dockerfile_arg SU_EXEC_SHA256)"
HIVE_LAUNCH_GID="$(dockerfile_arg HIVE_LAUNCH_GID)"
# `useradd -m -u 1001 -g node -G hive-launch -s /bin/bash dev`
DEV_LINE="$(grep -m1 'useradd -m -u .* dev' "$DOCKERFILE")"
# The Dockerfile line arrives as ` && useradd ... dev \` — strip the shell
# continuation and the `&&` so it can be re-emitted as its own RUN step.
DEV_USERADD="$(echo "$DEV_LINE" | sed -e 's/^[[:space:]]*&&[[:space:]]*//' -e 's/[[:space:]]*\\[[:space:]]*$//' -e 's/^[[:space:]]*//')"
DEV_UID="$(echo "$DEV_LINE" | sed -n 's/.*-u \([0-9]\+\).*/\1/p')"
DEV_GROUP="$(echo "$DEV_LINE" | sed -n 's/.*-g \([A-Za-z0-9_-]\+\).*/\1/p')"
LAUNCH_GROUP="$(echo "$DEV_LINE" | sed -n 's/.*-G \([A-Za-z0-9_-]\+\).*/\1/p')"
SU_EXEC_MODE="$(sed -n 's;.*chmod \([0-7]\{4\}\) /usr/local/bin/su-exec.*;\1;p' "$DOCKERFILE" | head -1)"
SU_EXEC_OWNER="$(sed -n 's;.*chown \([A-Za-z0-9_:-]\+\) /usr/local/bin/su-exec.*;\1;p' "$DOCKERFILE" | head -1)"
# The entrypoint's REAL privilege-drop invocation, extracted the same way
# test_ambient_cap_runtime.sh extracts it.
ENTRY_CAPS="$(sed -n 's/^[[:space:]]*_SETPRIV_CAPS="\(.*\)"[[:space:]]*$/\1/p' "$ENTRYPOINT" | head -1)"
ENTRY_ID="$(sed -n 's/^[[:space:]]*_SETPRIV_ID="\(.*\)"[[:space:]]*$/\1/p' "$ENTRYPOINT" | head -1)"
AGENT_UID_BASE="$(sed -n 's/^[[:space:]]*HIVE_UID_BASE=\([0-9]\+\)[[:space:]]*$/\1/p' "$ENTRYPOINT" | head -1)"
# `useradd --system -u "$AGENT_UID" -g node -d /data/home -M ... "hive-${agent_name}"`
# shellcheck disable=SC2016  # $AGENT_UID is literal text in the entrypoint, not an expansion
AGENT_GROUP="$(grep -m1 'useradd --system -u "\$AGENT_UID"' "$ENTRYPOINT" | sed -n 's/.*-g \([A-Za-z0-9_-]\+\).*/\1/p')"

echo "-- runtime: fixture parameters derived from src/Dockerfile + entrypoint.sh --"
printf '     runtime base = %s\n' "${RUNTIME_BASE:-<none>}"
printf '     su-exec      = %s (sha256 %s), %s %s\n' \
  "${SU_EXEC_COMMIT:0:12}" "${SU_EXEC_SHA256:0:12}" "${SU_EXEC_OWNER:-<none>}" "${SU_EXEC_MODE:-<none>}"
printf '     dev          = uid %s group %s, supplementary %s (gid %s)\n' \
  "${DEV_UID:-<none>}" "${DEV_GROUP:-<none>}" "${LAUNCH_GROUP:-<none>}" "${HIVE_LAUNCH_GID:-<none>}"
printf '     agent        = uid %s group %s\n' "${AGENT_UID_BASE:-<none>}" "${AGENT_GROUP:-<none>}"
printf '     setpriv      = %s %s\n' "${ENTRY_CAPS:-<none>}" "${ENTRY_ID:-<none>}"

derivation_ok=1
for v in RUNTIME_BASE SU_EXEC_COMMIT SU_EXEC_SHA256 HIVE_LAUNCH_GID DEV_UID DEV_GROUP DEV_USERADD \
         LAUNCH_GROUP SU_EXEC_MODE SU_EXEC_OWNER ENTRY_CAPS ENTRY_ID AGENT_UID_BASE AGENT_GROUP; do
  if [ -z "${!v}" ]; then
    bad "could not derive $v from src/Dockerfile / src/deploy/entrypoint.sh — the fixture would not mirror the shipped image, so the boot below would prove the wrong thing"
    derivation_ok=0
  fi
done
if [ "$derivation_ok" -eq 1 ]; then
  ok "every fixture parameter derived from the shipped image definition"
else
  echo ""
  echo "=== Results: $PASS passed, $FAIL failed ==="
  exit 1
fi

# Set HIVE_CAP_KEEP_WORK=<dir> to keep the generated Dockerfile, the probe and
# every per-run log (positive, isolation, one per mutation) for inspection —
# useful when a CI failure needs more than the summary lines below.
WORK="${HIVE_CAP_KEEP_WORK:-$(mktemp -d)}"
mkdir -p "$WORK"
cleanup() {
  if [ -z "${HIVE_CAP_KEEP_WORK:-}" ]; then
    rm -rf "$WORK"
    docker rmi -f "$FIXTURE_IMAGE" >/dev/null 2>&1 || true
  fi
}
FIXTURE_IMAGE="hive-capprobe:4379"
trap cleanup EXIT

# ── The probe: runs INSIDE the container, under the cap set being tested ─────
cat > "$WORK/probe.sh" <<'PROBE_EOF'
#!/bin/sh
# Executed inside the container. Prints one "STEP <name> PASS|FAIL <detail>" line
# per assertion plus MISSING_FROM_BOUNDING=<caps>, and exits non-zero on failure.
# It NEVER aborts early: the mutation runs need to see which specific steps break.
set -u

fails=0
step() {
  if [ "$2" -eq 0 ]; then
    echo "STEP $1 PASS ${3:-}"
  else
    echo "STEP $1 FAIL ${3:-}"
    fails=$((fails + 1))
  fi
}

# ── What the kernel actually gave us ────────────────────────────────────────
capbnd_hex="$(awk '/^CapBnd:/ { print $2 }' /proc/self/status)"
BND="$(capsh --decode=0x${capbnd_hex} 2>/dev/null | sed 's/^[^=]*=//' | tr -d '[:space:]')"
echo "CAPBND=0x${capbnd_hex}"
echo "CAPBND_NAMES=${BND}"

has_cap() { case ",${BND}," in *",cap_$1,"*) return 0 ;; *) return 1 ;; esac; }

missing=""
for c in ${EXPECT_CAPS}; do
  lc="$(echo "$c" | tr 'A-Z' 'a-z')"
  has_cap "$lc" || missing="${missing} ${c}"
done
echo "MISSING_FROM_BOUNDING=${missing# }"

# ── #3895 ISOLATION MODE ────────────────────────────────────────────────────
# Started as `dev` by the runtime (docker --user), so reaching dev needs no
# capability at all. That isolates the SUID su-exec hop from the entrypoint's
# setpriv drop: without this mode, removing SETUID kills the drop first and the
# su-exec assertion never gets to run, so it would prove nothing on its own.
#
# Here `dev` execs the 4750 root:hive-launch su-exec directly. A SUID-root exec
# raises the new permitted set to the BOUNDING set, so su-exec's
# setgroups()/setgid()/setuid() to the agent UID succeed if and only if SETGID and
# SETUID are actually in that bounding set — which is precisely what #3895 would
# have removed. The numeric `uid:gid` spec is the one manager.go's
# agentExecUserSpec() falls back to when the per-agent user is absent.
if [ "${AS_DEV:-0}" = "1" ]; then
  agent_gid="$(getent group "${AGENT_GROUP}" | cut -d: -f3)"
  hop_out="$(su-exec "${AGENT_UID}:${agent_gid}" id -u 2>&1)"
  hop_rc=$?
  [ "$hop_rc" -eq 0 ] && [ "$hop_out" = "${AGENT_UID}" ]
  step su-exec-suid-hop-from-dev $? "needs SETUID+SETGID in the bounding set; rc=${hop_rc} got '$(echo "$hop_out" | tr '\n' ' ' | cut -c1-160)' want ${AGENT_UID}"
  echo "FAILED_STEPS=${fails}"
  if [ "$fails" -eq 0 ]; then exit 0; else exit 1; fi
fi

# The bounding set must be EXACTLY the manifest's grant on the unmutated run.
# This is what proves `drop: ALL` actually applied and the harness is not
# silently running with the runtime's defaults.
if [ "${REQUIRE_EXACT}" = "1" ]; then
  want="$(echo "${EXPECT_CAPS}" | tr 'A-Z ' 'a-z\n' | sed '/^$/d' | sed 's/^/cap_/' | LC_ALL=C sort | paste -sd, -)"
  got="$(echo "${BND}" | tr ',' '\n' | sed '/^$/d' | LC_ALL=C sort | paste -sd, -)"
  [ "$want" = "$got" ]
  step capbnd-is-exactly-the-manifest-grant $? "want=${want} got=${got}"

  # Explicit canary: a capability in the runtime's DEFAULT set that the manifest
  # does not add. If this is present, drop:ALL did not apply.
  has_cap "${CANARY_CAP}" && r=1 || r=0
  step canary-${CANARY_CAP}-is-dropped $r "not in the manifest add list; present => drop:ALL did not apply"
fi

# ── Fixtures the entrypoint would create ────────────────────────────────────
# Mirrors entrypoint.sh's per-agent user creation. Done before the capability
# assertions so a chown failure below cannot be confused with a setup failure.
if ! id -u "${AGENT_USER}" >/dev/null 2>&1; then
  useradd --system -u "${AGENT_UID}" -g "${AGENT_GROUP}" -d /data/home -M -s /bin/bash "${AGENT_USER}" >/dev/null 2>&1 || true
fi
if ! id -u "${AGENT_USER}" >/dev/null 2>&1; then
  # useradd itself can need CHOWN depending on its config; fall back to the raw
  # passwd entry so the CHOWN mutation still exercises the chown STEP and not
  # this setup.
  agent_gid="$(getent group "${AGENT_GROUP}" | cut -d: -f3)"
  echo "${AGENT_USER}:x:${AGENT_UID}:${agent_gid}::/data/home:/bin/bash" >> /etc/passwd
fi
if ! id -u "${AGENT_USER}" >/dev/null 2>&1; then
  echo "SETUP FAIL: could not create the per-agent user ${AGENT_USER}"
  exit 90
fi

# ── CHOWN: entrypoint chowns per-agent dirs/beads to the agent UID ───────────
mkdir -p /data/agents/capprobe /data/beads/capprobe
chown -R "${AGENT_USER}:${AGENT_GROUP}" /data/agents/capprobe /data/beads/capprobe 2>/dev/null
owner="$(stat -c '%u' /data/agents/capprobe 2>/dev/null || echo -1)"
[ "$owner" = "${AGENT_UID}" ]
step chown-agent-dir-to-agent-uid $? "needs CHOWN; /data/agents/capprobe owner=${owner} want=${AGENT_UID}"

# ── DAC_OVERRIDE: root writes through a mode that denies it ─────────────────
# Root writing to a mode-0000 file is refused by generic_permission() (the owner
# class has no bits) unless CAP_DAC_OVERRIDE is in the effective set. DAC_READ_
# SEARCH, the other cap that would let this through, is not in the manifest.
: > /data/dac-probe 2>/dev/null
chmod 000 /data/dac-probe 2>/dev/null
echo probe > /data/dac-probe 2>/dev/null
[ -s /data/dac-probe ]
step dac-override-write-through-mode-000 $? "needs DAC_OVERRIDE; root write into a 0000 file"

# ── NET_ADMIN as root: the ACMM forced-egress REDIRECT ──────────────────────
if [ "${RUN_EGRESS_GATE}" = "1" ]; then
  gate_err="$( { iptables -t nat -N HIVE_CAPPROBE \
    && iptables -t nat -A HIVE_CAPPROBE -p tcp --dport 443 -j REDIRECT --to-ports 8443 \
    && iptables -t nat -C HIVE_CAPPROBE -p tcp --dport 443 -j REDIRECT --to-ports 8443 ; } 2>&1 )"
  r=$?
  step egress-gate-redirect-installs $r "needs NET_ADMIN; $(echo "$gate_err" | tr '\n' ' ' | cut -c1-160)"
  iptables -t nat -F HIVE_CAPPROBE >/dev/null 2>&1
  iptables -t nat -X HIVE_CAPPROBE >/dev/null 2>&1
fi

# ── The entrypoint's OWN root->dev drop, with its OWN flags ─────────────────
# Extracted from entrypoint.sh by the harness, not restated. Needs SETUID (reuid),
# SETGID (regid + init-groups) and NET_ADMIN (the inheritable/ambient raise).
drop_out="$(setpriv ${SETPRIV_CAPS} ${SETPRIV_ID} sh -c \
  'printf "%s %s %s " "$(id -u)" "$(id -g)" "$(id -Gn | tr " " "+")"; grep -m1 "^CapAmb:" /proc/self/status | awk "{print \$2}"' 2>&1)"
drop_rc=$?
# Only parse the fields when setpriv actually succeeded — on failure `drop_out` is
# an error message, and feeding a word of it to $(( )) is a fatal syntax error in
# dash that would abort the probe and silently skip every assertion below. The
# mutation runs depend on this script surviving its own failures.
d_uid=""; d_gid=""; d_groups=""; d_amb=""
if [ "$drop_rc" -eq 0 ]; then
  set -- $drop_out
  d_uid="${1:-}"; d_gid="${2:-}"; d_groups="${3:-}"; d_amb="${4:-}"
fi
drop_detail="$(echo "$drop_out" | tr '\n' ' ' | cut -c1-160)"
[ "$drop_rc" -eq 0 ] && [ "$d_uid" = "${DEV_UID}" ]
step entrypoint-setpriv-drop-to-dev $? "needs SETUID+SETGID; rc=${drop_rc} uid=${d_uid:-<none>} gid=${d_gid:-<none>} groups=${d_groups:-<none>} out=${drop_detail}"

# dev MUST retain the launcher group, or it cannot exec the 4750 su-exec helper.
case "+${d_groups}+" in
  *"+${LAUNCH_GROUP}+"*) r=0 ;;
  *) r=1 ;;
esac
step dropped-dev-keeps-launcher-group $r "needs SETGID (--init-groups); groups=${d_groups:-<none>} want ${LAUNCH_GROUP}"

# The #3874 ambient bit, now read back under the REAL bounding set rather than
# docker's defaults. CAP_NET_ADMIN is capability 12 -> mask 0x1000.
case "${d_amb}" in
  ""|*[!0-9A-Fa-f]*) amb_rc=1 ;;
  *) [ $(( 0x${d_amb} & 0x1000 )) -ne 0 ] && amb_rc=0 || amb_rc=1 ;;
esac
step ambient-net-admin-survives-drop $amb_rc "needs NET_ADMIN (and SETUID/SETGID to reach the drop at all); CapAmb=${d_amb:-<unset>}"

# ── The gosu fallback path the entrypoint takes when setpriv can't deliver ──
gosu_uid="$(gosu "${DEV_USER}" id -u 2>&1)"
[ "$gosu_uid" = "${DEV_UID}" ]
step gosu-fallback-drop-to-dev $? "needs SETUID+SETGID; got '${gosu_uid}'"

# ── THE #3895 ASSERTION: dev --su-exec--> per-agent UID ─────────────────────
# This is the hop that `--cap-drop SETUID` breaks and that no test booted before.
# su-exec is SUID-root and mode 4750 root:hive-launch, so exec'ing it as dev
# raises pP to the BOUNDING set; its setgroups()/setgid()/setuid() to the agent
# UID then succeed only if SETGID/SETUID are actually in that bounding set.
se_out="$(setpriv ${SETPRIV_ID} su-exec "${AGENT_USER}" id -u 2>&1)"
se_rc=$?
[ "$se_rc" -eq 0 ] && [ "$se_out" = "${AGENT_UID}" ]
step su-exec-dev-to-agent-uid $? "needs SETUID+SETGID; rc=${se_rc} got '$(echo "$se_out" | tr '\n' ' ' | cut -c1-160)' want ${AGENT_UID}"

# And the process must really RUN as the agent, not merely start: have it write a
# file and check the kernel recorded the agent as the owner.
setpriv ${SETPRIV_ID} su-exec "${AGENT_USER}" sh -c 'echo alive > /data/agents/capprobe/alive' >/dev/null 2>&1
se_owner="$(stat -c '%u' /data/agents/capprobe/alive 2>/dev/null || echo -1)"
[ "$se_owner" = "${AGENT_UID}" ]
step su-exec-agent-process-actually-runs $? "needs SETUID+SETGID(+CHOWN for the dir); file owner=${se_owner} want ${AGENT_UID}"

echo "FAILED_STEPS=${fails}"
[ "$fails" -eq 0 ]
PROBE_EOF

# ── The fixture image ────────────────────────────────────────────────────────
# Same runtime base, su-exec pin and identity contract as src/Dockerfile, without
# the ~GB of agent tooling that has no bearing on the capability contract. The
# su-exec mode/owner the image actually ends up with is ASSERTED against the
# Dockerfile's own values below, so this cannot quietly diverge.
cat > "$WORK/Dockerfile" <<EOF
FROM ${RUNTIME_BASE}
RUN apt-get update && apt-get install -y --no-install-recommends \\
      ca-certificates curl gcc libc6-dev gosu util-linux iptables libcap2-bin \\
 && rm -rf /var/lib/apt/lists/*
ARG SU_EXEC_COMMIT=${SU_EXEC_COMMIT}
ARG SU_EXEC_SHA256=${SU_EXEC_SHA256}
RUN curl -fsSL --retry 8 --retry-delay 5 --retry-max-time 300 --retry-connrefused --retry-all-errors \\
      -o /tmp/su-exec.c "https://raw.githubusercontent.com/ncopa/su-exec/\${SU_EXEC_COMMIT}/su-exec.c" \\
 && echo "\${SU_EXEC_SHA256}  /tmp/su-exec.c" | sha256sum -c - \\
 && gcc -Wall -o /usr/local/bin/su-exec /tmp/su-exec.c \\
 && rm /tmp/su-exec.c
ARG HIVE_LAUNCH_GID=${HIVE_LAUNCH_GID}
RUN groupadd --system --gid "\${HIVE_LAUNCH_GID}" ${LAUNCH_GROUP} \\
 && ${DEV_USERADD} \\
 && chown ${SU_EXEC_OWNER} /usr/local/bin/su-exec \\
 && chmod ${SU_EXEC_MODE} /usr/local/bin/su-exec
RUN mkdir -p /data/home
EOF

echo ""
echo "-- runtime: building the fixture image --"
if ! docker build -t "$FIXTURE_IMAGE" -f "$WORK/Dockerfile" "$WORK" >"$WORK/build.log" 2>&1; then
  echo "     docker build failed:"
  tail -30 "$WORK/build.log" | sed 's/^/     /'
  bad "could not build the capability fixture image — the runtime half of this job could not run"
  echo ""
  echo "=== Results: $PASS passed, $FAIL failed ==="
  exit 1
fi
ok "fixture image built from the shipped runtime base and su-exec pin"

# The fixture's su-exec must carry EXACTLY the mode/owner src/Dockerfile ships.
# If it does not, the su-exec assertions below would be testing a different
# binary contract than production runs.
SE_ACTUAL="$(docker run --rm "$FIXTURE_IMAGE" stat -c '%a %U:%G' /usr/local/bin/su-exec 2>/dev/null)"
SE_WANT="${SU_EXEC_MODE} ${SU_EXEC_OWNER}"
if [ "$SE_ACTUAL" = "$SE_WANT" ]; then
  ok "fixture su-exec is ${SE_ACTUAL}, matching src/Dockerfile's own chmod/chown"
else
  bad "fixture su-exec is '${SE_ACTUAL}' but src/Dockerfile declares '${SE_WANT}' — the fixture does not mirror the shipped SUID contract"
fi

# ── Running the probe ────────────────────────────────────────────────────────
docker_cap_args() {
  local caps="$1" out="--cap-drop=ALL" c
  for c in $caps; do out="$out --cap-add=$c"; done
  echo "$out"
}

# $1 = capability list to grant, $2 = REQUIRE_EXACT, $3 = output file,
# $4 (optional) = extra `docker run` flags, $5 (optional) = AS_DEV
run_probe() {
  local caps="$1" exact="$2" outfile="$3" extra="${4:-}" as_dev="${5:-0}"
  # shellcheck disable=SC2046,SC2086  # deliberate word-splitting of the flags
  docker run --rm $(docker_cap_args "$caps") $extra \
    -e "AS_DEV=$as_dev" \
    -v "$WORK/probe.sh:/probe.sh:ro" \
    -e "EXPECT_CAPS=$MANIFEST_ADD" \
    -e "REQUIRE_EXACT=$exact" \
    -e "CANARY_CAP=$CANARY_CAP" \
    -e "RUN_EGRESS_GATE=$RUN_EGRESS_GATE" \
    -e "DEV_USER=dev" \
    -e "DEV_UID=$DEV_UID" \
    -e "LAUNCH_GROUP=$LAUNCH_GROUP" \
    -e "AGENT_USER=hive-capprobe" \
    -e "AGENT_UID=$AGENT_UID_BASE" \
    -e "AGENT_GROUP=$AGENT_GROUP" \
    -e "SETPRIV_CAPS=$ENTRY_CAPS" \
    -e "SETPRIV_ID=$ENTRY_ID" \
    "$FIXTURE_IMAGE" sh /probe.sh >"$outfile" 2>&1
  return $?
}

# Canary: a capability in the runtime's DEFAULT bounding set that the manifest
# does not add. Picked from the defaults rather than hardcoded blindly — if the
# manifest ever adds it, pick another, otherwise the canary proves nothing.
CANARY_CAP="mknod"
case " $MANIFEST_ADD " in
  *" MKNOD "*) CANARY_CAP="net_raw" ;;
esac

# Preflight: iptables inside a container is environment-dependent (nf_tables
# backend, kernel modules on the host). Establish whether it works AT ALL here
# with NET_ADMIN before making its failure meaningful — otherwise a hostile CI
# runner would masquerade as a capability regression.
RUN_EGRESS_GATE=1
if ! docker run --rm --cap-add=NET_ADMIN "$FIXTURE_IMAGE" \
     sh -c 'iptables -t nat -N HIVE_PREFLIGHT && iptables -t nat -X HIVE_PREFLIGHT' >/dev/null 2>&1; then
  RUN_EGRESS_GATE=0
  echo "  NOTE: iptables/nat is not usable in a container on this host even WITH NET_ADMIN"
  echo "        (no nf_tables/iptables_nat module, or a restricted runtime). The ACMM"
  echo "        egress-gate step is omitted; every other assertion still runs, and"
  echo "        NET_ADMIN is still covered by the ambient-capability step."
fi

echo ""
echo "-- runtime: POSITIVE — boot under the manifest's EXACT grant --"
echo "     docker run $(docker_cap_args "$MANIFEST_ADD")"
if run_probe "$MANIFEST_ADD" 1 "$WORK/positive.log"; then
  sed 's/^/     /' "$WORK/positive.log"
  ok "the whole privilege chain works under the manifest's exact capability set"
else
  sed 's/^/     /' "$WORK/positive.log"
  missing_line="$(sed -n 's/^MISSING_FROM_BOUNDING=//p' "$WORK/positive.log")"
  bad "the SHIPPED capability set does not boot the privilege chain$( [ -n "$missing_line" ] && echo " (missing from the bounding set: ${missing_line})" ) — see the failing STEP lines above"
fi

# ── #3895 isolation: the su-exec hop, decoupled from the setpriv drop ────────
# In the runs above, removing SETUID breaks the root->dev drop first, so the
# su-exec assertions fail as collateral rather than on their own merits. These
# three runs start the container AS dev, so no capability is needed to get there
# and the only thing under test is the SUID hop to the agent UID — the exact
# transition #3895 would have broken.
echo ""
echo "-- runtime: #3895 ISOLATION — the SUID su-exec hop on its own --"
if run_probe "$MANIFEST_ADD" 0 "$WORK/iso-positive.log" "--user dev" 1; then
  sed 's/^/     /' "$WORK/iso-positive.log"
  ok "dev can su-exec to the per-agent UID under the manifest's exact capability set"
else
  sed 's/^/     /' "$WORK/iso-positive.log"
  bad "dev CANNOT su-exec to the per-agent UID under the shipped capability set — agent launch is broken in the pod"
fi

for victim in SETUID SETGID; do
  case " $MANIFEST_ADD " in
    *" $victim "*) : ;;
    *)
      bad "$victim is no longer in deployment.yaml's capabilities.add — the su-exec hop to per-agent UIDs cannot work in the pod (this is exactly what #3895 proposed and review rejected)"
      continue
      ;;
  esac
  reduced=""
  for c in $MANIFEST_ADD; do
    [ "$c" = "$victim" ] || reduced="$reduced $c"
  done
  if run_probe "${reduced# }" 0 "$WORK/iso-mut-$victim.log" "--user dev" 1; then
    bad "the su-exec hop still succeeded WITHOUT $victim — this isolation check is vacuous and would not have caught #3895; re-derive it"
  else
    ok "removing $victim alone breaks the su-exec hop to the agent UID (the #3895 regression, reproduced in-test)"
  fi
done

# ── Mutation ─────────────────────────────────────────────────────────────────
# Every capability in the manifest, removed one at a time. Each removal must break
# the chain, and the failure must name the capability. This is what keeps the
# positive run honest: a step that would pass with the capability gone was never
# testing that capability.
echo ""
echo "-- runtime: MUTATION — remove each capability in turn, require a break --"
unexercised=""
for victim in $MANIFEST_ADD; do
  reduced=""
  for c in $MANIFEST_ADD; do
    [ "$c" = "$victim" ] || reduced="$reduced $c"
  done
  reduced="${reduced# }"
  if run_probe "$reduced" 0 "$WORK/mut-$victim.log"; then
    echo "  ---- ${victim} removed: the chain still passed ----"
    unexercised="$unexercised $victim"
  else
    named="$(sed -n 's/^MISSING_FROM_BOUNDING=//p' "$WORK/mut-$victim.log" | tr -d ' ')"
    broke="$(sed -n 's/^STEP \([^ ]*\) FAIL.*/\1/p' "$WORK/mut-$victim.log" | tr '\n' ' ')"
    echo "     ${victim} removed -> failing steps: ${broke:-<none>}"
    if [ "$named" = "$victim" ]; then
      ok "dropping ${victim} breaks the privilege chain, and the failure names ${victim}"
    else
      bad "dropping ${victim} breaks the chain but the report names '${named:-<nothing>}' instead of ${victim} — the failure would not tell an operator which capability to restore"
    fi
  fi
done

for victim in $unexercised; do
  case " $UNEXERCISED_ALLOWLIST " in
    *" $victim "*)
      ok "${victim} is a documented unexercised grant (see UNEXERCISED_ALLOWLIST) — recorded, not silently ignored"
      ;;
    *)
      bad "${victim} is granted by deployment.yaml but removing it breaks NOTHING this job can reach: the grant is either unnecessary or its consumer is untested. Add a step that needs it, or add it to UNEXERCISED_ALLOWLIST with the kernel-level reason"
      ;;
  esac
done

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
