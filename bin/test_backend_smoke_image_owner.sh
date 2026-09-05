#!/usr/bin/env bash
# #6086: backend-smoke's pinned lane must test THIS repository's contributor
# image, not upstream's.
#
# The lane exists to run the checkout's suite against the CLI binaries the
# contributor image actually ships (backend-smoke.yml's own header). It named
# ghcr.io/hivecommons/hive-contributor:latest outright, so in a fork it
# exercised UPSTREAM's binaries and passed regardless of what the fork ships --
# while docker.yml already publishes to ghcr.io/<owner>/hive-contributor. The
# two halves disagreed: a fork published one image and tested another.
#
# A green check that is structurally incapable of failing for the reason it
# exists is worse than an absent one, which is why this is worth a suite
# despite the issue's own "low severity".
#
# WHY THIS EXTRACTS THE STEP RATHER THAN GREPPING IT. The selection is shell
# with a fallback branch, and the interesting cases are the ones a grep cannot
# see: that an uppercase owner is lowercased before it becomes an image
# reference, and that the fallback is LOUD. Same reasoning as
# bin/test_backend_smoke_attribution.sh, which extracts its JavaScript from
# this same workflow rather than keeping a copy that can drift.
#
# Run: bash bin/test_backend_smoke_image_owner.sh
# Exit codes: 0 the lane pins the right image, 1 it does not.
set -uo pipefail

PASS=0
FAIL=0

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKFLOW="${ROOT}/.github/workflows/backend-smoke.yml"

ok()  { echo "  PASS: $1"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "        $2"; FAIL=$((FAIL + 1)); }

echo "=== #6086: the pinned lane pins THIS repo's contributor image ==="

if [ ! -f "$WORKFLOW" ]; then
  bad "workflow not found: $WORKFLOW"
  echo; echo "=== $PASS passed, $FAIL failed ==="; exit 1
fi

# python3 + PyYAML is a hard requirement here, not an optional nicety: a skip
# would report green in exactly the environment where nothing ran, which is the
# failure the repo's wiring guard (#4363) exists to stop.
if ! command -v python3 >/dev/null 2>&1 || ! python3 -c 'import yaml' >/dev/null 2>&1; then
  bad "python3 + PyYAML unavailable, so the step was NOT checked"
  echo; echo "=== $PASS passed, $FAIL failed ==="; exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Pull the pinned lane's image-selection step straight out of the workflow, so
# this cannot drift from what CI runs.
python3 - "$WORKFLOW" >"$WORK/step.sh" <<'PY'
import sys, yaml
doc = yaml.safe_load(open(sys.argv[1]))
for job in (doc.get("jobs") or {}).values():
    for step in job.get("steps") or []:
        if str(step.get("name", "")).startswith("Pull the contributor image"):
            sys.stdout.write(step["run"])
            raise SystemExit(0)
raise SystemExit("no 'Pull the contributor image' step found")
PY
if [ ! -s "$WORK/step.sh" ]; then
  bad "could not extract the image-selection step from the workflow"
  echo; echo "=== $PASS passed, $FAIL failed ==="; exit 1
fi
ok "extracted the image-selection step from the workflow"

# A podman that succeeds only for references listed in PULLABLE, and records
# every reference it was asked for.
mkdir -p "$WORK/bin"
cat >"$WORK/bin/podman" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$PULL_LOG"
ref="${!#}"
case " ${PULLABLE:-} " in
  *" $ref "*) exit 0 ;;
esac
printf 'Error: %s: manifest unknown\n' "$ref" >&2
exit 125
STUB
chmod +x "$WORK/bin/podman"

# GitHub runs `run:` blocks under `bash -e -o pipefail`; the fallback's
# behaviour when NEITHER image pulls depends on that, so the harness matches it.
run_step() { # owner, pullable-refs
  : >"$WORK/pull.log"
  : >"$WORK/github_env"
  PATH="$WORK/bin:$PATH" \
    REPO_OWNER="$1" \
    PULLABLE="$2" \
    PULL_LOG="$WORK/pull.log" \
    GITHUB_ENV="$WORK/github_env" \
    bash -e -o pipefail "$WORK/step.sh" 2>&1
}

OWNER_IMG="ghcr.io/tuna-os/hive-contributor:latest"
UPSTREAM_IMG="ghcr.io/hivecommons/hive-contributor:latest"

# ── The fork case this issue is about ────────────────────────────────────
out="$(run_step 'tuna-os' "$OWNER_IMG $UPSTREAM_IMG")"
if grep -qF "CONTRIBUTOR_IMAGE=$OWNER_IMG" "$WORK/github_env"; then
  ok "a fork with its own published image pins that image"
else
  bad "the fork's own image was not pinned" "GITHUB_ENV: $(cat "$WORK/github_env")"
fi
if printf '%s' "$out" | grep -q '::warning'; then
  bad "a fork testing its OWN image still emitted the fallback warning" "$out"
else
  ok "no fallback warning when the owner's image is pullable"
fi

# ── Upstream must be unaffected ──────────────────────────────────────────
out="$(run_step 'hivecommons' "$UPSTREAM_IMG")"
if grep -qF "CONTRIBUTOR_IMAGE=$UPSTREAM_IMG" "$WORK/github_env"; then
  ok "upstream still pins ghcr.io/hivecommons/hive-contributor"
else
  bad "upstream's own image is no longer pinned" "GITHUB_ENV: $(cat "$WORK/github_env")"
fi

# ── The fallback: loud, not silent ───────────────────────────────────────
#
# A fork that has not published yet still gets a meaningful run rather than a
# pull failure -- but it is testing upstream's binaries, which is the whole
# defect, so it must SAY so.
out="$(run_step 'tuna-os' "$UPSTREAM_IMG")"
if grep -qF "CONTRIBUTOR_IMAGE=$UPSTREAM_IMG" "$WORK/github_env"; then
  ok "an unpublished fork falls back to the upstream image"
else
  bad "the fallback did not pin the upstream image" "GITHUB_ENV: $(cat "$WORK/github_env")"
fi
if printf '%s' "$out" | grep -q '::warning'; then
  ok "the fallback is announced as a workflow warning"
else
  bad "the fallback was silent" "a lane testing upstream's CLIs must say so: $out"
fi

# ── An uppercase owner must still resolve to a valid reference ───────────
#
# Container reference paths are lowercase-only, so "ghcr.io/Tuna-OS/..." is not
# a valid reference and podman rejects it before contacting any registry. Left
# uncased, every fork whose org is not already all-lowercase takes the fallback
# on EVERY run -- the same "your lane tests upstream's image" defect, reached by
# a different route. GHCR serves the owner lowercased, so the lowercased path is
# canonical rather than a guess.
out="$(run_step 'Tuna-OS' "$OWNER_IMG $UPSTREAM_IMG")"
if grep -qF "CONTRIBUTOR_IMAGE=$OWNER_IMG" "$WORK/github_env"; then
  ok "an uppercase owner is lowercased into a valid image reference"
else
  bad "an uppercase owner did not resolve to the fork's own image" \
      "it fell back instead; GITHUB_ENV: $(cat "$WORK/github_env")"
fi
if grep -q '[A-Z]' "$WORK/pull.log"; then
  bad "podman was asked to pull a reference containing uppercase" \
      "$(cat "$WORK/pull.log")"
else
  ok "no uppercase reference is ever handed to podman"
fi

# ── Neither image available is a hard failure, not a silent skip ─────────
#
# The lane's whole claim is "the shipped CLIs work". With no image at all it
# has tested nothing, and must not report success -- the same attribution
# argument bin/test_backend_smoke_attribution.sh pins for the other direction.
run_step 'tuna-os' '' >/dev/null 2>&1
rc=$?
if [ "$rc" -ne 0 ]; then
  ok "no pullable image at all fails the step (exit $rc)"
else
  bad "the step succeeded with no image pulled" "the lane would then run nothing and report green"
fi

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
