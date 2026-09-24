#!/usr/bin/env bash
set -euo pipefail

# CI guard for the exact bob argv hive launches. This intentionally does not
# use `--help`: bobshell 2.x accepts unknown options when help is requested,
# which is how the removed --auth-method flag escaped the pin bump smoke.

BOB_BIN="${BOB_BIN:-bob}"
GUARD_HOME="${HIVE_BOB_FLAG_GUARD_HOME:-$PWD/.bob-flag-guard-home}"
mkdir -p "$GUARD_HOME"
cleanup() { rm -rf "$GUARD_HOME"; }
trap cleanup EXIT

version="$("$BOB_BIN" --version 2>/dev/null | sed -n '1p')"
case "$version" in
  1.*)
    argv=(--accept-license --auth-method api-key --approval-mode yolo --trust)
    ;;
  *)
    argv=(chat --accept-license --auto-approve --trust --max-turns 1)
    ;;
esac

echo "bob version: ${version:-unknown}"
printf 'checking argv: %q' "$BOB_BIN"
printf ' %q' "${argv[@]}"
printf '\n'

out="$GUARD_HOME/out.txt"
err="$GUARD_HOME/err.txt"
set +e
if command -v timeout >/dev/null 2>&1; then
  HOME="$GUARD_HOME" BOBSHELL_API_KEY="flag-guard-key" BOB_API_KEY="flag-guard-key" \
    timeout --signal=TERM --kill-after=5s 10s "$BOB_BIN" "${argv[@]}" >"$out" 2>"$err"
else
  HOME="$GUARD_HOME" BOBSHELL_API_KEY="flag-guard-key" BOB_API_KEY="flag-guard-key" \
    perl -e 'alarm 10; exec @ARGV' "$BOB_BIN" "${argv[@]}" >"$out" 2>"$err"
fi
rc=$?
set -e

combined="$(cat "$out" "$err" 2>/dev/null || true)"
if printf '%s' "$combined" | grep -qiE 'unknown option|unknown argument|Invalid argument.*unknown'; then
  echo "$combined"
  echo "bob rejected one of hive's launch flags" >&2
  exit 1
fi

case "$rc" in
  0|1|124|143)
    echo "bob did not reject hive's launch flags (exit $rc)"
    ;;
  *)
    echo "$combined"
    echo "bob launch flag guard exited unexpectedly ($rc)" >&2
    exit "$rc"
    ;;
esac
