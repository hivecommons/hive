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
# SPA is picked up (or sooner, when Chromium's RSS crosses
# CHROMIUM_RSS_RELOAD_MIB), and a progress file for the healthcheck is kept
# fresh by ffmpeg itself. Nothing in this script ever prints a secret.
#
# Memory budget (hivecommons/hive OOM incident, 16x OOMKilled at 3 Gi): the
# x11grab input hands ffmpeg one MIT-SHM segment per raw 1080p frame (8.3 MiB),
# shared with Xvfb and charged to the pod's memory cgroup once. If x264 falls
# behind — Chromium's startup burst, a CPU-throttled pod — the input thread
# queues frames up to -thread_queue_size, so a 1024-deep queue can pin 8.5 GiB.
# VIDEO_QUEUE_FRAMES bounds that (256 frames ≈ 2.1 GiB worst case, inside the
# 6 Gi limit); beyond it frames are skipped instead of hoarded. Too shallow a
# queue (the old 16) drops frames on every encoder hiccup, and each drop is a
# PTS gap the Twitch player reads as "buffering" (hivecommons/hive#16440).
# Audio packets are tiny, so the audio queue stays deep.
#
# Stall watchdog: ffmpeg's -progress file is sampled every WATCHDOG_EVERY s;
# no new frames for STALL_FRAMES_SECONDS or speed < SLOW_SPEED for SLOW_SECONDS
# logs WARN with the stats and SIGTERMs ffmpeg so ffmpeg_loop restarts it. The
# last 20 samples, the last stall and the restart count are published to
# $RUN_DIR/health.json for the healthcheck and for operators.
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
# Reload the tab early when all Chromium processes together exceed this RSS
# (MiB), well under the pod limit, instead of letting the kernel pick a victim.
CHROMIUM_RSS_RELOAD_MIB="${CHROMIUM_RSS_RELOAD_MIB:-3584}"
RSS_CHECK_EVERY="${RSS_CHECK_EVERY:-30}"       # seconds between RSS checks
VIDEO_QUEUE_FRAMES="${VIDEO_QUEUE_FRAMES:-256}" # x11grab -thread_queue_size (raw frames, ~8 MiB each at 1080p)
# -thread_queue_size for the audio input (packets); AUDIO_QUEUE_PACKETS is the
# older name and still honoured.
AUDIO_QUEUE="${AUDIO_QUEUE:-${AUDIO_QUEUE_PACKETS:-1024}}"
STREAM_AUDIO="${STREAM_AUDIO:-pulse}"          # pulse | silent
PULSE_SINK_NAME="${PULSE_SINK_NAME:-hive}"
DISPLAY_NUM="${DISPLAY_NUM:-:99}"
AUTHPROXY_LISTEN="${AUTHPROXY_LISTEN:-127.0.0.1:8099}"
RUN_DIR="${RUN_DIR:-/run/stream}"
WATCHDOG_EVERY="${WATCHDOG_EVERY:-10}"          # seconds between progress-file samples
STALL_FRAMES_SECONDS="${STALL_FRAMES_SECONDS:-30}" # restart ffmpeg after this long with no new frames
SLOW_SPEED="${SLOW_SPEED:-0.90}"                # restart ffmpeg when speed stays below this…
SLOW_SECONDS="${SLOW_SECONDS:-60}"              # …for this long
HEALTH_SAMPLES="${HEALTH_SAMPLES:-20}"          # progress samples kept in health.json
BACKOFF_MIN=2
BACKOFF_MAX=60
KEYINT=$(( FPS * 2 ))                          # Twitch wants a 2 s keyframe interval
BUFSIZE="$(( ${BITRATE%k} * 2 ))k"

# nproc reports the node's cores inside a pod; x264 would spawn 1.5x that and
# thrash against the CPU quota. Size the encoder from the cgroup quota instead.
cgroup_cpus() {
  local q p v2=/sys/fs/cgroup/cpu.max v1=/sys/fs/cgroup/cpu/cpu.cfs_quota_us
  if [ -r "$v2" ] && read -r q p < "$v2" && [ "$q" != max ]; then
    echo $(( (q + p - 1) / p )); return
  fi
  if [ -r "$v1" ] && read -r q < "$v1" && read -r p < "${v1%quota_us}period_us" && [ "$q" -gt 0 ]; then
    echo $(( (q + p - 1) / p )); return
  fi
  nproc
}
ENC_THREADS="${ENC_THREADS:-$(cgroup_cpus)}"

# Duration strings for sleep (30m, 2h, 90s, 45) → seconds, for the reload timer.
to_seconds() {
  case "$1" in
    *h) echo $(( ${1%h} * 3600 )) ;;
    *m) echo $(( ${1%m} * 60 )) ;;
    *s) echo $(( ${1%s} )) ;;
    *)  echo $(( $1 )) ;;
  esac
}
RELOAD_SECONDS="$(to_seconds "$RELOAD_EVERY")"

case "$STREAM_AUDIO" in pulse|silent) ;; *) echo "STREAM_AUDIO must be 'pulse' or 'silent' (got '$STREAM_AUDIO')" >&2; exit 1 ;; esac

# STREAM_PROFILE=public-safe (the hivecommonsfleet deployment, #15966) fails
# closed here, before any process starts; unset, the gate is a no-op.
"$(dirname "$0")/public-safe-gate.sh"

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
      --disable-extensions --disable-dev-shm-usage --renderer-process-limit=1 \
      --js-flags=--max-old-space-size=1024 \
      --user-data-dir="$RUN_DIR/profile" --no-sandbox \
      "$KIOSK_URL" >"$RUN_DIR/chromium.log" 2>&1 || true
    (( SECONDS - started > 60 )) && backoff=$BACKOFF_MIN
    log "chromium exited; restarting in ${backoff}s"
    sleep "$backoff"; backoff=$(( backoff * 2 > BACKOFF_MAX ? BACKOFF_MAX : backoff * 2 ))
  done
}
chromium_loop &
pids+=($!)

# ---- 4. periodic reload (new SPA rolls) + Chromium RSS guard ---------------
reload_tab() {
  xdotool search --onlyvisible --class chromium >/dev/null 2>&1 || return 1
  xdotool search --onlyvisible --class chromium | head -1 | xargs -r -I{} xdotool key --window {} F5
}
# Sum of RSS (MiB) over every chromium process (browser, renderers, gpu, utility).
chromium_rss_mib() { ps -C chromium -o rss= 2>/dev/null | awk '{s+=$1} END{printf "%d", s/1024}'; }
reload_loop() {
  local since=0 rss
  while :; do
    sleep "$RSS_CHECK_EVERY"; since=$(( since + RSS_CHECK_EVERY ))
    rss="$(chromium_rss_mib)"
    if (( rss > CHROMIUM_RSS_RELOAD_MIB )); then
      reload_tab && log "chromium RSS ${rss}MiB > ${CHROMIUM_RSS_RELOAD_MIB}MiB; page reloaded early"
      since=0
    elif (( since >= RELOAD_SECONDS )); then
      reload_tab && log "page reloaded (RELOAD_EVERY=$RELOAD_EVERY, chromium RSS ${rss}MiB)"
      since=0
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
# Both go through aresample=async=1 so audio PTS is stretched/padded to the
# wallclock video timeline instead of drifting away from it.
AUDIO_IN=(-thread_queue_size "$AUDIO_QUEUE" -f lavfi -i "anullsrc=channel_layout=stereo:sample_rate=44100")
[ "$STREAM_AUDIO" = pulse ] && AUDIO_IN=(-thread_queue_size "$AUDIO_QUEUE" -f pulse -i "${PULSE_SINK_NAME}.monitor")
FFMPEG_PID="$RUN_DIR/ffmpeg.pid"
HEALTH_JSON="$RUN_DIR/health.json"
ffmpeg_loop() {
  local backoff=$BACKOFF_MIN out limit=() rc=0 fpid
  out="$(sink)" || exit 1
  (( STREAM_SECONDS > 0 )) && limit=(-t "$STREAM_SECONDS")
  # Give Chromium a moment to paint before the first frame goes out.
  sleep 5
  while :; do
    local started=$SECONDS
    log "ffmpeg starting (${VIDEO_SIZE}@${FPS}, ${BITRATE} CBR, keyint ${KEYINT}, x264 threads ${ENC_THREADS}, video queue ${VIDEO_QUEUE_FRAMES} frames, audio ${STREAM_AUDIO} queue ${AUDIO_QUEUE}, sink ${out%%/*}…)"
    # shellcheck disable=SC2086
    ffmpeg -nostdin -y -hide_banner -loglevel warning -progress "$PROGRESS" \
      -thread_queue_size "$VIDEO_QUEUE_FRAMES" -fflags +genpts -use_wallclock_as_timestamps 1 \
      -f x11grab -framerate "$FPS" -video_size "$VIDEO_SIZE" -draw_mouse 0 -i "$DISPLAY_NUM" \
      "${AUDIO_IN[@]}" \
      -c:v libx264 -preset "$X264_PRESET" -tune zerolatency -pix_fmt yuv420p -threads "$ENC_THREADS" \
      -b:v "$BITRATE" -minrate "$BITRATE" -maxrate "$BITRATE" -bufsize "$BUFSIZE" \
      -g "$KEYINT" -keyint_min "$KEYINT" -sc_threshold 0 -r "$FPS" \
      -af aresample=async=1 -c:a aac -b:a 128k -ac 2 -ar 44100 -shortest "${limit[@]}" \
      $out > >(sed -u "s#${TWITCH_STREAM_KEY:-__none__}#***#g") 2>&1 &
    fpid=$!
    echo "$fpid" > "$FFMPEG_PID"
    wait "$fpid" && rc=0 || rc=$?
    rm -f "$FFMPEG_PID"
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

# Last value of key= in the tail of the progress file ("" if absent).
progress_val() { { printf '%s\n' "$1" | grep -a "^$2=" | tail -1 | cut -d= -f2 | tr -d ' x'; } || true; }
# health.json: last HEALTH_SAMPLES watchdog samples, last stall, restart count.
# Values are numbers and ISO timestamps only, so no JSON escaping is needed.
write_health() {
  local state=$1 last_stall=$2 restarts=$3; shift 3
  local tmp="$HEALTH_JSON.tmp" first=1 s
  {
    printf '{"updated":"%s","state":"%s","watchdog_restarts":%d,"last_stall":%s,"samples":[' \
      "$(date -u +%FT%TZ)" "$state" "$restarts" "${last_stall:-null}"
    for s in "$@"; do
      (( first )) || printf ','
      first=0; printf '%s' "$s"
    done
    printf ']}\n'
  } > "$tmp" && mv -f "$tmp" "$HEALTH_JSON"
}
watchdog_loop() {
  local prev_frame=-1 stall_since=0 slow_since=0 restarts=0 last_stall="" samples=()
  local blk frame fps speed bitrate drop dup now delta slow reason state sample fpid
  while :; do
    sleep "$WATCHDOG_EVERY"
    blk="$(tail -c 4096 "$PROGRESS" 2>/dev/null || true)"
    frame=$(progress_val "$blk" frame); now=$(date +%s)
    if [ -z "$frame" ]; then prev_frame=-1; stall_since=0; slow_since=0; continue; fi
    fps=$(progress_val "$blk" fps); speed=$(progress_val "$blk" speed)
    bitrate=$(progress_val "$blk" bitrate); drop=$(progress_val "$blk" drop_frames); dup=$(progress_val "$blk" dup_frames)
    # A fresh ffmpeg truncates the progress file, so a smaller frame counter is a restart, not a stall.
    (( prev_frame < 0 || frame < prev_frame )) && prev_frame=$frame
    delta=$(( frame - prev_frame )); prev_frame=$frame
    if (( delta == 0 )); then (( stall_since == 0 )) && stall_since=$now; else stall_since=0; fi
    slow=$(awk -v s="${speed:-1}" -v t="$SLOW_SPEED" 'BEGIN { print (s + 0 < t + 0) ? 1 : 0 }')
    if (( slow )); then (( slow_since == 0 )) && slow_since=$now; else slow_since=0; fi
    sample=$(printf '{"t":"%s","frame":%d,"dframe":%d,"fps":"%s","speed":"%s","bitrate":"%s","drop":"%s","dup":"%s"}' \
      "$(date -u +%FT%TZ)" "$frame" "$delta" "${fps:-?}" "${speed:-?}" "${bitrate:-?}" "${drop:-?}" "${dup:-?}")
    samples+=("$sample"); (( ${#samples[@]} > HEALTH_SAMPLES )) && samples=("${samples[@]:1}")
    reason=""; state=ok
    (( slow_since > 0 && now - slow_since >= SLOW_SECONDS )) && reason="speed ${speed}x < ${SLOW_SPEED}x for $(( now - slow_since ))s"
    (( stall_since > 0 && now - stall_since >= STALL_FRAMES_SECONDS )) && reason="no new frames for $(( now - stall_since ))s"
    (( slow_since > 0 || stall_since > 0 )) && state=degraded
    if [ -n "$reason" ]; then
      state=stalled
      log "WARN watchdog: ${reason} (frame=${frame} dframe=${delta} fps=${fps:-?} speed=${speed:-?}x bitrate=${bitrate:-?} drop=${drop:-?} dup=${dup:-?})"
      fpid=$(cat "$FFMPEG_PID" 2>/dev/null || true)
      if [ -n "$fpid" ] && kill -0 "$fpid" 2>/dev/null; then
        log "WARN watchdog: restarting ffmpeg (pid ${fpid})"
        kill -TERM "$fpid" 2>/dev/null || true
        restarts=$(( restarts + 1 ))
      else
        log "WARN watchdog: ffmpeg is not running; leaving the restart to ffmpeg_loop"
      fi
      last_stall=$(printf '{"t":"%s","reason":"%s","frame":%d,"speed":"%s"}' "$(date -u +%FT%TZ)" "$reason" "$frame" "${speed:-?}")
      stall_since=0; slow_since=0; prev_frame=-1
    fi
    write_health "$state" "$last_stall" "$restarts" "${samples[@]}"
  done
}
watchdog_loop &
pids+=($!)

wait -n "${pids[@]}" || true
if [ -e "$RUN_DIR/done" ]; then log "bounded run complete"; exit 0; fi
log "a supervised process left the loop; exiting so the orchestrator restarts the pod"
exit 1
