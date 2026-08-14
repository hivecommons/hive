#!/usr/bin/env bash
# Static security contracts for the Kubernetes deployment and hub template.
set -euo pipefail

DEPLOYMENT="${1:-src/deploy/k8s/deployment.yaml}"
PROVISION="${2:-src/pkg/hub/saas_provision.go}"
RECONCILE="${3:-src/pkg/hub/netadmin_reconcile.go}"
fail=0

check() {
  local description="$1" pattern="$2" file="$3"
  if grep -qE -e "$pattern" -- "$file"; then
    echo "  ok: $description"
  else
    echo "  FAIL: $description"
    fail=1
  fi
}

check_absent() {
  local description="$1" pattern="$2" file="$3"
  if grep -qE -e "$pattern" -- "$file"; then
    echo "  FAIL: $description"
    fail=1
  else
    echo "  ok: $description"
  fi
}

echo "== Kubernetes privilege hardening contract =="
check "manifest drops SETUID" '^[[:space:]]*-[[:space:]]+SETUID[[:space:]]+#?' "$DEPLOYMENT"
check "manifest drops SETGID" '^[[:space:]]*-[[:space:]]+SETGID[[:space:]]+#?' "$DEPLOYMENT"
check "manifest keeps RuntimeDefault seccomp" 'type:[[:space:]]+RuntimeDefault' "$DEPLOYMENT"
check "hub template drops SETUID" '^[[:space:]]*-[[:space:]]+SETUID[[:space:]]*$' "$PROVISION"
check "hub template drops SETGID" '^[[:space:]]*-[[:space:]]+SETGID[[:space:]]*$' "$PROVISION"
check "reconcile targets capabilities.add" 'hiveContainerCapabilitiesAddPath' "$RECONCILE"
check_absent "reconcile does not patch the complete securityContext" 'path.*hiveContainerSecurityContextPath' "$RECONCILE"

if (( fail != 0 )); then
  echo "security hardening contract FAILED" >&2
  exit 1
fi
echo "security hardening contract passed"
