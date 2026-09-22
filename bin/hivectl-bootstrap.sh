#!/usr/bin/env bash
# Resolve hivectl for contributor commands and bootstrap it from the Hive image
# when a fresh checkout does not already have a compatible binary.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DIR="${ROOT}/bin"
HIVECTL_LOCAL="${BIN_DIR}/hivectl"
SOURCE_FILE="${BIN_DIR}/.hivectl.source"
HIVECTL_BOOTSTRAP_IMAGE_DEFAULT="ghcr.io/hivecommons/hive:stable"
HIVECTL_BOOTSTRAP_IMAGE="${HIVECTL_BOOTSTRAP_IMAGE:-${HIVECTL_IMAGE:-${HIVECTL_BOOTSTRAP_IMAGE_DEFAULT}}}"
HIVECTL_IMAGE_PATH_DEFAULT="/usr/local/share/hive/hivectl"
HIVECTL_IMAGE_PATH="${HIVECTL_IMAGE_PATH:-${HIVECTL_IMAGE_PATH_DEFAULT}}"
HIVECTL_EXTRACT_CONTAINER="hive-hivectl-bootstrap-$$"
HIVECTL_STAGING="${BIN_DIR}/.hivectl-bootstrap.$$"

# The #4210 ownership label set, from its single source of truth, so the
# throwaway extraction container stays visible to the Podman teardown contract.
# shellcheck source=bin/hive-podman-cleanup.sh disable=SC1091
. "${ROOT}/bin/hive-podman-cleanup.sh"

cleanup() {
  rm -rf "$HIVECTL_STAGING"
  if [[ -n "${HIVECTL_CREATED_CONTAINER:-}" ]] && command -v podman >/dev/null 2>&1; then
    podman rm "$HIVECTL_CREATED_CONTAINER" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

note() {
  printf '%s\n' "$*" >&2
}

refuse() {
  note "ERROR: $*"
  exit 1
}

require_podman() {
  if ! command -v podman >/dev/null 2>&1; then
    refuse "hivectl is not available and podman is required to bootstrap it from ${HIVECTL_BOOTSTRAP_IMAGE}. Install podman, pull the image, or set HIVECTL=/path/to/hivectl to use an explicit override."
  fi
}

current_image_digest() {
  local digest
  digest="$(podman image inspect --format '{{.Digest}}' "$HIVECTL_BOOTSTRAP_IMAGE" 2>/dev/null || true)"
  if [[ -z "$digest" ]]; then
    note "hivectl bootstrap: pulling ${HIVECTL_BOOTSTRAP_IMAGE} to find the matching hivectl"
    podman pull "$HIVECTL_BOOTSTRAP_IMAGE" >/dev/null || return 1
    digest="$(podman image inspect --format '{{.Digest}}' "$HIVECTL_BOOTSTRAP_IMAGE" 2>/dev/null || true)"
  fi
  [[ -n "$digest" ]] || return 1
  printf '%s\n' "$digest"
}

recorded_digest() {
  [[ -f "$SOURCE_FILE" ]] || return 1
  sed -n 's/^digest=//p' "$SOURCE_FILE" | head -n1
}

write_source_record() {
  local digest="$1" version_output
  version_output="$({ "$HIVECTL_LOCAL" version || true; } 2>&1)"
  {
    printf 'image=%s\n' "$HIVECTL_BOOTSTRAP_IMAGE"
    printf 'path=%s\n' "$HIVECTL_IMAGE_PATH"
    printf 'digest=%s\n' "$digest"
    printf 'version<<HIVECTL_VERSION\n'
    printf '%s\n' "$version_output"
    printf 'HIVECTL_VERSION\n'
  } >"$SOURCE_FILE"
}

copy_hivectl_from_image() {
  local destination="$1" label_arg
  local -a labels=()
  while IFS= read -r label_arg; do
    labels+=("$label_arg")
  done < <(hive_podman_labels hivectl-bootstrap) || return
  mkdir -p "$HIVECTL_STAGING" "$BIN_DIR"
  # Keep this intentionally parallel to install_hivectl() in
  # bin/hive-podman-setup.sh: podman create makes the image filesystem
  # addressable without running it, and podman cp lifts the host-side client
  # binary from the pinned cargo path.
  if ! podman create --name "$HIVECTL_EXTRACT_CONTAINER" "${labels[@]}" "$HIVECTL_BOOTSTRAP_IMAGE" >/dev/null; then
    return 1
  fi
  HIVECTL_CREATED_CONTAINER="$HIVECTL_EXTRACT_CONTAINER"
  if ! podman cp "${HIVECTL_EXTRACT_CONTAINER}:${HIVECTL_IMAGE_PATH}" "$destination"; then
    return 1
  fi
  if ! podman rm "$HIVECTL_EXTRACT_CONTAINER" >/dev/null; then
    return 1
  fi
  HIVECTL_CREATED_CONTAINER=""
}

install_staged_hivectl() {
  local staged="$1" digest="$2"
  if ! install -m 0755 "$staged" "$HIVECTL_LOCAL"; then
    return 1
  fi
  write_source_record "$digest"
}

extract_hivectl() {
  local digest="$1" staged="${HIVECTL_STAGING}/hivectl"
  copy_hivectl_from_image "$staged" || return 1
  install_staged_hivectl "$staged" "$digest" || return 1
  note "hivectl bootstrap: staged ${HIVECTL_LOCAL} from ${HIVECTL_BOOTSTRAP_IMAGE} (${digest})"
}

verified_installed_candidate() {
  local candidate="$1" digest staged
  require_podman
  digest="$(current_image_digest)" || refuse "could not inspect or pull ${HIVECTL_BOOTSTRAP_IMAGE}; refusing to run unverified ${candidate}"
  staged="${HIVECTL_STAGING}/candidate-hivectl"
  if ! copy_hivectl_from_image "$staged"; then
    refuse "could not extract ${HIVECTL_IMAGE_PATH} from ${HIVECTL_BOOTSTRAP_IMAGE}; refusing to run unverified ${candidate}"
  fi
  if cmp -s "$staged" "$candidate"; then
    printf '%s
' "$candidate"
    return 0
  fi
  note "hivectl bootstrap: ${candidate} does not match ${HIVECTL_BOOTSTRAP_IMAGE}; staging a checkout-local copy instead"
  install_staged_hivectl "$staged" "$digest" || refuse "could not stage ${HIVECTL_LOCAL}; refusing to run stale ${candidate}"
  printf '%s
' "$HIVECTL_LOCAL"
}

refresh_local_hivectl() {
  local digest old_digest
  require_podman
  digest="$(current_image_digest)" || refuse "could not inspect or pull ${HIVECTL_BOOTSTRAP_IMAGE}; refusing to run an unverified or stale ${HIVECTL_LOCAL}"
  old_digest="$(recorded_digest || true)"
  if [[ -x "$HIVECTL_LOCAL" && -n "$old_digest" && "$old_digest" == "$digest" ]]; then
    printf '%s\n' "$HIVECTL_LOCAL"
    return 0
  fi
  if [[ -x "$HIVECTL_LOCAL" && -z "$old_digest" ]]; then
    note "hivectl bootstrap: ${HIVECTL_LOCAL} has no source digest; refreshing from ${HIVECTL_BOOTSTRAP_IMAGE}"
  elif [[ -x "$HIVECTL_LOCAL" ]]; then
    note "hivectl bootstrap: ${HIVECTL_LOCAL} was extracted from ${old_digest}; refreshing for ${digest}"
  fi
  extract_hivectl "$digest" || refuse "could not extract ${HIVECTL_IMAGE_PATH} from ${HIVECTL_BOOTSTRAP_IMAGE}; refusing to run a stale or missing hivectl"
  printf '%s\n' "$HIVECTL_LOCAL"
}

resolve_hivectl() {
  local override="${HIVECTL:-}" candidate
  if [[ -n "$override" ]]; then
    if [[ -x "$override" ]]; then
      note "hivectl bootstrap: using HIVECTL=${override}; explicit override bypasses image freshness checks"
      printf '%s\n' "$override"
      return 0
    fi
    refuse "HIVECTL=${override} is not executable"
  fi

  if [[ -e "$HIVECTL_LOCAL" || -e "$SOURCE_FILE" ]]; then
    refresh_local_hivectl
    return 0
  fi

  for candidate in "${HOME}/.local/bin/hivectl" /usr/local/bin/hivectl; do
    if [[ -x "$candidate" ]]; then
      verified_installed_candidate "$candidate"
      return 0
    fi
  done

  if command -v hivectl >/dev/null 2>&1; then
    verified_installed_candidate "$(command -v hivectl)"
    return 0
  fi

  refresh_local_hivectl
}

main() {
  if [[ $# -eq 0 ]]; then
    refuse "usage: $0 <hivectl-args...>"
  fi
  local hivectl_bin
  hivectl_bin="$(resolve_hivectl)"
  exec "$hivectl_bin" "$@"
}

main "$@"
