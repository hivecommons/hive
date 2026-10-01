#!/usr/bin/env bash
# go wrapper — blocks in-pod Go test/vet execution for hive agents.
# Installed at /usr/local/go/bin/go, with the real toolchain renamed beside it.
#
# Agent identity can be signaled by HIVE_AGENT or HIVE_AGENT_ID. The Go
# manager's direct-launch path sets HIVE_AGENT only, while agent-launch.sh sets
# HIVE_AGENT_ID. Either means this is an agent sandbox.

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

AGENT_NAME="${HIVE_AGENT:-${HIVE_AGENT_ID:-}}"
if [[ "$is_blocked_subcommand" == "true" && -n "$AGENT_NAME" && "${HIVE_ALLOW_LOCAL_GO_TEST:-}" != "1" ]]; then
  echo "⛔ BLOCKED: ${AGENT_NAME}: 'go test'/'go vet' inside the hive pod is disabled for agents. Tests run in the repo's CI: commit, push, open the PR with hive-open-pr, and read the CI result. Running the suite here reads and mutates the live hive's state and has killed agent sessions (hivecommons/hive#9845)." >&2
  exit 2
fi

exec "$REAL_GO" "$@"
