# Root dashboard directory

This directory is a **legacy/static dashboard support area**, not the primary v2 dashboard server.

The live v2 dashboard is implemented in Go under `src/pkg/dashboard/`, with the current frontend embedded from `src/pkg/dashboard/static/index.html` and served through the Node auth/rewrite proxy in `src/proxy/`. The v2 image still copies this root `dashboard/` directory to `/opt/hive/dashboard/` because the v2 dashboard API calls `build-snapshot.mjs` from there when it builds read-only `/snapshot` pages, and the publish scripts use the same builder for kubestellar.io live snapshots.

## What is here

- `build-snapshot.mjs` — supported v2 snapshot builder. It reads the live v2 dashboard HTML, fetches dashboard API data, and writes a self-contained static snapshot.
- `publish-snapshot-v2.sh` — publishes per-instance v2 static snapshots and API docs into the `kubestellar/docs` site.
- `publish-snapshot.sh` — older single-instance snapshot publisher retained for the original `/live/hive` path.
- `openapi.json` — static OpenAPI snapshot used as a fallback when live `/api/openapi.json` is unavailable during snapshot publishing.
- `server.js` and `index.html` — legacy Node dashboard assets from the pre-v2 dashboard path. They are useful historical/reference material, but v2 production does not start `dashboard/server.js`. The legacy server binds `127.0.0.1` by default because its control endpoints are unauthenticated; set `HIVE_DASHBOARD_BIND=0.0.0.0` only behind an authenticated proxy.
- `agent-activity.py`, `agent-metrics.sh`, `agent-summaries.sh`, `api-collector.sh`, `token-collector.sh`, `health-check.sh` — operational/metrics helper scripts from the legacy dashboard stack.
- `ubersicht/` — macOS Übersicht widget assets.
- `test/` — Node tests for contributor/dashboard behavior that still lives with the legacy Node package.

## Running tests

```bash
cd dashboard
npm test
```

The package only declares the Node test script; it is separate from the Go test suite under `src/`.

## Editing guidance

For live v2 dashboard features, edit `src/pkg/dashboard/`, `src/pkg/dashboard/static/index.html`, or `src/proxy/`. Edit this directory when changing snapshot generation/publishing, the fallback OpenAPI bundle, or legacy helper scripts.
