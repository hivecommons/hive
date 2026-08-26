#!/usr/bin/env bash
# Every executable test suite under bin/ must be run by something (#4363).
#
# bin/gh-wrapper.test.sh was the author-gate regression suite for #3072/#3096 —
# 33 cases covering a SECURITY boundary — and no workflow, Justfile target, or
# hook invoked it. It ran only when someone remembered, which means a
# regression in the author gate would have landed green. It was not alone:
# test_hive_open_issue.sh and test_hive_podman_preflight_ids.sh were in the
# same state, the latter while both of its sibling preflight suites were wired.
#
# That is the shape of the bug this guard exists for. Each individual omission
# was invisible because nothing FAILS when a test file is simply never named —
# the suite passes locally, the file looks maintained, and CI is green because
# it never ran. Wiring the three that were orphaned fixes today; this stops the
# fourth from being written next month.
#
# The rule: a file in bin/ that looks like a test suite must be named by at
# least one workflow, the Justfile, or a githook. Deliberate exceptions go in
# UNWIRED_ON_PURPOSE with a reason, so "not in CI" stays a decision someone
# wrote down rather than something that happened.
#
# Run: bash bin/test_bin_suites_wired.sh
# Exit codes: 0 every suite is wired, 1 at least one is orphaned.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Places a suite can legitimately be run from.
SEARCH_PATHS=(".github" "Justfile" "githooks")

# Suites deliberately not in CI. Format: filename|reason. Keep the reason
# specific enough that a reviewer can judge whether it still holds; an empty
# list is the healthy state.
UNWIRED_ON_PURPOSE=(
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

printf '=== bin/ test suites are wired into something (#4363) ===\n\n'

shopt -s nullglob
for path in "${ROOT}"/bin/test_*.sh "${ROOT}"/bin/*.test.sh; do
  name="$(basename "$path")"

  # This guard is itself a suite and is wired like any other; skipping it would
  # be the exact hole it exists to close.
  checked=$((checked + 1))

  # Look for the basename anywhere something could invoke it. Grep rather than
  # parsing YAML on purpose: a step, a `just` recipe, and a hook all name the
  # file the same way, and a reference that is only a comment still tells a
  # reader where to look.
  refs=0
  for where in "${SEARCH_PATHS[@]}"; do
    [[ -e "${ROOT}/${where}" ]] || continue
    if grep -rqF -- "$name" "${ROOT}/${where}" 2>/dev/null; then
      refs=1
      break
    fi
  done

  if [[ "$refs" -eq 1 ]]; then
    printf '  PASS: %s\n' "$name"
    continue
  fi

  if is_exempt "$name"; then
    printf '  SKIP: %s — unwired on purpose: %s\n' "$name" "$(exempt_reason "$name")"
    continue
  fi

  printf '  FAIL: %s is not run by any workflow, Justfile target, or hook\n' "$name"
  printf '        Add a step next to its siblings in .github/workflows/v2-ci.yml:\n'
  printf '          - name: <what it guards>\n'
  printf '            working-directory: .\n'
  printf '            run: bash bin/%s\n' "$name"
  printf '        If it genuinely should not run in CI, add it to\n'
  printf '        UNWIRED_ON_PURPOSE in this file with the reason.\n'
  failures=$((failures + 1))
done

printf '\nChecked %d suite(s), %d orphaned.\n' "$checked" "$failures"
[[ "$failures" -eq 0 ]] || exit 1
exit 0
