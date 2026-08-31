#!/usr/bin/env bash
# #5370: on a pull request the arm64 lane must probe the PR's OWN code.
#
# The fault this closes is a guard that cannot fail. The lane probed
# ghcr.io/kubestellar/hive:stable — the published release-channel image — so on
# a PR it validated code already on v4 rather than the change proposed. A PR
# that REPAIRS a startup bug ran against the still-broken published image and
# stayed red; a PR that INTRODUCED one ran against the still-good published
# image and went green. The signal was inverted exactly when it mattered.
#
# Measured: #5342 broke startup (#5360) and merged green; the lane then stayed
# red across four merges, and #5368 — the fix — was red too.
#
# These are structural assertions against the workflow and probe. The lane's
# real behaviour needs an arm64 runner with Podman, which is the lane itself;
# what is checkable here is that the wiring which makes it probe PR code exists
# and has not been quietly removed.
#
# Run: bash src/deploy/test_arm64_lane_probes_pr_code.sh
set -uo pipefail

PASS=0
FAIL=0

HERE="$(cd "$(dirname "$0")" && pwd)"
LANE="$(cd "$HERE/../../.github/workflows" && pwd)/podman-arm64-lane.yml"
PROBE="$HERE/probe_arm64_image_startup.sh"

ok()  { echo "  PASS: $1"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "        $2"; FAIL=$((FAIL + 1)); }

echo "=== #5370: the arm64 lane probes PR code, not the published image ==="

for f in "$LANE" "$PROBE"; do
  [ -f "$f" ] || { bad "missing file: $f"; echo; echo "=== $PASS passed, $FAIL failed ==="; exit 1; }
done

# ── The lane must build on a pull request ────────────────────────────────
if grep -q "if: github.event_name == 'pull_request'" "$LANE" \
   && grep -qE '^\s+podman build' "$LANE"; then
  ok "the lane builds the image from the checkout on a pull request"
else
  bad "the lane does not build the PR's image" \
      "without this it probes the published (pre-PR) image and cannot gate the change under review"
fi

# That build must NOT push. The lane holds `contents: read` and no secrets;
# a push would need credentials it deliberately does not have.
if grep -qE '^\s+podman push' "$LANE"; then
  bad "the lane pushes an image" \
      "this lane has contents: read and no registry secrets — building is local-only by design"
else
  ok "the PR build is local-only (no push, no registry credentials needed)"
fi

# The permission block must stay minimal. A build step is not a reason to
# widen it, and quietly gaining packages: write would be a real regression.
if grep -qE '^\s+contents: read$' "$LANE" && ! grep -qE '^\s+packages: write' "$LANE"; then
  ok "the lane still declares only contents: read"
else
  bad "the lane's permissions changed" \
      "building the PR image locally must not require packages: write"
fi

# ── A build failure must be loud ─────────────────────────────────────────
# Falling back to the published image when the PR build fails would restore
# the inverted signal: a PR whose image does not build would go green against
# someone else's working image.
build_block="$(awk '/Build the arm64 image from this PR/,/^      - name: Select/' "$LANE")"
if grep -qE 'continue-on-error|\|\| true' <<<"$build_block"; then
  bad "the PR build step swallows failures" \
      "a PR that cannot build an image must fail the lane, not fall back to the published one"
else
  ok "the PR build step has no continue-on-error and no || true"
fi

# ── The probe must be told the image is local ────────────────────────────
# A locally-built image has no registry manifest and nothing to pull, so the
# probe's first two cases must be skipped explicitly rather than left to fail.
# Match the ARGUMENT being appended, not the word appearing in a comment.
# A grep for bare '--local' passes on the comment alone, which would make this
# assertion unable to fail — the same defect as the lane it is guarding.
if grep -qE 'args="\$\{args\}[[:space:]]+--local"' "$LANE"; then
  ok "the lane passes --local when probing a PR-built image"
else
  bad "the lane never passes --local to the probe" \
      "the probe would try to pull a local-only reference and fail for the wrong reason"
fi

if grep -q -- '--local) LOCAL_IMAGE="true"' "$PROBE"; then
  ok "the probe accepts --local"
else
  bad "the probe does not implement --local"
fi

# --local must not silently weaken the lane: the cases that carry the signal
# (binary executes, service starts) must still run. Only manifest+pull skip.
if grep -q 'cases: manifest + pull (SKIPPED: --local)' "$PROBE"; then
  ok "--local skips only the manifest and pull cases"
else
  bad "could not find the --local skip block in the probe"
fi

for c in 'the shipped binary executes' 'the service starts'; do
  # These printf headers must sit OUTSIDE the registry-vs-local branch, i.e.
  # they run on both paths. If one moved inside, --local would stop testing
  # the very thing #5370 is about.
  if grep -q -- "--- case: ${c}" "$PROBE" || grep -q -- "case: ${c}" "$PROBE"; then
    ok "the '${c}' case is still present"
  else
    bad "the '${c}' case is gone from the probe" \
        "that case is the signal a PR-built image exists to produce"
  fi
done

# A missing local image must FAIL, never skip. An arm64 lane that steps aside
# when its image is absent reports green while proving nothing — the same
# vacuous-pass shape #4336 already refused for a missing manifest.
if grep -q 'does not exist (was the build step skipped or did it fail?)' "$PROBE"; then
  ok "a missing local image fails the probe rather than skipping"
else
  bad "the probe does not fail on a missing local image"
fi

# ── The push path must be unchanged ──────────────────────────────────────
# #4336's stop condition still holds there: on a push the lane pulls the
# published image, and a missing arm64 manifest is a failure, not a skip. The
# fix for #5370 is scoped to pull_request and must not have relaxed that.
if grep -q 'no linux/${ARCH} image published for' "$PROBE" \
   || grep -q 'no linux/%s image is published' "$PROBE"; then
  ok "a missing published arm64 manifest still fails on the push path (#4336)"
else
  bad "the missing-manifest failure is gone" \
      "#4336: the fix for an absent arm64 image belongs in the publisher, not a lane that skips"
fi

# ── The stale default must not come back ─────────────────────────────────
# The dispatch input said 'default ghcr.io/kubestellar/hive:v4-latest' and
# nothing defaulted to that: an empty input means the probe's own fallback,
# HIVE_STANDALONE_IMAGE_HIVE = ghcr.io/kubestellar/hive:stable. Documenting a
# tag the code never uses is how #5370's description misdescribed the bug.
if grep -q "description: 'Image to probe (default ghcr.io/kubestellar/hive:v4-latest)'" "$LANE"; then
  bad "the workflow_dispatch input still claims a v4-latest default" \
      "an empty input resolves to ghcr.io/kubestellar/hive:stable via standalone-images.sh"
else
  ok "the dispatch input no longer claims a v4-latest default"
fi

# The actual fallback must still be the #4206 source of truth, not a literal.
if grep -q 'IMAGE="${IMAGE:-$HIVE_STANDALONE_IMAGE_HIVE}"' "$PROBE"; then
  ok "the probe default still comes from standalone-images.sh (#4206)"
else
  bad "the probe no longer defaults to HIVE_STANDALONE_IMAGE_HIVE" \
      "hardcoding a tag here breaks the single source of truth for image refs"
fi

# ── #5339: never an empty ${{ }} ─────────────────────────────────────────
# An empty expression makes GitHub refuse to parse the workflow, and the
# parser does not honour shell comments — so it breaks even inside one.
if grep -qE '\$\{\{[[:space:]]*\}\}' "$LANE"; then
  bad "the workflow contains an empty \${{ }} expression" \
      "#5339: GitHub refuses to parse the file, including inside comments"
else
  ok "no empty \${{ }} expression in the workflow (#5339)"
fi

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
