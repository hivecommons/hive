#!/usr/bin/env bash
# Startup and liveness probe for the streamer. Exit 1 restarts the container.
#
# The signal is the watchdog's heartbeat ($RUN_DIR/stream-healthy), which
# stream-watchdog.sh touches only while ffmpeg's frame counter advances and the
# channel is live on Twitch (or the live check is not configured, or it is in a
# deliberate grace/backoff wait). So a stale heartbeat means one of:
#   - ffmpeg has not pushed frames for HEARTBEAT_MAX_AGE_SECONDS (its own
#     relaunch loop could not get Twitch to accept a session in that time —
#     Twitch keeps a key locked for ~2 min after a publisher drops, so this
#     window must be well above that or the probe cascades restarts),
#   - the channel has been offline on Twitch for that long, or
#   - the watchdog itself is wedged or gone.
# Until the first heartbeat exists only the startup probe runs (its
# failureThreshold bounds the start-up time), so a missing file is a failure.
set -u
RUN_DIR="${RUN_DIR:-/run/stream}"
HEARTBEAT_FILE="${HEARTBEAT_FILE:-$RUN_DIR/stream-healthy}"
HEARTBEAT_MAX_AGE_SECONDS="${HEARTBEAT_MAX_AGE_SECONDS:-300}"
pgrep -x chromium >/dev/null 2>&1 || pgrep -f 'chromium.*--kiosk' >/dev/null 2>&1 || { echo "chromium not running"; exit 1; }
[ -e "$HEARTBEAT_FILE" ] || { echo "no heartbeat yet (ffmpeg has not pushed frames)"; exit 1; }
age=$(( $(date +%s) - $(stat -c %Y "$HEARTBEAT_FILE") ))
# Surface the watchdog's verdict (stream-watchdog.sh writes health.json).
h="$RUN_DIR/health.json"
wd=""
if [ -s "$h" ]; then
  wd=" $(sed -n 's/.*"state":"\([a-z-]*\)","twitch":"\([a-z]*\)","watchdog_restarts":\([0-9]*\),"live_restarts":\([0-9]*\).*/watchdog=\1 twitch=\2 restarts=\3 live_restarts=\4/p' "$h")"
fi
frames=$(grep -a '^frame=' "$RUN_DIR/ffmpeg-progress.txt" 2>/dev/null | tail -1 | cut -d= -f2)
[ "$age" -le "$HEARTBEAT_MAX_AGE_SECONDS" ] || { echo "heartbeat stale (${age}s > ${HEARTBEAT_MAX_AGE_SECONDS}s) frames=${frames:-0}${wd}"; exit 1; }
echo "ok heartbeat_age=${age}s frames=${frames:-0}${wd}"
