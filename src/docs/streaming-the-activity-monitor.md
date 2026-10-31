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
| **OOMKilled** / `Killed Xvfb` then `a supervised process left the loop` | The pod ran out of memory, almost always the x11grab frame queue after the encoder stalled (see [Resources and memory](#resources-and-memory)). Check `ipcs -m` in the pod: hundreds of 8 MiB segments means ffmpeg is hoarding frames — lower `VIDEO_QUEUE_FRAMES` or give the encoder CPU. |
| Pod is `Running` but Twitch is offline | `healthcheck.sh` in the pod; if `frames` is not increasing, the ffmpeg sink is failing — look for `rtmp` errors in the logs. |

## Resources and memory

The Deployment requests 1 CPU / 3 Gi and limits 3 CPU / 6 Gi. Where it goes:

| Consumer | Typical | Notes |
|---|---|---|
| Chromium (all processes) | ~1.0–1.3 Gi | `--renderer-process-limit=1`, `--js-flags=--max-old-space-size=1024`, `--disable-dev-shm-usage`; the supervisor reloads the tab when the sum of Chromium RSS passes `CHROMIUM_RSS_RELOAD_MIB` (3584) instead of waiting for the OOM killer. |
| ffmpeg + Xvfb shared frames | ≤ `VIDEO_QUEUE_FRAMES` × 8.3 MiB (16 → ~130 Mi) | x11grab hands ffmpeg one MIT-SHM segment per raw 1080p frame, attached to both processes and charged to the cgroup once. If x264 falls behind, the input thread queues frames up to `-thread_queue_size`; the old 1024-deep queue could pin 8.5 Gi and OOMKilled the pod in a loop. Frames are skipped when the bounded queue is full. |
| `/dev/shm` memory emptyDir | ≤ 512 Mi | Memory-backed emptyDirs count against the container limit. |
| ffmpeg private, PulseAudio, Xvfb framebuffer | ~150 Mi | PulseAudio runs with only the null sink loaded; the pulse input queue holds tiny packets. |

CPU: software x264 at 1080p30 `veryfast` needs about two cores on arm64 plus
Chromium's software rasteriser (`--disable-gpu`). x264 threads are sized from
the cgroup CPU quota (`ENC_THREADS`), not `nproc`, so the encoder does not
oversubscribe a throttled pod. If `ffmpeg-progress.txt` shows `speed=` below
`1.0x` or `drop_frames` climbing, drop `X264_PRESET` to `superfast` before
raising the CPU limit.

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

The main `twitch.tv/hivecommons` stream never shows Matrix; it remains the
full-screen Activity Monitor.

## Matrix mode (separate channel)

Matrix is an experimental, default-off code display for a separate channel,
[`twitch.tv/hivecommonsmatrix`](https://www.twitch.tv/hivecommonsmatrix).
It depends on the Matrix display renderer (`/?kiosk=1&view=matrix`,
[#15936](https://github.com/hivecommons/hive/issues/15936)); without that
renderer the dashboard ignores `view=matrix` and the kiosk serves the ordinary
Activity Monitor. When active it shows pushed added lines only, from positively
identified hive identities, on public repositories explicitly allowlisted in
`dashboard.matrix.repos`.
Private patches, unverified authors and excluded paths are never shown. Lines
touched by scrubbing are dropped entirely rather than displayed with redactions;
the built-in secret/path filters and `dashboard.matrix.exclude_paths` apply.
Dashboard authentication alone does not make a code stream safe for public
Twitch broadcasting: the public allowlist, author restriction and scrub/drop
rules are required.

[`overlays/hive-hosted-matrix/`](../deploy/stream/overlays/hive-hosted-matrix/kustomization.yaml)
reuses the streamer image and base as a separate instance. It renders Deployment,
ConfigMap and NetworkPolicy `matrix-hive-stream` in namespace `hive-stream-matrix`,
with separate pod selectors and secret references scoped to that namespace.
It opens `/?kiosk=1&view=matrix&sound=off` and sets `STREAM_AUDIO=silent`.
Its independent process and deployment isolate restarts and failures from the
existing stream. The `hive-hosted` overlay and `twitch.tv/hivecommons` are
unaffected.

**Keep this broadcast OFF until the owner authorizes it and provisions the
channel.** The overlay has `replicas: 0`; applying it starts no streamer.
Channel availability alone does not authorize activation. The owner must:

1. Confirm the deployed dashboard build serves the Matrix display for
   `view=matrix` (#15936). A build without it broadcasts the ordinary kiosk
   Activity Monitor on the Matrix channel, not code lines.
2. Enable `dashboard.matrix` with an explicit, reviewed public repo allowlist
   and verify the Matrix display and its author restriction and scrub/drop rules
   before any broadcast. An empty allowlist captures nothing.
3. Provision `hivecommonsmatrix` independently, create namespace
   `hive-stream-matrix`, and create the out-of-band
   `twitch-stream-matrix` Secret there with its
   `TWITCH_STREAM_KEY`. Provision a separate `dashboard-token` Secret there
   for the dashboard source. Never reuse the main channel's `twitch-stream`
   Secret, or put key values in source, issues or documentation.
4. Apply the Matrix overlay and deliberately scale `matrix-hive-stream` above
   zero only after authorization and readiness checks. Applying the overlay,
   creating the Twitch Secret and scaling up are all owner-only actions;
   this configuration does not authorize deployment or activation.

Review the manifests without applying them:

```sh
kubectl kustomize src/deploy/stream/overlays/hive-hosted-matrix
```

## The hivecommonsfleet broadcast

The hub-wide monitor (RFC [#15861](https://github.com/hivecommons/hive/issues/15861),
proposed by @hanthor) gets its **own** channel,
[`twitch.tv/hivecommonsfleet`](https://www.twitch.tv/hivecommonsfleet), and
its own deployment in
[`src/deploy/stream/fleet/`](../deploy/stream/fleet/kustomization.yaml)
([#15966](https://github.com/hivecommons/hive/issues/15966)). Everything above
— the `hive-stream` namespace, `k8s/`, `overlays/hive-hosted/` and the
`twitch.tv/hivecommons` full-screen Activity Monitor — is untouched by it; a
CI test (`src/internal/streamdeploy`) pins those files byte for byte.

| | `twitch.tv/hivecommons` | `twitch.tv/hivecommonsfleet` |
| --- | --- | --- |
| Manifests | `k8s/` + `overlays/hive-hosted/` | `fleet/` (standalone, shares no resource) |
| Namespace / Deployment | `hive-stream` / `hive-stream` | `hive-stream-fleet` / `hive-stream-fleet` |
| Source | hive's spoke dashboard, in-cluster, shared dashboard token | the hub's **public** origin, anonymous — no dashboard credential exists or is accepted |
| Stream key Secret | `twitch-stream` | `twitch-stream-hivecommonsfleet` |
| Egress | DNS, spoke Service, public 443/1935 | DNS, public 443/1935 only (no in-cluster rule) |
| Default | running | **disabled** |

It reuses the same `hive-stream` image. Its ConfigMap sets
`STREAM_PROFILE=public-safe`, which turns on two guards in the image:

- **`public-safe-gate.sh`** runs before any process starts and exits the
  container unless *all* of the following hold: `STREAM_ENABLED` is exactly
  `true`; `PUBLIC_SAFE_POLICY_ACK` equals the current policy version
  (`hivecommonsfleet-public-safe-v1`); `DASHBOARD_TOKEN` is **unset**;
  `DASHBOARD_URL` is an `https://` origin and neither it nor `DASHBOARD_PATH`
  holds the `CHANGE-ME` placeholder; and `TWITCH_STREAM_KEY` is installed (a
  local dry run with `STREAM_OUTPUT=null` or an absolute local `*.flv` path
  needs no key; a URL such as `rtmp://…/x.flv` or any other raw
  `STREAM_OUTPUT` is refused). Shipped defaults fail every one of
  these, and the Deployment is at `replicas: 0` on top.
- **The auth proxy runs anonymous.** It refuses to start with a token, strips
  any `Authorization` or `Cookie` header the browser sends and drops
  `Set-Cookie` from responses, so no session can form. The kiosk sees exactly
  what an anonymous visitor of the hub sees, and nothing else.

### Dashboard authentication does not protect Twitch output

A Twitch stream is public. Anything painted in the kiosk is visible to anyone,
recordable and clippable, whatever login the dashboard or hub requires and
whoever was allowed to open the page. A user's private, per-user Fleet
activity view (what *they* may see on the hub) therefore never authorizes a
broadcast of its rows, identities or repository data. That is why this
deployment carries no dashboard credential at all instead of a "viewer"
token: there is no account whose visibility could leak onto the stream.

### Public-safe content policy

Policy version **`hivecommonsfleet-public-safe-v1`**. A view may be streamed
on `hivecommonsfleet` only when every row and every value it renders is:

1. from a **public** repository (no private-repository rows, titles, numbers,
   labels or counts broken out per private repository);
2. information **GitHub already shows publicly** for that item: number,
   title, state, labels, public author login, CI and merge state;
3. served to an **anonymous** request — no per-user or per-hive access
   decision may widen what the page shows;
4. free of tokens, settings, operator or agent terminal output, chat,
   e-mail addresses, and any identity beyond public GitHub logins.

Aggregate totals that include private items are allowed only as a single
fleet-wide number that cannot be attributed to a repository.

Bump the version (here, in `public-safe-gate.sh` and in
`src/internal/streamdeploy`) whenever this text changes, so an old
acknowledgement cannot cover new content.

### Enabling it (owner only)

Nothing below is done by this repository or by the hive; it is the owner's
decision on #15861. Status as of this change: the channel is provisioned; the
stream key is **not** installed and the public-safe hub view does **not** yet
exist (it follows the Fleet activity page,
[#15965](https://github.com/hivecommons/hive/issues/15965)).

1. **Source.** Confirm a hub route renders the public-safe view anonymously
   and meets the policy above, then set `DASHBOARD_PATH` in
   `fleet/configmap.yaml` (e.g. with `?kiosk=1`). Open it in a private
   browser window first: what you see there is what Twitch will show.
2. **Stream key.** In the `hivecommonsfleet` Creator Dashboard, copy the
   Primary Stream key and install it without echoing it or writing it to a
   file, a commit or an issue comment:

   ```sh
   kubectl --context hive-oke apply -k src/deploy/stream/fleet   # namespace, config, NetworkPolicy; replicas 0
   kubectl --context hive-oke -n hive-stream-fleet create secret generic twitch-stream-hivecommonsfleet \
     --from-literal=TWITCH_STREAM_KEY="$(read -rs k; printf %s "$k")"
   ```

3. **Enable.** Set `STREAM_ENABLED: "true"` and
   `PUBLIC_SAFE_POLICY_ACK: "hivecommonsfleet-public-safe-v1"` in the
   ConfigMap, re-apply, then
   `kubectl --context hive-oke -n hive-stream-fleet scale deploy/hive-stream-fleet --replicas=1`.
   The logs must show `public-safe gate: enabled` and
   `authproxy: … (public-safe: anonymous, no credential, no cookies)`.

To stop it, scale back to `0` or set `STREAM_ENABLED: "false"`; the
`hivecommons` stream is unaffected either way. A gate refusal reads
`stream: public-safe gate: refusing to start: <reason>` in the pod log and
names the missing condition without printing any value.

## Display mode and URL views

For an attended screen (a wall monitor or TV someone can walk up to), use
**display mode** instead of kiosk: press **📺** on the Activity Monitor header,
or open `/?display=1`. It uses the same tracker-only layout but keeps the
cursor and your sound setting, hides every row action, and is left with
**Esc** or **d**. Entering it never requests full screen or a wake lock.
`?kiosk=1` is unchanged and wins when both flags are present.

Moving the pointer, tapping, or tabbing to it reveals a small control overlay
(top right) that hides again after 3 seconds idle. Each control is separate
and acts only when clicked:

| Control | What it does |
|---|---|
| **✕ Exit display** | Leaves display mode, the same as **Esc**. |
| **🔗 Copy link** | Copies a link to this display: `display=1`, the current `am.*` view (or the playlist's `am.view` + `interval`), `theme`, and the `density=tv` / `shift=1` display options when set, and nothing else — never a token or cookie. If the clipboard is unavailable or refused, the link is shown in a selectable field instead. |
| **⛶ Full screen** | The same full screen as the header's ⛶ button. Disabled with a "not supported" tooltip where the browser has no Fullscreen API. |
| **☀ Keep awake** | Asks the browser to keep the screen awake (Screen Wake Lock). Shows *on*, *off*, *denied* (with the browser's reason; it is not retried automatically) or is disabled where unsupported. While on, it is re-requested when the tab becomes visible again; it is released on exit and is not remembered after a reload. |

None of these controls writes anything to local storage. Kiosk mode never
shows the overlay.

### Playlists

A display can rotate between several views. Spell each view out as a
URL-encoded `am.*` query string in a repeated `am.view` parameter, and set the
time per view with `interval`:

```text
/?display=1&am.view=am.needsHuman%3D1&am.view=am.kind%3Dpr%26am.repo%3Dhivecommons%2Fhive&am.view=am.kind%3Dissue%26am.group%3Drepo&interval=2m
```

- `interval` is `Ns` or `Nm`, clamped to 10 seconds – 1 hour; the default is
  60 seconds.
- A view with no recognised `am.*` key is skipped. With fewer than two valid
  views the display does not rotate.
- The current view follows the wall clock, so a reload or a dropped
  connection resumes on the same view. Rotation happens in memory only: it
  does not navigate, refetch or write to local storage.
- A thin bar at the top shows the time until the next view; with reduced
  motion it is a static *next in Ns* label instead.
- Playlists exist only in the URL. There are no named presets in `hive.yaml`,
  and playlists apply only in display mode, never to `?kiosk=1`.

A display says honestly when its data is old. Its header shows the hive id,
a clock and a live dot. When the event stream has been silent for more than
two poll intervals (about a minute), or the tracker payload is older than two
server cache TTLs, the dot turns amber, the board dims and a banner reads
*Stale: last update N ago*. It recovers by itself when updates resume. If
the stream fails to reconnect five times in a row, the display reloads
itself after 30 s, doubling on each further reload up to 10 minutes; it also
reloads once after a hive upgrade finishes. A reload keeps the URL, so the
view survives. The ordinary Activity Monitor and `?kiosk=1` do not dim, and
the "updated … ago" chip behaves as before everywhere.

Two wall-screen options apply only together with `display=1` and are ignored
otherwise:

| Parameter | Effect |
|---|---|
| `density=tv` | Larger type and only the stage, title and actor of each row. No filters, row chips or pager: the rows that fit the screen, then a "+N more" footer. Recomputed when the window is resized. `tv` is the only value; anything else is ignored. |
| `shift=1` | OLED burn-in protection: the board moves 1–2 px to a new spot every 4 minutes. Off by default. |

With the operating system's *reduce motion* setting on, a display plays no
row movement, no burn-in shift and no dim transition.

Example: `/?display=1&density=tv&shift=1#section-activity-monitor-section`.

Any Activity Monitor filter can be described in the URL with `am.*`
parameters, so a display link reproduces the view in a fresh browser profile:

| Parameter | Values |
|---|---|
| `am.kind` | `pr`, `issue` (default all) |
| `am.repo` | `owner/name` |
| `am.q` | free text |
| `am.labels` | comma-separated label names |
| `am.needsHuman`, `am.onHold`, `am.attention` | `1` |
| `am.age` | `1h`, `1d`, `1w`, `stale` |
| `am.author` | `human`, `clanker`, `hive` |
| `am.acted` | `awaiting`, `acted`, `stuck` |
| `am.sort` | `furthest`, `waiting`, `newest`, `number` |
| `am.dir` | `asc` (default `desc`) |
| `am.group` | `repo` (default `none`) |

Example: `/?display=1&am.kind=pr&am.repo=hivecommons/hive&am.group=repo#section-activity-monitor-section`.
Unknown values fall back to the default. When any `am.*` parameter is present
it wins over the browser's saved filter, and that tab never writes its filter
or grouping back to local storage. Waiting on you is per user and stays on
`?filter=waiting-on-you`.

A link describes a view; it does not grant access. The viewer still signs in
with their own access, and display mode grants no extra privileges. Do not
put a display that tracks private repositories where people without access
can see it without reviewing what the tracker would show.

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
