#!/usr/bin/env bash
# Every test suite under src/deploy/ must be named by at least one workflow
# under .github/workflows/ (#5529).
#
# This is the src/deploy/ sibling of bin/test_bin_suites_wired.sh (#4363), and
# it exists because the same hole was measurably open here. src/deploy/ suites
# are wired INDIVIDUALLY, by name, in a workflow step. A new suite is therefore
# never picked up automatically, and an omission produces no failure — nothing
# fails when a test file is simply never named. The suite passes locally, the
# file looks maintained, and CI is green because it never ran.
#
# Three live instances, all found by accident rather than by a gate:
#
#   test_entrypoint_symlinks.sh        — referenced by NO workflow at all; the
#                                        omission this guard was written
#                                        against (#5529). Passes 8/0, so
#                                        nothing was broken — but the Copilot
#                                        token-persistence symlinks it guards
#                                        were not being checked either.
#   test_entrypoint_system_gitconfig.sh — same state, found incidentally during
#                                        #5388 and registered there.
#   test_hive_snapshot_unit_contract.sh — merged in #5499 with no registration;
#                                        caught only because a duplicate PR
#                                        happened to include it (#5504).
#
# The bar is "referenced ANYWHERE under .github/workflows/", not "referenced in
# v2-ci.yml". Several suites live correctly in specialised lanes —
# test_ambient_cap_runtime.sh and test_manifest_caps_runtime.sh in
# suid-contract.yml, test_image_suid_inventory.sh in docker.yml and
# suid-contract.yml, test_quadlet_generator_gate.sh in quadlet-gate.yml —
# because they need a runtime, an image build, or a generator those workflows
# already stand up. Demanding v2-ci.yml specifically would fail all four for
# being in the right place.
#
# Run: bash src/deploy/test_deploy_suites_wired.sh
# Exit codes: 0 every suite is wired, 1 at least one is orphaned.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "${ROOT}/.." && pwd)"
WORKFLOWS="${REPO_ROOT}/.github/workflows"

# Files matching test_*.sh that are NOT suites and must not be required to be
# wired as one. Format: filename|reason.
#
# test_lib.sh is the shared skip-discipline helper (#5388 item 3) that the
# suites SOURCE; running it standalone asserts nothing. It currently satisfies
# a naive reference check only because a v2-ci.yml comment happens to mention
# it by name — delete that comment and the guard would start demanding a
# library be run as a test. Naming it here makes it a decision rather than an
# accident of prose.
NOT_A_SUITE=(
  "test_lib.sh|shared skip-discipline helper sourced by the suites, not a suite itself"
)

# Real suites deliberately not in CI. Format: filename|reason. Keep the reason
# specific enough that a reviewer can judge whether it still holds; an empty
# list is the healthy state.
UNWIRED_ON_PURPOSE=(
  "test_container_resource_limits.sh|#6485 fix landed from a GitHub App branch, which cannot edit .github/workflows/; wire it into v2-ci.yml next to test_attach_hint_runtime.sh (maintainer follow-up), then delete this entry"
)

failures=0
checked=0

is_exempt() {
  local name="$1" entry
  for entry in "${UNWIRED_ON_PURPOSE[@]}"; do
    [[ "${entry%%|*}" == "$name" ]] && return 0
  done
  return 1
}

is_not_a_suite() {
  local name="$1" entry
  for entry in "${NOT_A_SUITE[@]}"; do
    [[ "${entry%%|*}" == "$name" ]] && return 0
  done
  return 1
}

not_a_suite_reason() {
  local name="$1" entry
  for entry in "${NOT_A_SUITE[@]}"; do
    if [[ "${entry%%|*}" == "$name" ]]; then
      printf '%s' "${entry#*|}"
      return 0
    fi
  done
  printf 'no reason recorded'
}

exempt_reason() {
  local name="$1" entry
  for entry in "${UNWIRED_ON_PURPOSE[@]}"; do
    if [[ "${entry%%|*}" == "$name" ]]; then
      printf '%s' "${entry#*|}"
      return 0
    fi
  done
  printf 'no reason recorded'
}

printf '=== src/deploy/ test suites are wired into a workflow (#5529) ===\n\n'

# A checker aimed at a path matching no files exits 0 and reports success,
# which is indistinguishable from "everything is wired" (#5388). Both the
# workflow directory and the suite glob are therefore asserted non-empty
# BEFORE any per-suite verdict is printed.
if [[ ! -d "$WORKFLOWS" ]]; then
  printf 'FATAL: %s does not exist — nothing to check references against.\n' "$WORKFLOWS"
  exit 1
fi

workflow_count=$(find "$WORKFLOWS" -maxdepth 1 -type f \( -name '*.yml' -o -name '*.yaml' \) | wc -l | tr -d ' ')
if [[ "$workflow_count" -eq 0 ]]; then
  printf 'FATAL: no workflow files under %s — every suite would report unwired.\n' "$WORKFLOWS"
  exit 1
fi

shopt -s nullglob
suites=("${ROOT}"/deploy/test_*.sh)
if [[ "${#suites[@]}" -eq 0 ]]; then
  printf 'FATAL: no src/deploy/test_*.sh suites matched — the glob is wrong,\n'
  printf '       not the tree. Passing here would assert nothing.\n'
  exit 1
fi

printf 'Scanning %d workflow file(s) for %d suite(s).\n\n' "$workflow_count" "${#suites[@]}"

for path in "${suites[@]}"; do
  name="$(basename "$path")"

  # Helpers are excluded before counting: they are not suites, so they are
  # neither "checked" nor eligible to be orphaned.
  if is_not_a_suite "$name"; then
    printf '  ----: %s — not a suite: %s\n' "$name" "$(not_a_suite_reason "$name")"
    continue
  fi

  # This guard is itself a suite and is checked like any other; skipping it
  # would be the exact hole it exists to close.
  checked=$((checked + 1))

  # Fixed-string grep for the basename anywhere under .github/workflows/.
  # Grep rather than parsing YAML on purpose: a `run:` line, a matrix entry and
  # a comment all name the file the same way, and a reference that is only a
  # comment still tells a reader where to look. -F so a name is never read as a
  # pattern.
  if grep -rqF -- "$name" "$WORKFLOWS" 2>/dev/null; then
    printf '  PASS: %s\n' "$name"
    continue
  fi

  if is_exempt "$name"; then
    printf '  SKIP: %s — unwired on purpose: %s\n' "$name" "$(exempt_reason "$name")"
    continue
  fi

  printf '  FAIL: %s is not named by any workflow under .github/workflows/\n' "$name"
  printf '        Add a step next to its siblings in .github/workflows/v2-ci.yml:\n'
  printf '          - name: <what it guards>\n'
  printf '            working-directory: .\n'
  printf '            run: bash src/deploy/%s\n' "$name"
  printf '        A specialised lane (suid-contract.yml, docker.yml,\n'
  printf '        quadlet-gate.yml) counts too if the suite needs what it\n'
  printf '        stands up. If it genuinely should not run in CI, add it to\n'
  printf '        UNWIRED_ON_PURPOSE in this file with the reason.\n'
  failures=$((failures + 1))
done

printf '\nChecked %d suite(s) against %d workflow(s), %d orphaned.\n' \
  "$checked" "$workflow_count" "$failures"
[[ "$failures" -eq 0 ]] || exit 1
exit 0
