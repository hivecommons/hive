#!/usr/bin/env bash
# Headless Activity Monitor streamer (hivecommons/hive#15594).
#
# Xvfb ──────► Chromium --kiosk ─► (loopback authproxy) ─► dashboard
#   │              │ tracker sounds (Web Audio)
#   │              ▼
#   │     PulseAudio null sink "hive" ─► hive.monitor
#   │                                        │
#   └── ffmpeg x11grab ◄────────────────────┘ (-f pulse) ─► Twitch RTMP
#
# STREAM_AUDIO=pulse (default) captures the dashboard's merge-zap / new-item
# buzz through the null sink; STREAM_AUDIO=silent skips PulseAudio, mutes
# Chromium and synthesises silence with anullsrc (Twitch still needs a track).
#
# Every process is supervised here: Chromium and ffmpeg are restarted with
# backoff when they exit, the page is reloaded every RELOAD_EVERY so a rolled
# SPA is picked up, and a progress file for the healthcheck is kept fresh by
# ffmpeg itself. Nothing in this script ever prints a secret.
set -euo pipefail

: "${DASHBOARD_URL:?DASHBOARD_URL is required (https origin of the hive dashboard)}"
DASHBOARD_TOKEN="${DASHBOARD_TOKEN:-}"
DASHBOARD_PATH="${DASHBOARD_PATH:-/?kiosk=1#section-activity-monitor-section}"
TWITCH_INGEST="${TWITCH_INGEST:-rtmp://live.twitch.tv/app}"
TWITCH_STREAM_KEY="${TWITCH_STREAM_KEY:-}"
# STREAM_OUTPUT overrides the RTMP sink: "null" discards (dry run), a path
# writes an FLV file, anything else is used verbatim as an ffmpeg output URL.
STREAM_OUTPUT="${STREAM_OUTPUT:-}"
STREAM_SECONDS="${STREAM_SECONDS:-0}"        # >0 = stop ffmpeg after N seconds (dry run)
VIDEO_SIZE="${VIDEO_SIZE:-1920x1080}"
FPS="${FPS:-30}"
BITRATE="${BITRATE:-4500k}"
X264_PRESET="${X264_PRESET:-veryfast}"
RELOAD_EVERY="${RELOAD_EVERY:-30m}"
STREAM_AUDIO="${STREAM_AUDIO:-pulse}"          # pulse | silent
PULSE_SINK_NAME="${PULSE_SINK_NAME:-hive}"
DISPLAY_NUM="${DISPLAY_NUM:-:99}"
AUTHPROXY_LISTEN="${AUTHPROXY_LISTEN:-127.0.0.1:8099}"
RUN_DIR="${RUN_DIR:-/run/stream}"
BACKOFF_MIN=2
BACKOFF_MAX=60
KEYINT=$(( FPS * 2 ))                          # Twitch wants a 2 s keyframe interval
BUFSIZE="$(( ${BITRATE%k} * 2 ))k"

case "$STREAM_AUDIO" in pulse|silent) ;; *) echo "STREAM_AUDIO must be 'pulse' or 'silent' (got '$STREAM_AUDIO')" >&2; exit 1 ;; esac

export DISPLAY="$DISPLAY_NUM" AUTHPROXY_LISTEN DASHBOARD_URL DASHBOARD_TOKEN
mkdir -p "$RUN_DIR/profile"; rm -f "$RUN_DIR/done"
# Per-user PulseAudio under RUN_DIR: the pod's only writable volume besides
# HOME, and the socket path Chromium and ffmpeg both resolve via PULSE_SERVER.
export XDG_RUNTIME_DIR="$RUN_DIR/pulse" PULSE_SERVER="unix:$RUN_DIR/pulse/native"
mkdir -p "$XDG_RUNTIME_DIR"; chmod 0700 "$XDG_RUNTIME_DIR"
PROGRESS="$RUN_DIR/ffmpeg-progress.txt"
: > "$PROGRESS"
log() { printf '%s stream: %s\n' "$(date -u +%FT%TZ)" "$*"; }

pids=()
cleanup() { trap - EXIT INT TERM; log "shutting down"; kill "${pids[@]}" 2>/dev/null || true; wait 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# ---- 1. Xvfb -----------------------------------------------------------------
Xvfb "$DISPLAY_NUM" -screen 0 "${VIDEO_SIZE}x24" -nolisten tcp -nocursor +extension RANDR >/dev/null 2>&1 &
pids+=($!)
for _ in $(seq 1 50); do xdpyinfo -display "$DISPLAY_NUM" >/dev/null 2>&1 && break; sleep 0.2; done
xdpyinfo -display "$DISPLAY_NUM" >/dev/null 2>&1 || { log "Xvfb failed to start"; exit 1; }
log "Xvfb up on $DISPLAY_NUM (${VIDEO_SIZE})"

# ---- 1b. PulseAudio null sink (STREAM_AUDIO=pulse) --------------------------
if [ "$STREAM_AUDIO" = pulse ]; then
  # Foreground child under the supervisor (not --start/--daemonize, which
  # drops the --load list). -n: no default.pa, so no udev/ALSA probing in a
  # pod; only the native socket and the null sink are loaded.
  # --exit-idle-time=-1 keeps it alive between Chromium restarts.
  pulseaudio --exit-idle-time=-1 --disallow-exit --realtime=no --high-priority=no \
    --log-target=stderr --log-level=error -n \
    --load="module-native-protocol-unix socket=$RUN_DIR/pulse/native" \
    --load="module-null-sink sink_name=$PULSE_SINK_NAME sink_properties=device.description=$PULSE_SINK_NAME" \
    > >(sed -u 's/^/pulseaudio: /') 2>&1 &
  pids+=($!)
  for _ in $(seq 1 50); do pactl info >/dev/null 2>&1 && break; sleep 0.2; done
  pactl info >/dev/null 2>&1 || { log "pulseaudio failed to start"; exit 1; }
  pactl list sinks short | awk '{print $2}' | grep -qx "$PULSE_SINK_NAME" \
    || pactl load-module module-null-sink "sink_name=$PULSE_SINK_NAME" "sink_properties=device.description=$PULSE_SINK_NAME" >/dev/null
  for _ in $(seq 1 50); do pactl list sinks short | awk '{print $2}' | grep -qx "$PULSE_SINK_NAME" && break; sleep 0.2; done
  pactl list sinks short | awk '{print $2}' | grep -qx "$PULSE_SINK_NAME" || { log "pulseaudio sink $PULSE_SINK_NAME did not appear"; exit 1; }
  pactl set-default-sink "$PULSE_SINK_NAME"
  pactl set-sink-mute "$PULSE_SINK_NAME" 0; pactl set-sink-volume "$PULSE_SINK_NAME" 100%
  log "pulseaudio up; default sink $PULSE_SINK_NAME (ffmpeg captures ${PULSE_SINK_NAME}.monitor)"
fi

# ---- 2. authproxy (adds Authorization: Bearer, token stays off-screen) ------
authproxy -listen "$AUTHPROXY_LISTEN" &
pids+=($!)
for _ in $(seq 1 50); do curl -fsS "http://$AUTHPROXY_LISTEN/-/authproxy/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
curl -fsS "http://$AUTHPROXY_LISTEN/-/authproxy/healthz" >/dev/null 2>&1 || { log "authproxy failed to start"; exit 1; }
KIOSK_URL="http://$AUTHPROXY_LISTEN$DASHBOARD_PATH"
log "authproxy up; kiosk URL $KIOSK_URL -> ${DASHBOARD_URL%%\?*} (token $([ -n "$DASHBOARD_TOKEN" ] && echo set || echo UNSET))"

# ---- 3. Chromium kiosk, restarted with backoff ------------------------------
CHROMIUM_AUDIO_FLAGS=()
[ "$STREAM_AUDIO" = silent ] && CHROMIUM_AUDIO_FLAGS=(--mute-audio)
chromium_loop() {
  local backoff=$BACKOFF_MIN
  while :; do
    local started=$SECONDS
    log "chromium starting"
    chromium \
      --kiosk --no-first-run --no-default-browser-check --disable-gpu \
      --autoplay-policy=no-user-gesture-required ${CHROMIUM_AUDIO_FLAGS[@]+"${CHROMIUM_AUDIO_FLAGS[@]}"} \
      --window-size="${VIDEO_SIZE/x/,}" --window-position=0,0 --force-device-scale-factor=1 \
      --disable-infobars --disable-session-crashed-bubble --noerrdialogs --hide-scrollbars \
      --disable-features=TranslateUI,MediaRouter --check-for-update-interval=31536000 \
      --disable-background-networking --disable-component-update --disable-sync \
      --user-data-dir="$RUN_DIR/profile" --no-sandbox \
      "$KIOSK_URL" >"$RUN_DIR/chromium.log" 2>&1 || true
    (( SECONDS - started > 60 )) && backoff=$BACKOFF_MIN
    log "chromium exited; restarting in ${backoff}s"
    sleep "$backoff"; backoff=$(( backoff * 2 > BACKOFF_MAX ? BACKOFF_MAX : backoff * 2 ))
  done
}
chromium_loop &
pids+=($!)

# ---- 4. periodic reload (new SPA rolls) -------------------------------------
reload_loop() {
  while :; do
    sleep "$RELOAD_EVERY"
    if xdotool search --onlyvisible --class chromium >/dev/null 2>&1; then
      xdotool search --onlyvisible --class chromium | head -1 | xargs -r -I{} xdotool key --window {} F5 && log "page reloaded (RELOAD_EVERY=$RELOAD_EVERY)"
    fi
  done
}
reload_loop &
pids+=($!)

# ---- 5. ffmpeg, restarted with backoff --------------------------------------
sink() {
  case "$STREAM_OUTPUT" in
    null) printf -- '-f null -' ;;
    '')   [ -n "$TWITCH_STREAM_KEY" ] || { log "TWITCH_STREAM_KEY is unset and STREAM_OUTPUT is empty — nothing to stream to"; return 1; }
          printf -- '-f flv %s/%s' "$TWITCH_INGEST" "$TWITCH_STREAM_KEY" ;;
    *.flv) printf -- '-f flv %s' "$STREAM_OUTPUT" ;;
    *)    printf -- '%s' "$STREAM_OUTPUT" ;;
  esac
}
# Audio input for ffmpeg: the null sink's monitor, or synthesised silence.
AUDIO_IN=(-f lavfi -i "anullsrc=channel_layout=stereo:sample_rate=44100")
[ "$STREAM_AUDIO" = pulse ] && AUDIO_IN=(-thread_queue_size 1024 -f pulse -i "${PULSE_SINK_NAME}.monitor")
ffmpeg_loop() {
  local backoff=$BACKOFF_MIN out limit=() rc=0
  out="$(sink)" || exit 1
  (( STREAM_SECONDS > 0 )) && limit=(-t "$STREAM_SECONDS")
  # Give Chromium a moment to paint before the first frame goes out.
  sleep 5
  while :; do
    local started=$SECONDS
    log "ffmpeg starting (${VIDEO_SIZE}@${FPS}, ${BITRATE} CBR, keyint ${KEYINT}, audio ${STREAM_AUDIO}, sink ${out%%/*}…)"
    # shellcheck disable=SC2086
    ffmpeg -nostdin -y -hide_banner -loglevel warning -progress "$PROGRESS" \
      -thread_queue_size 1024 -f x11grab -framerate "$FPS" -video_size "$VIDEO_SIZE" -draw_mouse 0 -i "$DISPLAY_NUM" \
      "${AUDIO_IN[@]}" \
      -c:v libx264 -preset "$X264_PRESET" -tune zerolatency -pix_fmt yuv420p \
      -b:v "$BITRATE" -minrate "$BITRATE" -maxrate "$BITRATE" -bufsize "$BUFSIZE" \
      -g "$KEYINT" -keyint_min "$KEYINT" -sc_threshold 0 -r "$FPS" \
      -c:a aac -b:a 128k -ac 2 -ar 44100 -shortest "${limit[@]}" \
      $out > >(sed -u "s#${TWITCH_STREAM_KEY:-__none__}#***#g") 2>&1 && rc=0 || rc=$?
    if (( STREAM_SECONDS > 0 )); then
      if (( rc != 0 )); then log "ffmpeg bounded run FAILED (exit $rc)"; exit "$rc"; fi
      log "ffmpeg finished a ${STREAM_SECONDS}s bounded run"; : > "$RUN_DIR/done"; return
    fi
    (( SECONDS - started > 60 )) && backoff=$BACKOFF_MIN
    log "ffmpeg exited (rc=$rc); restarting in ${backoff}s"
    sleep "$backoff"; backoff=$(( backoff * 2 > BACKOFF_MAX ? BACKOFF_MAX : backoff * 2 ))
  done
}
ffmpeg_loop &
pids+=($!)

wait -n "${pids[@]}" || true
if [ -e "$RUN_DIR/done" ]; then log "bounded run complete"; exit 0; fi
log "a supervised process left the loop; exiting so the orchestrator restarts the pod"
exit 1
