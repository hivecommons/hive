#!/usr/bin/env bash
# Liveness for the streamer: Chromium is running and ffmpeg pushed frames
# within the last STALL_SECONDS (its -progress file is rewritten per frame
# batch). Exit 1 restarts the container.
set -u
RUN_DIR="${RUN_DIR:-/run/stream}"
STALL_SECONDS="${STALL_SECONDS:-20}"
pgrep -x chromium >/dev/null 2>&1 || pgrep -f 'chromium.*--kiosk' >/dev/null 2>&1 || { echo "chromium not running"; exit 1; }
p="$RUN_DIR/ffmpeg-progress.txt"
[ -s "$p" ] || { echo "ffmpeg has not reported progress yet"; exit 1; }
age=$(( $(date +%s) - $(stat -c %Y "$p") ))
[ "$age" -le "$STALL_SECONDS" ] || { echo "ffmpeg progress stale (${age}s)"; exit 1; }
frames=$(grep -a '^frame=' "$p" | tail -1 | cut -d= -f2)
[ "${frames:-0}" -gt 0 ] || { echo "ffmpeg has encoded 0 frames"; exit 1; }
echo "ok frames=${frames} progress_age=${age}s"
