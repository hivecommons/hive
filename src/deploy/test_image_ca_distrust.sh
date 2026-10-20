#!/usr/bin/env bash
# test_image_ca_distrust.sh — prove a built hive image rejects certificate
# chains that end in the roots src/Dockerfile's "Layer 1b" distrusts.
#
# Why a runtime job and not a grep. The Dockerfile edits /etc/ca-certificates.conf
# and regenerates the bundle; a grep on the Dockerfile only proves the text is
# there. This runs the image and makes the clients it ships — curl (system
# bundle), node (NODE_OPTIONS=--use-openssl-ca → system bundle) — connect to
# public test endpoints whose chains terminate in the withdrawn roots. A TLS
# error is the pass condition; an HTTP response is the failure. A control
# fetch against a normally-trusted host proves the bundle was not emptied.
#
# Usage: test_image_ca_distrust.sh --image <ref>
#
# Network is required (the endpoints are public). If the test host itself is
# unreachable (DNS/connect failure rather than a TLS verify failure) the check
# is reported as SKIP, not PASS, so a sandbox without egress cannot go green
# by accident.
set -euo pipefail

IMAGE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --image) IMAGE="$2"; shift 2 ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done
[ -n "$IMAGE" ] || { echo "ERROR: --image <ref> is required" >&2; exit 2; }

# Test endpoints published alongside the root-program notice; each serves a
# chain ending in one of the two distrusted roots.
ENDPOINTS=(
  "https://dv-g3b-emudhra.we-have-a-problem.com/"
  "https://dv-g3b-aaa-emudhra.we-have-a-problem.com/"
)
CONTROL="https://api.github.com/"

PASS=0; FAIL=0; SKIP=0
ok()   { echo "  PASS  $*"; PASS=$((PASS+1)); }
bad()  { echo "  FAIL  $*"; FAIL=$((FAIL+1)); }
skip() { echo "  SKIP  $*"; SKIP=$((SKIP+1)); }

# in_image <cmd...>: run a command in the image as root with no entrypoint.
in_image() {
  docker run --rm --entrypoint sh "$IMAGE" -c "$1"
}

echo "=== CA distrust check: $IMAGE ==="

# curl: exit 60 is "peer certificate cannot be authenticated"; 6/7/28 are
# resolve/connect/timeout, i.e. the network, not the trust store.
for url in "${ENDPOINTS[@]}"; do
  rc=0
  in_image "curl -sS -o /dev/null --max-time 30 '$url'" >/dev/null 2>&1 || rc=$?
  case "$rc" in
    60) ok "curl rejects $url" ;;
    0)  bad "curl TRUSTED $url (HTTP response received; distrusted root still in bundle)" ;;
    6|7|28) skip "curl could not reach $url (rc=$rc)" ;;
    *)  bad "curl unexpected rc=$rc for $url" ;;
  esac
done
rc=0
in_image "curl -sS -o /dev/null --max-time 30 '$CONTROL'" >/dev/null 2>&1 || rc=$?
case "$rc" in
  0) ok "curl control still trusts $CONTROL" ;;
  6|7|28) skip "curl could not reach control $CONTROL (rc=$rc)" ;;
  *) bad "curl control rc=$rc for $CONTROL — bundle may have been emptied" ;;
esac

# node: the image sets NODE_OPTIONS=--use-openssl-ca, so this exercises the
# same system bundle through Node's TLS stack (and therefore the Copilot CLI,
# a Node SEA that honours NODE_OPTIONS). Print a token per outcome.
node_probe='node -e "fetch(process.argv[1]).then(r=>console.log(\"TRUSTED\",r.status)).catch(e=>{const c=e.cause?.code||e.message;console.log(/CERT|SSL|certificate/i.test(c)?\"REJECTED\":\"NETWORK\",c)})" '
if in_image "command -v node" >/dev/null 2>&1; then
  for url in "${ENDPOINTS[@]}"; do
    out="$(in_image "$node_probe '$url'" 2>&1 || true)"
    case "$out" in
      REJECTED*) ok "node rejects $url ($out)" ;;
      TRUSTED*)  bad "node TRUSTED $url ($out)" ;;
      NETWORK*)  skip "node could not reach $url ($out)" ;;
      *)         bad "node unexpected output for $url: $out" ;;
    esac
  done
  out="$(in_image "$node_probe '$CONTROL'" 2>&1 || true)"
  case "$out" in
    TRUSTED*) ok "node control still trusts $CONTROL" ;;
    NETWORK*) skip "node could not reach control $CONTROL ($out)" ;;
    *)        bad "node control: $out — system bundle may have been emptied" ;;
  esac
else
  skip "image has no node"
fi

echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="
[ "$FAIL" -eq 0 ]
