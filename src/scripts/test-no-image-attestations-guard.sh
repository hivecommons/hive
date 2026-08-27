#!/usr/bin/env bash
# test-no-image-attestations-guard.sh — proves
# check-no-image-attestations.sh actually catches a reverted `sbom: false` /
# `provenance: false` pair, against fixtures rather than the real docker.yml
# alone (a checker that has quietly stopped detecting anything would
# otherwise pass on the real file for the wrong reason).
#
# Usage: src/scripts/test-no-image-attestations-guard.sh
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${HERE}/check-no-image-attestations.sh"
REPO_ROOT="$(cd "${HERE}/../.." && pwd)"

fail=0
note_fail() { echo "  FAIL: $*"; fail=1; }
note_ok()   { echo "  ok: $*"; }

tmp_root=${TMPDIR:-"$HERE/../.test-tmp"}
mkdir -p "$tmp_root"
tmp=$(mktemp -d "$tmp_root/attestation-guard.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

write_fixture() {
  cat > "$tmp/fixture.yml"
}

# ---------------------------------------------------------------------------
# Case 1: a single well-formed build step passes.
# ---------------------------------------------------------------------------
write_fixture <<'EOF'
jobs:
  build:
    steps:
      - name: Build and push by digest
        uses: docker/build-push-action@53b7df96c91f9c12dcc8a07bcb9ccacbed38856a # v7.3.0
        with:
          context: .
          provenance: false
          sbom: false
          push: true
EOF
if bash "$GUARD" "$tmp/fixture.yml" >/dev/null 2>&1; then
  note_ok "a well-formed step (both flags false) passes"
else
  note_fail "a well-formed step should pass but the guard failed it"
fi

# ---------------------------------------------------------------------------
# Case 2: provenance: false reverted (missing) fails.
# ---------------------------------------------------------------------------
write_fixture <<'EOF'
jobs:
  build:
    steps:
      - name: Build and push by digest
        uses: docker/build-push-action@53b7df96c91f9c12dcc8a07bcb9ccacbed38856a # v7.3.0
        with:
          context: .
          sbom: false
          push: true
EOF
if bash "$GUARD" "$tmp/fixture.yml" >/dev/null 2>&1; then
  note_fail "a step missing provenance: false should fail the guard, but it passed"
else
  note_ok "a step missing provenance: false is caught"
fi

# ---------------------------------------------------------------------------
# Case 3: sbom: false reverted to sbom: true fails (not just missing).
# ---------------------------------------------------------------------------
write_fixture <<'EOF'
jobs:
  build:
    steps:
      - name: Build and push by digest
        uses: docker/build-push-action@53b7df96c91f9c12dcc8a07bcb9ccacbed38856a # v7.3.0
        with:
          context: .
          provenance: false
          sbom: true
          push: true
EOF
if bash "$GUARD" "$tmp/fixture.yml" >/dev/null 2>&1; then
  note_fail "sbom: true should fail the guard, but it passed"
else
  note_ok "sbom: true (reverted from false) is caught"
fi

# ---------------------------------------------------------------------------
# Case 4: multiple steps — one good, one bad — must still fail overall, and
# must not let a good step upstream mask a bad one downstream.
# ---------------------------------------------------------------------------
write_fixture <<'EOF'
jobs:
  build:
    steps:
      - name: Good step
        uses: docker/build-push-action@53b7df96c91f9c12dcc8a07bcb9ccacbed38856a # v7.3.0
        with:
          provenance: false
          sbom: false
      - name: Bad step
        uses: docker/build-push-action@53b7df96c91f9c12dcc8a07bcb9ccacbed38856a # v7.3.0
        with:
          push: true
EOF
if bash "$GUARD" "$tmp/fixture.yml" >/dev/null 2>&1; then
  note_fail "a bad step after a good one should still fail the guard"
else
  note_ok "a bad step is caught even when an earlier step in the same file is fine"
fi

# ---------------------------------------------------------------------------
# Case 5: no build-push-action steps at all is an error, not a silent pass —
# a renamed/replaced action must not make this guard assert nothing.
# ---------------------------------------------------------------------------
write_fixture <<'EOF'
jobs:
  build:
    steps:
      - name: Unrelated step
        run: echo hi
EOF
if bash "$GUARD" "$tmp/fixture.yml" >/dev/null 2>&1; then
  note_fail "a file with zero build-push-action steps should fail loudly, not pass"
else
  note_ok "zero build-push-action steps fails loudly instead of asserting nothing"
fi

# ---------------------------------------------------------------------------
# Case 6: the real docker.yml in this repo passes right now. This is the
# regression check: if a future PR removes one of the four sites, THIS check
# (not just the fixtures above) must fail CI.
# ---------------------------------------------------------------------------
if bash "$GUARD" "${REPO_ROOT}/.github/workflows/docker.yml" >/dev/null 2>&1; then
  note_ok "the real .github/workflows/docker.yml currently passes the guard"
else
  note_fail "the real .github/workflows/docker.yml does NOT currently pass the guard — investigate before trusting this guard in CI"
fi

echo
if [[ $fail -ne 0 ]]; then
  echo "RESULT: FAIL"
  exit 1
fi
echo "RESULT: PASS — check-no-image-attestations.sh catches every reverted case."
