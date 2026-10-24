#!/usr/bin/env bash
# Crux boundary scope for v2-tests.yml pull requests (hivecommons/hive#13536).
#
# Usage:
#   crux-scope.sh <base-rev> <head-rev>
#
# Prints exactly one word on stdout:
#   crux-only  every classified change is inside the Crux package set below
#   none       no classified change is inside the Crux set
#   mixed      changes on both sides of the boundary
#   full       a fail-safe path changed (module graph, shared harness,
#              pkg/config, workflows, CI/repo scripts), or the diff could not
#              be read: the caller must run the whole suite
#
# Files that no Go test reads (changelog.d/, CHANGELOG.md, docs/, src/docs/,
# root-level *.md) do not influence the verdict; a diff made only of those is `none`.
#
# This refines, never replaces, the affected-package selection in
# .github/scripts/affected-go-packages.sh: v2-tests.yml consults this verdict
# only on a `partial` selection and uses it to (a) skip the hub slices and the
# dashboard shuffle lane on a crux-only diff and (b) skip the agent slices on
# a diff that touches no Crux package. Everything the dependency analysis
# marks `all` stays `all` regardless of what this script says.
set -uo pipefail

base=${1:?base revision}
head=${2:?head revision}

# The Crux package set: pkg/agent and its subpackages plus the runtime
# packages the RFC lists. KEEP IN SYNC with the "Crux boundary" header
# comment in .github/workflows/v2-tests.yml.
CRUX_DIRS=(
  src/pkg/agent
  src/pkg/agentmode
  src/pkg/agentparse
  src/pkg/agentsmd
  src/pkg/claude
  src/pkg/sandbox
  src/pkg/kubejob
  src/pkg/toolapprove
  src/pkg/skillreg
  src/pkg/inference
  src/pkg/openrouter
  src/pkg/watchdog
  src/pkg/trajectory
  src/pkg/logscrub
  src/pkg/proxy
  src/pkg/pushbroker
  src/pkg/watsonx
)

changed=$(git diff --no-renames --name-only "$base" "$head" 2>/dev/null) || { echo full; exit 0; }

in_crux=0
out_crux=0
while IFS= read -r f; do
  [ -n "$f" ] || continue
  case "$f" in
    # Fail-safe: anything that can change how every package builds or tests.
    src/go.mod|src/go.sum|src/internal/*|src/pkg/config/*|.github/workflows/*|.github/scripts/*|src/scripts/*)
      echo full
      exit 0
      ;;
    # Never read by a Go test; neutral for the verdict.
    changelog.d/*|CHANGELOG.md|docs/*|src/docs/*) continue ;;
  esac
  case "$f" in
    */*) ;;
    *.md) continue ;;  # root-level README/CONTRIBUTING/AGENTS etc.
  esac
  hit=0
  for d in "${CRUX_DIRS[@]}"; do
    case "$f" in
      "$d"/*) hit=1; break ;;
    esac
  done
  if [ "$hit" = 1 ]; then in_crux=1; else out_crux=1; fi
done <<<"$changed"

if [ "$in_crux" = 1 ] && [ "$out_crux" = 0 ]; then
  echo crux-only
elif [ "$in_crux" = 0 ]; then
  echo none
else
  echo mixed
fi
