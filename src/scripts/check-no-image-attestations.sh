#!/usr/bin/env bash
# check-no-image-attestations.sh — asserts docker.yml never re-enables
# build-push-action's default provenance/SBOM attestations (#3760).
#
# WHY THIS GUARD EXISTS
#
# build-push-action attaches provenance and SBOM attestations by DEFAULT. An
# attestation can only be carried by an OCI image *index*, so turning it back
# on changes the per-arch digest docker.yml pushes from a plain image manifest
# to application/vnd.oci.image.index.v1+json. That is exactly the OCI-index
# form that let a `COPY --from=builder` layer ship an overlayfs metacopy
# redirect for /usr/local/bin/hive — containerd/k3s and rootless podman
# present that redirect as NON-EXECUTABLE at the image path until copy-up, so
# every hive pulling such an image crash-looped at boot with a bare EPERM on
# exec. `docker.yml` explains this at length next to each `sbom: false` /
# `provenance: false` pair; this script is the automated version of "please
# read the comment before reverting these two lines" (#3760).
#
# It deliberately does NOT scan tagged-release.yml (the tagged-release workflow):
# that workflow never builds an image at all — it retags an already-published
# digest and generates a separate, standalone SBOM file (Syft, attached to
# the GitHub Release) that never touches GHCR or the image manifest. See
# src/docs/releases.md, "Software bill of materials (SBOM)".
#
# Usage: src/scripts/check-no-image-attestations.sh [docker.yml path]
set -uo pipefail

DOCKER_WORKFLOW="${1:-.github/workflows/docker.yml}"

if [[ ! -f "$DOCKER_WORKFLOW" ]]; then
  echo "ERROR: ${DOCKER_WORKFLOW} not found" >&2
  exit 1
fi

fail=0

# Every `docker/build-push-action` step in this file must carry an explicit
# `provenance: false` and `sbom: false` in its `with:` block. Rather than
# assume a fixed count of build steps (new build jobs get added over time),
# find every step that uses build-push-action and check the with: block that
# follows it, up to the next `- name:` step boundary or EOF.
mapfile -t step_lines < <(grep -n 'uses: docker/build-push-action@' "$DOCKER_WORKFLOW" | cut -d: -f1)

if [[ ${#step_lines[@]} -eq 0 ]]; then
  echo "ERROR: no docker/build-push-action steps found in ${DOCKER_WORKFLOW} — the guard has nothing to check. If build-push-action was renamed or replaced, update this script rather than letting it pass on nothing." >&2
  exit 1
fi

total_lines=$(wc -l < "$DOCKER_WORKFLOW")

for start in "${step_lines[@]}"; do
  # Find the next step boundary (a line matching '^\s*- name:') after $start,
  # or fall back to end of file.
  end=$(awk -v s="$start" 'NR > s && /^[[:space:]]*- name:/ { print NR - 1; exit }' "$DOCKER_WORKFLOW")
  if [[ -z "$end" ]]; then
    end=$total_lines
  fi

  block="$(sed -n "${start},${end}p" "$DOCKER_WORKFLOW")"

  has_provenance_false=0
  has_sbom_false=0
  grep -qE '^\s*provenance:\s*false\s*$' <<<"$block" && has_provenance_false=1
  grep -qE '^\s*sbom:\s*false\s*$' <<<"$block" && has_sbom_false=1

  if [[ $has_provenance_false -eq 1 && $has_sbom_false -eq 1 ]]; then
    echo "  ok: build-push-action step at line ${start} carries provenance: false and sbom: false"
  else
    missing=()
    [[ $has_provenance_false -eq 0 ]] && missing+=("provenance: false")
    [[ $has_sbom_false -eq 0 ]] && missing+=("sbom: false")
    echo "  FAIL: build-push-action step at ${DOCKER_WORKFLOW}:${start} is missing: ${missing[*]}. Re-enabling either attaches attestations by default, which forces an OCI *index* manifest instead of a plain image manifest and reintroduces the #3760 overlayfs metacopy-redirect crash-loop (containerd/k3s and rootless podman present /usr/local/bin/hive as non-executable at the image path until copy-up). Read the comment directly above this step in ${DOCKER_WORKFLOW} before changing it."
    fail=1
  fi
done

echo
if [[ $fail -ne 0 ]]; then
  echo "RESULT: FAIL — one or more build-push-action steps would attach image attestations."
  exit 1
fi
echo "RESULT: PASS — every build-push-action step in ${DOCKER_WORKFLOW} keeps provenance/SBOM attestations off (#3760)."
