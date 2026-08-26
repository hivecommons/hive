#!/usr/bin/env bash
# test_image_suid_inventory.sh — RUNTIME assertion that the shipped image
# contains exactly ONE setuid/setgid binary: the su-exec launcher helper
# (#3866, CWE-250/CWE-732).
#
# Why a runtime job and not another grep. src/scripts/check-suid-contract.sh is
# a static check against src/Dockerfile, and it only ever looks at su-exec. It
# cannot see what the BASE IMAGE ships, and that is where the surface actually
# came from: node:26-slim (Debian 13) plus the passwd/util-linux packages the
# runtime stage installs leave eleven world-executable setuid/setgid binaries
# in the image —
#
#   4755 root:root    /usr/bin/{chfn,chsh,gpasswd,mount,newgrp,passwd,su,umount}
#   2755 root:shadow  /usr/bin/{chage,expiry}  /usr/sbin/unix_chkpwd
#
# — every one of them reachable by an agent UID (2001+), the exact population
# `chmod 4750 root:hive-launch` on su-exec exists to exclude. The pod cannot
# switch them off with no_new_privs, because `allowPrivilegeEscalation: false`
# would disable su-exec too and break agent launch (the C6 note in
# src/deploy/k8s/deployment.yaml). So the image is where they have to go, and
# src/Dockerfile now strips them.
#
# This job proves the strip works, on a real built image, and — the part a
# green check is worthless without — that it would FAIL if the strip regressed.
# It builds the same fixture twice, once WITHOUT the Dockerfile's strip block
# and once WITH it, and requires the un-stripped build to be caught. Then it
# execs su-exec as `dev` to show the surviving setuid bit still does its job.
#
# Everything the fixture uses — base digest, apt list, su-exec pin, launch GID,
# the dev useradd line, and the strip block itself — is DERIVED from
# src/Dockerfile at run time. This script keeps no copy of its own, so it cannot
# quietly test a contract the image no longer ships. Same discipline as
# test_manifest_caps_runtime.sh.
#
# Usage:
#   src/deploy/test_image_suid_inventory.sh                 # build the fixture
#   src/deploy/test_image_suid_inventory.sh --image REF     # check a built image
#   src/deploy/test_image_suid_inventory.sh --dockerfile P  # non-default path
set -uo pipefail

DOCKERFILE="src/Dockerfile"
TARGET_IMAGE=""

while [ $# -gt 0 ]; do
  case "$1" in
    --image) TARGET_IMAGE="${2:-}"; shift 2 ;;
    --dockerfile) DOCKERFILE="${2:-}"; shift 2 ;;
    -h|--help) sed -n '2,40p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

PASS=0
FAIL=0
ok()  { echo "  ok: $1"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); }

# The one line the whole contract reduces to. `%M %u:%g %p` from GNU find.
EXPECTED_INVENTORY="-rwsr-x--- root:hive-launch /usr/local/bin/su-exec"

# Enumerate every setuid/setgid file in an image, sorted, one per line.
INVENTORY_CMD='find / -xdev -type f \( -perm -4000 -o -perm -2000 \) -printf "%M %u:%g %p\n" | sort'

inventory_of() {
  docker run --rm --entrypoint /bin/sh "$1" -c "$INVENTORY_CMD" 2>/dev/null
}

echo "=== Image SUID/SGID inventory contract (#3866) ==="
echo ""

# ── Mode 1: check an image someone else built ────────────────────────────────
if [ -n "$TARGET_IMAGE" ]; then
  echo "-- checking $TARGET_IMAGE --"
  actual="$(inventory_of "$TARGET_IMAGE")"
  if [ -z "$actual" ]; then
    bad "could not read a SUID/SGID inventory out of $TARGET_IMAGE (image missing, or no /bin/sh)"
  elif [ "$actual" = "$EXPECTED_INVENTORY" ]; then
    ok "inventory is exactly the su-exec helper: $actual"
  else
    bad "inventory is not the single expected helper"
    echo "      want: $EXPECTED_INVENTORY"
    echo "      got:"
    echo "$actual" | sed 's/^/        /'
  fi
  echo ""
  echo "=== Results: $PASS passed, $FAIL failed ==="
  [ "$FAIL" -eq 0 ]
  exit $?
fi

# ── Mode 2: build the fixture from src/Dockerfile's own values ───────────────
if [ ! -f "$DOCKERFILE" ]; then
  echo "ERROR: Dockerfile not found at ${DOCKERFILE}" >&2
  exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "ERROR: docker is required to build the fixture image" >&2
  exit 1
fi

derive() { # derive <description> <sed/grep pipeline output>
  local desc="$1" val="$2"
  if [ -z "$val" ]; then
    echo "ERROR: could not derive ${desc} from ${DOCKERFILE} — it has been" >&2
    echo "       restructured. Fix this script rather than hardcoding a value:" >&2
    echo "       a fixture that does not mirror the shipped image proves nothing." >&2
    exit 1
  fi
  printf '%s' "$val"
}

RUNTIME_BASE="$(derive "the runtime base image" \
  "$(grep -m1 -E '^FROM .*AS runtime' "$DOCKERFILE" | sed -E 's/^FROM[[:space:]]+([^[:space:]]+).*/\1/')")"

SU_EXEC_COMMIT="$(derive "SU_EXEC_COMMIT" \
  "$(grep -m1 -E '^ARG SU_EXEC_COMMIT=' "$DOCKERFILE" | cut -d= -f2)")"
SU_EXEC_SHA256="$(derive "SU_EXEC_SHA256" \
  "$(grep -m1 -E '^ARG SU_EXEC_SHA256=' "$DOCKERFILE" | cut -d= -f2)")"
HIVE_LAUNCH_GID="$(derive "HIVE_LAUNCH_GID" \
  "$(grep -m1 -E '^ARG HIVE_LAUNCH_GID=' "$DOCKERFILE" | cut -d= -f2)")"

# The dev account line, verbatim — its supplementary group membership is what
# makes su-exec executable by dev and by nobody else.
DEV_USERADD="$(derive "the dev useradd line" \
  "$(grep -m1 -E 'useradd -m -u .* dev' "$DOCKERFILE" | sed -E 's/^[[:space:]]*&&[[:space:]]*//; s/[[:space:]]*\\$//')")"

# The apt package list the runtime stage installs. Taken from the FIRST
# apt-get install after the runtime FROM, which is the layer that drags in
# passwd/util-linux — i.e. the layer this test exists to police.
RUNTIME_FROM_LINE="$(grep -n -E '^FROM .*AS runtime' "$DOCKERFILE" | cut -d: -f1)"
APT_PACKAGES="$(derive "the runtime apt package list" \
  "$(sed -n "${RUNTIME_FROM_LINE},\$p" "$DOCKERFILE" \
     | sed -n '/^RUN apt-get update && apt-get install/,/rm -rf \/var\/lib\/apt\/lists/p' \
     | sed -E 's/^RUN apt-get update && apt-get install -y --no-install-recommends//' \
     | sed -E 's/&& rm -rf.*//; s/\\$//' | tr '\n' ' ' | tr -s ' ')")"

# The strip block itself, lifted verbatim out of the Dockerfile. If someone
# deletes it there, this extraction returns nothing and the job fails loudly
# instead of testing a step the image no longer performs.
STRIP_BLOCK="$(derive "the SUID strip RUN block" \
  "$(awk '/^RUN find \/ -xdev -type f .*-perm -4000/{p=1} p{print} p&&!/\\$/{exit}' "$DOCKERFILE")")"

echo "-- derived from ${DOCKERFILE} --"
echo "   runtime base : ${RUNTIME_BASE}"
echo "   launch GID   : ${HIVE_LAUNCH_GID}"
echo "   su-exec pin  : ${SU_EXEC_COMMIT}"
echo "   apt packages :${APT_PACKAGES}"
echo ""

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"; docker rmi -f hive-suid-fixture-raw:test hive-suid-fixture-stripped:test >/dev/null 2>&1 || true' EXIT

# The fixture reproduces the layers that decide the SUID inventory — the base,
# the apt list, and the su-exec install — and none of the ~GB of agent tooling,
# which installs through npm/pip and cannot set a setuid bit. The real image is
# still checked directly by the --image mode above, wired into the build
# workflow; this fixture is what makes the contract testable on every PR.
cat > "$WORK/Dockerfile.raw" <<EOF
FROM ${RUNTIME_BASE}
RUN apt-get update && apt-get install -y --no-install-recommends ${APT_PACKAGES} \\
 && rm -rf /var/lib/apt/lists/*
ARG SU_EXEC_COMMIT=${SU_EXEC_COMMIT}
ARG SU_EXEC_SHA256=${SU_EXEC_SHA256}
RUN curl -fsSL --retry 8 --retry-delay 5 --retry-max-time 300 --retry-connrefused --retry-all-errors \\
      -o /tmp/su-exec.c "https://raw.githubusercontent.com/ncopa/su-exec/\${SU_EXEC_COMMIT}/su-exec.c" \\
 && echo "\${SU_EXEC_SHA256}  /tmp/su-exec.c" | sha256sum -c - \\
 && gcc -Wall -o /usr/local/bin/su-exec /tmp/su-exec.c \\
 && rm /tmp/su-exec.c
ARG HIVE_LAUNCH_GID=${HIVE_LAUNCH_GID}
RUN groupadd --system --gid "\${HIVE_LAUNCH_GID}" hive-launch \\
 && ${DEV_USERADD} \\
 && chown root:hive-launch /usr/local/bin/su-exec \\
 && chmod 4750 /usr/local/bin/su-exec
EOF

# The stripped fixture is the raw one plus the Dockerfile's own block.
cp "$WORK/Dockerfile.raw" "$WORK/Dockerfile.stripped"
printf '%s\n' "$STRIP_BLOCK" >> "$WORK/Dockerfile.stripped"

build() { # build <tag> <dockerfile>
  if ! docker build -t "$1" -f "$2" "$WORK" >"$WORK/build.log" 2>&1; then
    return 1
  fi
}

# ── Negative control: the un-stripped image, which MUST look bad ─────────────
# A contract check that has never been seen to fail is not evidence. This build
# omits only the strip block, so whatever it finds is precisely what the strip
# removes — and it is what the shipped image carried before this change.
echo "-- negative control: building WITHOUT the strip block --"
if ! build hive-suid-fixture-raw:test "$WORK/Dockerfile.raw"; then
  echo "     docker build failed:"
  tail -30 "$WORK/build.log" | sed 's/^/     /'
  bad "could not build the un-stripped fixture — the negative control could not run"
  echo ""
  echo "=== Results: $PASS passed, $FAIL failed ==="
  exit 1
fi

raw_inventory="$(inventory_of hive-suid-fixture-raw:test)"
raw_count="$(printf '%s\n' "$raw_inventory" | grep -c . || true)"
echo "$raw_inventory" | sed 's/^/     /'

if [ "$raw_count" -gt 1 ]; then
  ok "un-stripped base+apt layers ship ${raw_count} setuid/setgid files, so there is something to strip"
else
  bad "un-stripped fixture shows ${raw_count} setuid/setgid file(s) — the negative control found nothing to catch, so a passing strip test would prove nothing"
fi

# The reason they matter is that they are world-executable: an agent UID that is
# in neither root nor hive-launch can still exec them.
# Column 10 of the mode string is the other-execute bit.
world_exec="$(printf '%s\n' "$raw_inventory" | grep -v ' /usr/local/bin/su-exec$' | grep -cE '^.{9}[xst]' || true)"
if [ "$world_exec" -gt 0 ]; then
  ok "${world_exec} of them are world-executable — reachable by every agent UID, which is the finding"
else
  bad "expected the base-image setuid binaries to be world-executable; none are, so the stated risk does not reproduce"
fi

if printf '%s\n' "$raw_inventory" | grep -q '^-rwsr-x--- root:hive-launch /usr/local/bin/su-exec$'; then
  ok "the fixture's su-exec matches the shipped 4750 root:hive-launch contract"
else
  bad "fixture su-exec is not 4750 root:hive-launch — the fixture does not mirror the shipped image"
fi

echo ""

# ── The real assertion: with the Dockerfile's strip block ────────────────────
echo "-- building WITH the Dockerfile's strip block --"
if ! build hive-suid-fixture-stripped:test "$WORK/Dockerfile.stripped"; then
  echo "     docker build failed:"
  tail -30 "$WORK/build.log" | sed 's/^/     /'
  bad "the strip block did not build — note it asserts its own result, so a genuine inventory drift also lands here"
  echo ""
  echo "=== Results: $PASS passed, $FAIL failed ==="
  exit 1
fi
ok "the strip block builds, and its in-build inventory assertion passed"

stripped_inventory="$(inventory_of hive-suid-fixture-stripped:test)"
echo "$stripped_inventory" | sed 's/^/     /'

if [ "$stripped_inventory" = "$EXPECTED_INVENTORY" ]; then
  ok "inventory is exactly the su-exec helper, and nothing else"
else
  bad "inventory is not the single expected helper"
  echo "      want: $EXPECTED_INVENTORY"
  echo "      got:"
  echo "$stripped_inventory" | sed 's/^/        /'
fi

# Named checks for the specific binaries the finding calls out, so a failure
# report says which one came back rather than just "the set differs".
for b in /usr/bin/su /usr/bin/mount /usr/bin/umount /usr/bin/passwd \
         /usr/bin/newgrp /usr/bin/gpasswd /usr/bin/chsh /usr/bin/chfn \
         /usr/bin/chage /usr/bin/expiry /usr/sbin/unix_chkpwd; do
  if printf '%s\n' "$stripped_inventory" | grep -q " ${b}\$"; then
    bad "${b} still carries a setuid/setgid bit"
  fi
done
ok "none of the eleven base-image setuid/setgid binaries survive the strip"

echo ""

# ── su-exec still works, which is the whole point of the exception ───────────
# Stripping the wrong bit would break agent launch, and it would break it in the
# pod rather than here. Exec the helper as `dev`, the way the agent manager
# does, and require the UID switch to actually happen.
echo "-- su-exec still performs the dev -> agent UID switch --"
switch_out="$(docker run --rm --user dev --entrypoint /bin/sh \
  hive-suid-fixture-stripped:test -c '/usr/local/bin/su-exec 2001:1000 id -u' 2>&1)"
if [ "$switch_out" = "2001" ]; then
  ok "dev execs the surviving setuid su-exec and lands on UID 2001"
else
  bad "su-exec no longer switches UID as dev (got: ${switch_out}) — the strip broke agent launch"
fi

# And the exclusion the 4750 mode buys is still real: an agent UID cannot.
agent_out="$(docker run --rm --user 2001:1000 --entrypoint /bin/sh \
  hive-suid-fixture-stripped:test -c '/usr/local/bin/su-exec 0:0 id -u 2>&1 || true' 2>&1)"
if printf '%s' "$agent_out" | grep -q '^0$'; then
  bad "an agent UID execed su-exec and became root — the hive-launch group gate is gone"
else
  ok "an agent UID still cannot exec su-exec (${agent_out%%$'\n'*})"
fi

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
