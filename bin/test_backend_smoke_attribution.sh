#!/usr/bin/env bash
# backend-smoke's failure-issue script says the true thing (#5883).
#
# The pinned lane moved to the self-hosted fleet (#5862), the fleet image has no
# podman, and every scheduled run died at the image pull in ~7s with exit 127.
# The failure-issue job then filed an issue announcing that "the CLI versions
# hive SHIPS in the contributor image failed the smoke — contributors are likely
# broken right now" about a suite that had never executed. The shipped CLIs were
# fine; the runner had no container engine.
#
# That script is JavaScript embedded in a YAML block scalar, and it runs ONLY on
# a scheduled failure, in production, where its output is an issue somebody acts
# on. Nothing exercised it. This suite extracts it from the workflow — not a
# copy, so it cannot drift from what CI runs — and executes it against fixtures
# built from the real run and from a real suite failure, asserting that an
# environment fault and a broken CLI are reported as the different things they
# are.
#
# Run: bash bin/test_backend_smoke_attribution.sh
# Exit codes: 0 the attribution is correct, 1 it is not.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKFLOW="${ROOT}/.github/workflows/backend-smoke.yml"
HARNESS="${ROOT}/bin/backend_smoke_attribution_harness.js"

if [[ ! -f "$WORKFLOW" ]]; then
  printf 'FAIL: %s not found\n' "$WORKFLOW" >&2
  exit 1
fi
if [[ ! -f "$HARNESS" ]]; then
  printf 'FAIL: %s not found\n' "$HARNESS" >&2
  exit 1
fi

# node is a hard requirement, not an optional nicety: skipping here would make
# this suite report green in exactly the environment where it did not run, which
# is the failure mode the repo's own wiring guard (#4363) exists to stop.
if ! command -v node >/dev/null 2>&1; then
  printf 'FAIL: node is not on PATH, so the attribution was NOT checked\n' >&2
  exit 1
fi

exec node "$HARNESS" "$WORKFLOW"
