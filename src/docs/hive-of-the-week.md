# Battle Log and Hive of the Week

Hive exposes a public showpiece feed for leaderboard pages:

- `GET /api/leaderboard/battle-log?limit=30` returns scrubbed public activity as actor → icon → target lines.
- `GET /api/leaderboard/hive-of-week` returns the current featured project, embed URLs, and README snippets.
- `GET /api/leaderboard/gource-log?project=owner/repo&week=YYYY-Www` returns a Gource custom log (`timestamp|username|type|path|colour`).

The Battle Log deliberately strips emails, URLs, token-like fields, and private-looking repository names from targets. Repository names are only emitted in Battle Log/gource/Hive-of-the-Week output when they appear in the comma-separated `HIVE_PUBLIC_REPOS` allowlist. It is for public display; the existing operator activity rail remains the control-panel view.

## Rendering

`src/scripts/gource-hive-of-the-week.sh` fetches the weekly source log and renders with a bee/swarm look:

- custom log input (`--log-format custom`)
- overview camera, highlighted users, hidden filenames/dirnames
- dark honey background (`060301`) with amber/honey event colours
- tuned bloom (`--bloom-multiplier 1.35`, `--bloom-intensity 0.55`)
- elastic swarm motion (`--elasticity 0.42`, `--user-scale 1.25`)
- `--output-ppm-stream -` piped to `ffmpeg` for MP4 output

The scheduled workflow is disabled until repository variable `HIVE_OF_WEEK_RENDER_ENABLED=true` is set and `HIVE_PUBLIC_BASE_URL` points at the public Hive hub. Configure `HIVE_PUBLIC_REPOS=owner/repo,owner/other` on the hub so only known-public projects can be featured. The workflow publishes `latest.mp4` and `latest.jpg` to the `hive-of-the-week` GitHub release. To archive source logs automatically, add secret `HIVE_PROXY_AUTH_TOKEN` containing the dashboard/proxy proof token for the hub; without that secret the render still publishes, but archive is skipped. Source logs can also be archived manually with:

```sh
curl -X POST "$HIVE_BASE_URL/api/leaderboard/hive-of-week/archive?project=owner/repo&week=YYYY-Www"
```

using an owner-authenticated dashboard session or token. Knowledge stores the gource log source only, never the video.

## README embed template

```md
[![Hive of the Week](https://github.com/hivecommons/hive/releases/download/hive-of-the-week/latest.jpg)](https://github.com/hivecommons/hive/releases/download/hive-of-the-week/latest.mp4)

Powered by Hive Battle Log · [Leaderboard](https://hive.example.com/contribute/leaderboard) · [Gource source](https://hive.example.com/api/leaderboard/gource-log?project=OWNER/REPO&week=YYYY-Www)
```

HTML embed:

```html
<video controls preload="metadata" poster="https://github.com/hivecommons/hive/releases/download/hive-of-the-week/latest.jpg">
  <source src="https://github.com/hivecommons/hive/releases/download/hive-of-the-week/latest.mp4" type="video/mp4">
</video>
```
