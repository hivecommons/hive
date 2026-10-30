# Streaming the Activity Monitor to Twitch

An unattended, 24/7 Twitch stream of the hive dashboard's **Activity Monitor**
(the stage tracker: every tracked issue and PR walking Open → Actionable →
Claimed → PR in progress → CI / Review → Merge queue). The first stream is
hive's own hosted spoke on `hive-oke`, channel
[`twitch.tv/hivecommons`](https://www.twitch.tv/hivecommons). The stream is
listed alongside the other community links in [Community and support](community.md).

Everything lives in [`src/deploy/stream/`](../deploy/stream/README.md): a
container that renders the dashboard in a virtual display and pushes it to
Twitch, and the Kubernetes manifests that run it next to the hive it watches.

```
Deployment hive-stream (ns hive-stream, 1 replica)
  Xvfb :99 1920x1080
  authproxy 127.0.0.1:8099 ──Authorization: Bearer──▶ hive dashboard Service :3002
  Chromium --kiosk http://127.0.0.1:8099/?kiosk=1#section-activity-monitor-section
    └─ tracker sounds ──▶ PulseAudio null sink "hive" (hive.monitor)
  ffmpeg x11grab 30 fps + pulse hive.monitor ──libx264 4500 kbps CBR + AAC 128 kbps──▶ rtmp://live.twitch.tv/app/<key>
```

### Sound

The stream carries the tracker's sounds — the zap when a PR merges and the
buzz when a new issue or PR opens. In kiosk mode they are **on by default**
(a regular browser tab keeps them opt-in); Chromium plays them into a
PulseAudio null sink inside the container and ffmpeg captures that sink's
monitor. Two knobs:

- `DASHBOARD_PATH="/?kiosk=1&sound=off#section-activity-monitor-section"`
  keeps the dashboard silent (the stream still carries a real, silent audio
  track). Append `&theme=light` or other display flags the same way.
- `STREAM_AUDIO=silent` skips PulseAudio entirely: Chromium runs
  `--mute-audio` and ffmpeg synthesises silence with `anullsrc`, the
  pre-sound behaviour. Default is `STREAM_AUDIO=pulse`.

Nothing here touches Twitch or the cluster until **you** create the Secrets and
apply the overlay — the steps below are all yours to run.

## 1. Twitch side (once)

Open the Creator Dashboard at `dashboard.twitch.tv` while logged in as
`hivecommons`.

1. **Settings → Stream → Stream Key & Preferences**. Copy the **Primary Stream
   key** (`live_…`). Treat it like a password: anyone holding it can broadcast
   on the channel. Use **Reset** here if it ever leaks; then update the Secret
   (step 2) and restart the pod.
2. Still on that page: **Disconnect protection** on (hides short drops during
   Chromium/ffmpeg restarts), **Latency mode** → Normal, and **Store past
   broadcasts** off unless you want 7-day VODs. Clips can stay on; nothing on
   the tracker is sensitive (see [Privacy](#privacy)).
3. **Stream Manager → Edit Stream Info**: title (e.g. `hive 🐝 Activity Monitor
   — autonomous PR pipeline, live`), **Category → Software and Game
   Development**, tags `programming`, `opensource`, `golang`, `ai`. Language
   English, **not** mature.
4. **Settings → Moderation**: Followers-only or Email-verified chat, AutoMod
   level 2 — nobody is watching chat on an unattended stream.
5. Optional: **Settings → Channel → Brand** for a panel pointing at
   `github.com/hivecommons/hive`.

## 2. Create the Secrets

Both Secrets are created out of band and never committed
(`src/deploy/stream/k8s/secret.yaml` is a template only). Namespace
`hive-stream` is dedicated: the hub provisioner owns the hosted spoke's
namespace outright and would reclaim anything placed there.

```sh
kubectl --context hive-oke create namespace hive-stream

# Twitch Creator Dashboard → Settings → Stream → Primary Stream key
kubectl --context hive-oke -n hive-stream create secret generic twitch-stream \
  --from-literal=TWITCH_STREAM_KEY='live_PASTE_THE_KEY_HERE'

# The spoke's shared dashboard token, copied from its own namespace
kubectl --context hive-oke -n hive-stream create secret generic dashboard-token \
  --from-literal=DASHBOARD_TOKEN="$(kubectl --context hive-oke \
    -n hive-hosted-hosted-available-oke-11-placeholder-r05x \
    get secret hive-secrets -o jsonpath='{.data.dashboard-token}' | base64 -d)"
```

The shared token works because the streamer reaches the dashboard over the
in-cluster Service and hive's hive is `hub_proxied: true`, so the spoke still
honours `Authorization: Bearer <dashboard.auth_token>`. The token is injected
by a loopback proxy inside the pod; it never appears in the URL, in Chromium's
history, in logs, or on screen. The proxy forwards only `GET`/`HEAD`: the shared
token is an unscoped owner credential, and a display never mutates, so a
misbehaving page in the kiosk cannot pause agents or change settings with it.

**Alternative — the public route.** To stream a hive you cannot reach
in-cluster (or from a laptop with `docker compose`), set `DASHBOARD_URL` to the
public route (`https://<hive>.hive.hivecommons.dev`) and `DASHBOARD_TOKEN` to
a **GitHub token** of a user who has access to that hive on the hub; the
route's `auth-url` check accepts `Authorization: Bearer <GitHub token>` and
needs no OAuth scopes. Prefer a dedicated read-only viewer account so the
stream never shows an owner's Welcome/NPS prompts or the action menus.

## 3. Apply

The image is published by `.github/workflows/stream-image.yml` on every merge
to `v5` that touches `src/deploy/stream/**`, as
`ghcr.io/hivecommons/hive-stream:<sha>` and `:v5-latest`. The overlay tracks
`v5-latest`.

```sh
kubectl --context hive-oke apply -k src/deploy/stream/overlays/hive-hosted
kubectl --context hive-oke -n hive-stream rollout status deploy/hive-stream
```

For another hive, copy `overlays/hive-hosted/` and change the two
`CHANGE-ME` patches (dashboard Service URL and the NetworkPolicy's namespace).

## 4. Verify

```sh
kubectl --context hive-oke -n hive-stream logs deploy/hive-stream -f
```

Healthy start-up reads:

```
stream: Xvfb up on :99 (1920x1080)
authproxy: 127.0.0.1:8099 -> http://hive.<ns>.svc.cluster.local:3002 (token set)
stream: authproxy up; kiosk URL http://127.0.0.1:8099/?kiosk=1#section-activity-monitor-section …
stream: chromium starting
stream: ffmpeg starting (1920x1080@30, 4500k CBR, keyint 60, sink -f flv …)
```

Then open **Stream Manager** on Twitch: the preview should show the tracker
within ~30 s, with the **Live** badge and bitrate around 4500 kbps. The pod's
`healthcheck.sh` (used by the liveness probe) demands that Chromium is running
and ffmpeg has written progress in the last 20 s:

```sh
kubectl --context hive-oke -n hive-stream exec deploy/hive-stream -- healthcheck.sh
# ok frames=12345
```

A page reload fires every `RELOAD_EVERY` (default `30m`) so a newly rolled
dashboard build is picked up without restarting the pod.

## Troubleshooting

| Symptom | Cause / fix |
| --- | --- |
| Stream shows a **login page** or **403** | Auth. Logs will say `authproxy: upstream redirected … to login`. In-cluster: the `dashboard-token` Secret is wrong or the hive is not `hub_proxied`. Public route: the GitHub token's user has no access to this hive. |
| **Black** or blank frame | Chromium crashed or has not painted yet. Check `kubectl logs` for `chromium exited; restarting`; the usual cause is `/dev/shm` too small (the Deployment mounts a 512 Mi memory emptyDir — keep it). |
| Twitch says **"Bitrate too high/unstable"** | Drop `BITRATE` to `3000k` in the ConfigMap, or `VIDEO_SIZE=1280x720` (and the matching `Xvfb` size is derived automatically). CPU-bound encodes also benefit from `X264_PRESET=superfast`. |
| Pod **CrashLoopBackOff** on start | `DASHBOARD_URL` unset, or ffmpeg cannot reach `live.twitch.tv:1935` — check the NetworkPolicy allows egress to public IPs and that the cluster permits outbound RTMP. |
| Stream drops every few minutes | Twitch rejected the key (reset in Creator Dashboard?). ffmpeg restarts with 2→60 s backoff; the logs show the masked `***` key, never the real one. |
| Old UI after a dashboard roll | Wait for the next `RELOAD_EVERY` tick or `kubectl rollout restart deploy/hive-stream`. |
| Pod is `Running` but Twitch is offline | `healthcheck.sh` in the pod; if `frames` is not increasing, the ffmpeg sink is failing — look for `rtmp` errors in the logs. |

Pure CPU budget: software x264 at 1080p30 `veryfast` uses roughly 1.5 cores;
the Deployment requests 1 CPU / 2 Gi and limits 2 CPU / 3 Gi.

## Privacy

The tracker renders what GitHub already shows publicly for the repos this hive
tracks: issue and PR numbers, titles, authors, labels, CI state and merge-queue
position. For hive's hive that is `hivecommons/hive`, a public repository. It
does **not** render tokens, secrets, settings, webhooks, chat, or the
operator's agent terminals; `?kiosk=1` additionally hides the sidebar, topbar,
avatar/settings menus, notices, the Welcome and NPS dialogs, and every other
dashboard section, and renders dark regardless of any stored theme
(append `&theme=light` for a light stream). Do not point the streamer at a hive tracking private
repositories without reviewing what the tracker would show.

## Quick manual test with OBS (no cluster)

1. In OBS add a **Browser** source, 1920×1080, URL
   `https://<hive>.hive.hivecommons.dev/?kiosk=1#section-activity-monitor-section`;
   tick **Shutdown source when not visible** off.
2. Right-click the source → **Interact**, log in with GitHub once (OBS keeps
   the session cookie).
3. **Settings → Stream**: Service Twitch, paste the Primary Stream key.
4. **Settings → Output**: 4500 kbps CBR, keyframe interval 2 s, x264
   `veryfast`.
5. **Start Streaming**.

## Running the container from a laptop

```sh
cd src/deploy/stream && cp .env.example .env   # fill DASHBOARD_URL / DASHBOARD_TOKEN / TWITCH_STREAM_KEY
docker compose up --build
```

Dry run without Twitch: `STREAM_OUTPUT=null STREAM_SECONDS=20 docker compose up`
(or `STREAM_OUTPUT=/run/stream/out/test.flv` with `./out` mounted) proves
capture and encode without a stream key.
