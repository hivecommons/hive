#!/usr/bin/env bash
# Regression coverage for kubestellar/hive#2833.
#
# Run: bash bin/contributor-agent.test.sh

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK_DIR="${ROOT_DIR}/.contributor-agent-test-work-$$"
HOOK_DIR="${WORK_DIR}/entrypoint.d"
HOME_DIR="${WORK_DIR}/home"

cleanup() {
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

mkdir -p "$HOOK_DIR" "$HOME_DIR"

cat >"${HOOK_DIR}/10-backend-override.sh" <<'HOOK'
backend_binary() {
  case "$1" in
    goose) echo "goose-from-entrypoint-hook" ;;
    *) echo "$1" ;;
  esac
}

backend_perm_flag() {
  case "$1" in
    goose) echo "--from-entrypoint-hook" ;;
    *) echo "" ;;
  esac
}
HOOK

run_resolve() {
  env -i \
    PATH="${PATH}" \
    HOME="$HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_ENTRYPOINT_HOOK_DIR="$HOOK_DIR" \
    HIVE_CONTRIBUTOR_AGENT_TEST_RESOLVE_BACKEND=1 \
    AGENT_BACKEND=goose \
    bash "${ROOT_DIR}/bin/contributor-agent.sh"
}

output="$(run_resolve)"

case "$output" in
  *"backend_binary=goose-from-entrypoint-hook"* ) ;;
  *)
    echo "expected entrypoint.d backend_binary override to survive; got:" >&2
    echo "$output" >&2
    exit 1
    ;;
esac

case "$output" in
  *"backend_perm_flag=--from-entrypoint-hook"* ) ;;
  *)
    echo "expected entrypoint.d backend_perm_flag override to survive; got:" >&2
    echo "$output" >&2
    exit 1
    ;;
esac

hook_output="$(
  env -i \
    PATH="${PATH}" \
    HOME="$HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_ENTRYPOINT_HOOK_DIR="${WORK_DIR}/empty-entrypoint.d" \
    HIVE_PRE_AGENT_HOOK='backend_binary(){ echo "goose-from-pre-agent-hook"; }; backend_perm_flag(){ echo "--from-pre-agent-hook"; }' \
    HIVE_CONTRIBUTOR_AGENT_TEST_RESOLVE_BACKEND=1 \
    AGENT_BACKEND=goose \
    bash "${ROOT_DIR}/bin/contributor-agent.sh"
)"

case "$hook_output" in
  *"backend_binary=goose-from-pre-agent-hook"* ) ;;
  *)
    echo "expected HIVE_PRE_AGENT_HOOK backend_binary override to survive; got:" >&2
    echo "$hook_output" >&2
    exit 1
    ;;
esac

case "$hook_output" in
  *"backend_perm_flag=--from-pre-agent-hook"* ) ;;
  *)
    echo "expected HIVE_PRE_AGENT_HOOK backend_perm_flag override to survive; got:" >&2
    echo "$hook_output" >&2
    exit 1
    ;;
esac

echo "contributor-agent hook override tests passed"
