#!/usr/bin/env bash
# Smoke test for the crux binary: start `crux serve` on a private unix socket
# with an empty runtime, drive it over the Crux API with `crux --server`
# (ps) and a raw GET of the health route, then stop it and check the server
# exited cleanly. Needs no agents, tmux or network; safe in CI.
#
#   CRUX_BIN   binary under test (default: bin/crux next to this module,
#              built here with `go build` when it does not exist yet)
#   SMOKE_DIR  scratch dir for the socket and work dir (default: .smoke here;
#              keep it short, a unix socket path is limited to ~100 bytes)
set -euo pipefail

here=$(cd "$(dirname "$0")/.." && pwd)
CRUX_BIN=${CRUX_BIN:-$here/bin/crux}
SMOKE_DIR=${SMOKE_DIR:-$here/.smoke}
SOCK=$SMOKE_DIR/sock/crux.sock
WORK=$SMOKE_DIR/work
# How long to wait for the socket to appear, in tenths of a second.
WAIT_TICKS=${WAIT_TICKS:-100}

rm -rf "$SMOKE_DIR"
mkdir -p "$WORK"
chmod 0700 "$SMOKE_DIR"

fail() { echo "smoke: FAIL: $*" >&2; exit 1; }

if [ ! -x "$CRUX_BIN" ]; then
  echo "smoke: building $CRUX_BIN"
  (cd "$here" && go build -o "$CRUX_BIN" ./cmd/crux) || fail "go build ./cmd/crux failed"
fi

echo "smoke: $("$CRUX_BIN" --version)"

export HIVE_WORK_DIR=$WORK
unset CRUX_SERVER CRUX_TOKEN CRUX_DEFS CRUX_SOCKET

"$CRUX_BIN" serve --socket "$SOCK" >"$SMOKE_DIR/serve.out" 2>"$SMOKE_DIR/serve.err" &
SERVE_PID=$!
cleanup() {
  if kill -0 "$SERVE_PID" 2>/dev/null; then
    kill "$SERVE_PID" 2>/dev/null || true
    wait "$SERVE_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

for _ in $(seq "$WAIT_TICKS"); do
  [ -S "$SOCK" ] && break
  kill -0 "$SERVE_PID" 2>/dev/null || { cat "$SMOKE_DIR/serve.err" >&2; fail "crux serve exited before listening"; }
  sleep 0.1
done
[ -S "$SOCK" ] || { cat "$SMOKE_DIR/serve.err" >&2; fail "no socket at $SOCK after $((WAIT_TICKS / 10))s"; }
echo "smoke: serving on $SOCK"

# Remote mode over the socket: an empty runtime lists no agents and exits 0.
"$CRUX_BIN" --server "$SOCK" ps >"$SMOKE_DIR/ps.out" || { cat "$SMOKE_DIR/ps.out" >&2; fail "crux --server ps failed"; }
echo "smoke: ps ok ($(wc -l <"$SMOKE_DIR/ps.out" | tr -d ' ') line(s))"

# The JSON form must be a (possibly empty) list.
"$CRUX_BIN" --server "$SOCK" ps --json >"$SMOKE_DIR/ps.json" || fail "crux --server ps --json failed"
case "$(tr -d ' \n' <"$SMOKE_DIR/ps.json")" in
  \[*\]|null) ;;
  *) cat "$SMOKE_DIR/ps.json" >&2; fail "ps --json did not print a list" ;;
esac

# Health: the API's GET /v1/health, the equivalent of a `crux health` verb.
if command -v curl >/dev/null 2>&1; then
  health=$(curl -fsS --unix-socket "$SOCK" http://crux/v1/health) || fail "GET /v1/health failed"
  echo "smoke: health $health"
else
  echo "smoke: curl not found; skipping GET /v1/health"
fi

# A missing token must not be needed on the socket; an unknown agent is a
# clean usage-class error, not a crash.
if "$CRUX_BIN" --server "$SOCK" stop no-such-agent >/dev/null 2>"$SMOKE_DIR/stop.err"; then
  fail "stop of an unknown agent succeeded"
fi
kill -0 "$SERVE_PID" 2>/dev/null || fail "crux serve died during the run"

kill -INT "$SERVE_PID"
trap - EXIT
if ! wait "$SERVE_PID"; then
  cat "$SMOKE_DIR/serve.err" >&2
  fail "crux serve exited non-zero on SIGINT"
fi
grep -q "serve stopped" "$SMOKE_DIR/serve.out" || { cat "$SMOKE_DIR/serve.out" >&2; fail "serve did not report a clean stop"; }
[ -S "$SOCK" ] && fail "socket $SOCK left behind after stop"
echo "smoke: PASS"
