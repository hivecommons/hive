#!/usr/bin/env bash
# go wrapper — blocks in-pod Go test/vet execution for hive agents.
# Installed at /usr/local/go/bin/go, with the real toolchain renamed beside it.
#
# Agent identity is signaled by HIVE_AGENT_ID, the same per-agent environment
# used by gh-wrapper.sh. The hive process itself does not set it.

set -euo pipefail

REAL_GO="${HIVE_GO_REAL:-/usr/local/go/bin/go-real}"

if [[ ! -x "$REAL_GO" ]]; then
  echo "go toolchain is not available" >&2
  exit 127
fi

is_blocked_subcommand=false
case "${1:-}" in
  test|vet)
    is_blocked_subcommand=true
    ;;
  tool)
    case "${2:-}" in
      vet)
        is_blocked_subcommand=true
        ;;
    esac
    ;;
esac

if [[ "$is_blocked_subcommand" == "true" && -n "${HIVE_AGENT_ID:-}" && "${HIVE_ALLOW_LOCAL_GO_TEST:-}" != "1" ]]; then
  echo "⛔ BLOCKED: 'go test'/'go vet' inside the hive pod is disabled for agents. Tests run in the repo's CI: commit, push, open the PR with hive-open-pr, and read the CI result. Running the suite here reads and mutates the live hive's state and has killed agent sessions (hivecommons/hive#9845)." >&2
  exit 2
fi

exec "$REAL_GO" "$@"
