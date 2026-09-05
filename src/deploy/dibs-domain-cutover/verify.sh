#!/usr/bin/env bash
# dibs.hivecommons.dev cutover verification (#5925).
#
# preflight.sh answers "is it safe to spend the Let's Encrypt window?".
# This script answers the question that comes after: "did the cutover actually
# work?" — and it exists because, for this particular sequence, the obvious
# answer is wrong three separate times. Every failure mode the issue's review
# turned up still returns HTTP 200:
#
#   * The new host served ingress-nginx's built-in SELF-SIGNED certificate,
#     because --default-ssl-certificate was not hive-hub/hive-wildcard-tls.
#     It answers 200. It fails certificate validation in a browser.
#   * The new host served a real certificate that does not carry
#     dibs.hivecommons.dev in its SANs. Also a 200 from a warm cache or a
#     lenient client.
#   * dibs rendering every visitor as SIGNED OUT — which is the broken state
#     this whole cutover exists to repair, and which looks completely healthy
#     from the outside. A signed-out dibs is a 200.
#
# And after the redirect lands, two more:
#
#   * The redirect Ingress is INERT because the application Ingress still
#     claims dibs.kubestellar.io. ingress-nginx resolves a duplicate host+path
#     claim in favour of the OLDER object and only logs a warning, so the
#     legacy host keeps answering 200 and every object looks applied.
#   * The redirect drops the path and query. `curl -sI https://legacy` on the
#     bare root passes; every deep link into dibs is silently broken.
#
# So this script never accepts a status code as evidence on its own. It reads
# the issuer and the SANs off the served certificate, it computes whether the
# hub session cookie's Domain can even reach the new host, and it verifies the
# redirect on a path WITH a query string.
#
# Everything here is read-only: DNS lookup, HTTPS GETs, an openssl handshake,
# and `kubectl get`. Nothing is applied, nothing is issued.
#
# It is meant to be run TWICE:
#
#   after step 4 — the dual-host window. Checks 1-5 must pass. Check 6 will
#                  report the window as OPEN (a warning, not a pass): the
#                  legacy host is knowingly serving a signed-out experience
#                  for as long as it lasts, and only step 6 ends it.
#   after step 6 — the finished state. Every check should pass.
#
# Run: bash src/deploy/dibs-domain-cutover/verify.sh
# Exit codes: 0 no failing check, 78 at least one failing check (EX_CONFIG).

set -uo pipefail

# Overridable so the same script verifies a rehearsal namespace, a renamed
# object, or the reverse cutover, without being edited.
DIBS_NAMESPACE="${DIBS_NAMESPACE:-dibs}"
DIBS_INGRESS="${DIBS_INGRESS:-dibs}"
DIBS_REDIRECT_INGRESS="${DIBS_REDIRECT_INGRESS:-dibs-kubestellar-redirect}"
DIBS_LEGACY_HOST="${DIBS_LEGACY_HOST:-dibs.kubestellar.io}"
DIBS_NEW_HOST="${DIBS_NEW_HOST:-dibs.hivecommons.dev}"
DIBS_EXPECTED_A="${DIBS_EXPECTED_A:-157.151.252.29}"
# The hub whose session cookie dibs rides on. The cutover is only meaningful
# relative to THIS host: it is the hub's registrable domain that decides
# whether the browser sends hive_hub_user to dibs at all.
HUB_HOST="${HUB_HOST:-hive.hivecommons.dev}"
# A path AND query, deliberately. A redirect that drops $request_uri passes a
# bare-root check and breaks every deep link into dibs.
DIBS_DEEP_PATH="${DIBS_DEEP_PATH:-/ideas/42?ref=x}"
# ingress-nginx's built-in certificate, served when no usable certificate is
# found for a host. Matching on the issuer CN is how a 200 is distinguished
# from a 200 nobody's browser will accept.
FAKE_ISSUER_MATCH="${FAKE_ISSUER_MATCH:-Kubernetes Ingress Controller Fake Certificate}"
CURL_TIMEOUT_SEC="${CURL_TIMEOUT_SEC:-20}"
# Optional. When set to a live hive_hub_user value, check 5 also proves the hub
# ACCEPTS that session, not merely that the cookie could reach dibs. Never
# echoed.
HIVE_HUB_COOKIE="${HIVE_HUB_COOKIE:-}"

PASS=0
WARN=0
FAIL=0

# Markers match preflight.sh and bin/hive-podman-preflight.sh so the reports
# read as one family.
_vf_pass() { echo "  ✓ $1"; PASS=$((PASS + 1)); }
_vf_warn() { echo "  △ $1"; WARN=$((WARN + 1)); }
_vf_fail() { echo "  ✗ $1"; FAIL=$((FAIL + 1)); }

# Same contract as preflight.sh: a check that could not RUN is a warning, never
# a pass. "I could not reach it" must never render as "it is correct" — that is
# the exact failure class this script exists to catch.
_vf_skip() { echo "  ? $1"; WARN=$((WARN + 1)); }

kget() { kubectl "$@" 2>/dev/null; }

# ── 1. DNS ─────────────────────────────────────────────────────────────────
check_dns() {
  echo "1. DNS for ${DIBS_NEW_HOST}"
  local answer="" rc=0
  # WHY THE EXIT STATUS IS READ AND NOT JUST THE OUTPUT.
  #
  # `dig +short` writes its ";; communications error ..." diagnostics to
  # STDOUT, and signals a resolver it could not reach through its EXIT STATUS
  # (9) rather than through an empty answer. Reading stdout alone therefore
  # turns "there is no resolver here" into a non-empty answer string, which
  # compares unequal to the expected address and gets reported as a WRONG
  # RECORD. Measured 2026-09-04 on a host with no local resolver: this check
  # reported
  #
  #   dibs.hivecommons.dev resolves to ';; communications error to
  #   127.0.0.53#53: connection refused ...', expected 157.151.252.29
  #
  # which is the one conclusion this check must never reach on its own
  # evidence — #5925 records that the A record ALREADY EXISTS and warns to
  # confirm it "rather than re-create" it. Both scripts state the contract
  # that a check which could not RUN is a warning and never a pass; this is
  # that contract applied to the direction nobody tested.
  if command -v dig >/dev/null 2>&1; then
    answer="$(dig +short A "$DIBS_NEW_HOST" 2>/dev/null)"; rc=$?
    if [ "$rc" -ne 0 ]; then
      _vf_skip "the resolver could not answer for ${DIBS_NEW_HOST} (dig exit ${rc}) — DNS was NOT checked, and this says nothing about the record"
      return
    fi
    # Strip the diagnostics even on a zero exit: a partial answer must not
    # carry them into the address comparison either.
    answer="$(printf '%s\n' "$answer" | grep -v '^;;' | tr '\n' ' ')"
  elif command -v getent >/dev/null 2>&1; then
    answer="$(getent ahostsv4 "$DIBS_NEW_HOST" 2>/dev/null | awk '{print $1}' | sort -u | tr '\n' ' ')"; rc=$?
    # getent exits 2 for "name not found", which IS an answer and falls through
    # to the empty-answer verdict below. Any other non-zero code is a lookup it
    # could not perform.
    if [ "$rc" -ne 0 ] && [ "$rc" -ne 2 ]; then
      _vf_skip "the resolver could not answer for ${DIBS_NEW_HOST} (getent exit ${rc}) — DNS was NOT checked, and this says nothing about the record"
      return
    fi
  else
    _vf_skip "no dig or getent available to resolve ${DIBS_NEW_HOST}"
    return
  fi
  answer="$(printf '%s' "$answer" | sed 's/[[:space:]]*$//')"

  if [ -z "$answer" ]; then
    # Earned now that a resolver failure returns above: the resolver answered,
    # and its answer was "no A record".
    _vf_fail "${DIBS_NEW_HOST} has no A record — step 1 (the Cloudflare A record) is missing, so nothing below can be trusted"
    return
  fi
  case " $answer " in
    *" $DIBS_EXPECTED_A "*)
      _vf_pass "${DIBS_NEW_HOST} resolves to ${DIBS_EXPECTED_A}" ;;
    *)
      _vf_fail "${DIBS_NEW_HOST} resolves to '${answer}', expected ${DIBS_EXPECTED_A}" ;;
  esac
}

# ── 2. the served certificate ──────────────────────────────────────────────
# The check the whole sequence is scheduled around. A 200 here proves only that
# something answered; this proves WHAT it answered with.
check_certificate() {
  echo "2. certificate served for ${DIBS_NEW_HOST}"
  if ! command -v openssl >/dev/null 2>&1; then
    _vf_skip "openssl is not available; the issuer and SANs could not be read, which is the only thing that distinguishes a real certificate from ingress-nginx's self-signed default"
    return
  fi

  local cert issuer subject sans
  cert="$(openssl s_client -connect "${DIBS_NEW_HOST}:443" -servername "$DIBS_NEW_HOST" </dev/null 2>/dev/null \
    | openssl x509 -noout -issuer -subject -ext subjectAltName 2>/dev/null)"
  if [ -z "$cert" ]; then
    _vf_skip "could not complete a TLS handshake with ${DIBS_NEW_HOST}; the certificate could not be inspected"
    return
  fi

  issuer="$(printf '%s\n' "$cert" | sed -n 's/^issuer=//p' | head -1)"
  subject="$(printf '%s\n' "$cert" | sed -n 's/^subject=//p' | head -1)"
  sans="$(printf '%s\n' "$cert" | grep -o 'DNS:[^,]*' | sed 's/^DNS://' | tr -d ' ' | tr '\n' ' ')"

  case "$issuer" in
    *"$FAKE_ISSUER_MATCH"*)
      _vf_fail "${DIBS_NEW_HOST} is served ingress-nginx's SELF-SIGNED default certificate (issuer: ${issuer}). The host answers, and no browser will accept it. --default-ssl-certificate is not hive-hub/hive-wildcard-tls, or the SAN patch has not taken effect."
      return ;;
  esac
  if [ -n "$issuer" ] && [ "$issuer" = "$subject" ]; then
    _vf_fail "${DIBS_NEW_HOST} is served a SELF-SIGNED certificate (issuer equals subject: ${issuer})"
    return
  fi
  _vf_pass "issuer is ${issuer:-<unreadable>}, not the ingress-nginx default"

  case " $sans " in
    *" ${DIBS_NEW_HOST} "*)
      _vf_pass "certificate SANs cover ${DIBS_NEW_HOST}" ;;
    *)
      _vf_fail "certificate SANs do NOT include ${DIBS_NEW_HOST} (got: ${sans:-<none>}). The wildcard covers *.hive.hivecommons.dev — one level BELOW this name — so it never picks the host up on its own; the step 3 SAN patch is what covers it." ;;
  esac
}

# ── 3. the new host serves the application over a chain a client accepts ───
check_new_host_serves() {
  echo "3. ${DIBS_NEW_HOST} serves the application"
  if ! command -v curl >/dev/null 2>&1; then
    _vf_skip "curl is not available to request https://${DIBS_NEW_HOST}/"
    return
  fi

  # No -k. A chain a real client rejects must fail here rather than being
  # reported as a cheerful 200.
  local code out
  if ! out="$(curl -sS -o /dev/null -w '%{http_code}' --max-time "$CURL_TIMEOUT_SEC" "https://${DIBS_NEW_HOST}/" 2>&1)"; then
    # curl's TLS errors run to a dozen lines of help text; the first line is
    # the diagnosis and the rest buries the report.
    _vf_fail "https://${DIBS_NEW_HOST}/ could not be fetched with certificate validation on: $(printf '%s\n' "$out" | head -1)"
    return
  fi
  code="$(printf '%s' "$out" | tr -dc '0-9')"
  case "$code" in
    200) _vf_pass "https://${DIBS_NEW_HOST}/ returns 200 over a validating chain" ;;
    30*) _vf_fail "https://${DIBS_NEW_HOST}/ returns ${code} — the CANONICAL host is redirecting; the app and redirect Ingresses are the wrong way round" ;;
    "")  _vf_fail "https://${DIBS_NEW_HOST}/ returned no status code" ;;
    *)   _vf_fail "https://${DIBS_NEW_HOST}/ returns ${code}, expected 200" ;;
  esac
}

# ── 4. the session cookie can reach the new host ───────────────────────────
# The reason this issue is a repair and not a rename. dibs has no login of its
# own: it forwards the browser's hive_hub_user cookie to the hub's
# /api/saas/whoami. The browser sends that cookie to dibs ONLY because of the
# cookie's Domain attribute, which the hub derives from the registrable domain
# of its OWN canonical host (sessionCookieDomain, src/pkg/hub/saas.go). So the
# bridge has a precondition: the hub and dibs must share a registrable domain.
#
# The parent domain is derived here the same way sessionCookieParentDomain's
# fallback does — strip the first label — which agrees with the Go code's
# public-suffix lookup for a hub host of the shape <label>.<name>.<tld>. A
# parent of any other shape is reported as a warning rather than a pass,
# because the naive derivation is not guaranteed to be the registrable domain
# under a multi-label public suffix.
check_cookie_scope() {
  echo "4. hub session cookie can reach ${DIBS_NEW_HOST}"
  local parent labels
  parent="${HUB_HOST#*.}"
  if [ "$parent" = "$HUB_HOST" ] || [ -z "$parent" ]; then
    _vf_skip "HUB_HOST='${HUB_HOST}' has no parent domain to derive a cookie scope from"
    return
  fi
  labels="$(printf '%s' "$parent" | tr -cd '.' | wc -c)"
  labels=$((labels + 1))

  if [ "$labels" -ne 2 ]; then
    _vf_warn "derived cookie domain '.${parent}' from HUB_HOST='${HUB_HOST}' has ${labels} labels; under a multi-label public suffix this is not necessarily the registrable domain — confirm against sessionCookieParentDomain in src/pkg/hub/saas.go"
  fi

  # RFC 6265 5.3 domain-match: the cookie reaches the host when the host IS
  # the domain or is a subdomain of it.
  local reaches=1
  [ "$DIBS_NEW_HOST" = "$parent" ] && reaches=0
  case "$DIBS_NEW_HOST" in *".$parent") reaches=0 ;; esac
  if [ "$reaches" -ne 0 ]; then
    _vf_fail "the hub at ${HUB_HOST} scopes hive_hub_user to '.${parent}', which does NOT cover ${DIBS_NEW_HOST}. dibs will render every visitor as signed out, and no configuration fixes it — a browser ignores a Set-Cookie whose Domain does not cover the sending host (RFC 6265 5.3)."
    return
  fi
  _vf_pass "hive_hub_user is scoped to '.${parent}', which covers ${DIBS_NEW_HOST}"

  # The legacy host, for contrast: this is WHY step 6 is mandatory rather than
  # tidy-up. Dual-serving cannot be made to work.
  local legacyReaches=1
  [ "$DIBS_LEGACY_HOST" = "$parent" ] && legacyReaches=0
  case "$DIBS_LEGACY_HOST" in *".$parent") legacyReaches=0 ;; esac
  if [ "$legacyReaches" -eq 0 ]; then
    _vf_warn "${DIBS_LEGACY_HOST} is ALSO inside '.${parent}' — the signed-out problem this cutover repairs does not apply to this configuration; re-read the plan before trusting it"
  else
    _vf_pass "${DIBS_LEGACY_HOST} is outside '.${parent}', so it cannot be signed in — dual-serving is impossible and step 6 (redirect) is mandatory"
  fi

  if [ -z "$HIVE_HUB_COOKIE" ]; then
    echo "    note: set HIVE_HUB_COOKIE to a live hive_hub_user value to also prove the hub ACCEPTS the session."
    echo "    Neither check replaces the browser test: sign in at https://${HUB_HOST}, then load https://${DIBS_NEW_HOST}"
    echo "    and confirm you are recognized. Only that exercises dibs's own forwarding code, which lives in another repo."
    return
  fi
  if ! command -v curl >/dev/null 2>&1; then
    _vf_skip "curl is not available to check the session against https://${HUB_HOST}/api/saas/whoami"
    return
  fi
  local whoami
  # The cookie value is passed on the command line to curl only; it is never
  # echoed, and no response body is printed.
  whoami="$(curl -sS -o /dev/null -w '%{http_code}' --max-time "$CURL_TIMEOUT_SEC" \
    -H "Cookie: hive_hub_user=${HIVE_HUB_COOKIE}" \
    "https://${HUB_HOST}/api/saas/whoami" 2>/dev/null)"
  case "$whoami" in
    200) _vf_pass "the hub accepts the supplied session at /api/saas/whoami (200)" ;;
    401) _vf_fail "the hub REJECTS the supplied session at /api/saas/whoami (401) — dibs would resolve this visitor as signed out even with the cookie in scope" ;;
    "")  _vf_skip "/api/saas/whoami returned no status code" ;;
    *)   _vf_warn "/api/saas/whoami returned ${whoami}; expected 200 for a live session" ;;
  esac
}

# ── 5. the legacy host ─────────────────────────────────────────────────────
# Phase-aware. Before step 6 the legacy host still serves the application, and
# that is an expected intermediate state — but it is a WARNING, never a pass,
# because for as long as it lasts the legacy host is knowingly signed out.
check_legacy_redirect() {
  echo "5. ${DIBS_LEGACY_HOST} redirects to ${DIBS_NEW_HOST}"
  if ! command -v curl >/dev/null 2>&1; then
    _vf_skip "curl is not available to request https://${DIBS_LEGACY_HOST}"
    return
  fi

  local probe code location want
  want="https://${DIBS_NEW_HOST}${DIBS_DEEP_PATH}"
  if ! probe="$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' --max-time "$CURL_TIMEOUT_SEC" \
      "https://${DIBS_LEGACY_HOST}${DIBS_DEEP_PATH}" 2>&1)"; then
    _vf_skip "could not fetch https://${DIBS_LEGACY_HOST}${DIBS_DEEP_PATH}: ${probe}"
    return
  fi
  code="${probe%% *}"
  location="${probe#* }"
  [ "$location" = "$code" ] && location=""

  # Anything that is not a redirect means the APPLICATION is still answering on
  # the legacy host — the dual-host window. Which status it answers with is the
  # application's business, not this check's: dibs returns 200 on / and 401 on
  # an authenticated deep path, and both are the same fact here (no redirect is
  # happening). Measured against the live host on 2026-09-04, which is why this
  # does not key on 200 alone.
  case "$code" in
    308) : ;;
    30*)
      _vf_fail "${DIBS_LEGACY_HOST} returns ${code}, expected 308. The staged annotation sets permanent-redirect-code: \"308\"; another object is answering, or the annotation did not apply."
      return ;;
    "")
      _vf_fail "${DIBS_LEGACY_HOST} returned no status code"
      return ;;
    5*)
      _vf_warn "${DIBS_LEGACY_HOST} is not redirecting — the dual-host window is open, and the application answered ${code} (a server error worth looking at separately). Step 6 closes the window; until it does, visitors arriving there are signed out."
      return ;;
    *)
      _vf_warn "${DIBS_LEGACY_HOST} still SERVES the application (HTTP ${code}) — the dual-host window is open. That is expected between steps 4 and 6, and it is not a resting state: visitors arriving there are signed out. Step 6 closes it."
      return ;;
  esac

  if [ "$location" = "$want" ]; then
    _vf_pass "308 to ${location} — path and query survive the redirect"
    return
  fi
  case "$location" in
    "https://${DIBS_NEW_HOST}"|"https://${DIBS_NEW_HOST}/")
      _vf_fail "308 to ${location}: the redirect DROPS the path and query. A bare-root check passes and every deep link into dibs breaks. \$request_uri is not reaching the permanent-redirect annotation." ;;
    "")
      _vf_fail "308 with no Location header" ;;
    *)
      _vf_fail "308 to '${location}', expected '${want}'" ;;
  esac
}

# ── 6. exactly one Ingress claims the legacy host ──────────────────────────
# ingress-nginx resolves a duplicate host+path claim in favour of the OLDER
# object and only logs a warning, so a collision does not fail loudly — it
# makes the redirect quietly not happen while every object looks applied.
check_ingress_ownership() {
  echo "6. sole ownership of ${DIBS_LEGACY_HOST} in namespace ${DIBS_NAMESPACE}"
  if ! command -v kubectl >/dev/null 2>&1; then
    _vf_skip "kubectl is not available; Ingress ownership was not checked"
    return
  fi
  local listing
  if ! listing="$(kget -n "$DIBS_NAMESPACE" get ingress \
      -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.rules[*].host}{"\n"}{end}')"; then
    _vf_skip "could not list Ingresses in namespace ${DIBS_NAMESPACE}"
    return
  fi
  if [ -z "$listing" ]; then
    _vf_skip "no Ingresses readable in namespace ${DIBS_NAMESPACE}"
    return
  fi

  local owners=""
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    case "$line" in
      *"$DIBS_LEGACY_HOST"*) owners="${owners}${line%% *} " ;;
    esac
  done <<EOF
$listing
EOF
  owners="$(printf '%s' "$owners" | sed 's/[[:space:]]*$//')"

  local count
  count="$(printf '%s' "$owners" | wc -w | tr -d ' ')"
  case "$count" in
    0)
      _vf_warn "no Ingress in ${DIBS_NAMESPACE} claims ${DIBS_LEGACY_HOST}; the legacy host is served by nothing (existing links 404 rather than redirect)" ;;
    1)
      if [ "$owners" = "$DIBS_REDIRECT_INGRESS" ]; then
        _vf_pass "${DIBS_LEGACY_HOST} is claimed only by ${DIBS_REDIRECT_INGRESS}"
      else
        _vf_warn "${DIBS_LEGACY_HOST} is claimed only by '${owners}' (the application Ingress) — step 6 has not been applied yet"
      fi ;;
    *)
      _vf_fail "${DIBS_LEGACY_HOST} is claimed by ${count} Ingresses (${owners}). ingress-nginx honours the OLDEST and only logs a warning, so the redirect is inert while every object looks applied. Remove the host from the application Ingress first." ;;
  esac
}

main() {
  echo "=== dibs.hivecommons.dev cutover verification (#5925) ==="
  echo "Read-only. Run after step 4 (expect check 5 to report the window OPEN) and"
  echo "again after step 6 (expect everything to pass). A 200 is never accepted as"
  echo "evidence on its own: every failure this catches also answers 200."
  echo

  check_dns
  echo
  check_certificate
  echo
  check_new_host_serves
  echo
  check_cookie_scope
  echo
  check_legacy_redirect
  echo
  check_ingress_ownership

  echo
  echo "pass=${PASS} warn=${WARN} fail=${FAIL}"
  if [ "$FAIL" -gt 0 ]; then
    echo "NOT VERIFIED: the cutover is not finished. See src/docs/dibs-domain-cutover.md#rollback."
    return 78
  fi
  if [ "$WARN" -gt 0 ]; then
    echo "Not finished: read the warnings above. A skipped check is not a passed one, and"
    echo "an open dual-host window is not a resting state."
  fi
  return 0
}

main "$@"
