#!/usr/bin/env bash
# Contract tests for src/deploy/dibs-domain-cutover/verify.sh (#5925).
#
# The verifier's whole reason to exist is that every failure mode of the dibs
# cutover still answers HTTP 200 — a self-signed certificate, a certificate
# without the new SAN, an inert redirect, a redirect that drops the path, and a
# dibs that renders every visitor signed out. So a verifier that reports a pass
# when one of those is true is not a weak check, it is an actively harmful one:
# it is the thing someone reads before deciding the cutover is finished and the
# old certificate can be retired.
#
# Every case here EXECUTES the script against stub openssl/curl/dig/kubectl and
# asserts on its exit code and its report. No cluster, no network.
#
# Run: bash bin/test_dibs_cutover_verify.sh
# Exit codes: 0 all cases pass, 1 at least one failed.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT/src/deploy/dibs-domain-cutover/verify.sh"

pass=0
fail=0

ok()  { echo "  ✓ $1"; pass=$((pass + 1)); }
bad() { echo "  ✗ $1"; fail=$((fail + 1)); }

STUB_DIR=""

setup_stubs() {
  STUB_DIR="$ROOT/.dibs-verify-test-stubs"
  rm -rf "$STUB_DIR"
  mkdir -p "$STUB_DIR/bin" "$STUB_DIR/bin-nossl" "$STUB_DIR/core"

  # A curated core so a host binary can never leak into a case. PATH is
  # REPLACED, not prefixed, and `bash` itself is resolved through it.
  local t p
  for t in bash sed grep tr wc awk head sort cut cat env; do
    p="$(command -v "$t" 2>/dev/null)" || continue
    [ -n "$p" ] && ln -sf "$p" "$STUB_DIR/core/$t"
  done

  cat >"$STUB_DIR/bin/curl" <<'STUB'
#!/usr/bin/env bash
# Stub curl: one branch per request the verifier makes, each answered from an
# env var so a case can shape the "cluster" without a network.
args="$*"
case "$args" in
  *"/api/saas/whoami"*)
    printf '%s' "${STUB_WHOAMI_CODE-200}"; exit 0 ;;
  *"dibs.kubestellar.io"*)
    [ "${STUB_LEGACY_CURL_FAIL:-0}" = "0" ] || { echo "curl: (7) Failed to connect" >&2; exit 7; }
    printf '%s' "${STUB_LEGACY_PROBE-308 https://dibs.hivecommons.dev/ideas/42?ref=x}"; exit 0 ;;
  *"dibs.hivecommons.dev"*)
    [ "${STUB_NEW_CURL_FAIL:-0}" = "0" ] || { echo "curl: (60) SSL certificate problem: self-signed certificate" >&2; exit 60; }
    printf '%s' "${STUB_NEW_CODE-200}"; exit 0 ;;
esac
exit 1
STUB

  cat >"$STUB_DIR/bin/openssl" <<'STUB'
#!/usr/bin/env bash
# Stub openssl: s_client is the handshake (its output is only a pipe carrier),
# x509 prints the certificate text the case wants inspected.
case "${1-}" in
  s_client)
    [ "${STUB_TLS_FAIL:-0}" = "0" ] || exit 1
    echo "-----BEGIN CERTIFICATE-----" ;;
  x509)
    [ "${STUB_TLS_FAIL:-0}" = "0" ] || exit 1
    cat >/dev/null
    printf '%s\n' "${STUB_CERT_TEXT-}" ;;
  *) exit 1 ;;
esac
exit 0
STUB

  cat >"$STUB_DIR/bin/dig" <<'STUB'
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

  cat >"$STUB_DIR/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  *"get ingress"*)
    [ "${STUB_KUBECTL_OK:-1}" = "1" ] || exit 1
    printf '%s' "${STUB_INGRESS_LIST-}"; exit 0 ;;
esac
exit 1
STUB

  chmod +x "$STUB_DIR/bin/"*
  # The no-openssl PATH: identical, minus openssl, so the "a check that could
  # not run is not a pass" contract can be exercised.
  for t in curl dig kubectl; do cp "$STUB_DIR/bin/$t" "$STUB_DIR/bin-nossl/$t"; done
  chmod +x "$STUB_DIR/bin-nossl/"*
}

# shellcheck disable=SC2329 # invoked by the EXIT trap below.
teardown_stubs() { [ -n "$STUB_DIR" ] && rm -rf "$STUB_DIR"; }

run_verify() {
  OUT="$(PATH="$STUB_DIR/bin:$STUB_DIR/core" bash "$SCRIPT" 2>&1)"
  RC=$?
}

run_verify_nossl() {
  OUT="$(PATH="$STUB_DIR/bin-nossl:$STUB_DIR/core" bash "$SCRIPT" 2>&1)"
  RC=$?
}

# The finished state: step 6 applied, everything as the plan intends.
healthy_env() {
  export STUB_DIG_A="157.151.252.29"
  export STUB_CERT_TEXT="issuer=C=US, O=Let's Encrypt, CN=R11
subject=CN=hive.hivecommons.dev
X509v3 Subject Alternative Name:
    DNS:*.hive.hivecommons.dev, DNS:hive.hivecommons.dev, DNS:dibs.hivecommons.dev"
  export STUB_NEW_CODE="200"
  export STUB_LEGACY_PROBE="308 https://dibs.hivecommons.dev/ideas/42?ref=x"
  export STUB_INGRESS_LIST="dibs dibs.hivecommons.dev
dibs-kubestellar-redirect dibs.kubestellar.io"
}

clear_env() {
  unset STUB_DIG_A STUB_CERT_TEXT STUB_NEW_CODE STUB_LEGACY_PROBE \
        STUB_INGRESS_LIST STUB_TLS_FAIL STUB_NEW_CURL_FAIL STUB_DIG_FAIL \
        STUB_LEGACY_CURL_FAIL STUB_WHOAMI_CODE STUB_KUBECTL_OK \
        HIVE_HUB_COOKIE HUB_HOST
}

echo "=== dibs cutover verification contract (#5925) ==="
echo

[ -f "$SCRIPT" ] || { echo "  ✗ $SCRIPT not found"; exit 1; }
setup_stubs
trap teardown_stubs EXIT

# ── the positive control ───────────────────────────────────────────────────
# Without this every assertion below could pass because the script always
# fails, which would make the whole suite vacuous.
echo "positive control"
clear_env; healthy_env
run_verify
if [ "$RC" -eq 0 ]; then
  ok "a completed cutover exits 0"
else
  bad "completed cutover exited $RC, want 0:\n$OUT"
fi
case "$OUT" in
  *"fail=0"*) ok "completed cutover reports fail=0" ;;
  *) bad "completed cutover did not report fail=0:\n$OUT" ;;
esac
case "$OUT" in
  *"warn=0"*) ok "completed cutover reports warn=0 — no check silently skipped" ;;
  *) bad "completed cutover carries warnings:\n$OUT" ;;
esac

# ── check 1: DNS ───────────────────────────────────────────────────────────
echo
echo "check 1: DNS"
clear_env; healthy_env
export STUB_DIG_A=""
run_verify
if [ "$RC" -eq 78 ]; then
  ok "an unresolvable new host blocks (exit 78)"
else
  bad "unresolvable host exited $RC, want 78:\n$OUT"
fi

clear_env; healthy_env
export STUB_DIG_A="203.0.113.9"
run_verify
if [ "$RC" -eq 78 ]; then
  ok "a new host pointing at the WRONG address blocks"
else
  bad "wrong A record exited $RC, want 78:\n$OUT"
fi

# A resolver that could not answer is not a wrong record. Measured 2026-09-04
# on a host with no local resolver, this check reported "resolves to ';;
# communications error ...', expected 157.151.252.29" — a hard FAIL naming the
# one conclusion it had no evidence for. #5925 records that the A record
# ALREADY EXISTS and says to confirm it "rather than re-create", so this
# message sends an operator to undo a step that is already done.
clear_env; healthy_env
export STUB_DIG_FAIL=9
run_verify
case "$OUT" in
  *"resolver could not answer"*) ok "a resolver that could not answer is reported as unchecked" ;;
  *) bad "resolver failure not reported as unchecked:\n$OUT" ;;
esac
case "$OUT" in
  *"communications error"*) bad "dig's stdout diagnostics were read as an address:\n$OUT" ;;
  *) ok "dig's stdout diagnostics never reach the address comparison" ;;
esac
case "$OUT" in
  *"expected 157.151.252.29"*) bad "a resolver failure was reported as a WRONG record:\n$OUT" ;;
  *) ok "a resolver failure is never reported as a wrong record" ;;
esac
if [ "$RC" -eq 0 ]; then
  ok "a resolver failure warns rather than blocking an otherwise-complete cutover"
else
  bad "resolver failure exited $RC, want 0 (everything else healthy):\n$OUT"
fi

# The other half of the contract: a resolver that DID answer, with no record,
# is still a genuine finding. The guard above must not swallow it.
clear_env; healthy_env
export STUB_DIG_A=""
run_verify
case "$OUT" in
  *"has no A record"*) ok "an answered lookup with no A record is still a failure" ;;
  *) bad "NXDOMAIN no longer reported as a missing record:\n$OUT" ;;
esac

# ── check 2: the served certificate ────────────────────────────────────────
# The failure the issue's review flagged as only discoverable after the
# issuance is spent: a host that answers 200 under ingress-nginx's self-signed
# default certificate.
echo
echo "check 2: the served certificate"
clear_env; healthy_env
export STUB_CERT_TEXT="issuer=O=Acme Co, CN=Kubernetes Ingress Controller Fake Certificate
subject=O=Acme Co, CN=Kubernetes Ingress Controller Fake Certificate
X509v3 Subject Alternative Name:
    DNS:ingress.local"
run_verify
if [ "$RC" -eq 78 ]; then
  ok "ingress-nginx's fake certificate blocks (exit 78)"
else
  bad "fake certificate exited $RC, want 78:\n$OUT"
fi
case "$OUT" in
  *"SELF-SIGNED"*) ok "names the actual failure (self-signed certificate)" ;;
  *) bad "does not name the self-signed certificate:\n$OUT" ;;
esac
case "$OUT" in
  *"default-ssl-certificate"*) ok "points at the setting that causes it" ;;
  *) bad "does not point at --default-ssl-certificate:\n$OUT" ;;
esac

# A real certificate that simply does not carry the new name. Every other
# check passes; only the SAN list distinguishes it.
clear_env; healthy_env
export STUB_CERT_TEXT="issuer=C=US, O=Let's Encrypt, CN=R11
subject=CN=hive.hivecommons.dev
X509v3 Subject Alternative Name:
    DNS:*.hive.hivecommons.dev, DNS:hive.hivecommons.dev"
run_verify
if [ "$RC" -eq 78 ]; then
  ok "a real certificate MISSING the new host in its SANs blocks"
else
  bad "missing SAN exited $RC, want 78:\n$OUT"
fi
case "$OUT" in
  *"SANs do NOT include"*) ok "says which fact failed (the SAN list)" ;;
  *) bad "does not name the SAN list:\n$OUT" ;;
esac

# An arbitrary self-signed certificate, not ingress-nginx's: issuer equals
# subject. The fake-issuer string match must not be the only defence.
clear_env; healthy_env
export STUB_CERT_TEXT="issuer=CN=dibs.hivecommons.dev
subject=CN=dibs.hivecommons.dev
X509v3 Subject Alternative Name:
    DNS:dibs.hivecommons.dev"
run_verify
if [ "$RC" -eq 78 ]; then
  ok "any self-signed certificate blocks, not only the ingress-nginx one"
else
  bad "issuer==subject exited $RC, want 78:\n$OUT"
fi

# ── check 3: the new host serves ───────────────────────────────────────────
echo
echo "check 3: the new host serves"
clear_env; healthy_env
export STUB_NEW_CURL_FAIL=1
run_verify
if [ "$RC" -eq 78 ]; then
  ok "a chain curl refuses to validate blocks"
else
  bad "invalid chain exited $RC, want 78:\n$OUT"
fi

clear_env; healthy_env
export STUB_NEW_CODE="503"
run_verify
if [ "$RC" -eq 78 ]; then
  ok "a non-200 from the canonical host blocks"
else
  bad "503 exited $RC, want 78:\n$OUT"
fi

# The Ingresses applied the wrong way round: the canonical host redirects.
clear_env; healthy_env
export STUB_NEW_CODE="308"
run_verify
if [ "$RC" -eq 78 ]; then
  ok "a canonical host that REDIRECTS blocks (Ingresses reversed)"
else
  bad "redirecting canonical host exited $RC, want 78:\n$OUT"
fi

# ── check 4: the session cookie ────────────────────────────────────────────
# The reason this issue is a repair and not a rename. If the hub and dibs do
# not share a registrable domain, dibs renders every visitor signed out and
# nothing else in this report would notice.
echo
echo "check 4: the hub session cookie reaches dibs"
clear_env; healthy_env
export HUB_HOST="hive.kubestellar.io"
run_verify
if [ "$RC" -eq 78 ]; then
  ok "a hub on a DIFFERENT registrable domain blocks (dibs would be signed out)"
else
  bad "cross-domain hub exited $RC, want 78:\n$OUT"
fi
case "$OUT" in
  *"signed out"*) ok "names the symptom an operator would actually see" ;;
  *) bad "does not name the signed-out symptom:\n$OUT" ;;
esac

clear_env; healthy_env
export HIVE_HUB_COOKIE="s3cr3t-session-value"
export STUB_WHOAMI_CODE="401"
run_verify
if [ "$RC" -eq 78 ]; then
  ok "a session the hub REJECTS blocks, even with the cookie in scope"
else
  bad "rejected session exited $RC, want 78:\n$OUT"
fi
case "$OUT" in
  *s3cr3t*) bad "the supplied session value was echoed into the report" ;;
  *) ok "the supplied session value is never echoed" ;;
esac

clear_env; healthy_env
export HIVE_HUB_COOKIE="s3cr3t-session-value"
export STUB_WHOAMI_CODE="200"
run_verify
if [ "$RC" -eq 0 ]; then
  ok "a session the hub accepts passes"
else
  bad "accepted session exited $RC, want 0:\n$OUT"
fi

# Without a cookie the script must still say what it has NOT proven — the
# browser test is the only thing that exercises dibs's own forwarding code.
clear_env; healthy_env
run_verify
case "$OUT" in
  *"another repo"*) ok "discloses that dibs's own forwarding code is untested here" ;;
  *) bad "claims more than it verified:\n$OUT" ;;
esac

# ── check 5: the redirect ──────────────────────────────────────────────────
echo
echo "check 5: the legacy redirect"
# The dual-host window. Expected between steps 4 and 6 — so NOT a failure —
# but it must never read as a finished cutover either.
clear_env; healthy_env
export STUB_LEGACY_PROBE="200"
export STUB_INGRESS_LIST="dibs dibs.kubestellar.io dibs.hivecommons.dev"
run_verify
if [ "$RC" -eq 0 ]; then
  ok "an open dual-host window is not a failure"
else
  bad "dual-host window exited $RC, want 0:\n$OUT"
fi

# Measured on the live host 2026-09-04: dibs answers 200 on / and 401 on an
# authenticated deep path. Both mean the same thing here — no redirect is
# happening — so keying the window on 200 alone would report a false failure
# for the entire duration of the window on the default deep path.
clear_env; healthy_env
export STUB_LEGACY_PROBE="401"
export STUB_INGRESS_LIST="dibs dibs.kubestellar.io dibs.hivecommons.dev"
run_verify
if [ "$RC" -eq 0 ]; then
  ok "an application 401 on the deep path is the window, not a failed redirect"
else
  bad "legacy 401 exited $RC, want 0:\n$OUT"
fi
case "$OUT" in
  *"HTTP 401"*) ok "reports the status the application actually answered" ;;
  *) bad "does not report the observed status:\n$OUT" ;;
esac

clear_env; healthy_env
export STUB_LEGACY_PROBE="502"
export STUB_INGRESS_LIST="dibs dibs.kubestellar.io dibs.hivecommons.dev"
run_verify
case "$OUT" in
  *"server error"*) ok "a 5xx on the legacy host is called out separately" ;;
  *) bad "5xx not distinguished from an ordinary window status:\n$OUT" ;;
esac
case "$OUT" in
  *"warn=0"*) bad "dual-host window reported warn=0, which reads as finished:\n$OUT" ;;
  *) ok "dual-host window carries warnings rather than a clean report" ;;
esac
case "$OUT" in
  *"not a resting state"*) ok "says the window must be closed, not parked in" ;;
  *) bad "does not warn that the window is not a resting state:\n$OUT" ;;
esac

# The silent-pass the issue's review called out: a redirect that answers on the
# root and drops everything after it.
clear_env; healthy_env
export STUB_LEGACY_PROBE="308 https://dibs.hivecommons.dev/"
run_verify
if [ "$RC" -eq 78 ]; then
  ok "a redirect that DROPS the path and query blocks"
else
  bad "path-dropping redirect exited $RC, want 78:\n$OUT"
fi
case "$OUT" in
  *"request_uri"*) ok "points at \$request_uri, the annotation that carries the path" ;;
  *) bad "does not point at \$request_uri:\n$OUT" ;;
esac

clear_env; healthy_env
export STUB_LEGACY_PROBE="301 https://dibs.hivecommons.dev/ideas/42?ref=x"
run_verify
if [ "$RC" -eq 78 ]; then
  ok "a 301 blocks — the staged annotation sets 308, so something else answered"
else
  bad "301 exited $RC, want 78:\n$OUT"
fi

clear_env; healthy_env
export STUB_LEGACY_PROBE="308 https://example.invalid/ideas/42?ref=x"
run_verify
if [ "$RC" -eq 78 ]; then
  ok "a 308 to the wrong destination blocks"
else
  bad "wrong Location exited $RC, want 78:\n$OUT"
fi

# ── check 6: Ingress ownership ─────────────────────────────────────────────
# ingress-nginx honours the OLDER object on a duplicate host+path claim and
# only logs a warning, so this collision makes the redirect inert while every
# object looks applied.
echo
echo "check 6: sole ownership of the legacy host"
clear_env; healthy_env
export STUB_INGRESS_LIST="dibs dibs.kubestellar.io dibs.hivecommons.dev
dibs-kubestellar-redirect dibs.kubestellar.io"
run_verify
if [ "$RC" -eq 78 ]; then
  ok "two Ingresses claiming the legacy host block"
else
  bad "host collision exited $RC, want 78:\n$OUT"
fi
case "$OUT" in
  *OLDEST*) ok "explains that ingress-nginx honours the oldest claim" ;;
  *) bad "does not explain the collision resolution:\n$OUT" ;;
esac

clear_env; healthy_env
export STUB_INGRESS_LIST="dibs dibs.kubestellar.io dibs.hivecommons.dev"
export STUB_LEGACY_PROBE="200"
run_verify
case "$OUT" in
  *"step 6 has not been applied"*) ok "the app Ingress still owning the host is reported, not passed" ;;
  *) bad "pre-step-6 ownership not reported:\n$OUT" ;;
esac

# ── the skip contract ──────────────────────────────────────────────────────
# The property that makes the report safe to act on: a check that could not run
# must never read as a passed one. This is the whole difference between "the
# cutover is verified" and "nothing contradicted me".
echo
echo "skip is not pass"
clear_env; healthy_env
run_verify_nossl
case "$OUT" in
  *"openssl is not available"*) ok "a missing openssl says the certificate was NOT inspected" ;;
  *) bad "missing openssl did not disclaim the certificate check:\n$OUT" ;;
esac
case "$OUT" in
  *"warn=0"*) bad "missing openssl reported warn=0, which reads as verified:\n$OUT" ;;
  *) ok "missing openssl carries a warning rather than a clean report" ;;
esac

clear_env; healthy_env
export STUB_TLS_FAIL=1
run_verify
case "$OUT" in
  *"could not complete a TLS handshake"*) ok "a failed handshake is reported, not passed over" ;;
  *) bad "failed handshake not reported:\n$OUT" ;;
esac

clear_env; healthy_env
export STUB_KUBECTL_OK=0
run_verify
case "$OUT" in
  *"warn=0"*) bad "an unreadable namespace reported warn=0:\n$OUT" ;;
  *) ok "an unreadable namespace carries a warning" ;;
esac

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ] || exit 1
exit 0
