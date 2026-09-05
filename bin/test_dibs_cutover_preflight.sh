#!/usr/bin/env bash
# Contract tests for src/deploy/dibs-domain-cutover/preflight.sh (#5925).
#
# The preflight exists because the dibs cutover is scheduled around a Let's
# Encrypt quota window: a step that fails AFTER issuance costs a week. That
# makes the preflight's own correctness load-bearing — a check that silently
# passes when it should fail is worse than no check, because it is the thing
# someone will trust before spending the window.
#
# Every case here EXECUTES the script against stub kubectl/dig/curl, and asserts
# on its exit code and its report. No cluster, no network.
#
# Run: bash bin/test_dibs_cutover_preflight.sh
# Exit codes: 0 all cases pass, 1 at least one failed.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT/src/deploy/dibs-domain-cutover/preflight.sh"

pass=0
fail=0

ok()   { echo "  ✓ $1"; pass=$((pass + 1)); }
bad()  { echo "  ✗ $1"; fail=$((fail + 1)); }

# Fixture knobs the stub kubectl reads from the environment. Each case sets the
# ones it cares about and inherits sane defaults.
STUB_DIR=""

setup_stubs() {
  STUB_DIR="$ROOT/.dibs-preflight-test-stubs"
  rm -rf "$STUB_DIR"
  mkdir -p "$STUB_DIR"
  cat >"$STUB_DIR/kubectl" <<'STUB'
#!/usr/bin/env bash
# Stub kubectl: answers only the read-only queries the preflight makes.
# Every answer comes from an env var so a case can shape the "cluster".
args="$*"
case "$args" in
  *"get namespace"*)
    [ "${STUB_NS_OK:-1}" = "1" ] || exit 1
    echo "namespace/dibs"; exit 0 ;;
  *"get deployment"*)
    [ -n "${STUB_CONTROLLER_ARGS+x}" ] || exit 1
    printf '%s' "$STUB_CONTROLLER_ARGS"; exit 0 ;;
  *"get certificate"*)
    [ -n "${STUB_CERT_SANS+x}" ] || exit 1
    printf '%s' "$STUB_CERT_SANS"; exit 0 ;;
  *"get ingress"*"ingressClassName"*)
    printf '%s' "${STUB_INGRESS_CLASS-nginx}"; exit 0 ;;
  *"get ingress"*"secretName"*)
    printf '%s' "${STUB_INGRESS_SECRET-hive-tls-hc}"; exit 0 ;;
  *"get ingress"*"annotations"*)
    printf '%s' "${STUB_INGRESS_ANNOS-}"; exit 0 ;;
  *"get ingress"*"items"*)
    printf '%s' "${STUB_INGRESS_LIST-$'dibs dibs.kubestellar.io,\n'}"; exit 0 ;;
  *) exit 1 ;;
esac
STUB
  cat >"$STUB_DIR/dig" <<'STUB'
#!/usr/bin/env bash
# A resolver that could not be reached is NOT an empty answer. dig writes its
# ";; communications error ..." diagnostics to STDOUT and reports the failure
# through its EXIT STATUS (9). The stub could not express that shape before, so
# nothing tested the direction where the script reads a broken resolver as a
# wrong A record.
if [ "${STUB_DIG_FAIL:-0}" != "0" ]; then
  echo ";; communications error to 127.0.0.53#53: connection refused"
  echo ";; no servers could be reached"
  exit "${STUB_DIG_FAIL}"
fi
printf '%s' "${STUB_DIG_A-}"
[ -n "${STUB_DIG_A-}" ] && echo
exit 0
STUB
  cat >"$STUB_DIR/curl" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  *crt.sh*)
    [ "${STUB_CRT_FAIL:-0}" = "0" ] || exit 22
    printf '%s' "${STUB_CRT_JSON:-[]}"
    exit 0 ;;
  *) exit 1 ;;
esac
STUB
  chmod +x "$STUB_DIR/kubectl" "$STUB_DIR/dig" "$STUB_DIR/curl"
}

# shellcheck disable=SC2329 # invoked by the EXIT trap below.
teardown_stubs() { [ -n "$STUB_DIR" ] && rm -rf "$STUB_DIR"; }

# run_preflight prints the report and sets RC. PATH is replaced, not prefixed,
# except for the coreutils the script needs — so a real kubectl on the host can
# never leak into a case.
run_preflight() {
  OUT="$(PATH="$STUB_DIR:/usr/bin:/bin" bash "$SCRIPT" 2>&1)"
  RC=$?
}

# A healthy cluster: every assumption the staged manifests make holds.
healthy_env() {
  export STUB_NS_OK=1
  export STUB_CONTROLLER_ARGS="/nginx-ingress-controller --publish-service=ingress-nginx/ingress-nginx-controller --default-ssl-certificate=hive-hub/hive-wildcard-tls"
  export STUB_CERT_SANS="*.hive.hivecommons.dev *.lke.hive.hivecommons.dev hive.hivecommons.dev"
  export STUB_INGRESS_CLASS="nginx"
  export STUB_INGRESS_SECRET="hive-tls-hc"
  export STUB_INGRESS_ANNOS=""
  export STUB_INGRESS_LIST="dibs dibs.kubestellar.io,"
  export STUB_DIG_A="157.151.252.29"
  # Genuine headroom: crt.sh DID observe the domain, and the certificates it
  # returned are older than the window. This used to be "[]", which is the
  # response crt.sh gives when it cannot service the query at all — a baseline
  # that encoded the defect below as the healthy state, so nothing could catch
  # it.
  export STUB_CRT_JSON='[
  {"issuer_name":"C=US, O=Let'"'"'s Encrypt, CN=R13","common_name":"hive.hivecommons.dev","name_value":"hive.hivecommons.dev","not_before":"2026-08-01T10:00:00"}
]'
  export CRT_SH_NOW_UTC=2026-09-04T15:00:00Z
}

clear_env() {
  unset STUB_NS_OK STUB_CONTROLLER_ARGS STUB_CERT_SANS STUB_INGRESS_CLASS \
        STUB_INGRESS_SECRET STUB_INGRESS_ANNOS STUB_INGRESS_LIST STUB_DIG_A \
        STUB_CRT_JSON STUB_CRT_FAIL STUB_DIG_FAIL LE_CERT_LIMIT CRT_SH_NOW_UTC
}

echo "=== dibs cutover preflight contract (#5925) ==="
echo

[ -f "$SCRIPT" ] || { echo "  ✗ $SCRIPT not found"; exit 1; }
setup_stubs
trap teardown_stubs EXIT

# ── the positive control ───────────────────────────────────────────────────
# Without this, every assertion below could pass because the script always
# fails, which would make the whole suite vacuous.
echo "positive control"
clear_env; healthy_env
run_preflight
if [ "$RC" -eq 0 ]; then
  ok "a cluster meeting every assumption exits 0"
else
  bad "healthy cluster exited $RC, want 0:\n$OUT"
fi
case "$OUT" in
  *"fail=0"*) ok "healthy cluster reports fail=0" ;;
  *) bad "healthy cluster did not report fail=0:\n$OUT" ;;
esac

# ── check 1: the default SSL certificate ───────────────────────────────────
# This is the assumption the whole plan rests on and the one the staged README
# only hedges as "should". If it does not hold, the new host is served
# ingress-nginx's self-signed cert AFTER the issuance is spent.
echo
echo "check 1: --default-ssl-certificate"
clear_env; healthy_env
export STUB_CONTROLLER_ARGS="/nginx-ingress-controller --publish-service=ingress-nginx/ingress-nginx-controller"
run_preflight
if [ "$RC" -eq 78 ]; then
  ok "a controller with NO --default-ssl-certificate blocks (exit 78)"
else
  bad "missing --default-ssl-certificate exited $RC, want 78:\n$OUT"
fi
case "$OUT" in
  *"self-signed"*) ok "names the actual consequence (self-signed certificate)" ;;
  *) bad "does not explain the consequence:\n$OUT" ;;
esac

clear_env; healthy_env
export STUB_CONTROLLER_ARGS="/nginx-ingress-controller --default-ssl-certificate=other-ns/other-cert"
run_preflight
if [ "$RC" -eq 78 ]; then
  ok "a controller pointing at a DIFFERENT default certificate blocks"
else
  bad "wrong --default-ssl-certificate exited $RC, want 78:\n$OUT"
fi

# ── check 2: the Certificate the patch targets ─────────────────────────────
echo
echo "check 2: Certificate SANs"
clear_env; healthy_env
export STUB_CERT_SANS="hive.hivecommons.dev dibs.hivecommons.dev"
run_preflight
case "$OUT" in
  *"ALREADY a SAN"*) ok "an already-present SAN warns instead of inviting a redundant re-issue" ;;
  *) bad "present SAN not reported:\n$OUT" ;;
esac
if [ "$RC" -eq 0 ]; then
  ok "an already-present SAN is a warning, not a block"
else
  bad "present SAN exited $RC, want 0:\n$OUT"
fi

# ── check 3: drift that `kubectl apply` would silently overwrite ───────────
# 02-*.yaml is a full object, so any live annotation it omits is dropped.
echo
echo "check 3: live Ingress drift"
clear_env; healthy_env
export STUB_INGRESS_ANNOS='map[nginx.ingress.kubernetes.io/proxy-body-size:50m]'
run_preflight
case "$OUT" in
  *"would DROP them"*) ok "live annotations are reported as at risk from a full-object apply" ;;
  *) bad "annotation drop hazard not reported:\n$OUT" ;;
esac
case "$OUT" in
  *"proxy-body-size:50m"*) ok "the report names the annotations that would be lost" ;;
  *) bad "annotations not echoed back:\n$OUT" ;;
esac

clear_env; healthy_env
export STUB_INGRESS_SECRET="some-other-secret"
run_preflight
if [ "$RC" -eq 78 ]; then
  ok "a drifted TLS secret blocks"
else
  bad "drifted TLS secret exited $RC, want 78:\n$OUT"
fi

# ── check 4: the collision that makes the redirect silently inert ──────────
# ingress-nginx keeps the OLDER Ingress on a duplicate host+path and only logs
# a warning, so this failure mode is invisible in the cluster.
echo
echo "check 4: host collisions"
clear_env; healthy_env
export STUB_INGRESS_LIST="dibs dibs.kubestellar.io,
dibs-kubestellar-redirect dibs.kubestellar.io,"
run_preflight
if [ "$RC" -eq 78 ]; then
  ok "two Ingresses claiming the legacy host blocks (the redirect would be inert)"
else
  bad "host collision exited $RC, want 78:\n$OUT"
fi
case "$OUT" in
  *"OLDEST"*) ok "explains that ingress-nginx keeps the oldest claim" ;;
  *) bad "collision message does not explain the resolution rule:\n$OUT" ;;
esac

# The expected post-step-6 shape — app Ingress on the new host only, redirect
# Ingress on the legacy host — must NOT be reported as a collision.
clear_env; healthy_env
export STUB_INGRESS_LIST="dibs dibs.hivecommons.dev,
dibs-kubestellar-redirect dibs.kubestellar.io,"
run_preflight
case "$OUT" in
  *"MORE THAN ONE"*) bad "the intended end state was reported as a collision:\n$OUT" ;;
  *) ok "the intended end state (one host each) is not a collision" ;;
esac

# ── check 5: DNS ───────────────────────────────────────────────────────────
echo
echo "check 5: DNS"
clear_env; healthy_env
export STUB_DIG_A=""
run_preflight
case "$OUT" in
  *"does not resolve yet"*) ok "an absent A record is reported as step 1 not done" ;;
  *) bad "missing DNS not reported:\n$OUT" ;;
esac
if [ "$RC" -eq 0 ]; then
  ok "an absent A record warns rather than blocks (step 1 is simply pending)"
else
  bad "missing DNS exited $RC, want 0:\n$OUT"
fi

clear_env; healthy_env
export STUB_DIG_A="203.0.113.9"
run_preflight
if [ "$RC" -eq 78 ]; then
  ok "an A record pointing somewhere unexpected blocks"
else
  bad "wrong A record exited $RC, want 78:\n$OUT"
fi

# ── check 6: Let's Encrypt headroom via crt.sh ─────────────────────────────
echo
echo "check 6: Let's Encrypt headroom"
clear_env; healthy_env
export STUB_CRT_JSON='[
  {"issuer_name":"C=US, O=Let'\''s Encrypt, CN=R13","common_name":"dibs.hivecommons.dev","name_value":"dibs.hivecommons.dev","not_before":"2026-09-04T14:00:00"},
  {"issuer_name":"C=US, O=Let'\''s Encrypt, CN=R13","common_name":"hive.hivecommons.dev","name_value":"hive.hivecommons.dev","not_before":"2026-09-04T13:00:00"}
]'
export LE_CERT_LIMIT=2
export CRT_SH_NOW_UTC=2026-09-04T15:00:00Z
run_preflight
if [ "$RC" -eq 78 ]; then
  ok "an exhausted Let's Encrypt registered-domain window blocks"
else
  bad "exhausted LE window exited $RC, want 78:\n$OUT"
fi
case "$OUT" in
  *"2 Let's Encrypt certificate(s)"*) ok "reports the crt.sh count used for the decision" ;;
  *) bad "LE count not reported:\n$OUT" ;;
esac

clear_env; healthy_env
export STUB_CRT_FAIL=1
run_preflight
case "$OUT" in
  *"could not query crt.sh"*) ok "crt.sh unavailability warns instead of pretending there is headroom" ;;
  *) bad "crt.sh failure not reported as a warning:\n$OUT" ;;
esac

# The positive control for the guard below: rows WERE returned, none of them in
# the window. That is a real zero and must still pass, or the guard is simply
# refusing to ever report headroom.
clear_env; healthy_env
export LE_CERT_LIMIT=50
run_preflight
case "$OUT" in
  *"0 Let's Encrypt certificate(s)"*"headroom 50"*) ok "certificates outside the window are real headroom and still pass" ;;
  *) bad "genuine headroom not reported:\n$OUT" ;;
esac

# The defect. crt.sh answers a query it cannot service with HTTP 200 and a body
# of "[]" — measured 2026-09-04, six consecutive times for hivecommons.dev,
# while that domain has a live Let's Encrypt wildcard and ~50 certificates
# minted the previous afternoon. Every guard in the script passed and the parse
# yielded 0, so this reported "✓ 0 certificate(s) ... headroom 50": a confident
# PASS on the one gate protecting the irreversible, quota-spending step, and a
# 429 there costs TWO slots against the weekly cap, not one.
clear_env; healthy_env
export STUB_CRT_JSON="[]"
run_preflight
# Matched on the PASS line's shape ("; headroom N"), not the bare word: the
# warning this must produce says "NOT headroom" and would match a looser test.
case "$OUT" in
  *"; headroom "*) bad "an empty crt.sh answer was reported as quota headroom:\n$OUT" ;;
  *) ok "an empty crt.sh answer is never reported as headroom" ;;
esac
case "$OUT" in
  *"NO certificates at all"*) ok "an empty crt.sh answer is reported as an unanswered query" ;;
  *) bad "empty crt.sh answer not called out:\n$OUT" ;;
esac
case "$OUT" in
  *"✓ 0 Let's Encrypt"*) bad "an empty crt.sh answer still rendered as a PASS:\n$OUT" ;;
  *) ok "an empty crt.sh answer does not render as a pass" ;;
esac

# ── check 5 again: a resolver that could not answer ─────────────────────────
# Sharper here than in verify.sh: this check's empty-answer verdict tells the
# operator the A record "has not been done", which under a resolver outage is
# an instruction to create a record #5925 has already confirmed exists.
echo
echo "check 5: a resolver that could not answer"
clear_env; healthy_env
export STUB_DIG_FAIL=9
run_preflight
case "$OUT" in
  *"resolver could not answer"*) ok "a resolver that could not answer is reported as unchecked" ;;
  *) bad "resolver failure not reported as unchecked:\n$OUT" ;;
esac
case "$OUT" in
  *"has not been done"*) bad "a resolver failure told the operator to create an existing record:\n$OUT" ;;
  *) ok "a resolver failure never claims step 1 is undone" ;;
esac
case "$OUT" in
  *"expected 157.151.252.29"*) bad "a resolver failure was reported as a WRONG record:\n$OUT" ;;
  *) ok "a resolver failure is never reported as a wrong record" ;;
esac
if [ "$RC" -eq 0 ]; then
  ok "a resolver failure warns rather than blocking"
else
  bad "resolver failure exited $RC, want 0:\n$OUT"
fi

# ── the skip contract ──────────────────────────────────────────────────────
# A check that could not run must never read as a passed one. This is the
# property that makes the script safe to trust before spending the window.
echo
echo "skip is not pass"
clear_env; healthy_env
export STUB_NS_OK=0
run_preflight
case "$OUT" in
  *"not the same as passing"*) ok "an unreachable cluster says so explicitly" ;;
  *) bad "unreachable cluster did not disclaim its own silence:\n$OUT" ;;
esac
case "$OUT" in
  *"warn=0"*) bad "unreachable cluster reported warn=0, which reads as clean:\n$OUT" ;;
  *) ok "unreachable cluster carries warnings rather than a clean report" ;;
esac

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ] || exit 1
exit 0
