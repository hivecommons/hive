#!/usr/bin/env bash
# Contract test for the standalone image-reference source of truth (#4206).
# Run: bash src/deploy/test_standalone_image_refs.sh
#
# Enforces that every standalone deployment asset takes its image references
# from src/deploy/standalone-images.sh, so the Docker Compose stack and the
# Podman assets that land later cannot drift apart. Pure string analysis: it
# contacts no registry, pulls nothing, and runs offline.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=src/deploy/standalone-images.sh disable=SC1091
source "${ROOT}/src/deploy/standalone-images.sh"

# Standalone deployment assets. The Podman globs match nothing today and start
# being enforced automatically the moment those assets land, which is the point
# — drift detection must not need a second edit to switch on.
DOCKER_ASSETS=(
  "src/docker-compose.yaml"
)
PODMAN_ASSET_GLOBS=(
  "src/deploy/quadlet/*.container"
  "src/deploy/quadlet/*.pod"
  "src/deploy/quadlet/*.kube"
  "src/deploy/podman-compose.yaml"
  "src/podman-compose.yaml"
)

# The retired rolling tag of the v2 branch. It resolves to a different digest
# than the v4 release channels, so an asset still naming it is running other
# code, not merely an older spelling.
RETIRED_TAG="v2-latest"

pass_count=0
failures=0

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  failures=$((failures + 1))
}

ok() {
  pass_count=$((pass_count + 1))
}

# Collects `image: <ref>` values out of an asset, ignoring comments. Handles
# both the Compose `image:` key and the Quadlet `Image=` key.
asset_image_refs() {
  local file="$1"
  sed -e 's/[[:space:]]*#.*$//' "$file" \
    | grep -oE '^[[:space:]]*(image:|Image=)[[:space:]]*[^[:space:]]+' \
    | sed -e 's/^[[:space:]]*//' -e 's/^image:[[:space:]]*//' -e 's/^Image=[[:space:]]*//' \
    | grep -v '^$' || true
}

collect_assets() {
  local asset glob
  for asset in "${DOCKER_ASSETS[@]}"; do
    [[ -f "${ROOT}/${asset}" ]] && printf '%s\n' "$asset"
  done
  for glob in "${PODMAN_ASSET_GLOBS[@]}"; do
    for asset in ${ROOT}/${glob}; do
      [[ -f "$asset" ]] && printf '%s\n' "${asset#"${ROOT}/"}"
    done
  done
  return 0
}

# --- 1. Every canonical reference is already fully qualified ----------------
#
# Podman's Quadlet generator warns on short names and `podman auto-update`
# refuses them outright, so the source of truth must not hand out a reference
# that resolves through unqualified-search-registries.

while IFS=$'\t' read -r component canonical; do
  [[ -n "$component" ]] || continue
  normalized="$(hive_standalone_image_normalize "$canonical")"
  if [[ "$normalized" == "$canonical" ]]; then
    ok
  else
    fail "canonical reference for '${component}' is not fully qualified: ${canonical} (resolves to ${normalized})"
  fi

  if [[ "$canonical" == *"$RETIRED_TAG"* ]]; then
    fail "canonical reference for '${component}' still names the retired ${RETIRED_TAG} tag"
  else
    ok
  fi
done < <(hive_standalone_images_list)

# --- 2. Every asset reference matches a canonical reference -----------------
#
# This is the Docker/Podman drift check. Comparison is on the normalized form,
# so Compose may keep Docker's short spelling while the Podman assets carry the
# fully qualified one, and both still have to point at the same image.

canonical_normalized=()
while IFS=$'\t' read -r _component canonical; do
  [[ -n "$canonical" ]] || continue
  canonical_normalized+=("$(hive_standalone_image_normalize "$canonical")")
done < <(hive_standalone_images_list)

asset_count=0
while IFS= read -r asset; do
  [[ -n "$asset" ]] || continue
  asset_count=$((asset_count + 1))

  while IFS= read -r ref; do
    [[ -n "$ref" ]] || continue

    # A reference built locally rather than pulled is out of scope here.
    [[ "$ref" == \$* ]] && continue

    if [[ "$ref" == *"$RETIRED_TAG"* ]]; then
      fail "${asset} references the retired ${RETIRED_TAG} tag: ${ref}"
      continue
    fi

    normalized="$(hive_standalone_image_normalize "$ref")"
    matched="false"
    for canonical in "${canonical_normalized[@]}"; do
      [[ "$normalized" == "$canonical" ]] && matched="true" && break
    done

    if [[ "$matched" == "true" ]]; then
      ok
    else
      fail "${asset} uses an image reference that is not in src/deploy/standalone-images.sh: ${ref}"
      printf '      (normalized: %s)\n' "$normalized" >&2
      printf '      Add it to the source of truth, or change the asset to match one of:\n' >&2
      printf '        %s\n' "${canonical_normalized[@]}" >&2
    fi
  done < <(asset_image_refs "${ROOT}/${asset}")
done < <(collect_assets)

[[ "$asset_count" -gt 0 ]] || fail "no standalone deployment assets were found to check"

# --- 3. Digest pinning survives ---------------------------------------------
#
# Where the source of truth pins a digest, the asset must carry the same one.
# A tag-only asset reference against a digest-pinned canonical is exactly the
# supply-chain regression the pins exist to prevent.

while IFS= read -r asset; do
  [[ -n "$asset" ]] || continue
  while IFS= read -r ref; do
    [[ -n "$ref" ]] || continue
    [[ "$ref" == \$* ]] && continue

    normalized="$(hive_standalone_image_normalize "$ref")"
    ref_repo="${normalized%%@*}"
    ref_repo="${ref_repo%%:*}"

    while IFS=$'\t' read -r _component canonical; do
      [[ -n "$canonical" ]] || continue
      canonical_norm="$(hive_standalone_image_normalize "$canonical")"
      canonical_repo="${canonical_norm%%@*}"
      canonical_repo="${canonical_repo%%:*}"
      [[ "$ref_repo" == "$canonical_repo" ]] || continue

      canonical_digest="$(hive_standalone_image_digest "$canonical_norm")"
      [[ -n "$canonical_digest" ]] || continue

      ref_digest="$(hive_standalone_image_digest "$normalized")"
      if [[ "$ref_digest" == "$canonical_digest" ]]; then
        ok
      else
        fail "${asset} drops or changes the digest pin for ${canonical_repo}: asset has '${ref_digest:-none}', source of truth has '${canonical_digest}'"
      fi
    done < <(hive_standalone_images_list)
  done < <(asset_image_refs "${ROOT}/${asset}")
done < <(collect_assets)

# --- 4. The Hive image tracks a live channel --------------------------------
#
# `v4-latest` is deliberately NOT accepted here. It is the same digest as
# `stable` today (src/docs/release-channels.md), but the channels split the
# day a soak/promotion policy lands (#3702) — and accepting both spellings is
# what let the probes hard-code `v4-latest` while the deployment ran `stable`
# without CI noticing (#4486).

hive_ref="$(hive_standalone_image hive)"
case "$hive_ref" in
  ghcr.io/hivecommons/hive:stable|ghcr.io/hivecommons/hive@sha256:*|ghcr.io/hivecommons/hive:*@sha256:*)
    ok
    ;;
  *)
    fail "the Hive image should track a live release channel or a pinned digest, got: ${hive_ref}"
    ;;
esac

# --- 5. Probes and the release qualification defer to the source of truth ---
#
# #4486: every Podman probe and the SELinux release qualification hard-coded
# `v4-latest` while the deployment assets ran `stable`. The two are the same
# digest today (src/docs/release-channels.md), so nothing failed — but on the
# day the channels split (#3702) the probes would silently measure code the
# operators are not running, which is exactly the drift this file exists to
# prevent. Each script must source standalone-images.sh for its default and
# must not carry a literal Hive reference; --image / IMAGE stay the overrides.

PROBE_SCRIPTS=(
  "src/deploy/probe_arm64_image_startup.sh"
  "src/deploy/probe_podman_ipv6_egress.sh"
  "src/deploy/probe_podman_rootful_netadmin.sh"
  "src/deploy/probe_podman_rootless_netadmin.sh"
  "src/deploy/probe_podman_selinux_avc.sh"
  "src/deploy/qualify_podman_selinux.sh"
)

for script in "${PROBE_SCRIPTS[@]}"; do
  if [[ ! -f "${ROOT}/${script}" ]]; then
    fail "probe script named in the source-of-truth check does not exist: ${script}"
    continue
  fi

  if grep -q 'standalone-images\.sh' "${ROOT}/${script}" \
    && grep -Eq '\$\{?HIVE_STANDALONE_IMAGE_HIVE\}?' "${ROOT}/${script}"; then
    ok
  else
    fail "${script} does not take its image default from src/deploy/standalone-images.sh (#4486)"
  fi

  if grep -Eq 'ghcr\.io/kubestellar/hive[:@]' "${ROOT}/${script}"; then
    fail "${script} hard-codes a Hive image reference; the default must come from standalone-images.sh, with --image / IMAGE as the overrides (#4486)"
  else
    ok
  fi
done

if [[ "$failures" -gt 0 ]]; then
  printf '\n%d standalone image-reference contract failure(s)\n' "$failures" >&2
  exit 1
fi

printf 'PASS: %d standalone image-reference assertions across %d asset(s)\n' \
  "$pass_count" "$asset_count"
