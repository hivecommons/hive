# Hive dashboard reverse proxy

`src/proxy` is the Node/Express reverse proxy that fronts the dashboard, Go API, terminal, contribution, leaderboard, and snapshot routes.

## Role in the architecture

The proxy listens on `HIVE_PROXY_PORT` (default `3001`) and forwards API traffic to the Go dashboard API at `HIVE_API_URL` or `127.0.0.1:${HIVE_API_PORT}` (default `3002`). It also proxies ttyd on `/terminal`, contribution WebSockets on `/api/contribute/ws`, and selected public pages such as `/leaderboard` and `/snapshot`.

## Auth headers

For mutating `/api` requests, the proxy requires a bearer token when `HIVE_DASHBOARD_TOKEN` is set. It strips user-supplied `X-Hive-User`, `X-Hive-Role`, and `X-Hive-Internal` headers before proxying and injects `X-Hive-Internal` itself for trusted API calls. This prevents a browser client from forging dashboard-internal identity headers.

Terminal access requires either the hosted hub identity cookie plus per-hive authorization, or a spoke-minted `hive_terminal_assertion` cookie prepared by the Go dashboard. The shared dashboard token is never accepted from `?token=` terminal URLs; token-secured browser clients must call `/api/terminal/handoff` with the normal Authorization header before navigating to `/terminal`.

## Security headers

Every response gets a restrictive CSP, `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin`, and no-store cache headers. `X-Frame-Options: DENY` is set except for `/snapshot` when the Go API returns an explicit HTTPS frame-ancestor allowlist; that route relies on CSP `frame-ancestors` because `X-Frame-Options` has no allowlist form.

## Local development

```bash
cd src/proxy
npm install
HIVE_DASHBOARD_TOKEN=dev HIVE_API_URL=http://127.0.0.1:3002 npm start
npm test
```

Set `HIVE_STATIC_DIR` to serve a built dashboard from a non-default path.
