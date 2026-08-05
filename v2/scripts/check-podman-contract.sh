#!/usr/bin/env bash
# check-podman-contract.sh — static contract check for rootless-Podman
# handling in the Justfile's contribute-hive recipe.
#
# Part of #2535 Option C (CI seam for the rootless-Podman path). This is a
# grep-based static check, not a live rootless-Podman run — see
# v2/docs/podman-rootless-ci.md for what this covers and what a future live
# test would add. It does NOT touch the runtime auto-detect default (that's
# Option A, explicitly deferred).
#
# Usage: v2/scripts/check-podman-contract.sh [path-to-Justfile]
set -euo pipefail

JUSTFILE="${1:-Justfile}"

if [[ ! -f "$JUSTFILE" ]]; then
  echo "ERROR: Justfile not found at ${JUSTFILE}" >&2
  exit 1
fi

fail=0

check() {
  local desc="$1" pattern="$2"
  if grep -qE "$pattern" "$JUSTFILE"; then
    echo "  ok: ${desc}"
  else
    echo "  FAIL: ${desc} (expected to find pattern: ${pattern})"
    fail=1
  fi
}

check_absent() {
  local desc="$1" pattern="$2"
  if grep -qE "$pattern" "$JUSTFILE"; then
    echo "  FAIL: ${desc} (found forbidden pattern: ${pattern})"
    fail=1
  else
    echo "  ok: ${desc}"
  fi
}

echo "== Podman rootless contract check (${JUSTFILE}) =="

# 1. Rootless UID mapping must still be wired for the podman branch.
check "rootless UID mapping (--userns=keep-id) present" \
  '\-\-userns=keep-id'

# 2. SELinux-friendly volume labels must still be applied for podman mounts.
check "SELinux volume label (:Z) present" \
  ':Z'

# 3. The macOS carve-out must still special-case Podman + Darwin together.
check "macOS network carve-out still references darwin" \
  'OSTYPE.*darwin'

# 4. No engine socket should ever be mounted into the contributor container —
# mirrors the static contract test projectbluefin/review uses for the same
# invariant (cited in #2535).
check_absent "no docker.sock mount" \
  '/var/run/docker\.sock'
check_absent "no podman.sock mount" \
  '/run/podman/podman\.sock'

# 5. The auto-detect default must remain docker-then-podman — this check
# exists so a future PR can't silently flip Option A in here. If you are
# HERE to intentionally implement Option A, update this check deliberately
# alongside v2/docs/podman-rootless-ci.md, don't just delete it.
check "auto-detect still tries docker before podman" \
  'command -v docker.*RUNTIME=docker'

if [[ "$fail" -ne 0 ]]; then
  echo ""
  echo "Rootless Podman contract check FAILED — see v2/docs/podman-rootless-ci.md"
  exit 1
fi

echo ""
echo "Rootless Podman contract check passed."
