#!/usr/bin/env bash
# test_kind_quickstart.sh — smoke test for the Try-Hive-on-kind overlay (#11657).
#
# Creates a kind cluster, applies src/deploy/kustomize/overlays/kind, waits for
# the rollout, checks /api/health through a port-forward, then deletes the
# cluster. Skips cleanly when kind or kubectl is not installed. Pulling the Hive
# image dominates the runtime; tune with HIVE_KIND_ROLLOUT_TIMEOUT.
#
# Run: bash src/deploy/test_kind_quickstart.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=src/deploy/test_lib.sh
. "${HERE}/test_lib.sh"

OVERLAY="${HERE}/kustomize/overlays/kind"
CLUSTER="hive-kind-smoke-$$"
TIMEOUT="${HIVE_KIND_ROLLOUT_TIMEOUT:-600s}"
LOCAL_PORT="${HIVE_KIND_LOCAL_PORT:-13002}"
PF_PID=""

echo "=== #11657: Try Hive on kind quickstart ==="

for tool in kind kubectl; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    hive_test_skip "$tool not installed — cannot run the kind quickstart"
    hive_test_report
    exit $?
  fi
done

cleanup() {
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
  kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

kind create cluster --name "$CLUSTER" --wait 120s
hive_test_pass "kind cluster $CLUSTER created"

kubectl --context "kind-${CLUSTER}" apply -k "$OVERLAY"
hive_test_pass "overlay applied"

if kubectl --context "kind-${CLUSTER}" -n hive rollout status deploy/hive --timeout="$TIMEOUT"; then
  hive_test_pass "deploy/hive rolled out"
else
  hive_test_fail "deploy/hive rollout" "$(kubectl --context "kind-${CLUSTER}" -n hive describe pod -l app.kubernetes.io/name=hive 2>&1 | tail -20)"
fi

kubectl --context "kind-${CLUSTER}" -n hive port-forward svc/hive "${LOCAL_PORT}:3002" >/dev/null 2>&1 &
PF_PID=$!

healthy=0
for _ in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:${LOCAL_PORT}/api/health" >/dev/null 2>&1; then
    healthy=1
    break
  fi
  sleep 2
done
if [ "$healthy" = 1 ]; then
  hive_test_pass "/api/health responded via port-forward"
else
  hive_test_fail "/api/health via port-forward" "no 2xx within 60s"
fi

hive_test_report
