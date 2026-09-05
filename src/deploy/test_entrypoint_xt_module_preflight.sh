#!/usr/bin/env bash
# Regression coverage for the "fail loudly" half of #6003.
#
# test_entrypoint_iptables_gate.sh and test_entrypoint_egress_ruleset.sh cover
# what happens when a required append fails part way through building the real
# HIVE_PROXY chain. This suite covers the step BEFORE that: the netfilter
# extension preflight, which probes the modules in a throwaway chain so the
# operator is told which kernel module is missing instead of reading exit 4
# from whichever append happened to run first.
#
# The three properties asserted here are the ones the maintainer asked for on
# #6003, and each one is load bearing:
#
#   1. A missing required extension exits EXACTLY 77, the documented preflight
#      code (sysexits.h EX_NOPERM) that entrypoint.sh already uses for "the
#      environment cannot supply what the egress gate needs".
#   2. The FATAL output NAMES the module (xt_REDIRECT / xt_mark), because the
#      loud failure is the signal to go load it on the node.
#   3. No partially built HIVE_PROXY chain is left installed. A half built
#      chain is what produced the mute hive on the healthy node: it redirected
#      nothing while the spoke reported healthy, which is strictly worse than
#      a crashloop.
#
# xt_owner stays OPTIONAL - OpenShift/OVN runs without it and the packet-mark
# exemption covers the proxy there - so its absence must warn and continue.
#
# Run: bash src/deploy/test_entrypoint_xt_module_preflight.sh
# Exit codes: 0 all cases pass, 1 a case failed.

set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENTRYPOINT="$HERE/entrypoint.sh"
WORK="$HERE/.test-entrypoint-xt-preflight-$$"
BIN="$WORK/bin"
mkdir -p "$BIN"
trap 'rm -rf "$WORK"' EXIT

fail() {
  echo "FAIL: $1"
  [ $# -gt 1 ] && echo "      $2"
  exit 1
}

assert_contains() {
  local file="$1" needle="$2"
  grep -Fq -- "$needle" "$file" || fail "expected $file to contain: $needle" "$(cat "$file")"
}

assert_not_contains() {
  local file="$1" needle="$2"
  if grep -Fq -- "$needle" "$file"; then
    fail "expected $file not to contain: $needle" "$(cat "$file")"
  fi
}

# Extract the shipped preflight block from entrypoint.sh so this suite tests
# the real code rather than a copy that can drift away from it.
BLOCK="$WORK/preflight.sh"
awk '
  index($0, "HIVE-EGRESS-PREFLIGHT-BEGIN") { on = 1; next }
  index($0, "HIVE-EGRESS-PREFLIGHT-END") { on = 0 }
  on { sub(/^      /, ""); print }
' "$ENTRYPOINT" > "$BLOCK"

if ! grep -q 'HIVE_PROXY_PREFLIGHT' "$BLOCK"; then
  fail 'could not extract the netfilter preflight block from entrypoint.sh'
fi

# Fake iptables. HIVE_FAKE_XT_MISSING is a comma separated list of extension
# keys (mark, redirect, owner) whose `-A` must fail the way a real kernel
# without the module does: the iptables warning plus RULE_APPEND / ENOENT.
cat > "$BIN/iptables-shim" <<'SHIM'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$HIVE_FAKE_IPTABLES_CALLS"

missing() {
  case ",${HIVE_FAKE_XT_MISSING:-}," in
    *",$1,"*) return 0 ;;
    *) return 1 ;;
  esac
}

no_module() {
  printf 'Warning: Extension %s revision 0 not supported, missing kernel module?\n' "$1" >&2
  printf 'iptables v1.8.11 (nf_tables): RULE_APPEND failed (No such file or directory): rule in chain %s\n' "$2" >&2
  exit 4
}

case "$*" in
  # Real iptables fails the delete once the rule is gone; returning 0 forever
  # would spin the `while ... -D OUTPUT` teardown loop in entrypoint.sh.
  *'-D OUTPUT -j HIVE_PROXY'*) exit 1 ;;
  *'-A HIVE_PROXY_PREFLIGHT -m mark'*) missing mark && no_module mark HIVE_PROXY_PREFLIGHT ;;
  *'-A HIVE_PROXY_PREFLIGHT -p tcp --dport 443 -j REDIRECT'*) missing redirect && no_module REDIRECT HIVE_PROXY_PREFLIGHT ;;
  *'-A HIVE_PROXY_PREFLIGHT -m owner'*) missing owner && no_module owner HIVE_PROXY_PREFLIGHT ;;
  *'-N HIVE_PROXY_PREFLIGHT'*) missing chain && exit 3 ;;
esac
exit 0
SHIM
chmod +x "$BIN/iptables-shim"

# $1 = case name, $2 = HIVE_FAKE_XT_MISSING, $3 = HIVE_PROXY_ADVISORY_OK
run_preflight() {
  local name="$1" xt_missing="$2" advisory="$3"
  local script="$WORK/$name.run.sh"
  CASE_OUT="$WORK/$name.out"
  CASE_CALLS="$WORK/$name.calls"
  : > "$CASE_CALLS"
  cat > "$script" <<SCRIPT
set -e
export PATH="$BIN:/usr/bin:/bin"
export HIVE_FAKE_IPTABLES_CALLS="$CASE_CALLS"
export HIVE_FAKE_XT_MISSING="$xt_missing"
IPT="$BIN/iptables-shim"
PROXY_UID=1001
PROXY_PORT=18443
PROXY_ADVISORY_OK="$advisory"
HIVE_PROXY_EGRESS_MARK=0x1112
EXIT_NET_ADMIN_REQUIRED=77
_ipt_err_log="$WORK/$name.ipt.err"
_ipt_missing_modules=""
hive_iptables_error_text() {
  _hive_ipt_err_text="\$(cat "\$1" 2>/dev/null || true)"
  [ -n "\$_hive_ipt_err_text" ] || _hive_ipt_err_text="no stderr captured"
  printf '%s' "\$_hive_ipt_err_text"
  unset _hive_ipt_err_text
}
hive_flush_iptables_proxy_chain() {
  while \$IPT -t nat -D OUTPUT -j HIVE_PROXY 2>/dev/null; do :; done
  \$IPT -t nat -F HIVE_PROXY 2>/dev/null || true
}
. "$BLOCK"
echo "PREFLIGHT_SURVIVED=true"
SCRIPT
  set +e
  bash "$script" > "$CASE_OUT" 2>&1
  CASE_CODE=$?
  set -e
}

# ── Case 1: xt_REDIRECT missing (the vllm-d worker-2-4ptps shape) ────────────
run_preflight redirect-missing redirect false
[ "$CASE_CODE" = 77 ] || fail "missing xt_REDIRECT must exit 77, got $CASE_CODE" "$(cat "$CASE_OUT")"
assert_contains "$CASE_OUT" 'xt_REDIRECT'
assert_contains "$CASE_OUT" 'FATAL'
assert_contains "$CASE_OUT" '/etc/modules-load.d/'
assert_not_contains "$CASE_OUT" 'PREFLIGHT_SURVIVED=true'
# The partial-chain teardown ran, and the probe chain never reached OUTPUT.
assert_contains "$CASE_CALLS" '-F HIVE_PROXY'
assert_not_contains "$CASE_CALLS" '-A OUTPUT -j HIVE_PROXY_PREFLIGHT'

# ── Case 2: xt_mark missing - the exemption the gate cannot do without ───────
run_preflight mark-missing mark false
[ "$CASE_CODE" = 77 ] || fail "missing xt_mark must exit 77, got $CASE_CODE" "$(cat "$CASE_OUT")"
assert_contains "$CASE_OUT" 'xt_mark'
assert_contains "$CASE_CALLS" '-F HIVE_PROXY'

# ── Case 3: BOTH missing - the operator gets both names on one line ──────────
run_preflight both-missing 'mark,redirect,owner' false
[ "$CASE_CODE" = 77 ] || fail "both modules missing must exit 77, got $CASE_CODE" "$(cat "$CASE_OUT")"
assert_contains "$CASE_OUT" 'xt_mark, xt_REDIRECT'
assert_contains "$CASE_OUT" 'xt_owner'

# ── Case 4: xt_owner missing ALONE stays non-fatal (OpenShift/OVN) ───────────
run_preflight owner-only owner false
[ "$CASE_CODE" = 0 ] || fail "missing xt_owner alone must not abort, got $CASE_CODE" "$(cat "$CASE_OUT")"
assert_contains "$CASE_OUT" 'PREFLIGHT_SURVIVED=true'
assert_contains "$CASE_OUT" 'xt_owner'
assert_not_contains "$CASE_OUT" 'FATAL'

# ── Case 5: everything present - preflight is a no-op and cleans up ──────────
run_preflight all-present '' false
[ "$CASE_CODE" = 0 ] || fail "healthy node must pass preflight, got $CASE_CODE" "$(cat "$CASE_OUT")"
assert_contains "$CASE_OUT" 'PREFLIGHT_SURVIVED=true'
assert_not_contains "$CASE_OUT" 'FATAL'
# The throwaway chain is deleted whether or not the probes passed.
assert_contains "$CASE_CALLS" '-X HIVE_PROXY_PREFLIGHT'
assert_not_contains "$CASE_CALLS" '-F HIVE_PROXY '

# ── Case 6: the explicit advisory opt-in still wins, and still says so ───────
run_preflight advisory-optin redirect true
[ "$CASE_CODE" = 0 ] || fail "HIVE_PROXY_ADVISORY_OK=true must not exit 77, got $CASE_CODE" "$(cat "$CASE_OUT")"
assert_contains "$CASE_OUT" 'PREFLIGHT_SURVIVED=true'
assert_contains "$CASE_OUT" 'HIVE_PROXY_ADVISORY_OK=true'
assert_contains "$CASE_OUT" 'xt_REDIRECT'
# Even on the opt-in path the partial chain is torn down first.
assert_contains "$CASE_CALLS" '-F HIVE_PROXY'

# ── Case 7: probe chain uncreatable is NOT reported as a missing module ──────
run_preflight chain-uncreatable chain false
[ "$CASE_CODE" = 0 ] || fail "unusable probe chain must fall through, got $CASE_CODE" "$(cat "$CASE_OUT")"
assert_contains "$CASE_OUT" 'PREFLIGHT_SURVIVED=true'
assert_not_contains "$CASE_OUT" 'FATAL'
assert_not_contains "$CASE_OUT" 'xt_REDIRECT'

echo 'PASS: entrypoint netfilter extension preflight names the module and exits 77 (#6003)'
