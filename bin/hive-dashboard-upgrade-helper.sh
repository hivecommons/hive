#!/usr/bin/env bash
# Closed host-side executor for dashboard-triggered Hive upgrades.
#
# This helper intentionally accepts one operation, "upgrade", and a small
# allow-list of runtimes. It passes image references as argv to the existing
# reviewed host lifecycle scripts; it never evaluates shell text from Hive.

set -euo pipefail

usage() {
  cat >&2 <<'EOF'
Usage: hive-dashboard-upgrade-helper.sh upgrade --runtime <podman-quadlet|docker-compose> --ref ghcr.io/hivecommons/hive:<tag|@sha256> [--podman-mode <rootless|rootful>]
EOF
  exit 64
}

cmd="${1:-}"; shift || true
[ "$cmd" = "upgrade" ] || usage

runtime=""
ref=""
podman_mode=""
while [ $# -gt 0 ]; do
  case "$1" in
    --runtime) runtime="${2:-}"; shift 2 ;;
    --ref) ref="${2:-}"; shift 2 ;;
    --podman-mode) podman_mode="${2:-}"; shift 2 ;;
    *) usage ;;
  esac
done

if ! [[ "$ref" =~ ^ghcr\.io/hivecommons/hive(:[A-Za-z0-9._-]+|@sha256:[0-9a-f]{64})$ ]]; then
  echo "ERROR: ref must name ghcr.io/hivecommons/hive by tag or sha256 digest" >&2
  exit 64
fi

repo_root="${HIVE_SRC_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
case "$runtime" in
  podman-quadlet)
    update_script="${HIVE_PODMAN_UPDATE_SCRIPT:-${repo_root}/bin/hive-podman-update.sh}"
    [ -x "$update_script" ] || { echo "ERROR: podman update script is not executable: $update_script" >&2; exit 78; }
    case "$podman_mode" in
      rootless) exec "$update_script" pin "$ref" --rootless ;;
      rootful) exec "$update_script" pin "$ref" --rootful ;;
      *) echo "ERROR: podman mode must be rootless or rootful" >&2; exit 64 ;;
    esac
    ;;
  docker-compose)
    deploy_script="${HIVE_COMPOSE_DEPLOY_SCRIPT:-${repo_root}/src/deploy/blue-green-deploy.sh}"
    [ -x "$deploy_script" ] || { echo "ERROR: compose deploy script is not executable: $deploy_script" >&2; exit 78; }
    exec "$deploy_script" --skip-build --image-ref "$ref"
    ;;
  *)
    echo "ERROR: unsupported runtime: $runtime" >&2
    exit 64
    ;;
esac
