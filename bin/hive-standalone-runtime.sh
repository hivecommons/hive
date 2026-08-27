#!/usr/bin/env bash
# Select the container engine for standalone Hive deployments.
#
# This layer intentionally selects only the engine. Runtime-specific lifecycle
# assets (Compose, Quadlet, and so on) remain responsible for their own
# provider and version checks.

HIVE_DEPLOY_RUNTIME_DEFAULT="docker"

hive_deploy_runtime_select() {
  local runtime="${HIVE_DEPLOY_RUNTIME:-$HIVE_DEPLOY_RUNTIME_DEFAULT}"

  case "$runtime" in
    docker|podman)
      printf '%s\n' "$runtime"
      ;;
    *)
      printf 'ERROR: unsupported HIVE_DEPLOY_RUNTIME=%q; expected docker or podman\n' "$runtime" >&2
      return 64
      ;;
  esac
}

hive_deploy_runtime_require() {
  local runtime
  runtime="$(hive_deploy_runtime_select)" || return

  if ! command -v "$runtime" >/dev/null 2>&1; then
    printf 'ERROR: selected Hive deployment runtime %q is not installed or not in PATH\n' "$runtime" >&2
    return 127
  fi

  printf '%s\n' "$runtime"
}

hive_deploy_runtime_report() {
  local runtime
  runtime="$(hive_deploy_runtime_require)" || return
  printf 'Hive deployment runtime: %s (%s)\n' "$runtime" "$(command -v "$runtime")"
}

hive_deploy_runtime_run() {
  local runtime
  runtime="$(hive_deploy_runtime_require)" || return

  printf 'Hive deployment runtime: %s (%s)\n' "$runtime" "$(command -v "$runtime")" >&2
  "$runtime" "$@"
}

hive_deploy_runtime_usage() {
  cat <<'EOF'
Usage: hive-standalone-runtime.sh [report]
       hive-standalone-runtime.sh run [--] <runtime-arguments...>

HIVE_DEPLOY_RUNTIME selects docker or podman. It defaults to docker.
The run command never falls back to a different runtime.
EOF
}

hive_deploy_runtime_main() {
  local action="${1:-report}"

  case "$action" in
    report)
      [[ $# -le 1 ]] || {
        hive_deploy_runtime_usage >&2
        return 64
      }
      hive_deploy_runtime_report
      ;;
    run)
      shift
      [[ "${1:-}" == "--" ]] && shift
      [[ $# -gt 0 ]] || {
        printf 'ERROR: run requires runtime arguments\n' >&2
        hive_deploy_runtime_usage >&2
        return 64
      }
      hive_deploy_runtime_run "$@"
      ;;
    -h|--help|help)
      hive_deploy_runtime_usage
      ;;
    *)
      printf 'ERROR: unknown action %q\n' "$action" >&2
      hive_deploy_runtime_usage >&2
      return 64
      ;;
  esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  set -euo pipefail
  hive_deploy_runtime_main "$@"
fi
