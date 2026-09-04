#!/usr/bin/env bash
# Regression test for #6003: failed non-optional IPv4 iptables appends must be
# captured, cleaned up, and routed through the advisory/fail-closed decision
# instead of escaping through `set -e` as a raw iptables exit code.

set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENTRYPOINT="$HERE/entrypoint.sh"
WORK="$HERE/.test-entrypoint-iptables-gate-$$"
BIN="$WORK/bin"
BODY="$WORK/gate-body.sh"
CALLS="$WORK/calls.log"
mkdir -p "$BIN"
trap 'rm -rf "$WORK"' EXIT

awk '
  /^[[:space:]]*PROXY_PORT=18443$/ { on=1 }
  on && /^[[:space:]]*# Drop to non-root user/ { exit }
  on { sub(/^    /, ""); print }
' "$ENTRYPOINT" > "$WORK/body.raw"
if ! grep -q '^PROXY_PORT=18443$' "$WORK/body.raw"; then
  echo "FAIL: could not extract egress gate block from $ENTRYPOINT"
  exit 1
fi
# The extracted block sits inside an enclosing entrypoint branch whose opener
# is above the marker; run_gate wraps it in an equivalent `if true; then`.
cp "$WORK/body.raw" "$BODY"

cat > "$BIN/iptables-nft" <<'SHIM'
#!/usr/bin/env sh
printf '%s %s\n' "${0##*/}" "$*" >> "$HIVE_FAKE_IPTABLES_CALLS"
case "$*" in
  *-nL*) [ -f "$HIVE_FAKE_IPTABLES_CHAIN" ] && exit 0 || exit 1 ;;
  *'-N HIVE_PROXY'*) : > "$HIVE_FAKE_IPTABLES_CHAIN" ;;
  *'-D OUTPUT -j HIVE_PROXY'*) exit 1 ;;
esac
case "${HIVE_FAKE_IPTABLES_FAIL:-}" in
  mark)
    case "$*" in
      *'-m mark --mark'*|*'-j REDIRECT'*)
        printf 'fake iptables module missing for %s\n' "$*" >&2
        exit 4
        ;;
    esac
    ;;
esac
exit 0
SHIM
chmod +x "$BIN/iptables-nft"

cat > "$BIN/python3" <<'SHIM'
#!/usr/bin/env sh
exit 0
SHIM
cat > "$BIN/sleep" <<'SHIM'
#!/usr/bin/env sh
exit 0
SHIM
chmod +x "$BIN/python3" "$BIN/sleep"

run_gate() {
  local name="$1"
  local advisory="$2"
  local fail_mode="$3"
  local out="$WORK/${name}.out"
  local script="$WORK/${name}.run.sh"
  : > "$CALLS"
  rm -f "$WORK/${name}.chain"
  cat > "$script" <<SCRIPT
set -e
export PATH="$BIN:/usr/bin:/bin"
export HIVE_FAKE_IPTABLES_CALLS="$CALLS"
export HIVE_FAKE_IPTABLES_FAIL="$fail_mode"
export HIVE_FAKE_IPTABLES_CHAIN="$WORK/${name}.chain"
export HIVE_IPTABLES_ERR_LOG="$WORK/${name}.iptables.err"
export HIVE_PROXY_ADVISORY_OK="$advisory"
EXIT_NET_ADMIN_REQUIRED=77
_cap_net_admin_in_bset=true
PROXY_UID=1001
HIVE_PROXY_EGRESS_MARK=0x1112
# Keep this test focused on IPv4; IPv6 is covered by egress_gate_ipv6_test.go.
HIVE_IP6TABLES_ERR_LOG="$WORK/${name}.ip6tables.err"
if true; then
SCRIPT
  cat "$BODY" >> "$script"
  sh "$script" > "$out" 2>&1
  printf '%s' "$?" > "$WORK/${name}.code"
}

assert_contains() {
  local file="$1"
  local needle="$2"
  if ! grep -Fq -- "$needle" "$file"; then
    echo "FAIL: expected $file to contain: $needle"
    echo "--- $file ---"
    cat "$file"
    exit 1
  fi
}

assert_not_contains() {
  local file="$1"
  local needle="$2"
  if grep -Fq -- "$needle" "$file"; then
    echo "FAIL: expected $file not to contain: $needle"
    echo "--- $file ---"
    cat "$file"
    exit 1
  fi
}

run_gate success false ''
if [[ "$(cat "$WORK/success.code")" != 0 ]]; then
  echo "FAIL: successful iptables ruleset exited $(cat "$WORK/success.code")"
  cat "$WORK/success.out"
  exit 1
fi
assert_contains "$CALLS" 'iptables-nft -t nat -A HIVE_PROXY -m mark --mark 0x1112 -j RETURN'
assert_contains "$CALLS" 'iptables-nft -t nat -A HIVE_PROXY -p tcp --dport 443 -j REDIRECT --to-ports 18443'
assert_contains "$CALLS" 'iptables-nft -t nat -A OUTPUT -j HIVE_PROXY'
assert_not_contains "$WORK/success.out" 'ADVISORY-ONLY'
assert_not_contains "$WORK/success.out" 'FATAL'

run_gate advisory true mark
if [[ "$(cat "$WORK/advisory.code")" != 0 ]]; then
  echo "FAIL: advisory iptables failure exited $(cat "$WORK/advisory.code")"
  cat "$WORK/advisory.out"
  exit 1
fi
assert_contains "$WORK/advisory.out" 'iptables packet-mark exemption append failed (exit 4): fake iptables module missing'
assert_contains "$WORK/advisory.out" 'iptables ruleset incomplete; flushed HIVE_PROXY'
assert_contains "$WORK/advisory.out" 'ADVISORY-ONLY'
assert_contains "$CALLS" 'iptables-nft -t nat -F HIVE_PROXY'

run_gate enforcing false mark
if [[ "$(cat "$WORK/enforcing.code")" == 0 ]]; then
  echo "FAIL: enforcing iptables failure unexpectedly exited 0"
  cat "$WORK/enforcing.out"
  exit 1
fi
assert_contains "$WORK/enforcing.out" 'iptables packet-mark exemption append failed (exit 4): fake iptables module missing'
assert_contains "$WORK/enforcing.out" 'iptables ruleset incomplete; flushed HIVE_PROXY'
assert_contains "$WORK/enforcing.out" 'FATAL: could not establish forced proxy egress'
assert_contains "$CALLS" 'iptables-nft -t nat -F HIVE_PROXY'

echo 'PASS: entrypoint iptables gate handles failed appends explicitly (#6003)'
