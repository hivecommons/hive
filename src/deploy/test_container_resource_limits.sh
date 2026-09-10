#!/usr/bin/env bash
# The contributor container must run inside the same resource envelope that
# contribute-k8s has always applied to this workload (kubestellar/hive#6485).
# Run: bash src/deploy/test_container_resource_limits.sh
#
# WHAT WENT WRONG. `just contribute-hive <backend>` (container mode) started
# the contributor container with no --memory / --cpus at all, while
# `just contribute-k8s` gave the SAME workload a 1Gi request / 4Gi limit.
# On a contributor workstation the unbounded container grew until
# systemd-oomd crossed its 90% memory+swap thresholds and SIGKILLed the
# whole cgroup — the operator saw only "exited with code 137" and the
# assigned task was abandoned mid-flight.
#
# Three contracts pinned here:
#   1. the container-run invocation carries ${RESOURCE_FLAGS};
#   2. the shipped flag-construction logic, EVALUATED (not restated), yields
#      the k8s-envelope defaults, honours HIVE_CONTAINER_MEMORY /
#      HIVE_CONTAINER_CPUS overrides, and can be disabled with `none`;
#   3. the recipe explains an OOM/137 exit and names the override, instead
#      of printing a bare exit code.
set -uo pipefail

PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; [ $# -gt 1 ] && echo "        $2"; FAIL=$((FAIL + 1)); }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
JUSTFILE="${ROOT}/Justfile"

echo "=== contributor container resource limits (#6485) ==="

# ── 1. The run invocation carries the resource flags ─────────────────────────
#
# Anchored to the SAME container-run invocation that passes
# HIVE_CONTAINER_NAME (the anchor test_attach_hint_runtime.sh uses), not
# merely anywhere in a 2,000-line Justfile.
RUN_BLOCK="$(awk '/"\$RUNTIME" run -d/{inblock=1} inblock{print} inblock && /hive_image/{exit}' "$JUSTFILE")"
if ! grep -qF -- '-e HIVE_CONTAINER_NAME=' <<<"$RUN_BLOCK"; then
  fail "locate the contributor container-run invocation in the Justfile" \
       "the anchors moved; this test cannot verify what the recipe passes"
elif grep -qF -- '${RESOURCE_FLAGS}' <<<"$RUN_BLOCK"; then
  pass "the container-run invocation carries \${RESOURCE_FLAGS}"
else
  fail "the container-run invocation carries \${RESOURCE_FLAGS}" \
       "without it the limits are computed and then never applied"
fi

# ── 2. The shipped flag construction, evaluated ──────────────────────────────
#
# Extract the construction block from its shipped source and RUN it — a copy
# restated here would keep passing after the real one regressed.
FLAG_BLOCK="$(awk '
  /CONTAINER_MEMORY="\$\{HIVE_CONTAINER_MEMORY/ {inblock=1}
  inblock {print}
  inblock && /--cpus \$\{CONTAINER_CPUS\}/ {getline; print; exit}
' "$JUSTFILE")"

# render_flags [VAR=value ...] — evaluates the shipped block under a clean
# environment plus the given overrides, and prints the resulting flags.
render_flags() {
  env -i "$@" FLAG_BLOCK="$FLAG_BLOCK" bash -c 'eval "$FLAG_BLOCK"; echo "$RESOURCE_FLAGS"'
}

if [ -z "$FLAG_BLOCK" ]; then
  fail "extract the resource-flag construction from the Justfile" \
       "the anchor moved — this test cannot evaluate the shipped logic"
else
  pass "resource-flag construction extracted from the Justfile (not restated here)"

  # Defaults must be the k8s limit envelope, read from ITS shipped source.
  K8S_MEM="$(grep -oE 'readonly MEM_LIMIT="[^"]+"' "$JUSTFILE" | grep -oE '"[^"]+"' | tr -d '"')"
  K8S_CPU="$(grep -oE 'readonly CPU_LIMIT="[^"]+"' "$JUSTFILE" | grep -oE '"[^"]+"' | tr -d '"')"
  # 4Gi (k8s quantity) ↔ 4g (docker/podman byte suffix)
  WANT_MEM="$(tr -d 'i' <<<"$K8S_MEM" | tr 'A-Z' 'a-z')"

  GOT="$(render_flags)"
  WANT="--memory ${WANT_MEM} --memory-swap ${WANT_MEM} --cpus ${K8S_CPU}"
  if [ "$GOT" = "$WANT" ]; then
    pass "defaults equal the contribute-k8s limit envelope (${K8S_MEM}/${K8S_CPU} CPUs)"
  else
    fail "defaults equal the contribute-k8s limit envelope" "got:  $GOT
        want: $WANT"
  fi

  GOT="$(render_flags HIVE_CONTAINER_MEMORY=6g HIVE_CONTAINER_CPUS=4)"
  WANT="--memory 6g --memory-swap 6g --cpus 4"
  if [ "$GOT" = "$WANT" ]; then
    pass "HIVE_CONTAINER_MEMORY / HIVE_CONTAINER_CPUS override the defaults"
  else
    fail "HIVE_CONTAINER_MEMORY / HIVE_CONTAINER_CPUS override the defaults" "got:  $GOT
        want: $WANT"
  fi

  GOT="$(render_flags HIVE_CONTAINER_MEMORY=none HIVE_CONTAINER_CPUS=none)"
  if [ -z "${GOT// /}" ]; then
    pass "HIVE_CONTAINER_MEMORY=none / HIVE_CONTAINER_CPUS=none lift the limits"
  else
    fail "HIVE_CONTAINER_MEMORY=none / HIVE_CONTAINER_CPUS=none lift the limits" \
         "got: $GOT"
  fi
fi

# ── 3. An OOM/137 exit is explained, not just numbered ───────────────────────
#
# The block that reports the final exit code must consult .State.OOMKilled and
# point at the override — a bare "exited with code 137" is the exact failure
# mode #6485 reported.
EXIT_BLOCK="$(awk '/exited with code \$\{FINAL_EXIT\}/{inblock=1} inblock{print}' "$JUSTFILE" | head -25)"
if [ -z "$EXIT_BLOCK" ]; then
  fail "locate the final-exit report in the Justfile" \
       "the anchor moved; this test cannot verify the OOM explanation"
else
  if grep -qF '.State.OOMKilled' <<<"$EXIT_BLOCK"; then
    pass "the final-exit report consults .State.OOMKilled"
  else
    fail "the final-exit report consults .State.OOMKilled" \
         "a memory kill would again surface as a bare exit code"
  fi
  if grep -qF 'HIVE_CONTAINER_MEMORY' <<<"$EXIT_BLOCK"; then
    pass "the final-exit report names the HIVE_CONTAINER_MEMORY override"
  else
    fail "the final-exit report names the HIVE_CONTAINER_MEMORY override" \
         "the operator is told what happened but not what to do about it"
  fi
fi

echo ""
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
