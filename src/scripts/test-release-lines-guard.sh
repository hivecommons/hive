#!/usr/bin/env bash
# test-release-lines-guard.sh — executes src/scripts/check-release-lines.sh
# against fixtures to prove it still detects the failures it exists to catch.
#
# #4405 asks for the demonstration explicitly: "Adding a plausible future branch
# name to the repository's release lines without updating the workflows fails
# the check. Demonstrate the failure." Case 2 below is that demonstration, run
# in CI against the REAL workflows rather than pasted into a PR description, so
# it keeps being true.
#
# The rest cover the ways the guard could rot into a green no-op: a checker that
# only understands one of the two YAML spellings of `branches:` silently stops
# covering v2-ci.yml, the highest-value entry in the table.
#
# Cases 11-15 cover the same demand for the lists that are NOT triggers (#4462)
# — docker.yml's `LONG_LIVED` push policy. That one can rot in an extra way a
# branch filter cannot: the variable can be renamed, or defined twice, leaving
# an entry that asserts nothing while the guard still prints a line about it.
#
# Usage: src/scripts/test-release-lines-guard.sh
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../.." && pwd)"
GUARD="${HERE}/check-release-lines.sh"
REAL_MANIFEST="${REPO_ROOT}/.github/release-lines.yml"
REAL_WORKFLOWS="${REPO_ROOT}/.github/workflows"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail=0
pass() { echo "  ok: $*"; }
bad()  { echo "  FAIL: $*"; fail=1; }

# expect <expected-exit> <description> -- <guard args...>
# On an unexpected result the guard's own output is echoed, since that output is
# the thing under test.
expect() {
  local want="$1" desc="$2"; shift 3
  local out rc
  out="$(bash "$GUARD" "$@" 2>&1)"; rc=$?
  if [[ $rc -eq $want ]]; then
    LAST_OUT="$out"
    pass "$desc"
    return 0
  fi
  bad "$desc (guard exited ${rc}, expected ${want})"
  echo "$out" | sed 's/^/      | /'
  LAST_OUT="$out"
  return 1
}

expect_mentions() {
  local needle="$1" desc="$2"
  if grep -qF -- "$needle" <<< "$LAST_OUT"; then
    pass "$desc"
  else
    bad "$desc (no mention of '${needle}' in the guard's output)"
    echo "$LAST_OUT" | sed 's/^/      | /'
  fi
}

echo "== Release-line guard self-test =="

# ---------------------------------------------------------------------------
# 1. The repository as it stands is in sync.
# ---------------------------------------------------------------------------
expect 0 "repository is in sync with its own manifest" -- "$REAL_MANIFEST" "$REAL_WORKFLOWS"
# The manifest side can rot too: dropping docker.yml's `env_lists` entry would
# leave the run green by having nothing to check. Assert the real run does
# assert the push policy, so that deletion is a failing test rather than a
# quieter guard.
expect_mentions "env LONG_LIVED" "the real run asserts docker.yml's LONG_LIVED push policy"

# ---------------------------------------------------------------------------
# 2. THE DEMONSTRATION (#4405 acceptance criterion 1). Add a plausible future
#    release line to the manifest and change nothing else: every pinned
#    workflow must be reported as not covering it.
# ---------------------------------------------------------------------------
sed 's/^release_lines:.*/release_lines: [v2, v4, v5]/' "$REAL_MANIFEST" > "${TMP}/v5-manifest.yml"
expect 1 "a new release line with no workflow edits fails the check" -- \
  "${TMP}/v5-manifest.yml" "$REAL_WORKFLOWS"
expect_mentions "missing: v5" "the failure names the branch that would not be covered"
for wf in v2-ci.yml v2-tests.yml podman-contract.yml podman-rootful-lane.yml \
          podman-rootless-lane.yml podman-arm64-lane.yml quadlet-gate.yml \
          suid-contract.yml scorecard.yml; do
  expect_mentions "FAIL: ${wf}" "${wf} is reported"
done
# docker.yml's TRIGGER is deliberately unpinned and must NOT be dragged into the
# failure — while its push policy, which is not a trigger, must be.
if grep -Eq "FAIL: docker\.yml:[0-9]+ branches" <<< "$LAST_OUT"; then
  bad "docker.yml's '**' trigger was flagged; it is deliberate"
else
  pass "docker.yml's '**' trigger is recognised as deliberate, not flagged"
fi
expect_mentions "FAIL: docker.yml" "docker.yml's LONG_LIVED push policy is reported (#4462)"
expect_mentions "env LONG_LIVED" "the push-policy failure names the env list, not a trigger"

# ---------------------------------------------------------------------------
# Fixtures for the remaining cases.
# ---------------------------------------------------------------------------
mk_manifest() {
  cat > "$1" <<'EOF'
release_lines: [v2, v4]
pinned:
  flow-form.yml: []
  block-form.yml: []
unpinned:
  wild.yml: '**'
env_lists:
  envy.yml LONG_LIVED: [mk]
EOF
}

WFDIR="${TMP}/workflows"
mkdir -p "$WFDIR"

write_flow()  { printf 'on:\n  push:\n    branches: [%s]\n' "$1" > "${WFDIR}/flow-form.yml"; }
write_block() {
  { printf 'on:\n  push:\n    branches:\n'
    for b in $1; do printf '      - %s\n' "$b"; done
  } > "${WFDIR}/block-form.yml"
}
write_wild()  { printf 'on:\n  push:\n    branches:\n      - %s\n' "$1" > "${WFDIR}/wild.yml"; }
# A workflow carrying a hand-written branch list that is NOT a trigger, in the
# shape docker.yml's gate job uses. It has no branch filter at all, so it is
# classified by `env_lists` alone.
write_env()   {
  { printf 'jobs:\n  gate:\n    steps:\n      - name: Decide push policy\n'
    printf '        env:\n'
    printf '          %s\n' "$1"
  } > "${WFDIR}/envy.yml"
}

MANIFEST="${TMP}/manifest.yml"
mk_manifest "$MANIFEST"

# 3. Both spellings in sync -> pass.
write_flow "v2, v4"; write_block "v2 v4"; write_wild "'**'"
write_env 'LONG_LIVED: "v2 v4 mk"'
expect 0 "flow sequence and block sequence both parse when in sync" -- "$MANIFEST" "$WFDIR"

# 4. Flow spelling (`branches: [v2, v4]`) out of sync is caught. This is the
#    v2-ci.yml shape — a guard that only reads block sequences misses it.
write_flow "v2"
expect 1 "a stale flow-sequence list is caught" -- "$MANIFEST" "$WFDIR"
expect_mentions "FAIL: flow-form.yml" "the flow-sequence workflow is named"
expect_mentions "missing: v4" "the flow-sequence failure names the missing branch"
write_flow "v2, v4"

# 5. Block spelling out of sync is caught.
write_block "v2"
expect 1 "a stale block-sequence list is caught" -- "$MANIFEST" "$WFDIR"
expect_mentions "FAIL: block-form.yml" "the block-sequence workflow is named"
write_block "v2 v4"

# 6. Quoted entries and inline comments do not confuse the parser.
write_flow '"v2", "v4"'
expect 0 "quoted flow entries parse" -- "$MANIFEST" "$WFDIR"
{ printf 'on:\n  push:\n    branches:\n'
  printf "      - 'v2'   # oldest supported line\n"
  printf '      - v4\n'
} > "${WFDIR}/block-form.yml"
expect 0 "quoted block entries with inline comments parse" -- "$MANIFEST" "$WFDIR"
write_block "v2 v4"

# 7. A workflow that carries a branch filter but is in neither manifest list is
#    reported: the next pinned workflow somebody adds is the next #4339.
printf 'on:\n  push:\n    branches: [v4]\n' > "${WFDIR}/newcomer.yml"
expect 1 "an unclassified pinned workflow is caught" -- "$MANIFEST" "$WFDIR"
expect_mentions "not classified" "the unclassified workflow explains what to do"
rm "${WFDIR}/newcomer.yml"

# 8. A workflow declared unpinned that loses its wildcard is caught, so
#    docker.yml cannot quietly acquire the very problem this guards against.
write_wild "v4"
expect 1 "an unpinned workflow narrowed to a fixed list is caught" -- "$MANIFEST" "$WFDIR"
expect_mentions "FAIL: wild.yml" "the narrowed workflow is named"
write_wild "'**'"

# 9. A manifest entry whose workflow lost its branch filter is caught: a guard
#    that checks a file with nothing to check reports green for no reason.
printf 'on:\n  workflow_dispatch:\n' > "${WFDIR}/flow-form.yml"
expect 1 "a pinned workflow with no branch filter at all is caught" -- "$MANIFEST" "$WFDIR"
expect_mentions "no branch filter" "the missing filter is explained"
write_flow "v2, v4"

# 10. A manifest entry naming a workflow that no longer exists is caught.
rm "${WFDIR}/block-form.yml"
expect 1 "a manifest entry for a deleted workflow is caught" -- "$MANIFEST" "$WFDIR"
expect_mentions "does not exist" "the deleted workflow is explained"
write_block "v2 v4"

# ---------------------------------------------------------------------------
# 11. A hand-written list that is not a trigger, in sync, parses in each of the
#     spellings a workflow env value can take.
# ---------------------------------------------------------------------------
write_env "LONG_LIVED: 'v2 v4 mk'"
expect 0 "a single-quoted env list parses" -- "$MANIFEST" "$WFDIR"
write_env 'LONG_LIVED: v2 v4 mk'
expect 0 "an unquoted env list parses" -- "$MANIFEST" "$WFDIR"
write_env 'LONG_LIVED: [v2, v4, mk]'
expect 0 "a flow-sequence env list parses" -- "$MANIFEST" "$WFDIR"
write_env 'LONG_LIVED: "v2 v4 mk"'

# ---------------------------------------------------------------------------
# 12. THE #4462 DEMONSTRATION on fixtures: a release line missing from a list
#     that is not a trigger is caught. Nothing about the workflow's triggers
#     changes, which is exactly why the branch-filter check cannot see it.
# ---------------------------------------------------------------------------
write_env 'LONG_LIVED: "v2 mk"'
expect 1 "a stale env list is caught" -- "$MANIFEST" "$WFDIR"
expect_mentions "FAIL: envy.yml" "the workflow holding the stale list is named"
expect_mentions "missing: v4" "the env-list failure names the missing branch"
expect_mentions "env LONG_LIVED" "the failure identifies the list by name"

# A branch the manifest does not declare as an extra is caught too: the check
# is an equality, so a retired release line left in the list is reported in the
# same run as one that was never added.
write_env 'LONG_LIVED: "v2 v4 mk v9"'
expect 1 "an undeclared extra branch in an env list is caught" -- "$MANIFEST" "$WFDIR"
expect_mentions "unexpected: v9" "the surplus branch is named"
write_env 'LONG_LIVED: "v2 v4 mk"'

# ---------------------------------------------------------------------------
# 13. The list is renamed or deleted. A branch filter cannot vanish without the
#     workflow visibly changing shape; an env var can, and the entry would then
#     assert nothing while still looking checked.
# ---------------------------------------------------------------------------
write_env 'PUBLISH_BRANCHES: "v2 v4 mk"'
expect 1 "an env list that no longer exists under that name is caught" -- "$MANIFEST" "$WFDIR"
expect_mentions "no 'LONG_LIVED:' assignment found" "the renamed list is explained"
write_env 'LONG_LIVED: "v2 v4 mk"'

# ---------------------------------------------------------------------------
# 14. Defined twice: the guard must refuse to guess which one is the policy
#     rather than checking the first and reporting green.
# ---------------------------------------------------------------------------
{ printf 'jobs:\n  gate:\n    steps:\n      - name: Decide push policy\n'
  printf '        env:\n          LONG_LIVED: "v2 v4 mk"\n'
  printf '  other:\n    steps:\n      - env:\n          LONG_LIVED: "v2"\n'
} > "${WFDIR}/envy.yml"
expect 1 "an env list defined twice is caught rather than guessed at" -- "$MANIFEST" "$WFDIR"
expect_mentions "is assigned 2 times" "the ambiguity is explained"
write_env 'LONG_LIVED: "v2 v4 mk"'

# Prose that merely mentions the variable is not a second definition: the real
# docker.yml names LONG_LIVED in a comment above the job, and case 1 would fail
# if that counted.
{ printf '# The long-lived branch set is defined ONCE below (LONG_LIVED).\n'
  printf 'jobs:\n  gate:\n    steps:\n      - env:\n'
  printf '          LONG_LIVED: "v2 v4 mk"   # release lines + standing branches\n'
  # shellcheck disable=SC2016 # the fixture is shell source, not an expansion
  printf '        run: for b in $LONG_LIVED; do :; done\n'
} > "${WFDIR}/envy.yml"
expect 0 "a comment and a shell reference are not counted as definitions" -- "$MANIFEST" "$WFDIR"
write_env 'LONG_LIVED: "v2 v4 mk"'

# ---------------------------------------------------------------------------
# 15. A manifest entry naming a workflow that no longer exists is caught here
#     too — the same rot as a stale `pinned` entry, in the other section.
# ---------------------------------------------------------------------------
rm "${WFDIR}/envy.yml"
expect 1 "an env_lists entry for a deleted workflow is caught" -- "$MANIFEST" "$WFDIR"
expect_mentions "does not exist" "the deleted workflow is explained"
write_env 'LONG_LIVED: "v2 v4 mk"'

echo
if [[ $fail -ne 0 ]]; then
  echo "RESULT: FAIL — the release-line guard is not detecting what it claims to."
  exit 1
fi
echo "RESULT: PASS — the release-line guard still catches a stale allow-list."
