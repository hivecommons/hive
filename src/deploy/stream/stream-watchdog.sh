#!/usr/bin/env bash
# Stream watchdog, run by entrypoint.sh next to ffmpeg_loop. It is the part of
# the supervisor that decides whether the broadcast is really reaching Twitch,
# and it is a separate script so test-stream-watchdog.sh can drive it with a
# fake ffmpeg and a stubbed live check.
#
# Two questions every tick (WATCHDOG_EVERY s):
#
#   1. Is ffmpeg alive and advancing?  Sampled from its -progress file
#      ($PROGRESS): no new frames for STALL_FRAMES_SECONDS, or speed below
#      SLOW_SPEED for SLOW_SECONDS, is a stall → SIGTERM ffmpeg so ffmpeg_loop
#      relaunches it.
#
#   2. Is the channel actually live on Twitch?  Checked every LIVE_CHECK_EVERY
#      s once TWITCH_CHANNEL_LOGIN is set. ffmpeg can sit at speed=1x for hours
#      after Twitch's ingest drops the session (hivecommons/hive, 2026-11-04
#      and 2026-11-05: pod 1/1 Running, channel offline until a human ran
#      `kubectl rollout restart`), so the progress file alone is not enough.
#      LIVE_OFFLINE_THRESHOLD consecutive "offline" answers outside the
#      LIVE_GRACE_SECONDS window after an ffmpeg (re)start → SIGTERM ffmpeg,
#      then hold off for an exponentially growing LIVE_RESTART_BACKOFF_MIN..MAX
#      before judging again. LIVE_RESTART_LIMIT such relaunches in a row with
#      the channel still offline → exit non-zero; entrypoint.sh then exits so
#      Kubernetes restarts the whole container (fresh Xvfb, Chromium, sockets).
#      Lookup errors (DNS, 5xx, malformed body) are "unknown" and never count.
#
# Heartbeat: $HEARTBEAT_FILE is touched only when ffmpeg is advancing AND the
# channel is live (or the live check is not configured). healthcheck.sh fails
# the liveness probe when it is older than HEARTBEAT_MAX_AGE_SECONDS, so a
# wedged watchdog is also recovered by the orchestrator.
#
# Logging: one line per state change (live, offline n/N, restarting, giving
# up), never a line per tick. Nothing here prints a secret.
#
# Files shared with entrypoint.sh (all under RUN_DIR):
#   ffmpeg-progress.txt  ffmpeg -progress output (written by ffmpeg)
#   ffmpeg.pid           pid of the running ffmpeg (ffmpeg_loop)
#   ffmpeg.started       epoch seconds of the last ffmpeg start (ffmpeg_loop)
#   health.json          samples, state, counters (written here)
#   stream-healthy       heartbeat (written here)
set -euo pipefail

RUN_DIR="${RUN_DIR:-/run/stream}"
PROGRESS="${PROGRESS:-$RUN_DIR/ffmpeg-progress.txt}"
FFMPEG_PID="${FFMPEG_PID:-$RUN_DIR/ffmpeg.pid}"
FFMPEG_STARTED="${FFMPEG_STARTED:-$RUN_DIR/ffmpeg.started}"
HEALTH_JSON="${HEALTH_JSON:-$RUN_DIR/health.json}"
HEARTBEAT_FILE="${HEARTBEAT_FILE:-$RUN_DIR/stream-healthy}"
WATCHDOG_EVERY="${WATCHDOG_EVERY:-10}"             # seconds between progress-file samples
STALL_FRAMES_SECONDS="${STALL_FRAMES_SECONDS:-30}" # restart ffmpeg after this long with no new frames
SLOW_SPEED="${SLOW_SPEED:-0.90}"                   # restart ffmpeg when speed stays below this…
SLOW_SECONDS="${SLOW_SECONDS:-60}"                 # …for this long
HEALTH_SAMPLES="${HEALTH_SAMPLES:-20}"             # progress samples kept in health.json
TWITCH_CHANNEL_LOGIN="${TWITCH_CHANNEL_LOGIN:-}"   # unset = no live check, progress only
LIVE_CHECK_EVERY="${LIVE_CHECK_EVERY:-60}"         # seconds between Twitch live checks
LIVE_GRACE_SECONDS="${LIVE_GRACE_SECONDS:-240}"    # after an ffmpeg start, before offline counts (Twitch holds a key lock ~2 min after a publisher drops)
LIVE_OFFLINE_THRESHOLD="${LIVE_OFFLINE_THRESHOLD:-3}" # consecutive offline checks before acting
LIVE_RESTART_BACKOFF_MIN="${LIVE_RESTART_BACKOFF_MIN:-30}"  # hold after a live-triggered restart…
LIVE_RESTART_BACKOFF_MAX="${LIVE_RESTART_BACKOFF_MAX:-300}" # …doubling up to this
LIVE_RESTART_LIMIT="${LIVE_RESTART_LIMIT:-5}"      # relaunches in a row still offline → exit
LIVE_CHECK_TIMEOUT="${LIVE_CHECK_TIMEOUT:-10}"     # curl --max-time for a live check
# Twitch's public web Client-Id: the one twitch.tv itself sends to gql.twitch.tv
# for anonymous page loads, so no secret and no app registration is needed.
# Override if Twitch rotates it; set TWITCH_HELIX_CLIENT_ID/_SECRET to use the
# official Helix API with your own app instead.
TWITCH_LIVE_CHECK_CLIENT_ID="${TWITCH_LIVE_CHECK_CLIENT_ID:-ue6666qo983tsx6so1t0vnawi233wa}"
TWITCH_GQL_URL="${TWITCH_GQL_URL:-https://gql.twitch.tv/gql}"
TWITCH_HELIX_CLIENT_ID="${TWITCH_HELIX_CLIENT_ID:-}"
TWITCH_HELIX_CLIENT_SECRET="${TWITCH_HELIX_CLIENT_SECRET:-}"
TWITCH_HELIX_URL="${TWITCH_HELIX_URL:-https://api.twitch.tv/helix}"
TWITCH_OAUTH_URL="${TWITCH_OAUTH_URL:-https://id.twitch.tv/oauth2/token}"
# Test hook: a command that prints live|offline|unknown instead of asking Twitch.
TWITCH_LIVE_CHECK_CMD="${TWITCH_LIVE_CHECK_CMD:-}"
WATCHDOG_EXIT_CODE=3

log() { printf '%s stream: %s\n' "$(date -u +%FT%TZ)" "$*"; }

# Last value of key= in the tail of the progress file ("" if absent).
progress_val() { { printf '%s\n' "$1" | grep -a "^$2=" | tail -1 | cut -d= -f2 | tr -d ' x'; } || true; }

# ---- live check ---------------------------------------------------------------
# Prints live | offline | unknown. "unknown" is any failure to get an answer.
helix_token=""
helix_fetch_token() {
  local body
  body="$(curl -sS --max-time "$LIVE_CHECK_TIMEOUT" -X POST "$TWITCH_OAUTH_URL" \
    --data-urlencode "client_id=$TWITCH_HELIX_CLIENT_ID" \
    --data-urlencode "client_secret=$TWITCH_HELIX_CLIENT_SECRET" \
    --data-urlencode "grant_type=client_credentials" 2>/dev/null)" || return 1
  helix_token="$(printf '%s' "$body" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')"
  [ -n "$helix_token" ]
}
live_check_helix() {
  local body code
  for _ in 1 2; do
    [ -n "$helix_token" ] || helix_fetch_token || { echo unknown; return; }
    body="$(curl -sS --max-time "$LIVE_CHECK_TIMEOUT" -w '\n%{http_code}' \
      -H "Client-Id: $TWITCH_HELIX_CLIENT_ID" -H "Authorization: Bearer $helix_token" \
      "$TWITCH_HELIX_URL/streams?user_login=$TWITCH_CHANNEL_LOGIN" 2>/dev/null)" || { echo unknown; return; }
    code="${body##*$'\n'}"; body="${body%$'\n'*}"
    case "$code" in
      200) ;;
      401) helix_token=""; continue ;;   # expired app token: fetch once more
      *)   echo unknown; return ;;
    esac
    case "$body" in
      *'"type":"live"'*) echo live ;;
      *'"data":[]'*)     echo offline ;;
      *)                 echo unknown ;;
    esac
    return
  done
  echo unknown
}
live_check_gql() {
  local body
  body="$(curl -sS --max-time "$LIVE_CHECK_TIMEOUT" -H "Client-Id: $TWITCH_LIVE_CHECK_CLIENT_ID" \
    -H 'Content-Type: application/json' "$TWITCH_GQL_URL" \
    -d "{\"query\":\"{user(login:\\\"$TWITCH_CHANNEL_LOGIN\\\"){stream{type}}}\"}" 2>/dev/null)" || { echo unknown; return; }
  case "$body" in
    *'"stream":{"type":"live"}'*) echo live ;;
    *'"stream":null'*)            echo offline ;;
    *)                            echo unknown ;;
  esac
}
live_check() {
  local r
  if [ -n "$TWITCH_LIVE_CHECK_CMD" ]; then
    r="$(bash -c "$TWITCH_LIVE_CHECK_CMD" 2>/dev/null || true)"
  elif [ -n "$TWITCH_HELIX_CLIENT_ID" ] && [ -n "$TWITCH_HELIX_CLIENT_SECRET" ]; then
    r="$(live_check_helix)"
  else
    r="$(live_check_gql)"
  fi
  case "$r" in live|offline) echo "$r" ;; *) echo unknown ;; esac
}

# ---- state files --------------------------------------------------------------
# health.json: last HEALTH_SAMPLES watchdog samples, last stall, counters and
# the Twitch verdict. Values are numbers, enum words and ISO timestamps only,
# so no JSON escaping is needed.
write_health() {
  local state=$1 twitch=$2 last_stall=$3 restarts=$4 live_restarts=$5; shift 5
  local tmp="$HEALTH_JSON.tmp" first=1 s
  {
    printf '{"updated":"%s","state":"%s","twitch":"%s","watchdog_restarts":%d,"live_restarts":%d,"last_stall":%s,"samples":[' \
      "$(date -u +%FT%TZ)" "$state" "$twitch" "$restarts" "$live_restarts" "${last_stall:-null}"
    for s in "$@"; do
      (( first )) || printf ','
      first=0; printf '%s' "$s"
    done
    printf ']}\n'
  } > "$tmp" && mv -f "$tmp" "$HEALTH_JSON"
}
# SIGTERM the running ffmpeg (ffmpeg_loop relaunches it). Returns 1 if none.
kill_ffmpeg() {
  local fpid
  fpid=$(cat "$FFMPEG_PID" 2>/dev/null || true)
  [ -n "$fpid" ] && kill -0 "$fpid" 2>/dev/null || return 1
  log "WARN watchdog: restarting ffmpeg (pid ${fpid}): $1"
  kill -TERM "$fpid" 2>/dev/null || true
}

# ---- main loop ----------------------------------------------------------------
prev_frame=-1 stall_since=0 slow_since=0 restarts=0 last_stall="" samples=()
twitch=skipped prev_twitch="" offline_n=0 live_restarts=0 last_live_check=0 hold_until=0
live_backoff=$LIVE_RESTART_BACKOFF_MIN
[ -n "$TWITCH_CHANNEL_LOGIN" ] && twitch=unknown
if [ -n "$TWITCH_CHANNEL_LOGIN" ]; then
  log "watchdog: live check for twitch.tv/${TWITCH_CHANNEL_LOGIN} every ${LIVE_CHECK_EVERY}s ($( [ -n "$TWITCH_HELIX_CLIENT_ID" ] && [ -n "$TWITCH_HELIX_CLIENT_SECRET" ] && echo helix || echo gql ); grace ${LIVE_GRACE_SECONDS}s, ${LIVE_OFFLINE_THRESHOLD} misses restart ffmpeg, ${LIVE_RESTART_LIMIT} restarts exit)"
else
  log "watchdog: TWITCH_CHANNEL_LOGIN unset; progress-only (no Twitch live check)"
fi
while :; do
  sleep "$WATCHDOG_EVERY"
  now=$(date +%s)
  blk="$(tail -c 4096 "$PROGRESS" 2>/dev/null || true)"
  frame=$(progress_val "$blk" frame)
  advancing=0 state=ok reason=""
  if [ -z "$frame" ]; then
    prev_frame=-1; stall_since=0; slow_since=0; state=starting
  else
    fps=$(progress_val "$blk" fps); speed=$(progress_val "$blk" speed)
    bitrate=$(progress_val "$blk" bitrate); drop=$(progress_val "$blk" drop_frames); dup=$(progress_val "$blk" dup_frames)
    # A fresh ffmpeg truncates the progress file, so a smaller frame counter is a restart, not a stall.
    (( prev_frame < 0 || frame < prev_frame )) && prev_frame=$frame
    delta=$(( frame - prev_frame )); prev_frame=$frame
    if (( delta == 0 )); then (( stall_since == 0 )) && stall_since=$now; else stall_since=0; advancing=1; fi
    slow=$(awk -v s="${speed:-1}" -v t="$SLOW_SPEED" 'BEGIN { print (s + 0 < t + 0) ? 1 : 0 }')
    if (( slow )); then (( slow_since == 0 )) && slow_since=$now; else slow_since=0; fi
    sample=$(printf '{"t":"%s","frame":%d,"dframe":%d,"fps":"%s","speed":"%s","bitrate":"%s","drop":"%s","dup":"%s"}' \
      "$(date -u +%FT%TZ)" "$frame" "$delta" "${fps:-?}" "${speed:-?}" "${bitrate:-?}" "${drop:-?}" "${dup:-?}")
    samples+=("$sample"); (( ${#samples[@]} > HEALTH_SAMPLES )) && samples=("${samples[@]:1}")
    (( slow_since > 0 && now - slow_since >= SLOW_SECONDS )) && reason="speed ${speed}x < ${SLOW_SPEED}x for $(( now - slow_since ))s"
    (( stall_since > 0 && now - stall_since >= STALL_FRAMES_SECONDS )) && reason="no new frames for $(( now - stall_since ))s"
    (( slow_since > 0 || stall_since > 0 )) && state=degraded
    if [ -n "$reason" ]; then
      state=stalled; advancing=0
      log "WARN watchdog: ${reason} (frame=${frame} dframe=${delta} fps=${fps:-?} speed=${speed:-?}x bitrate=${bitrate:-?} drop=${drop:-?} dup=${dup:-?})"
      if kill_ffmpeg "$reason"; then restarts=$(( restarts + 1 )); else log "WARN watchdog: ffmpeg is not running; leaving the restart to ffmpeg_loop"; fi
      last_stall=$(printf '{"t":"%s","reason":"%s","frame":%d,"speed":"%s"}' "$(date -u +%FT%TZ)" "$reason" "$frame" "${speed:-?}")
      stall_since=0; slow_since=0; prev_frame=-1
    fi
  fi

  # ---- Twitch live check ----
  if [ -n "$TWITCH_CHANNEL_LOGIN" ] && (( now - last_live_check >= LIVE_CHECK_EVERY )); then
    last_live_check=$now
    started=$(cat "$FFMPEG_STARTED" 2>/dev/null || echo 0)
    if (( now < hold_until )); then
      twitch=restarting # backing off after a live-triggered restart; do not judge yet
    elif (( started == 0 || now - started < LIVE_GRACE_SECONDS )); then
      twitch=grace
      [ "$prev_twitch" = grace ] || log "watchdog: twitch.tv/${TWITCH_CHANNEL_LOGIN} grace period (${LIVE_GRACE_SECONDS}s after ffmpeg start)"
      prev_twitch=grace
    else
      verdict="$(live_check)"
      case "$verdict" in
        live)
          twitch=live; offline_n=0
          if [ "$prev_twitch" != live ]; then
            log "live on twitch.tv/${TWITCH_CHANNEL_LOGIN}$( (( live_restarts > 0 )) && echo " (after ${live_restarts} ffmpeg restart(s))" )"
          fi
          live_restarts=0; live_backoff=$LIVE_RESTART_BACKOFF_MIN; prev_twitch=live
          ;;
        offline)
          twitch=offline; offline_n=$(( offline_n + 1 ))
          log "offline on twitch.tv/${TWITCH_CHANNEL_LOGIN} (${offline_n}/${LIVE_OFFLINE_THRESHOLD})"
          prev_twitch=offline
          if (( offline_n >= LIVE_OFFLINE_THRESHOLD )); then
            offline_n=0; live_restarts=$(( live_restarts + 1 ))
            if (( live_restarts >= LIVE_RESTART_LIMIT )); then
              log "ERROR watchdog: twitch.tv/${TWITCH_CHANNEL_LOGIN} still offline after ${live_restarts} ffmpeg restarts; exiting so the pod is restarted"
              write_health giving-up "$twitch" "$last_stall" "$restarts" "$live_restarts" "${samples[@]}"
              exit "$WATCHDOG_EXIT_CODE"
            fi
            log "offline (${LIVE_OFFLINE_THRESHOLD}/${LIVE_OFFLINE_THRESHOLD}) → restarting ffmpeg (${live_restarts}/${LIVE_RESTART_LIMIT}); next check in $(( LIVE_GRACE_SECONDS + live_backoff ))s"
            kill_ffmpeg "channel offline on Twitch" || log "WARN watchdog: ffmpeg is not running; leaving the restart to ffmpeg_loop"
            hold_until=$(( now + LIVE_GRACE_SECONDS + live_backoff )); state=offline; twitch=restarting
            live_backoff=$(( live_backoff * 2 > LIVE_RESTART_BACKOFF_MAX ? LIVE_RESTART_BACKOFF_MAX : live_backoff * 2 ))
            prev_twitch=restarting
          fi
          ;;
        *)
          twitch=unknown
          [ "$prev_twitch" = unknown ] || log "WARN watchdog: Twitch live check for ${TWITCH_CHANNEL_LOGIN} returned no answer; not counting it"
          prev_twitch=unknown
          ;;
      esac
    fi
  fi

  # Heartbeat: ffmpeg advancing and the channel live (or the live check not
  # configured). "grace"/"restarting"/"unknown" are deliberate waits, not
  # failures, so they keep the heartbeat too: the probe is for a wedged
  # watchdog, the LIVE_RESTART_LIMIT exit above is for a channel that stays
  # offline. Only a counted "offline" verdict withholds it.
  if (( advancing )); then
    case "$twitch" in skipped|live|grace|restarting|unknown) touch "$HEARTBEAT_FILE" ;; esac
  fi
  write_health "$state" "$twitch" "$last_stall" "$restarts" "$live_restarts" "${samples[@]}"
done
