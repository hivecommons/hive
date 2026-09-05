#!/usr/bin/env bash
# Regression coverage for #6003 beyond test_entrypoint_iptables_gate.sh: the
# shipped IPv4 and IPv6 append blocks must treat the mark plus enforcement plus
# OUTPUT rules as required, while owner exemptions remain optional.

set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENTRYPOINT="$HERE/entrypoint.sh"
WORK="$HERE/.test-entrypoint-egress-ruleset-$$"
BIN="$WORK/bin"
mkdir -p "$BIN"
trap 'rm -rf "$WORK"' EXIT

extract() {
  local family="$1" out="$2"
  awk -v b="HIVE-EGRESS-RULESET-${family}-BEGIN" -v e="HIVE-EGRESS-RULESET-${family}-END" '
    index($0, b) { on = 1; next }
    index($0, e) { on = 0 }
    on { sub(/^        /, ""); sub(/^      /, ""); print }
  ' "$ENTRYPOINT" > "$out"
}

V4="$WORK/ruleset-v4.sh"
V6="$WORK/ruleset-v6.sh"
extract V4 "$V4"
extract V6 "$V6"

if ! grep -q -- 'REDIRECT --to-ports' "$V4"; then
  echo 'FAIL: could not extract IPv4 egress ruleset from entrypoint.sh'
  exit 1
fi
if ! grep -q -- 'REJECT --reject-with' "$V6"; then
  echo 'FAIL: could not extract IPv6 egress ruleset from entrypoint.sh'
  exit 1
fi

cat > "$BIN/iptables-shim" <<'SHIM'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$HIVE_FAKE_IPTABLES_CALLS"

has_failure() {
  case ",${HIVE_FAKE_IPTABLES_FAILS:-}," in
    *",$1,"*) return 0 ;;
    *) return 1 ;;
  esac
}

fail_append() {
  printf 'fake iptables module missing for %s\n' "$1" >&2
  exit 4
}

case "$*" in
  *'-A HIVE_PROXY -m owner'*|*'-A HIVE_PROXY6 -m owner'*) has_failure owner && fail_append owner ;;
  *'-A HIVE_PROXY -m mark'*|*'-A HIVE_PROXY6 -m mark'*) has_failure mark && fail_append mark ;;
  *'-A HIVE_PROXY -p tcp --dport 443 -j REDIRECT'*) has_failure redirect && fail_append redirect ;;
  *'-A HIVE_PROXY6 -p tcp --dport 443 -j REJECT'*) has_failure reject && fail_append reject ;;
  *'-A OUTPUT -j HIVE_PROXY'|*'-A OUTPUT -j HIVE_PROXY6') has_failure output && fail_append output ;;
esac
exit 0
SHIM
chmod +x "$BIN/iptables-shim"

cat > "$BIN/python3" <<'SHIM'
#!/usr/bin/env sh
exit 0
SHIM
chmod +x "$BIN/python3"

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

run_ruleset() {
  local family="$1" block="$2" failures="$3" name="$4"
  local script="$WORK/$name.run.sh" out="$WORK/$name.out" calls="$WORK/$name.calls"
  : > "$calls"
  cat > "$script" <<SCRIPT
set -e
export PATH="$BIN:/usr/bin:/bin"
export HIVE_FAKE_IPTABLES_CALLS="$calls"
export HIVE_FAKE_IPTABLES_FAILS="$failures"
IPT="$BIN/iptables-shim"
IP6T="$BIN/iptables-shim"
PROXY_UID=1001
PROXY_PORT=18443
HIVE_PROXY_EGRESS_MARK=0x1112
_ipt_err_log="$WORK/$name.ipt.err"
_ip6t_err_log="$WORK/$name.ip6t.err"
_ipt_chain_ok=true
_ip6_chain_ok=true
_ipt_try=1
_ip6_try=1
_iptables_ok=false
_ip6tables_ok=false
hive_iptables_error_text() {
  _hive_ipt_err_text="\$(cat "\$1" 2>/dev/null || true)"
  [ -n "\$_hive_ipt_err_text" ] || _hive_ipt_err_text="no stderr captured"
  printf '%s' "\$_hive_ipt_err_text"
  unset _hive_ipt_err_text
}
hive_run_iptables_required() {
  _hive_ipt_desc="\$1"
  _hive_ipt_err_file="\$2"
  shift 2
  if "\$@" 2>"\$_hive_ipt_err_file"; then
    unset _hive_ipt_desc _hive_ipt_err_file
    return 0
  else
    _hive_ipt_rc=\$?
    echo "[entrypoint] ERROR: \${_hive_ipt_desc} failed (exit \${_hive_ipt_rc}): \$(hive_iptables_error_text "\$_hive_ipt_err_file")" >&2
    unset _hive_ipt_desc _hive_ipt_err_file _hive_ipt_rc
    return 1
  fi
}
hive_flush_iptables_proxy_chain() {
  \$IPT -t nat -F HIVE_PROXY 2>/dev/null || true
}
hive_flush_ip6tables_proxy_chain() {
  \$IP6T -F HIVE_PROXY6 2>/dev/null || true
}
. "$block"
if [ "$family" = V4 ]; then
  echo "OK=\$_iptables_ok"
else
  echo "OK=\$_ip6tables_ok"
fi
SCRIPT
  set +e
  bash "$script" > "$out" 2>&1
  local code=$?
  set -e
  printf '%s' "$code" > "$WORK/$name.code"
}

check_case() {
  local family="$1" failures="$2" want_ok="$3" want_jump="$4" name="$5"
  local block="$V4" chain='HIVE_PROXY' hook='-A OUTPUT -j HIVE_PROXY'
  local ok_var='OK=true' flush='-F HIVE_PROXY'
  if [ "$family" = V6 ]; then
    block="$V6"; chain='HIVE_PROXY6'; hook='-A OUTPUT -j HIVE_PROXY6'; flush='-F HIVE_PROXY6'
  fi
  run_ruleset "$family" "$block" "$failures" "$name"
  local code calls out
  code="$(cat "$WORK/$name.code")"
  calls="$WORK/$name.calls"
  out="$WORK/$name.out"
  [ "$code" = 0 ] || fail "$name exited $code under set -e" "$(cat "$out")"
  if [ "$want_ok" = true ]; then
    assert_contains "$out" "$ok_var"
  else
    assert_contains "$out" 'OK=false'
    assert_contains "$calls" "$flush"
  fi
  if [ "$want_jump" = yes ]; then
    assert_contains "$calls" "$hook"
  else
    assert_not_contains "$calls" "$hook"
  fi
  case "$failures" in
    *mark*) assert_contains "$out" 'packet-mark exemption append failed (exit 4)' ;;
    *redirect*) assert_contains "$out" 'HTTPS REDIRECT append failed (exit 4)' ;;
    *reject*) assert_contains "$out" 'IPv6 HTTPS REJECT append failed (exit 4)' ;;
  esac
  assert_contains "$calls" "$chain"
}

check_case V4 '' true yes v4-success
check_case V4 owner true yes v4-owner-optional
check_case V4 mark false no v4-mark-required
check_case V4 redirect false no v4-redirect-required
check_case V6 '' true yes v6-success
check_case V6 owner true yes v6-owner-optional
check_case V6 mark false no v6-mark-required
check_case V6 reject false no v6-reject-required

echo 'PASS: entrypoint egress rulesets judge required append results (#6003)'
