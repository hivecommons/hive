#!/usr/bin/env bash
# Tests for stream-watchdog.sh's decision logic: a fake "ffmpeg" (a sleep whose
# pid and start time are published the way ffmpeg_loop does), a progress file
# the test advances or freezes, and TWITCH_LIVE_CHECK_CMD stubbed to read a
# verdict file. No Xvfb, Chromium, ffmpeg or network involved.
#
#   bash src/deploy/stream/test-stream-watchdog.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
WATCHDOG="$HERE/stream-watchdog.sh"
ROOT="$(mktemp -d)"
trap 'cleanup_case; rm -rf "$ROOT"' EXIT
fails=0

# ---- harness ------------------------------------------------------------------
RUN_DIR="" WD_PID="" FF_PID="" PROG_PID="" WD_LOG=""
# Fake ffmpeg_loop: relaunch a sleep whenever the previous one dies, publishing
# ffmpeg.pid and ffmpeg.started like entrypoint.sh (started is backdated by
# $1 s so the test chooses whether it is inside the grace window).
fake_ffmpeg_loop() {
  local backdate=$1 p
  while :; do
    sleep 300 & p=$!
    echo "$p" > "$RUN_DIR/ffmpeg.pid"; echo $(( $(date +%s) - backdate )) > "$RUN_DIR/ffmpeg.started"
    echo "$p" >> "$RUN_DIR/ffmpeg.launches"
    wait "$p" 2>/dev/null || true
  done
}
# Fake progress: append a growing frame counter unless $RUN_DIR/freeze exists.
fake_progress() {
  local n=0
  while :; do
    [ -e "$RUN_DIR/freeze" ] || { n=$(( n + 30 )); printf 'frame=%d\nfps=30\nbitrate=4500kbits/s\nspeed=1.0x\ndrop_frames=0\ndup_frames=0\nprogress=continue\n' "$n" >> "$RUN_DIR/ffmpeg-progress.txt"; }
    sleep 0.1
  done
}
start_case() { # name backdate_seconds [ENV=VAL…]
  local name=$1 backdate=$2; shift 2
  RUN_DIR="$ROOT/$name"; mkdir -p "$RUN_DIR"; WD_LOG="$RUN_DIR/watchdog.log"
  : > "$RUN_DIR/ffmpeg-progress.txt"; : > "$RUN_DIR/verdict"; : > "$RUN_DIR/checks"
  fake_ffmpeg_loop "$backdate" & FF_PID=$!
  fake_progress & PROG_PID=$!
  sleep 0.3
  env -i PATH="$PATH" HOME="$HOME" RUN_DIR="$RUN_DIR" \
    WATCHDOG_EVERY=0.2 LIVE_CHECK_EVERY=0 LIVE_GRACE_SECONDS=0 \
    STALL_FRAMES_SECONDS=1 SLOW_SECONDS=1 \
    LIVE_RESTART_BACKOFF_MIN=1 LIVE_RESTART_BACKOFF_MAX=2 \
    TWITCH_LIVE_CHECK_CMD="echo check >> '$RUN_DIR/checks'; cat '$RUN_DIR/verdict'" \
    "$@" bash "$WATCHDOG" > "$WD_LOG" 2>&1 & WD_PID=$!
}
cleanup_case() {
  for p in "$WD_PID" "$PROG_PID" "$FF_PID"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done
  [ -n "$FF_PID" ] && { kill "$(cat "$RUN_DIR/ffmpeg.pid" 2>/dev/null)" 2>/dev/null || true; }
  wait 2>/dev/null || true
  WD_PID="" PROG_PID="" FF_PID=""
}
set_verdict() { printf '%s\n' "$1" > "$RUN_DIR/verdict"; }
launches() { wc -l < "$RUN_DIR/ffmpeg.launches" | tr -d ' '; }
launches_is() { [ "$(launches)" = "$1" ]; }
launches_at_least() { [ "$(launches)" -ge "$1" ]; }
heartbeat_mtime() { stat -c %Y "$RUN_DIR/stream-healthy" 2>/dev/null || stat -f %m "$RUN_DIR/stream-healthy" 2>/dev/null || echo 0; }
# wait_for <seconds> <command…>: poll until the command succeeds.
wait_for() { local t=$1; shift; local i; for (( i = 0; i < t * 10; i++ )); do "$@" && return 0; sleep 0.1; done; return 1; }
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; echo "--- watchdog log ($WD_LOG):"; sed 's/^/    /' "$WD_LOG" 2>/dev/null || true; fails=$(( fails + 1 )); }
check() { local msg=$1; shift; if "$@"; then pass "$msg"; else fail "$msg"; fi; }
log_has() { grep -q -- "$1" "$WD_LOG"; }

# ---- 1. live channel: heartbeat, one "live" line, ffmpeg untouched -------------
start_case live 1000 TWITCH_CHANNEL_LOGIN=chan
set_verdict live
check "live: heartbeat file appears" wait_for 3 test -e "$RUN_DIR/stream-healthy"
sleep 1
check "live: logs 'live on twitch.tv/chan' exactly once" test "$(grep -c 'live on twitch.tv/chan' "$WD_LOG")" = 1
check "live: ffmpeg was never restarted" test "$(launches)" = 1
check "live: health.json says twitch=live" grep -q '"twitch":"live"' "$RUN_DIR/health.json"
cleanup_case

# ---- 2. offline: N misses restart ffmpeg, heartbeat withheld meanwhile ---------
start_case offline 1000 TWITCH_CHANNEL_LOGIN=chan LIVE_OFFLINE_THRESHOLD=3 LIVE_RESTART_LIMIT=5
set_verdict live
check "offline: starts live" wait_for 3 log_has 'live on twitch.tv/chan'
hb0=$(heartbeat_mtime); sleep 1.1
set_verdict offline
check "offline: ffmpeg relaunched after 3 misses" wait_for 5 launches_is 2
check "offline: counted misses are logged 1/3..3/3" log_has 'offline on twitch.tv/chan (3/3)'
check "offline: restart line names the attempt" log_has '(3/3) → restarting ffmpeg (1/5)'
check "offline: heartbeat kept while live" test "$(heartbeat_mtime)" -ge "$hb0"
# Backoff: the hold is grace (0) + backoff (1 s, then 2 s); no second restart inside it.
sleep 0.5
check "offline: no relaunch inside the backoff hold" test "$(launches)" = 2
set_verdict live
check "offline: recovery logs live again with the restart count" wait_for 5 log_has 'live on twitch.tv/chan (after 1 ffmpeg restart(s))'
check "offline: live_restarts reset in health.json" wait_for 2 grep -q '"live_restarts":0' "$RUN_DIR/health.json"
cleanup_case

# ---- 3. give up: M relaunches still offline → non-zero exit -------------------
start_case giveup 1000 TWITCH_CHANNEL_LOGIN=chan LIVE_OFFLINE_THRESHOLD=1 LIVE_RESTART_LIMIT=2
set_verdict offline
rc=0; wait "$WD_PID" || rc=$?; WD_PID=""
check "give-up: watchdog exits 3 after LIVE_RESTART_LIMIT relaunches" test "$rc" = 3
check "give-up: exit is logged" log_has 'still offline after 2 ffmpeg restarts; exiting'
check "give-up: ffmpeg was relaunched once before exiting" test "$(launches)" = 2
check "give-up: health.json records giving-up" grep -q '"state":"giving-up"' "$RUN_DIR/health.json"
cleanup_case

# ---- 4. grace window: offline does not count right after an ffmpeg start ------
start_case grace 0 TWITCH_CHANNEL_LOGIN=chan LIVE_GRACE_SECONDS=600 LIVE_OFFLINE_THRESHOLD=1
set_verdict offline
sleep 1.5
check "grace: no restart inside LIVE_GRACE_SECONDS" test "$(launches)" = 1
check "grace: Twitch was not asked" test ! -s "$RUN_DIR/checks"
check "grace: heartbeat still written" test -e "$RUN_DIR/stream-healthy"
check "grace: logged once" test "$(grep -c 'grace period' "$WD_LOG")" = 1
cleanup_case

# ---- 5. unknown (lookup error) never counts -----------------------------------
start_case unknown 1000 TWITCH_CHANNEL_LOGIN=chan LIVE_OFFLINE_THRESHOLD=1
set_verdict garbage
sleep 1.5
check "unknown: no restart on a failed lookup" test "$(launches)" = 1
check "unknown: warned once, not per tick" test "$(grep -c 'returned no answer' "$WD_LOG")" = 1
check "unknown: heartbeat still written" test -e "$RUN_DIR/stream-healthy"
cleanup_case

# ---- 6. no TWITCH_CHANNEL_LOGIN: progress-only ---------------------------------
start_case nologin 1000
sleep 1
check "no login: live check never runs" test ! -s "$RUN_DIR/checks"
check "no login: heartbeat from progress alone" test -e "$RUN_DIR/stream-healthy"
check "no login: announced as progress-only" log_has 'progress-only'
cleanup_case

# ---- 7. stall: frozen progress file restarts ffmpeg ---------------------------
start_case stall 1000 TWITCH_CHANNEL_LOGIN=chan
set_verdict live
check "stall: running first" wait_for 3 test -e "$RUN_DIR/stream-healthy"
touch "$RUN_DIR/freeze"
check "stall: ffmpeg restarted after STALL_FRAMES_SECONDS" wait_for 5 launches_at_least 2
check "stall: reason logged" log_has 'no new frames for'
rm -f "$RUN_DIR/freeze"
cleanup_case

echo
if (( fails > 0 )); then echo "$fails check(s) FAILED"; exit 1; fi
echo "all stream-watchdog checks passed"
