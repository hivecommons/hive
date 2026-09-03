#!/usr/bin/env bash
# The one image-reference source of truth for standalone Hive deployments
# (#4206).
#
# Every standalone deployment asset -- the Docker Compose stack today, the
# Podman Quadlet units later -- takes its image references from here. Two
# assets that each hard-code their own reference drift silently: one gets
# bumped, the other keeps pulling last quarter's image, and nothing fails until
# an operator notices the two runtimes are running different code.
#
# References here are FULLY QUALIFIED, because `podman auto-update` and the
# Quadlet generator both require a registry-qualified name, and a short name
# resolves through whatever `unqualified-search-registries` happens to say on
# the host. Docker may spell the same image without the `docker.io/` prefix;
# hive_standalone_image_normalize exists so the two spellings compare equal.
#
# Digest pinning stays available and is used where an asset pins one: the
# `tag@sha256:...` form is carried through unchanged.
#
# src/deploy/test_standalone_image_refs.sh enforces that every asset agrees
# with this file, and runs in CI.

# The Hive image itself. `stable` is the operator-blessed release channel; see
# src/docs/operator-reference.md#image-provenance-and-tags and
# src/docs/release-channels.md. Do NOT use `v2-latest`: it was the rolling tag
# of the retired v2 branch. src/deploy/k8s/backup-cronjob.yaml already moved
# off it for the same reason (#3709).
HIVE_STANDALONE_IMAGE_HIVE="ghcr.io/hivecommons/hive:stable"

# The authenticating gateway. Digest-pinned against floating-tag supply-chain
# risk; refresh the tag and the digest together, as src/docker-compose.yaml
# documents.
HIVE_STANDALONE_IMAGE_GATEWAY="docker.io/library/nginx:alpine@sha256:4a73073bd557c65b759505da037898b61f1be6cbcc3c2c3aeac22d2a470c1752"

# The optional auto-update profile. Docker-only by design: it is built on the
# Docker socket and a filtered Docker API proxy, and #4188 rules out repointing
# it at the Podman socket. Listed here so the profile cannot drift either.
HIVE_STANDALONE_IMAGE_DOCKER_SOCKET_PROXY="docker.io/tecnativa/docker-socket-proxy:0.3.0@sha256:9e4b9e7517a6b660f2cc903a19b257b1852d5b3344794e3ea334ff00ae677ac2"
HIVE_STANDALONE_IMAGE_WATCHTOWER="docker.io/containrrr/watchtower:1.7.1@sha256:6dd50763bbd632a83cb154d5451700530d1e44200b268a4e9488fefdfcf2b038"

# Component name -> variable name. The component names are what assets and the
# contract test refer to.
hive_standalone_image_components() {
  cat <<'EOF'
hive
gateway
docker-socket-proxy
watchtower
EOF
}

# Prints the canonical reference for one component.
hive_standalone_image() {
  local component="${1:-}"

  case "$component" in
    hive) printf '%s\n' "$HIVE_STANDALONE_IMAGE_HIVE" ;;
    gateway) printf '%s\n' "$HIVE_STANDALONE_IMAGE_GATEWAY" ;;
    docker-socket-proxy) printf '%s\n' "$HIVE_STANDALONE_IMAGE_DOCKER_SOCKET_PROXY" ;;
    watchtower) printf '%s\n' "$HIVE_STANDALONE_IMAGE_WATCHTOWER" ;;
    *)
      printf 'ERROR: unknown standalone image component %q\n' "$component" >&2
      return 64
      ;;
  esac
}

# Expands a reference to its fully qualified form, the way a registry client
# resolves a short name. `nginx:alpine` and `docker.io/library/nginx:alpine`
# are the same image; this makes them the same string so Docker's spelling and
# Podman's required spelling can be compared.
hive_standalone_image_normalize() {
  local ref="${1:-}"
  local first="${ref%%/*}"

  [[ -n "$ref" ]] || return 64

  # A first segment carrying a dot, a port, or the literal localhost is a
  # registry host; anything else is part of the repository path.
  if [[ "$ref" == */* ]] && { [[ "$first" == *.* ]] || [[ "$first" == *:* ]] || [[ "$first" == "localhost" ]]; }; then
    printf '%s\n' "$ref"
    return 0
  fi

  if [[ "$ref" == */* ]]; then
    printf 'docker.io/%s\n' "$ref"
  else
    printf 'docker.io/library/%s\n' "$ref"
  fi
}

# The digest of a reference, or the empty string when it is not pinned.
hive_standalone_image_digest() {
  local ref="${1:-}"

  case "$ref" in
    *@sha256:*) printf 'sha256:%s\n' "${ref##*@sha256:}" ;;
    *) printf '\n' ;;
  esac
}

hive_standalone_images_list() {
  local component
  while IFS= read -r component; do
    [[ -n "$component" ]] || continue
    printf '%s\t%s\n' "$component" "$(hive_standalone_image "$component")"
  done < <(hive_standalone_image_components)
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  set -euo pipefail
  case "${1:-list}" in
    list) hive_standalone_images_list ;;
    get) hive_standalone_image "${2:-}" ;;
    normalize) hive_standalone_image_normalize "${2:-}" ;;
    -h|--help|help)
      cat <<'EOF'
Usage: standalone-images.sh [list]
       standalone-images.sh get <component>
       standalone-images.sh normalize <image-reference>

Components: hive, gateway, docker-socket-proxy, watchtower.
EOF
      ;;
    *)
      printf 'ERROR: unknown action %q\n' "$1" >&2
      exit 64
      ;;
  esac
fi
