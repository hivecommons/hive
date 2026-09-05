#!/usr/bin/env bash
# #6083: every runs-on that reaches for the self-hosted fleet must degrade to a
# GitHub-hosted runner in a repository that has no fleet.
#
# THE FAULT THIS CLOSES, and why it is worse than "those checks don't run".
#
# 24 runs-on declarations named [self-hosted, hive] outright. In a fork those
# jobs do not fail -- they sit in `queued` forever. A red X is a bug report; a
# job that never starts is invisible, and the pull request simply never reaches
# a conclusion.
#
# It compounds. A run stuck that way holds its workflow's CONCURRENCY GROUP, so
# the next push's run of the same workflow queues behind it; and a job waiting
# on a label no runner carries CANNOT BE CANCELLED, because there is no runner
# to deliver the cancellation to. Measured on a fork at v4: one pull request
# produced 26 runs, 14 still queued an hour later, and a cancel returned
# 202 Accepted while the run stayed `queued` 22 minutes later. The wedge does
# not clear by itself or by operator action short of deleting the run.
#
# The fix is an indirection, not a removal: vars.HIVE_RUNNER_LABELS carries the
# fleet's labels where a fleet exists, and every expression falls back to a
# GitHub-hosted runner where the variable is unset. Upstream sets it once; a
# fork sets nothing and its CI runs.
#
# This guard exists because the labels were correct once already: the
# fork-guarded spelling in most of these files was added deliberately (#5855,
# #5862-#5868) and three jobs were still left bare in the same files. The
# inconsistency is the normal outcome of hand-editing 24 sites, so it is
# checked rather than remembered.
#
# Run: bash src/deploy/test_runner_labels_portable.sh
# Exit codes: 0 every runs-on is fork-portable, 1 at least one is not.
set -uo pipefail

PASS=0
FAIL=0

HERE="$(cd "$(dirname "$0")" && pwd)"
WORKFLOWS="$(cd "$HERE/../../.github/workflows" && pwd)"

ok()  { echo "  PASS: $1"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "        $2"; FAIL=$((FAIL + 1)); }

echo "=== #6083: runner labels degrade to a GitHub-hosted runner in a fork ==="

# A checker pointed at a path matching no files exits 0 and reports success,
# which is indistinguishable from "everything is portable". Assert the corpus
# is non-empty BEFORE any verdict is printed.
shopt -s nullglob
files=("$WORKFLOWS"/*.yml "$WORKFLOWS"/*.yaml)
shopt -u nullglob
if [ "${#files[@]}" -eq 0 ]; then
  bad "no workflow files under $WORKFLOWS" "passing here would assert nothing"
  echo; echo "=== $PASS passed, $FAIL failed ==="; exit 1
fi
ok "${#files[@]} workflow file(s) to check"

# ── No runs-on may name the fleet without going through the variable ──────
#
# Comments and job steps legitimately mention self-hosted runners in prose, so
# this looks only at runs-on lines rather than at the whole file.
bare=""
for f in "${files[@]}"; do
  while IFS= read -r line; do
    case "$line" in
      *HIVE_RUNNER_LABELS*) continue ;;
    esac
    bare+="$(basename "$f"): ${line#"${line%%[![:space:]]*}"}"$'\n'
  done < <(grep -h 'runs-on:.*self-hosted' "$f" 2>/dev/null)
done
if [ -z "$bare" ]; then
  ok "every runs-on naming the fleet goes through vars.HIVE_RUNNER_LABELS"
else
  bad "a runs-on names the self-hosted fleet directly" \
      "in a fork these jobs queue forever, cannot be cancelled, and wedge the workflow's concurrency group:"
  printf '%s' "$bare" | sed 's/^/          /'
fi

# ── Every use of the variable must carry a GitHub-hosted fallback ─────────
#
# vars.HIVE_RUNNER_LABELS on its own is an empty string in a fork, and
# `runs-on: ${{ vars.HIVE_RUNNER_LABELS }}` there resolves to nothing at all --
# the same permanent queue this issue is about, reached by a different route.
# The fallback is what makes the indirection safe, so it is required rather
# than conventional.
#
# The fallback is spelled inside fromJSON's argument on purpose:
#
#   fromJSON(vars.HIVE_RUNNER_LABELS || '["ubuntu-latest"]')
#
# so fromJSON is handed valid JSON whether or not the variable is set. Writing
# it as `vars.X && fromJSON(vars.X) || 'ubuntu-latest'` would instead depend on
# GitHub short-circuiting && before evaluating fromJSON('') -- true today, but
# an undocumented detail to rest a fleet-wide CI outage on.
missing=""
for f in "${files[@]}"; do
  while IFS= read -r line; do
    # The fallback must sit INSIDE fromJSON's argument. Matching merely
    # "ubuntu-latest appears somewhere on the line" is not enough: the
    # fork-guarded spelling already ends in a bare ubuntu-latest fallback, so
    # that outer one would mask fromJSON(vars.HIVE_RUNNER_LABELS) with no inner
    # default -- the one form that ERRORS in a fork instead of degrading.
    # Found by mutating exactly that while this guard was being written.
    case "$line" in
      *"[\"ubuntu-latest\"]')"*) continue ;;
    esac
    missing+="$(basename "$f"): ${line#"${line%%[![:space:]]*}"}"$'\n'
  done < <(grep -h 'runs-on:.*HIVE_RUNNER_LABELS' "$f" 2>/dev/null)
done
if [ -z "$missing" ]; then
  ok "every HIVE_RUNNER_LABELS runs-on falls back to a GitHub-hosted runner"
else
  bad "a runs-on uses HIVE_RUNNER_LABELS with no GitHub-hosted fallback" \
      "unset, the expression resolves to no runner at all:"
  printf '%s' "$missing" | sed 's/^/          /'
fi

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
