# `src/deploy/stream/` — headless Twitch streamer for the Activity Monitor

Puts the dashboard's Activity Monitor (stage tracker) on an unattended 24/7
Twitch stream. Operator guide, Twitch-side setup, and troubleshooting:
[`src/docs/streaming-the-activity-monitor.md`](../../docs/streaming-the-activity-monitor.md).

| File | Purpose |
| --- | --- |
| `Dockerfile` | `debian:bookworm-slim` + Xvfb, Chromium, ffmpeg, xdotool, tini; builds `authproxy` in a `golang` stage. Build context is the **repo root**. Published as `ghcr.io/hivecommons/hive-stream:<sha>` / `:v5-latest` by `.github/workflows/stream-image.yml`. |
| `entrypoint.sh` | Supervisor: Xvfb → authproxy → Chromium `--kiosk` (restart with backoff) → periodic reload (`RELOAD_EVERY`) → PulseAudio null sink `hive` → ffmpeg `x11grab` + `pulse hive.monitor` (or `anullsrc` with `STREAM_AUDIO=silent`) → libx264 CBR + AAC → RTMP. Masks the stream key in ffmpeg output. |
| `authproxy/` | Loopback reverse proxy (stdlib Go) that adds `Authorization: Bearer $DASHBOARD_TOKEN` to every dashboard request so the token is never in a URL, in Chromium's history, or on screen. Forwards only `GET`/`HEAD` (anything else is answered `405` locally, before the token is attached) because the shared token is an unscoped owner credential and a kiosk only reads. Logs when the upstream answers with a login redirect. |
| `healthcheck.sh` | Chromium alive + ffmpeg progress file fresher than 20 s with `frame>0`; also prints the stall watchdog's state from `/run/stream/health.json`. Used by the image `HEALTHCHECK` and the Deployment's probes. |
| `docker-compose.yaml`, `.env.example` | Run the same image from a laptop. |
| `k8s/` | Kustomize base: Namespace `hive-stream`, ConfigMap, Deployment (1 replica, non-root, 512 Mi `/dev/shm`, 1–3 CPU / 3–6 Gi), NetworkPolicy (DNS, dashboard Service, public RTMP/HTTPS only). `secret.yaml` is a **template** and is not in the kustomization. |
| `overlays/hive-hosted/` | hive's own hosted spoke on `hive-oke`. |
| `overlays/hive-hosted-matrix/` | Inactive, silent Matrix instance for `twitch.tv/hivecommonsmatrix`: separate namespace and identity, `twitch-stream-matrix` secret reference, zero replicas; owner-only activation. |
| `fleet/` | Standalone, **disabled-by-default** Kustomize base for the hub-wide monitor on `twitch.tv/hivecommonsfleet` (#15966): namespace `hive-stream-fleet`, `replicas: 0`, `STREAM_PROFILE=public-safe`, stream-key Secret `twitch-stream-hivecommonsfleet` (name only; `secret.yaml` is a template), no dashboard credential, public egress only. Shares no resource with `k8s/` or `overlays/`. |
| `public-safe-gate.sh` | Run by `entrypoint.sh` before any process. No-op unless `STREAM_PROFILE=public-safe`; then it exits unless the owner enabled the stream, acknowledged the public-safe policy, set a real `https://` source, left `DASHBOARD_TOKEN` unset and installed a stream key. |

## Environment

| Variable | Default | Meaning |
| --- | --- | --- |
| `DASHBOARD_URL` | required | Dashboard origin (`http://hive.<ns>.svc.cluster.local:3002` in-cluster, or the public route). |
| `DASHBOARD_TOKEN` | — | Spoke `dashboard.auth_token` (in-cluster) or a GitHub token of a hub user with access (public route). |
| `DASHBOARD_PATH` | `/?kiosk=1#section-activity-monitor-section` | Path Chromium opens. `?kiosk=1` is the dashboard's unattended display mode; tracker sounds are on there unless `&sound=off` is added. |
| `STREAM_AUDIO` | `pulse` | `pulse` captures the tracker sounds via a PulseAudio null sink; `silent` mutes Chromium and streams `anullsrc` silence. |
| `TWITCH_STREAM_KEY` | — | Primary Stream key. |
| `TWITCH_INGEST` | `rtmp://live.twitch.tv/app` | Ingest. The default is Twitch's anycast entry, which resolves to the nearest PoP (~5 ms from `hive-oke`). If the TCP connect RTT from the pod is >80 ms (`curl -s -o /dev/null -w '%{time_connect}\n' telnet://live.twitch.tv:1935`), pick a nearer host from [Twitch's ingest list](https://help.twitch.tv/s/twitch-ingest-recommendation) or use `rtmps://ingest.global-contribute.live-video.net/app/`. |
| `VIDEO_SIZE` / `FPS` / `BITRATE` / `X264_PRESET` | `1920x1080` / `30` / `4500k` / `veryfast` | Encode settings. Keyframe interval is `2×FPS`. |
| `RELOAD_EVERY` | `30m` | Page reload cadence (picks up dashboard rolls). |
| `VIDEO_QUEUE_FRAMES` | `256` | x11grab `-thread_queue_size` in raw frames (~8.3 MiB each at 1080p; 256 ≈ 2.1 GiB worst case). Too shallow drops frames on every encoder hiccup, and each drop is a PTS gap the player shows as buffering; too deep risks OOM. |
| `AUDIO_QUEUE` | `1024` | Audio input `-thread_queue_size` in packets (`AUDIO_QUEUE_PACKETS` is the older, still-honoured name). |
| `WATCHDOG_EVERY` / `STALL_FRAMES_SECONDS` / `SLOW_SPEED` / `SLOW_SECONDS` | `10` / `30` / `0.90` / `60` | Stall watchdog: sample the progress file every `WATCHDOG_EVERY` s; restart ffmpeg after `STALL_FRAMES_SECONDS` with no new frames, or after `SLOW_SECONDS` of `speed` below `SLOW_SPEED`. |
| `HEALTH_SAMPLES` | `20` | Watchdog samples kept in `/run/stream/health.json`. |
| `STREAM_OUTPUT` | — | Dry run: `null` discards, `*.flv` writes a file, anything else is a raw ffmpeg output. |
| `STREAM_SECONDS` | `0` | `>0` stops ffmpeg after N s and exits 0 (dry run); a non-zero ffmpeg exit fails the run. |
| `STREAM_PROFILE` | — | `public-safe` (only `fleet/`) turns on `public-safe-gate.sh` and makes `authproxy` anonymous: no token, browser `Authorization`/`Cookie` stripped, `Set-Cookie` dropped. Unset keeps the behaviour above. |
| `STREAM_ENABLED` / `PUBLIC_SAFE_POLICY_ACK` | `false` / — | Read only under `STREAM_PROFILE=public-safe`: the owner's enable and acknowledgement of policy `hivecommonsfleet-public-safe-v1` ([docs](../../docs/streaming-the-activity-monitor.md#public-safe-content-policy)). |

## Dry run (no Twitch, no cluster)

```sh
docker build -t hive-stream:local -f src/deploy/stream/Dockerfile .
docker run --rm --shm-size=512m -v "$PWD/src/deploy/stream/out:/out" \
  -e DASHBOARD_URL=https://<hive>.hive.hivecommons.dev -e DASHBOARD_TOKEN=<github token> \
  -e STREAM_OUTPUT=/out/test.flv -e STREAM_SECONDS=20 hive-stream:local
ffmpeg -ss 15 -i src/deploy/stream/out/test.flv -frames:v 1 src/deploy/stream/out/frame.png
```

`out/` is git-ignored.

## Why does the player buffer?

The Twitch player shows a spinner whenever its buffer runs dry. Three things do that, and the encoder stats tell them apart:

1. **The encoder is not keeping up** — `speed` < 1x, `fps` < `FPS`, `drop_frames` climbing. Cause: CPU throttling (check `nr_throttled` in the pod's `cpu.stat`), a too-slow `X264_PRESET`, or Chromium's startup/reload burst stealing cores. The watchdog restarts ffmpeg when this lasts; fix the budget (CPU limit, preset, `ENC_THREADS`).
2. **Frames arrive with gaps** — `speed` ≈ 1x but `drop_frames` ticks up. x11grab could not hand the frame to x264 in time and skipped it; the next frame's PTS jumps, the player's buffer underruns for exactly that gap. Raise `VIDEO_QUEUE_FRAMES` (it absorbs the hiccup instead of dropping) and keep `-use_wallclock_as_timestamps`/`aresample=async=1`, which the entrypoint sets so audio and video PTS cannot drift apart.
3. **The network to the ingest is the problem** — encoder stats are perfect, but Twitch's `video_stats` overlay shows a low *Download bitrate*, rising *Skipped frames*, or a *Buffer size* that keeps hitting 0. Check the RTT from the pod to `TWITCH_INGEST` (see above) and whether the `bitrate` line stays flat at `BITRATE`; a sagging bitrate with `speed` ≈ 1x means RTMP back-pressure.

### Diagnosis recipe

Sample the progress file for three minutes and compute per-interval deltas (a healthy 30 fps stream adds ~150 frames per 5 s):

```sh
POD=$(kubectl -n hive-stream-matrix get pod -l app.kubernetes.io/name=hive-stream-matrix -o jsonpath='{.items[0].metadata.name}')
for i in $(seq 36); do
  kubectl -n hive-stream-matrix exec "$POD" -- sh -c \
    'date -u +%T; grep -a -E "^(frame|fps|bitrate|drop_frames|dup_frames|speed)=" /run/stream/ffmpeg-progress.txt | tail -6 | tr "\n" " "; echo'
  sleep 5
done
```

Flag any interval with fewer than 140 new frames or `speed` < 0.97x. The watchdog does the same every 10 s and keeps its last 20 samples, the last stall and its restart count in `/run/stream/health.json`:

```sh
kubectl -n hive-stream-matrix exec "$POD" -- cat /run/stream/health.json
kubectl -n hive-stream-matrix logs "$POD" | grep 'WARN watchdog'
```

What each progress number means:

| Field | Meaning |
| --- | --- |
| `frame` | Frames encoded since ffmpeg started. Delta per 5 s should be `5×FPS` (150 at 30 fps). |
| `fps` | Encoder output rate over the last window; should sit at `FPS`. |
| `speed` | Encoded time ÷ wall time. `1x` is real time; below `0.97x` the stream is falling behind and the player will stall once Twitch's buffer drains. |
| `bitrate` | Output bitrate; CBR should hold it within a few percent of `BITRATE`. |
| `drop_frames` | Frames x11grab skipped because the encoder was busy and the queue was full — each is a PTS gap. A handful per hour is noise; dozens per minute is a stall. |
| `dup_frames` | Frames duplicated to keep the output rate constant; non-zero means the capture briefly produced nothing. |

In the Twitch player, open the settings cog → *Advanced* → *Video Stats*: *Download bitrate* near `BITRATE`, *Buffer size* steady above ~1 s and *Skipped frames* flat means the stream is fine and the spinner is the viewer's network; *Skipped frames* climbing with a healthy encoder points at the ingest path.
