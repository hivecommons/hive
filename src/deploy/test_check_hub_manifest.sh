#!/usr/bin/env bash
# Tests check-hub-manifest.py (#19344, #19345): the hub Deployment must have
# exactly the expected containers and no literal secret env values.
# Run: bash src/deploy/test_check_hub_manifest.sh
set -uo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHK="$DIR/check-hub-manifest.py"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
PASS=0; FAIL=0
if ! python3 -c 'import yaml' >/dev/null 2>&1; then echo "SKIP: PyYAML missing"; exit 0; fi

expect() { # name want_rc file [grep]
  local out rc
  out="$(python3 "$CHK" "$3" 2>&1)"; rc=$?
  if [ "$rc" = "$2" ] && { [ -z "${4:-}" ] || grep -q "$4" <<<"$out"; }; then
    echo "  PASS: $1"; PASS=$((PASS+1))
  else
    echo "  FAIL: $1 (rc=$rc) $out"; FAIL=$((FAIL+1))
  fi
}

mk() { # file extra_container extra_env
  cat > "$1" <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: hive-hub}
spec:
  template:
    spec:
      containers:
      - name: hub
        env:
        - name: HIVE_MODE
          value: hub
        - name: HIVE_HUB_OAUTH_CLIENT_SECRET
          valueFrom: {secretKeyRef: {name: hive-hub-secrets, key: oauth}}
$3
$2
YAML
}

mk "$TMP/good.yaml" "" ""
expect "expected set passes" 0 "$TMP/good.yaml"

mk "$TMP/extra.yaml" "      - name: hive-hub
        image: old" ""
expect "stray hive-hub container fails" 1 "$TMP/extra.yaml" "unexpected container 'hive-hub'"

mk "$TMP/lit1.yaml" "" "        - {name: HIVE_NPS_RELAY_PULL_SECRET, value: x}"
expect "literal NPS pull secret fails" 1 "$TMP/lit1.yaml" "HIVE_NPS_RELAY_PULL_SECRET"

sed 's/valueFrom.*/value: x/' "$TMP/good.yaml" > "$TMP/lit2.yaml"
expect "literal OAuth secret fails" 1 "$TMP/lit2.yaml" "HIVE_HUB_OAUTH_CLIENT_SECRET"

sed 's/secretKeyRef: {name: hive-hub-secrets, key: oauth}/configMapKeyRef: {name: cm, key: oauth}/' "$TMP/good.yaml" > "$TMP/cm.yaml"
expect "configMapKeyRef for a secret fails" 1 "$TMP/cm.yaml" "without valueFrom.secretKeyRef"

mk "$TMP/nosrc.yaml" "" "        - name: HIVE_NPS_RELAY_PULL_SECRET"
expect "secret env with no source fails" 1 "$TMP/nosrc.yaml" "HIVE_NPS_RELAY_PULL_SECRET"

cat > "$TMP/init.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: {name: hive-hub}
spec:
  template:
    spec:
      initContainers:
      - name: init
        env: [{name: HIVE_HUB_OAUTH_CLIENT_SECRET, value: x}]
      containers: [{name: hub}]
YAML
expect "literal secret in initContainer fails" 1 "$TMP/init.yaml" "'init'"

cat > "$TMP/list.yaml" <<'YAML'
apiVersion: v1
kind: List
items:
- apiVersion: apps/v1
  kind: Deployment
  metadata: {name: hive-hub}
  spec: {template: {spec: {containers: [{name: hub}, {name: hive-hub}]}}}
YAML
expect "stray container inside a List fails" 1 "$TMP/list.yaml" "unexpected container"

cat > "$TMP/other.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: {name: legacy-redirect}
spec: {template: {spec: {containers: [{name: whatever}]}}}
YAML
expect "other deployments ignored" 0 "$TMP/other.yaml"

echo "passed=$PASS failed=$FAIL"
[ "$FAIL" = 0 ]
