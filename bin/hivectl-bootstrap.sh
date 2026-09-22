#!/usr/bin/env bash
# Resolve hivectl for contributor commands and bootstrap it from the Hive image
# when a fresh checkout does not already have a compatible binary.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DIR="${ROOT}/bin"
HIVECTL_LOCAL="${BIN_DIR}/hivectl"
SOURCE_FILE="${BIN_DIR}/.hivectl.source"
HIVECTL_BOOTSTRAP_REPOSITORY="ghcr.io/hivecommons/hive"
HIVECTL_BOOTSTRAP_CHANNEL_V4="stable"
HIVECTL_BOOTSTRAP_CHANNEL_V5="stable"
HIVECTL_BOOTSTRAP_CHANNEL_V6="edge"
HIVECTL_BOOTSTRAP_CHANNEL_DEFAULT="${HIVECTL_BOOTSTRAP_CHANNEL_V5}"
HIVECTL_BOOTSTRAP_IMAGE_DEFAULT="${HIVECTL_BOOTSTRAP_REPOSITORY}:${HIVECTL_BOOTSTRAP_CHANNEL_DEFAULT}"
HIVECTL_BOOTSTRAP_IMAGE_OVERRIDE="${HIVECTL_BOOTSTRAP_IMAGE:-${HIVECTL_IMAGE:-}}"
HIVECTL_BOOTSTRAP_IMAGE="${HIVECTL_BOOTSTRAP_IMAGE_OVERRIDE:-}"
HIVECTL_IMAGE_PATH_DEFAULT="/usr/local/share/hive/hivectl"
HIVECTL_IMAGE_PATH="${HIVECTL_IMAGE_PATH:-${HIVECTL_IMAGE_PATH_DEFAULT}}"
HIVECTL_EXTRACT_CONTAINER="hive-hivectl-bootstrap-$$"
HIVECTL_STAGING="${BIN_DIR}/.hivectl-bootstrap.$$"
HIVECTL_SKEW_EXIT=66
HIVECTL_SOURCE_DIR="${ROOT}/src/pkg/hivectl"

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
  ensure_bootstrap_image
  if ! command -v podman >/dev/null 2>&1; then
    refuse "hivectl is not available and podman is required to bootstrap it from ${HIVECTL_BOOTSTRAP_IMAGE}. Install podman, pull the image, or set HIVECTL=/path/to/hivectl to use an explicit override."
  fi
}

checkout_release_tag() {
  local tag
  tag="$(git -C "$ROOT" describe --tags --abbrev=0 2>/dev/null || true)"
  [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
  printf '%s\n' "$tag"
}

image_probe_succeeds() {
  local image="$1"
  command -v podman >/dev/null 2>&1 || return 1
  podman image inspect --format '{{.Digest}}' "$image" >/dev/null 2>&1 && return 0
  podman pull "$image" >/dev/null 2>&1
}

checkout_branch_channel() {
  local branch channel
  branch="$(git -C "$ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
  case "$branch" in
    v4) channel="$HIVECTL_BOOTSTRAP_CHANNEL_V4" ;;
    v5) channel="$HIVECTL_BOOTSTRAP_CHANNEL_V5" ;;
    v6) channel="$HIVECTL_BOOTSTRAP_CHANNEL_V6" ;;
    *) channel="$HIVECTL_BOOTSTRAP_CHANNEL_DEFAULT" ;;
  esac
  printf '%s\n' "$channel"
}

resolve_bootstrap_image() {
  local tag image channel
  if [[ -n "$HIVECTL_BOOTSTRAP_IMAGE_OVERRIDE" ]]; then
    printf '%s\n' "$HIVECTL_BOOTSTRAP_IMAGE_OVERRIDE"
    return 0
  fi

  if tag="$(checkout_release_tag)"; then
    image="${HIVECTL_BOOTSTRAP_REPOSITORY}:${tag}"
    if image_probe_succeeds "$image"; then
      note "hivectl bootstrap: using release image ${image} for checkout tag ${tag}"
      printf '%s\n' "$image"
      return 0
    fi
    note "hivectl bootstrap: release image ${image} is not locally available or reachable; falling back to the branch channel"
  fi

  channel="$(checkout_branch_channel)"
  if [[ "$channel" == "$HIVECTL_BOOTSTRAP_CHANNEL_DEFAULT" ]]; then
    printf '%s\n' "$HIVECTL_BOOTSTRAP_IMAGE_DEFAULT"
    return 0
  fi
  printf '%s:%s\n' "$HIVECTL_BOOTSTRAP_REPOSITORY" "$channel"
}

ensure_bootstrap_image() {
  if [[ -z "$HIVECTL_BOOTSTRAP_IMAGE" ]]; then
    HIVECTL_BOOTSTRAP_IMAGE="$(resolve_bootstrap_image)"
  fi
}

current_image_digest() {
  local digest
  ensure_bootstrap_image
  digest="$(podman image inspect --format '{{.Digest}}' "$HIVECTL_BOOTSTRAP_IMAGE" 2>/dev/null || true)"
  if [[ -z "$digest" ]]; then
    note "hivectl bootstrap: pulling ${HIVECTL_BOOTSTRAP_IMAGE} to find the matching hivectl"
    podman pull "$HIVECTL_BOOTSTRAP_IMAGE" >/dev/null || return 1
    digest="$(podman image inspect --format '{{.Digest}}' "$HIVECTL_BOOTSTRAP_IMAGE" 2>/dev/null || true)"
  fi
  [[ -n "$digest" ]] || return 1
  printf '%s\n' "$digest"
}

current_image_revision() {
  local revision
  ensure_bootstrap_image
  revision="$(podman image inspect --format '{{ index .Labels "org.opencontainers.image.revision" }}' "$HIVECTL_BOOTSTRAP_IMAGE" 2>/dev/null || true)"
  [[ -n "$revision" && "$revision" != "<no value>" ]] || return 1
  printf '%s\n' "$revision"
}

recorded_digest() {
  [[ -f "$SOURCE_FILE" ]] || return 1
  sed -n 's/^digest=//p' "$SOURCE_FILE" | head -n1
}

write_source_record() {
  local digest="$1" version_output image_revision
  version_output="$({ "$HIVECTL_LOCAL" version || true; } 2>&1)"
  image_revision="$(current_image_revision || true)"
  {
    printf 'image=%s\n' "$HIVECTL_BOOTSTRAP_IMAGE"
    printf 'path=%s\n' "$HIVECTL_IMAGE_PATH"
    printf 'digest=%s\n' "$digest"
    [[ -z "$image_revision" ]] || printf 'image_revision=%s\n' "$image_revision"
    printf 'version<<HIVECTL_VERSION\n'
    printf '%s\n' "$version_output"
    printf 'HIVECTL_VERSION\n'
  } >"$SOURCE_FILE"
}

source_record_version_output() {
  [[ -f "$SOURCE_FILE" ]] || return 1
  sed -n '/^version<<HIVECTL_VERSION$/,/^HIVECTL_VERSION$/p' "$SOURCE_FILE" |
    sed '1d;$d'
}

source_record_image_revision() {
  [[ -f "$SOURCE_FILE" ]] || return 1
  sed -n 's/^image_revision=//p' "$SOURCE_FILE" | head -n1
}

hivectl_version_output() {
  local candidate="$1"
  if [[ "$candidate" == "$HIVECTL_LOCAL" ]] && [[ -f "$SOURCE_FILE" ]]; then
    source_record_version_output
    return 0
  fi
  { "$candidate" version || true; } 2>&1
}

extract_version_commit() {
  local version_output="$1"
  printf '%s\n' "$version_output" |
    sed -nE 's/.*(commit|revision|rev|git)[^0-9a-fA-F]*([0-9a-fA-F]{7,40}).*/\2/p' |
    head -n1
}

checkout_version_label() {
  git -C "$ROOT" describe --tags --dirty --always 2>/dev/null ||
    git -C "$ROOT" rev-parse --short HEAD 2>/dev/null ||
    printf 'unknown'
}

checkout_revision_label() {
  git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || printf 'unknown'
}

refuse_skewed_hivectl() {
  local version_output="$1" binary_commit="$2"
  local checkout_version checkout_revision image_clause
  checkout_version="$(checkout_version_label)"
  checkout_revision="$(checkout_revision_label)"
  if [[ -n "$HIVECTL_BOOTSTRAP_IMAGE_OVERRIDE" ]]; then
    image_clause="you overrode the image with ${HIVECTL_BOOTSTRAP_IMAGE}"
  else
    image_clause="image ${HIVECTL_BOOTSTRAP_IMAGE}"
  fi
  note "ERROR: refusing to run a skewed hivectl: checkout ${checkout_version} (${checkout_revision}) has hivectl source changes after staged binary commit ${binary_commit}; ${image_clause} produced:"
  note "$version_output"
  note "Set HIVECTL_BOOTSTRAP_IMAGE=ghcr.io/hivecommons/hive:<matching-tag-or-channel> to choose a matching image, or HIVECTL=/path/to/hivectl to use an explicit binary override."
  exit "$HIVECTL_SKEW_EXIT"
}

verify_hivectl_checkout_skew() {
  local candidate="$1" version_output binary_commit
  [[ -d "$HIVECTL_SOURCE_DIR" ]] || return 0
  git -C "$ROOT" rev-parse --verify HEAD >/dev/null 2>&1 || return 0

  version_output="$(hivectl_version_output "$candidate")"
  binary_commit="$(extract_version_commit "$version_output" || true)"
  if [[ -z "$binary_commit" && "$candidate" == "$HIVECTL_LOCAL" ]]; then
    binary_commit="$(source_record_image_revision || true)"
    [[ -n "$binary_commit" ]] || binary_commit="$(current_image_revision || true)"
  fi
  if [[ -z "$binary_commit" && "$candidate" != "$HIVECTL_LOCAL" ]]; then
    binary_commit="$(current_image_revision || true)"
  fi
  [[ -n "$binary_commit" ]] || return 0

  if git -C "$ROOT" merge-base --is-ancestor "$binary_commit" HEAD 2>/dev/null &&
    [[ -z "$(git -C "$ROOT" log --oneline "${binary_commit}..HEAD" -- src/pkg/hivectl)" ]]; then
    return 0
  fi

  refuse_skewed_hivectl "$version_output" "$binary_commit"
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
    verify_hivectl_checkout_skew "$candidate"
    printf '%s\n' "$candidate"
    return 0
  fi
  note "hivectl bootstrap: ${candidate} does not match ${HIVECTL_BOOTSTRAP_IMAGE}; staging a checkout-local copy instead"
  install_staged_hivectl "$staged" "$digest" || refuse "could not stage ${HIVECTL_LOCAL}; refusing to run stale ${candidate}"
  verify_hivectl_checkout_skew "$HIVECTL_LOCAL"
  printf '%s\n' "$HIVECTL_LOCAL"
}

refresh_local_hivectl() {
  local digest old_digest
  require_podman
  digest="$(current_image_digest)" || refuse "could not inspect or pull ${HIVECTL_BOOTSTRAP_IMAGE}; refusing to run an unverified or stale ${HIVECTL_LOCAL}"
  old_digest="$(recorded_digest || true)"
  if [[ -x "$HIVECTL_LOCAL" && -n "$old_digest" && "$old_digest" == "$digest" ]]; then
    verify_hivectl_checkout_skew "$HIVECTL_LOCAL"
    printf '%s\n' "$HIVECTL_LOCAL"
    return 0
  fi
  if [[ -x "$HIVECTL_LOCAL" && -z "$old_digest" ]]; then
    note "hivectl bootstrap: ${HIVECTL_LOCAL} has no source digest; refreshing from ${HIVECTL_BOOTSTRAP_IMAGE}"
  elif [[ -x "$HIVECTL_LOCAL" ]]; then
    note "hivectl bootstrap: ${HIVECTL_LOCAL} was extracted from ${old_digest}; refreshing for ${digest}"
  fi
  extract_hivectl "$digest" || refuse "could not extract ${HIVECTL_IMAGE_PATH} from ${HIVECTL_BOOTSTRAP_IMAGE}; refusing to run a stale or missing hivectl"
  verify_hivectl_checkout_skew "$HIVECTL_LOCAL"
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

  ensure_bootstrap_image

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
