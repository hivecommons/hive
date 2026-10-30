# `src/deploy/stream/` — headless Twitch streamer for the Activity Monitor

Puts the dashboard's Activity Monitor (stage tracker) on an unattended 24/7
Twitch stream. Operator guide, Twitch-side setup, and troubleshooting:
[`src/docs/streaming-the-activity-monitor.md`](../../docs/streaming-the-activity-monitor.md).

| File | Purpose |
| --- | --- |
| `Dockerfile` | `debian:bookworm-slim` + Xvfb, Chromium, ffmpeg, xdotool, tini; builds `authproxy` in a `golang` stage. Build context is the **repo root**. Published as `ghcr.io/hivecommons/hive-stream:<sha>` / `:v5-latest` by `.github/workflows/stream-image.yml`. |
| `entrypoint.sh` | Supervisor: Xvfb → authproxy → Chromium `--kiosk` (restart with backoff) → periodic reload (`RELOAD_EVERY`) → ffmpeg `x11grab` + `anullsrc` → libx264 CBR → RTMP. Masks the stream key in ffmpeg output. |
| `authproxy/` | Loopback reverse proxy (stdlib Go) that adds `Authorization: Bearer $DASHBOARD_TOKEN` to every dashboard request so the token is never in a URL, in Chromium's history, or on screen. Logs when the upstream answers with a login redirect. |
| `healthcheck.sh` | Chromium alive + ffmpeg progress file fresher than 20 s with `frame>0`. Used by the image `HEALTHCHECK` and the Deployment's probes. |
| `docker-compose.yaml`, `.env.example` | Run the same image from a laptop. |
| `k8s/` | Kustomize base: Namespace `hive-stream`, ConfigMap, Deployment (1 replica, non-root, 512 Mi `/dev/shm`, 1–2 CPU / 2–3 Gi), NetworkPolicy (DNS, dashboard Service, public RTMP/HTTPS only). `secret.yaml` is a **template** and is not in the kustomization. |
| `overlays/hive-hosted/` | hive's own hosted spoke on `hive-oke`. |

## Environment

| Variable | Default | Meaning |
| --- | --- | --- |
| `DASHBOARD_URL` | required | Dashboard origin (`http://hive.<ns>.svc.cluster.local:3002` in-cluster, or the public route). |
| `DASHBOARD_TOKEN` | — | Spoke `dashboard.auth_token` (in-cluster) or a GitHub token of a hub user with access (public route). |
| `DASHBOARD_PATH` | `/?kiosk=1#section-activity-monitor-section` | Path Chromium opens. `?kiosk=1` is the dashboard's unattended display mode. |
| `TWITCH_STREAM_KEY` | — | Primary Stream key. |
| `TWITCH_INGEST` | `rtmp://live.twitch.tv/app` | Ingest; pick a closer one from Twitch's ingest list if needed. |
| `VIDEO_SIZE` / `FPS` / `BITRATE` / `X264_PRESET` | `1920x1080` / `30` / `4500k` / `veryfast` | Encode settings. Keyframe interval is `2×FPS`. |
| `RELOAD_EVERY` | `30m` | Page reload cadence (picks up dashboard rolls). |
| `STREAM_OUTPUT` | — | Dry run: `null` discards, `*.flv` writes a file, anything else is a raw ffmpeg output. |
| `STREAM_SECONDS` | `0` | `>0` stops ffmpeg after N s and exits 0 (dry run); a non-zero ffmpeg exit fails the run. |

## Dry run (no Twitch, no cluster)

```sh
docker build -t hive-stream:local -f src/deploy/stream/Dockerfile .
docker run --rm --shm-size=512m -v "$PWD/src/deploy/stream/out:/out" \
  -e DASHBOARD_URL=https://<hive>.hive.hivecommons.dev -e DASHBOARD_TOKEN=<github token> \
  -e STREAM_OUTPUT=/out/test.flv -e STREAM_SECONDS=20 hive-stream:local
ffmpeg -ss 15 -i src/deploy/stream/out/test.flv -frames:v 1 src/deploy/stream/out/frame.png
```

`out/` is git-ignored.
