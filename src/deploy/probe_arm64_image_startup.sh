#!/usr/bin/env bash
# arm64 image pull and service-startup probe (kubestellar/hive#4336).
#
# Every Podman result so far is amd64 only — #4199 and #4200 both record that
# as unproven. This probe asks the two questions #4188 asks of arm64, and only
# those two: does the published arm64 image come down, and does the service
# actually start from it.
#
# DELIBERATELY NOT THE THREE-CASE MATRIX. #4211 names a duplicated full matrix
# as the failure mode to avoid: the two architectures came back
# capability-identical, so re-running the egress-gate cases here doubles cost
# for coverage the identical-capability result says will not diverge. The gate
# is #4334's lane (rootless) and #4335's (rootful), both on amd64. This probe
# runs the container with HIVE_PROXY_ADVISORY_OK=true precisely so the gate is
# taken OUT of the picture rather than silently re-measured: without it the
# entrypoint fails closed with exit 77 and nothing about startup gets tested.
# If the two architectures ever DO diverge, that is the signal to widen this
# lane — not a reason to widen it now.
#
# WHY PULL RATHER THAN BUILD. #4188 asks for "build/pull plus startup". The
# arm64 BUILD is already a per-push CI gate: .github/workflows/docker.yml
# builds linux/arm64 on ubuntu-24.04-arm on every branch and asserts the
# embedded commit matches. What no lane covered is the other half — that the
# published arm64 artifact pulls under Podman and starts. So this probe pulls,
# and #4336's stop condition applies: if no arm64 image is published for the
# tag, record it and stop rather than building one ad hoc in CI.
#
# Cases:
#   manifest   the tag advertises a linux/arm64 image        -> required
#   pull       that image comes down and IS arm64            -> required
#   exec       /usr/local/bin/hive runs and reports a version -> required
#   startup    the container serves GET /api/health          -> required
#
# The exec case is not filler. #3760 was a binary that shipped as a
# non-executable overlayfs metacopy redirect at the image path — an image that
# pulls fine and cannot run. That class of failure is exactly what an
# architecture-specific lane is for.
#
# Usage:
#   src/deploy/probe_arm64_image_startup.sh [options]
#
#   --image REF     image to probe (default: the hive ref in standalone-images.sh)
#   --arch ARCH     architecture to require and pull (default arm64)
#   --local         the image is already in the local store; skip the manifest
#                   and pull cases and probe what is there (#5370)
#   --store DIR     reuse a probe store instead of creating one (kept on exit)
#   --shared-store  deliberately use the caller's default Podman store
#   --port PORT     host port for the published API port (default 18402)
#   --timeout SEC   how long to wait for /api/health (default 180)
#
# Exit codes: 0 the image pulled and the service started, 1 it did not,
# 78 a prerequisite is missing (EX_CONFIG).

set -uo pipefail

# The image default comes from the #4206 source of truth, so the probe
# measures the same reference the deployment assets run (#4486). IMAGE in the
# environment or --image stays the deliberate override.
# shellcheck source=src/deploy/standalone-images.sh disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/standalone-images.sh"

IMAGE="${IMAGE:-$HIVE_STANDALONE_IMAGE_HIVE}"
ARCH="arm64"
STORE=""
SHARED_STORE="false"
# --local (#5370): probe an image that already exists in the local store rather
# than one to be fetched from a registry. Needed because a PR's image is built
# on the runner and never published — docker.yml skips its build job on
# pull_request and only pushes from long-lived branches, so a PR head SHA has
# no published image to pull. See podman-arm64-lane.yml.
LOCAL_IMAGE="false"
HOST_PORT="18402"
HEALTH_TIMEOUT="180"
OWN_STORE="false"

PROBE_NAME="hive-arm64-probe-$$"
API_PORT="3002"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --image) IMAGE="${2:?--image needs a value}"; shift 2 ;;
    --arch) ARCH="${2:?--arch needs a value}"; shift 2 ;;
    --local) LOCAL_IMAGE="true"; shift ;;
    --store) STORE="${2:?--store needs a value}"; shift 2 ;;
    --shared-store) SHARED_STORE="true"; shift ;;
    --port) HOST_PORT="${2:?--port needs a value}"; shift 2 ;;
    --timeout) HEALTH_TIMEOUT="${2:?--timeout needs a value}"; shift 2 ;;
    -h|--help)  sed -n '2,50p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) printf 'ERROR: unknown argument %q\n' "$1" >&2; exit 78 ;;
  esac
done

fail_prereq() {
  printf 'PREREQUISITE MISSING: %s\n' "$1" >&2
  exit 78
}

command -v podman >/dev/null 2>&1 || fail_prereq "podman is not installed or not on PATH"
command -v curl >/dev/null 2>&1 || fail_prereq "curl is not installed or not on PATH"

# This is an arm64 lane. Running it on a foreign architecture would either fail
# confusingly or quietly measure emulation instead of the thing #4188 asked
# for, so it refuses rather than pretending.
host_arch="$(uname -m)"
case "$host_arch" in
  aarch64|arm64) host_arch="arm64" ;;
  x86_64|amd64)  host_arch="amd64" ;;
esac
[[ "$host_arch" == "$ARCH" ]] || \
  fail_prereq "this probe must run ON ${ARCH} (host is ${host_arch}); it measures the native ${ARCH} path, not emulation"

# A locally-built image lives in the caller's default store, so a throwaway
# store would not contain it and every case below would fail for a reason that
# has nothing to do with the image. Couple the two rather than letting the
# combination fail confusingly ten lines later.
if [[ "$LOCAL_IMAGE" == "true" ]]; then
  SHARED_STORE="true"
  if [[ -n "$STORE" ]]; then
    fail_prereq "--local uses the caller's store and cannot be combined with --store"
  fi
fi

WORK="$(mktemp -d)"
# shellcheck disable=SC2329 # invoked through the EXIT trap below
cleanup() {
  pod rm -f "$PROBE_NAME" >/dev/null 2>&1
  rm -rf "$WORK"
  # Files a container wrote are owned by mapped subordinate UIDs, so a plain
  # rm cannot remove a store this script created. Delete it from inside the
  # user namespace, and fall back for the pre-container case.
  if [[ "$OWN_STORE" == "true" && -n "$STORE" ]]; then
    podman unshare rm -rf "$STORE" 2>/dev/null || rm -rf "$STORE" 2>/dev/null
  fi
  return 0
}
trap cleanup EXIT

# Isolated storage for anything that pulls, per the runner map. Every podman
# call goes through pod(); nothing addresses the caller's default store unless
# --shared-store says so explicitly.
POD=(podman)
if [[ "$SHARED_STORE" != "true" ]]; then
  if [[ -z "$STORE" ]]; then
    STORE="$(mktemp -d -t hive-arm64-probe-store-XXXXXX)"
    OWN_STORE="true"
  fi
  mkdir -p "${STORE}/graph" "${STORE}/run"
  POD=(podman --root "${STORE}/graph" --runroot "${STORE}/run")
fi
pod() { "${POD[@]}" "$@"; }

failures=0
note_fail() { printf '  RESULT: FAIL — %s\n' "$1"; failures=$((failures + 1)); }
note_ok()   { printf '  RESULT: ok — %s\n' "$1"; }

# ── Environment, printed for comparison against the amd64 lanes ─────────────
# #4211 measured the two architectures capability-identical. These are the
# fields that claim would be falsified by, so they are reported on every run
# rather than assumed to still hold.
printf '== Hive %s image pull and startup probe (#4336) ==\n' "$ARCH"
pod info --format \
  'podman={{.Version.Version}} arch={{.Host.Arch}} rootless={{.Host.Security.Rootless}} cgroups={{.Host.CgroupsVersion}} runtime={{.Host.OCIRuntime.Name}} netbackend={{.Host.NetworkBackend}} rootlessnet={{.Host.RootlessNetworkCmd}}'
printf 'kernel=%s host_arch=%s\n' "$(uname -r)" "$host_arch"
printf 'store=%s (throwaway=%s)\nimage=%s\n\n' "${STORE:-<caller default>}" "$OWN_STORE" "$IMAGE"

# ── Case 1: the tag must advertise a linux/<arch> image ────────────────────
# A missing arm64 manifest FAILS. It must not be a skip: an arm64 lane that
# silently steps aside when the image is not there is the same vacuous pass
# #4211 warns about, and the whole point of this lane is that arm64 stopped
# being unproven.
if [[ "$LOCAL_IMAGE" == "true" ]]; then
  # #5370: a locally-built image has no registry manifest and nothing to pull,
  # so cases 1 and 2 have no meaning here. They are SKIPPED, not faked green —
  # and the two cases that carry this lane's real signal (the binary executes,
  # the service starts) run exactly as they do on the published path. Those are
  # the ones a change to entrypoint.sh or the Dockerfile can break, and they
  # are the ones probing the published image could never test on a PR.
  #
  # The publisher-facing guarantee is unchanged: on a push the image is pulled
  # and a missing arm64 manifest still fails, per #4336.
  printf -- '--- cases: manifest + pull (SKIPPED: --local) ---\n'
  printf '  %s is a local build, not a registry reference.\n' "$IMAGE"
  printf '  A PR head SHA has no published image (docker.yml skips its build job\n'
  printf '  on pull_request and pushes only from long-lived branches), so there is\n'
  printf '  nothing to pull. Verifying the image exists locally instead.\n'
  if ! pod image exists "$IMAGE"; then
    printf '  RECORDED: %s is not in the local store.\n' "$IMAGE"
    note_fail "local image ${IMAGE} does not exist (was the build step skipped or did it fail?)"
    printf '\nSUMMARY: %d failure(s)\n' "$failures"
    exit 1
  fi
  local_arch="$(pod image inspect "$IMAGE" --format '{{.Architecture}}' 2>/dev/null)"
  printf '  architecture: %s\n' "${local_arch:-?}"
  if [[ "$local_arch" != "$ARCH" ]]; then
    note_fail "local image reports architecture=${local_arch:-unknown}, expected ${ARCH}"
    printf '\nSUMMARY: %d failure(s)\n' "$failures"
    exit 1
  fi
  note_ok "the local image is ${ARCH} and is present"
else

printf -- '--- case: manifest advertises linux/%s ---\n' "$ARCH"
manifest="${WORK}/manifest.json"

# A multi-arch tag is an index and lists one entry per platform. A reference
# that resolves to a single image manifest has no platform entries at all,
# which is a different finding from "the index omits this architecture" and is
# reported as such rather than as a blank list.
advertised() {
  local found
  found="$(grep -o '"architecture": *"[^"]*"' "$manifest" \
    | sed 's/.*"\([^"]*\)"$/\1/' | sort -u | tr '\n' ' ')"
  if [[ -n "${found// /}" ]]; then
    printf '%s' "$found"
  else
    printf '(none — this reference is a single image manifest, not a multi-arch index)'
  fi
}

if ! pod manifest inspect "$IMAGE" >"$manifest" 2>"${WORK}/manifest.err"; then
  printf '  podman manifest inspect failed:\n'
  sed 's/^/    /' "${WORK}/manifest.err"
  note_fail "cannot read the manifest for ${IMAGE}"
elif grep -q "\"architecture\": *\"${ARCH}\"" "$manifest"; then
  printf '  architectures advertised: %s\n' "$(advertised)"
  note_ok "the tag publishes a linux/${ARCH} image"
else
  printf '  architectures advertised: %s\n' "$(advertised)"
  printf '  RECORDED: no linux/%s image is published for %s.\n' "$ARCH" "$IMAGE"
  printf '            This lane does NOT build one ad hoc — see #4336. Fix the\n'
  printf '            publisher (.github/workflows/docker.yml builds linux/%s)\n' "$ARCH"
  printf '            rather than making this lane paper over it.\n'
  note_fail "no linux/${ARCH} image published for ${IMAGE}"
fi

# Nothing below can mean anything without the image, so stop here rather than
# emitting a cascade of failures that all have one cause.
if [[ "$failures" -ne 0 ]]; then
  printf '\nSUMMARY: %d failure(s)\n' "$failures"
  exit 1
fi

# ── Case 2: the image pulls, and what came down really is <arch> ───────────
printf -- '\n--- case: pull linux/%s ---\n' "$ARCH"
if pod pull -q --arch "$ARCH" "$IMAGE" >/dev/null 2>"${WORK}/pull.err"; then
  pulled_arch="$(pod image inspect "$IMAGE" --format '{{.Architecture}}' 2>/dev/null)"
  pulled_digest="$(pod image inspect "$IMAGE" --format '{{.Digest}}' 2>/dev/null)"
  printf '  digest: %s\n  architecture: %s\n' "${pulled_digest:-?}" "${pulled_arch:-?}"
  if [[ "$pulled_arch" == "$ARCH" ]]; then
    note_ok "pulled image is ${ARCH}"
  else
    note_fail "pulled image reports architecture=${pulled_arch:-unknown}, expected ${ARCH}"
  fi
else
  sed 's/^/    /' "${WORK}/pull.err"
  note_fail "could not pull ${IMAGE} for ${ARCH}"
fi

fi  # end of the registry-vs-local branch opened before case 1 (#5370)

# ── Case 3: the shipped binary actually executes ───────────────────────────
# #3760: an image can pull cleanly and still carry a /usr/local/bin/hive that
# the runtime presents as non-executable. Cheap to check, and precisely the
# kind of thing that can differ per architecture.
printf -- '\n--- case: the shipped binary executes ---\n'
if version_out="$(pod run --rm --entrypoint /usr/local/bin/hive "$IMAGE" --version 2>&1)"; then
  printf '  hive --version: %s\n' "$version_out"
  note_ok "/usr/local/bin/hive runs on ${ARCH}"
else
  printf '  output: %s\n' "$version_out"
  note_fail "/usr/local/bin/hive did not run (the #3760 shape)"
fi

# ── Case 4: the service starts and answers its readiness probe ─────────────
# /api/health is what src/deploy/k8s/deployment.yaml uses as the readiness
# probe, so "started" here means the same thing it means in production rather
# than a log line that only proves a process was spawned.
#
# HIVE_PROXY_ADVISORY_OK=true is deliberate and is NOT a matrix case: it takes
# the forced-egress gate out of the picture so what is measured is startup. An
# agent IS configured, so the startup path exercised is the real one (uid map,
# agent directories) rather than the shortcut a config with no agents takes.
printf -- '\n--- case: the service starts (GET /api/health) ---\n'
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

pod rm -f "$PROBE_NAME" >/dev/null 2>&1
if ! pod run -d --name "$PROBE_NAME" \
      -e HIVE_PROXY_ADVISORY_OK=true \
      -v "${WORK}/hive.yaml:/etc/hive/hive.yaml:ro,Z" \
      -p "127.0.0.1:${HOST_PORT}:${API_PORT}" \
      "$IMAGE" >/dev/null 2>"${WORK}/run.err"; then
  sed 's/^/    /' "${WORK}/run.err"
  note_fail "the container did not start"
else
  health=""
  deadline=$(( SECONDS + HEALTH_TIMEOUT ))
  while (( SECONDS < deadline )); do
    health="$(curl -sS -m 5 -o /dev/null -w '%{http_code}' \
      "http://127.0.0.1:${HOST_PORT}/api/health" 2>/dev/null)"
    [[ "$health" == "200" ]] && break
    # A container that has already exited will never become healthy.
    if [[ "$(pod inspect -f '{{.State.Running}}' "$PROBE_NAME" 2>/dev/null)" != "true" ]]; then
      break
    fi
    sleep 3
  done

  printf '  entrypoint milestones:\n'
  pod logs "$PROBE_NAME" 2>&1 | grep -E '^\[entrypoint\] (Starting|WARN: proxy egress)' | sed 's/^/    /'

  if [[ "$health" == "200" ]]; then
    printf '  GET /api/health -> %s\n' "$health"
    note_ok "the service started and answered its readiness probe on ${ARCH}"
  else
    printf '  GET /api/health -> %s (after %ss)\n' "${health:-no response}" "$HEALTH_TIMEOUT"
    printf '  container state: %s (exit %s)\n' \
      "$(pod inspect -f '{{.State.Status}}' "$PROBE_NAME" 2>/dev/null)" \
      "$(pod inspect -f '{{.State.ExitCode}}' "$PROBE_NAME" 2>/dev/null)"
    printf '  last 40 log lines:\n'
    pod logs --tail 40 "$PROBE_NAME" 2>&1 | sed 's/^/    /'
    note_fail "the service did not answer /api/health within ${HEALTH_TIMEOUT}s"
  fi
fi

printf '\nSUMMARY: %d failure(s)\n' "$failures"
[[ "$failures" -eq 0 ]] || exit 1
exit 0
