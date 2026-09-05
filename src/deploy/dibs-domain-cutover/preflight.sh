#!/usr/bin/env bash
# dibs.hivecommons.dev cutover preflight (#5925).
#
# The cutover in src/docs/dibs-domain-cutover.md is scheduled around a scarce
# resource: Let's Encrypt caps hivecommons.dev at 50 certificates per rolling
# 168 hours, the quota was exhausted on 2026-09-03, and the issue's own warning
# is that a 429 pushes the retry deadline further out. So the expensive failure
# is not "a step fails" — it is "a step fails AFTER the certificate has been
# re-issued", because that spends the window the whole plan is waiting for and
# the next attempt is a week away.
#
# Every assumption the staged manifests rest on is checkable BEFORE the hold
# expires, read-only, at zero quota cost. That is all this script does.
#
# It answers six questions and nothing else:
#
#   1. Will the new host actually get a real certificate? The staged Ingress
#      gives dibs.hivecommons.dev NO tls: block and relies on the ingress-nginx
#      controller's --default-ssl-certificate being hive-hub/hive-wildcard-tls.
#      If that flag is absent or points elsewhere, ingress-nginx serves its
#      built-in SELF-SIGNED certificate, and step 5 fails on a host that answers
#      200 — after the issuance is already spent. This is also the one
#      assumption that departs from every other Ingress in this repo, which all
#      name a secretName explicitly (src/pkg/hub/saas_provision.go).
#   2. Does the Certificate exist where the patch targets it, and is the SAN
#      already there? Patching a Certificate that is not at
#      hive-hub/hive-wildcard-tls fails — but only once someone is mid-sequence.
#   3. Has the live Ingress drifted from the staged copy?
#      02-dibs-ingress-dual-host.yaml is a FULL object, not a patch, so
#      `kubectl apply` overwrites the live dibs/dibs Ingress wholesale —
#      silently dropping any annotation the hand-managed live object carries and
#      the staged file does not (proxy body size, rate limits, auth-url). The
#      README says to compare by hand; this is that comparison.
#   4. Is either host already claimed by a DIFFERENT Ingress? ingress-nginx
#      resolves a duplicate host+path collision in favour of the OLDER Ingress
#      and only logs a warning, so a collision does not fail loudly — it makes
#      the redirect quietly not happen.
#   5. Does the DNS record exist yet, and does it point where step 1 says?
#   6. Does the Let's Encrypt registered-domain window have headroom? This is a
#      read-only crt.sh count of recent Let's Encrypt certificates, not an ACME
#      probe and not a dry-run issuance.
#
# Nothing here mutates anything: only `kubectl get`, DNS lookup, and a read-only
# crt.sh query. It is safe to run at any time, and is meant to be run EARLY —
# the whole value is in learning the answers before 2026-09-10, not during.
#
# Run: bash src/deploy/dibs-domain-cutover/preflight.sh
# Exit codes: 0 no failing check, 78 at least one failing check (EX_CONFIG).

set -uo pipefail

# The assumptions the staged manifests encode. Overridable so the same script
# checks a rehearsal namespace or a renamed object without being edited.
DIBS_NAMESPACE="${DIBS_NAMESPACE:-dibs}"
DIBS_INGRESS="${DIBS_INGRESS:-dibs}"
DIBS_LEGACY_HOST="${DIBS_LEGACY_HOST:-dibs.kubestellar.io}"
DIBS_NEW_HOST="${DIBS_NEW_HOST:-dibs.hivecommons.dev}"
DIBS_LEGACY_TLS_SECRET="${DIBS_LEGACY_TLS_SECRET:-hive-tls-hc}"
CERT_NAMESPACE="${CERT_NAMESPACE:-hive-hub}"
CERT_NAME="${CERT_NAME:-hive-wildcard-tls}"
# The controller Deployment whose args carry --default-ssl-certificate.
INGRESS_NS="${INGRESS_NS:-ingress-nginx}"
INGRESS_DEPLOY="${INGRESS_DEPLOY:-ingress-nginx-controller}"
# Expected A record, from the issue's step 1.
DIBS_EXPECTED_A="${DIBS_EXPECTED_A:-157.151.252.29}"
LE_REGISTERED_DOMAIN="${LE_REGISTERED_DOMAIN:-hivecommons.dev}"
LE_CERT_WINDOW_HOURS="${LE_CERT_WINDOW_HOURS:-168}"
LE_CERT_LIMIT="${LE_CERT_LIMIT:-50}"
CRT_SH_TIMEOUT_SEC="${CRT_SH_TIMEOUT_SEC:-20}"

PASS=0
WARN=0
FAIL=0

# Markers match bin/hive-podman-preflight.sh so the two read as one report.
_pf_pass() { echo "  ✓ $1"; PASS=$((PASS + 1)); }
_pf_warn() { echo "  △ $1"; WARN=$((WARN + 1)); }
_pf_fail() { echo "  ✗ $1"; FAIL=$((FAIL + 1)); }

# A check that could not RUN is a warning, never a pass. An unreachable cluster
# must not read as "preconditions met" — that is the same silent-green failure
# this script exists to prevent.
_pf_skip() { echo "  ? $1"; WARN=$((WARN + 1)); }

kget() { kubectl "$@" 2>/dev/null; }

have_cluster() {
  command -v kubectl >/dev/null 2>&1 || return 1
  kget get namespace "$DIBS_NAMESPACE" -o name >/dev/null || return 1
  return 0
}

# ── 1. default SSL certificate ─────────────────────────────────────────────
check_default_ssl_certificate() {
  echo "1. ingress-nginx default certificate serves ${DIBS_NEW_HOST}"
  local args
  args="$(kget -n "$INGRESS_NS" get deployment "$INGRESS_DEPLOY" \
    -o jsonpath='{.spec.template.spec.containers[*].args[*]}')"
  if [ -z "$args" ]; then
    _pf_skip "could not read ${INGRESS_NS}/${INGRESS_DEPLOY} args; set INGRESS_NS/INGRESS_DEPLOY if the controller lives elsewhere"
    return
  fi

  local flag="" token
  for token in $args; do
    case "$token" in
      --default-ssl-certificate=*) flag="${token#--default-ssl-certificate=}" ;;
    esac
  done

  local want="${CERT_NAMESPACE}/${CERT_NAME}"
  if [ -z "$flag" ]; then
    _pf_fail "controller has NO --default-ssl-certificate, so ${DIBS_NEW_HOST} would be served ingress-nginx's self-signed certificate"
    echo "      The staged 02-dibs-ingress-dual-host.yaml gives that host no tls: block on purpose"
    echo "      (an Ingress can only name a Secret in its OWN namespace, and the wildcard lives in"
    echo "      ${CERT_NAMESPACE}). Fix this BEFORE step 3 spends the issuance: either set"
    echo "      --default-ssl-certificate=${want} on the controller, or replicate the secret into"
    echo "      ${DIBS_NAMESPACE} and give the host its own tls: block."
    return
  fi
  if [ "$flag" != "$want" ]; then
    _pf_fail "--default-ssl-certificate is '${flag}', not '${want}'; ${DIBS_NEW_HOST} would be served that certificate instead of the wildcard"
    return
  fi
  _pf_pass "--default-ssl-certificate=${want}"
}

# ── 2. the Certificate the patch targets ───────────────────────────────────
check_certificate_sans() {
  echo "2. Certificate ${CERT_NAMESPACE}/${CERT_NAME} and its SAN list"
  local dns_names
  dns_names="$(kget -n "$CERT_NAMESPACE" get certificate "$CERT_NAME" \
    -o jsonpath='{.spec.dnsNames[*]}')"
  if [ -z "$dns_names" ]; then
    _pf_skip "could not read Certificate ${CERT_NAMESPACE}/${CERT_NAME}; confirm the name and namespace the step 3 patch targets"
    return
  fi

  case " $dns_names " in
    *" $DIBS_NEW_HOST "*)
      _pf_warn "${DIBS_NEW_HOST} is ALREADY a SAN; skip the step 3 patch rather than re-adding it"
      return ;;
  esac
  _pf_pass "${DIBS_NEW_HOST} is absent, so the step 3 patch is the right operation"
  # Printed because "there is already a wildcard" is exactly the reasoning that
  # would talk someone out of step 3. The wildcard covers *.hive.hivecommons.dev;
  # dibs.hivecommons.dev is one level up and is not matched by it.
  echo "      live SANs: ${dns_names}"
}

# ── 3. drift between the live Ingress and the staged copy ──────────────────
check_ingress_drift() {
  echo "3. live ${DIBS_NAMESPACE}/${DIBS_INGRESS} Ingress matches the staged assumptions"
  local class
  class="$(kget -n "$DIBS_NAMESPACE" get ingress "$DIBS_INGRESS" -o jsonpath='{.spec.ingressClassName}')"
  if [ -z "$class" ]; then
    _pf_skip "could not read Ingress ${DIBS_NAMESPACE}/${DIBS_INGRESS}"
    return
  fi
  if [ "$class" = "nginx" ]; then
    _pf_pass "ingressClassName=nginx"
  else
    _pf_fail "ingressClassName is '${class}', staged manifests assume 'nginx'"
  fi

  local secret
  secret="$(kget -n "$DIBS_NAMESPACE" get ingress "$DIBS_INGRESS" -o jsonpath='{.spec.tls[*].secretName}')"
  case " $secret " in
    *" $DIBS_LEGACY_TLS_SECRET "*)
      _pf_pass "legacy host still on ${DIBS_LEGACY_TLS_SECRET}" ;;
    *)
      _pf_fail "live TLS secret is '${secret}', staged manifests assume '${DIBS_LEGACY_TLS_SECRET}'" ;;
  esac

  # The annotation check is the point of this section: every live annotation the
  # staged full object omits is dropped the moment it is applied.
  local annos
  annos="$(kget -n "$DIBS_NAMESPACE" get ingress "$DIBS_INGRESS" \
    -o jsonpath='{range .metadata.annotations}{@}{end}')"
  # kubectl renders an absent map as "" and a present one as its serialisation;
  # kubectl.kubernetes.io/last-applied-configuration is noise either way.
  annos="$(printf '%s' "$annos" | sed 's/kubectl.kubernetes.io\/last-applied-configuration[^ ]*//')"
  case "$(printf '%s' "$annos" | tr -d '[:space:]')" in
    ""|"map[]"|"{}")
      _pf_pass "live Ingress carries no annotations for the staged copy to drop" ;;
    *)
      _pf_warn "live Ingress carries annotations; 02-dibs-ingress-dual-host.yaml has none, and applying it would DROP them"
      echo "      live annotations: ${annos}"
      echo "      Copy anything cluster-local into the staged file before applying it." ;;
  esac
}

# ── 4. host collisions ─────────────────────────────────────────────────────
check_host_collisions() {
  echo "4. no other Ingress already claims either host"
  local rows
  rows="$(kget -n "$DIBS_NAMESPACE" get ingress \
    -o jsonpath='{range .items[*]}{.metadata.name}{" "}{range .spec.rules[*]}{.host}{","}{end}{"\n"}{end}')"
  if [ -z "$rows" ]; then
    _pf_skip "could not list Ingresses in ${DIBS_NAMESPACE}"
    return
  fi

  local host
  for host in "$DIBS_LEGACY_HOST" "$DIBS_NEW_HOST"; do
    local owners="" line name hosts
    while IFS= read -r line; do
      [ -n "$line" ] || continue
      name="${line%% *}"
      hosts=",${line#* },"
      hosts="${hosts// /}"
      case "$hosts" in
        *",${host},"*) owners="${owners}${name} " ;;
      esac
    done <<EOF
$rows
EOF
    owners="${owners% }"
    case "$owners" in
      "")
        _pf_pass "${host} is claimed by no Ingress" ;;
      "$DIBS_INGRESS")
        _pf_pass "${host} is claimed only by ${DIBS_INGRESS}" ;;
      *\ *)
        _pf_fail "${host} is claimed by MORE THAN ONE Ingress (${owners}); ingress-nginx keeps the OLDEST and only logs a warning, so one of them is silently inert" ;;
      *)
        _pf_warn "${host} is claimed by '${owners}', not '${DIBS_INGRESS}'" ;;
    esac
  done
}

# ── 5. DNS ─────────────────────────────────────────────────────────────────
check_dns() {
  echo "5. DNS for ${DIBS_NEW_HOST}"
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
  #
  # It matters more here than in verify.sh: this check's empty-answer verdict
  # tells the operator the A record "has not been done", which under a resolver
  # outage is an instruction to create a record that already exists.
  if command -v dig >/dev/null 2>&1; then
    answer="$(dig +short A "$DIBS_NEW_HOST" 2>/dev/null)"; rc=$?
    if [ "$rc" -ne 0 ]; then
      _pf_skip "the resolver could not answer for ${DIBS_NEW_HOST} (dig exit ${rc}) — DNS was NOT checked, and this says nothing about the record"
      return
    fi
    answer="$(printf '%s\n' "$answer" | grep -v '^;;' | tr '\n' ' ')"
  elif command -v getent >/dev/null 2>&1; then
    answer="$(getent ahostsv4 "$DIBS_NEW_HOST" 2>/dev/null | awk '{print $1}' | sort -u | tr '\n' ' ')"; rc=$?
    # getent exits 2 for "name not found" — an answer, not a failure to look.
    if [ "$rc" -ne 0 ] && [ "$rc" -ne 2 ]; then
      _pf_skip "the resolver could not answer for ${DIBS_NEW_HOST} (getent exit ${rc}) — DNS was NOT checked, and this says nothing about the record"
      return
    fi
  else
    _pf_skip "no dig or getent available to resolve ${DIBS_NEW_HOST}"
    return
  fi
  answer="$(printf '%s' "$answer" | sed 's/[[:space:]]*$//')"

  if [ -z "$answer" ]; then
    _pf_warn "${DIBS_NEW_HOST} does not resolve yet — step 1 (the Cloudflare A record) has not been done"
    return
  fi
  case " $answer " in
    *" $DIBS_EXPECTED_A "*)
      _pf_pass "${DIBS_NEW_HOST} resolves to ${DIBS_EXPECTED_A}" ;;
    *)
      _pf_fail "${DIBS_NEW_HOST} resolves to '${answer}', expected ${DIBS_EXPECTED_A}" ;;
  esac
}

# ── 6. Let's Encrypt registered-domain headroom ────────────────────────────
check_le_headroom() {
  echo "6. Let's Encrypt headroom for ${LE_REGISTERED_DOMAIN}"
  if ! command -v curl >/dev/null 2>&1; then
    _pf_skip "curl is not available to query crt.sh; cannot estimate Let's Encrypt headroom"
    return
  fi
  if ! command -v python3 >/dev/null 2>&1; then
    _pf_skip "python3 is not available to parse crt.sh JSON; cannot estimate Let's Encrypt headroom"
    return
  fi

  local url json counts total count
  url="https://crt.sh/?q=${LE_REGISTERED_DOMAIN}&output=json"
  if ! json="$(curl -fsSL --max-time "$CRT_SH_TIMEOUT_SEC" "$url" 2>/dev/null)"; then
    _pf_skip "could not query crt.sh for ${LE_REGISTERED_DOMAIN}; do not treat this as quota headroom"
    return
  fi

  # WHY THE ROW TOTAL IS READ AND NOT JUST THE IN-WINDOW COUNT.
  #
  # crt.sh answers a query it cannot service with HTTP 200 and a body of "[]" —
  # not a 5xx, not malformed JSON, so every guard above passes and the parse
  # below yields 0. Measured 2026-09-04: six consecutive requests for
  # hivecommons.dev returned exactly that, while the domain demonstrably has a
  # live Let's Encrypt wildcard and, per #5925, ~50 certificates minted in a
  # burst the previous afternoon. This check reported
  #
  #   ✓ 0 Let's Encrypt certificate(s) for hivecommons.dev in the last 168h;
  #     headroom 50
  #
  # which is a confident PASS on the one gate that guards the irreversible,
  # quota-spending step — and it is the reading this script's own contract
  # forbids: a check that could not RUN is a warning, never a pass.
  #
  # Zero rows is genuinely ambiguous. It means either "this domain has never
  # had a certificate" or "crt.sh did not answer". For a domain we are about to
  # re-issue an EXISTING wildcard for, the first is not a possibility, so zero
  # rows can only be the second. Zero rows IN THE WINDOW, out of rows that were
  # actually returned, is a real and useful answer and still passes.
  if ! counts="$(printf '%s' "$json" | \
      LE_CERT_WINDOW_HOURS="$LE_CERT_WINDOW_HOURS" \
      LE_REGISTERED_DOMAIN="$LE_REGISTERED_DOMAIN" \
      CRT_SH_NOW_UTC="${CRT_SH_NOW_UTC:-}" \
      python3 -c '
import datetime as dt
import json
import os
import sys

def parse_time(value):
    value = str(value or "").strip().replace(" ", "T")
    if not value:
        return None
    if value.endswith("Z"):
        value = value[:-1] + "+00:00"
    try:
        parsed = dt.datetime.fromisoformat(value)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=dt.timezone.utc)
    return parsed.astimezone(dt.timezone.utc)

now = parse_time(os.environ.get("CRT_SH_NOW_UTC")) or dt.datetime.now(dt.timezone.utc)
window_hours = int(os.environ.get("LE_CERT_WINDOW_HOURS", "168"))
domain = os.environ.get("LE_REGISTERED_DOMAIN", "").lower()
cutoff = now - dt.timedelta(hours=window_hours)

try:
    rows = json.load(sys.stdin)
except json.JSONDecodeError:
    print("parse-error", file=sys.stderr)
    sys.exit(2)
if isinstance(rows, dict):
    rows = [rows]

seen = set()
count = 0
# total counts every row crt.sh returned FOR THIS DOMAIN, regardless of issuer
# or age. It answers "did crt.sh observe this domain at all", which is what
# separates a real zero from an unanswered query. Deliberately not filtered by
# issuer: a domain whose only certificates came from another CA has still been
# observed, and reading that as "no data" would be its own false alarm.
total = 0
for row in rows:
    names = (str(row.get("common_name", "")) + "\n" + str(row.get("name_value", ""))).lower()
    if domain and domain not in names:
        continue
    total += 1
    issuer = str(row.get("issuer_name", ""))
    if "let" not in issuer.lower() or "encrypt" not in issuer.lower():
        continue
    not_before = parse_time(row.get("not_before"))
    if not_before is None or not_before < cutoff:
        continue
    ident = str(row.get("id") or row.get("min_cert_id") or row)
    if ident in seen:
        continue
    seen.add(ident)
    count += 1
print(total, count)
')"; then
    _pf_skip "crt.sh returned data that could not be parsed; do not treat this as quota headroom"
    return
  fi

  total="${counts%% *}"
  count="${counts##* }"
  case "$total$count" in
    ''|*[!0-9]*)
      _pf_skip "crt.sh returned an unexpected count '${counts}'; do not treat this as quota headroom"
      return ;;
  esac
  if [ "$total" -eq 0 ]; then
    # See the note above: for a domain already serving Let's Encrypt
    # certificates, "no rows at all" is an unanswered query wearing a 200.
    _pf_skip "crt.sh returned NO certificates at all for ${LE_REGISTERED_DOMAIN} — not credible for a domain already serving Let's Encrypt certificates, so this is an unanswered query and NOT headroom; retry, or count from the CA's own rate-limit tooling before spending the window"
    return
  fi
  case "$LE_CERT_LIMIT" in
    ''|*[!0-9]*)
      _pf_skip "LE_CERT_LIMIT='${LE_CERT_LIMIT}' is not a positive integer; cannot estimate headroom"
      return ;;
  esac

  if [ "$count" -ge "$LE_CERT_LIMIT" ]; then
    _pf_fail "${count} Let's Encrypt certificate(s) for ${LE_REGISTERED_DOMAIN} in the last ${LE_CERT_WINDOW_HOURS}h; limit is ${LE_CERT_LIMIT}, so issuing now risks a 429"
    return
  fi
  _pf_pass "${count} Let's Encrypt certificate(s) for ${LE_REGISTERED_DOMAIN} in the last ${LE_CERT_WINDOW_HOURS}h; headroom $((LE_CERT_LIMIT - count))"
}

main() {
  echo "=== dibs.hivecommons.dev cutover preflight (#5925) ==="
  echo "Read-only. Run this BEFORE the Let's Encrypt hold expires: every check here"
  echo "is free now, and every one of them fails expensively after issuance."
  echo

  if have_cluster; then
    check_default_ssl_certificate
    echo
    check_certificate_sans
    echo
    check_ingress_drift
    echo
    check_host_collisions
    echo
  else
    echo "  ? no reachable cluster (kubectl missing, unconfigured, or namespace ${DIBS_NAMESPACE} unreadable)"
    echo "    Cluster checks 1-4 were SKIPPED, which is not the same as passing."
    WARN=$((WARN + 1))
    echo
  fi
  check_dns
  echo
  check_le_headroom

  echo
  echo "pass=${PASS} warn=${WARN} fail=${FAIL}"
  if [ "$FAIL" -gt 0 ]; then
    echo "BLOCKED: fix the failing checks before step 3 spends the issuance window."
    return 78
  fi
  if [ "$WARN" -gt 0 ]; then
    echo "Proceed only after reading the warnings above; a skipped check is not a passed one."
  fi
  return 0
}

main "$@"
