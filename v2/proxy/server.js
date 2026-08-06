import express from 'express';
import { createProxyMiddleware } from 'http-proxy-middleware';
import path from 'path';
import fs from 'fs';
import crypto from 'crypto';
import { fileURLToPath } from 'url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));

const PROXY_PORT = parseInt(process.env.HIVE_PROXY_PORT || '3001', 10);
const GO_API_PORT = parseInt(process.env.HIVE_API_PORT || '3002', 10);
const GO_API_URL = process.env.HIVE_API_URL || `http://127.0.0.1:${GO_API_PORT}`;
const TTYD_PORT = parseInt(process.env.HIVE_TTYD_PORT || '7681', 10);
const TTYD_URL = `http://127.0.0.1:${TTYD_PORT}`;
const DASHBOARD_TOKEN = process.env.HIVE_DASHBOARD_TOKEN || '';
const STATIC_DIR = process.env.HIVE_STATIC_DIR || path.join(__dirname, 'public');
// C2 domain separation: the proxy verifies the hub-minted `hive_hub_user`
// session/terminal cookie, which is HMAC'd with the derived SESSION sub-key —
// NOT the master hub secret. A hub-hosted hive is injected HIVE_SESSION_KEY and
// never the master, so a spoke operator cannot forge a session cookie.
//
// SESSION_KEY resolution mirrors the Go SpokeSessionKey() helper:
//   1. HIVE_SESSION_KEY (modern least-privilege provisioning) if set, else
//   2. derive it from HIVE_HUB_SECRET via HMAC-SHA256(master, "hive-session-v1")
//      — the backward-compatible path for self-hosted/legacy spokes that still
//      hold the master. Both yield the identical key the hub signs with.
// The info string MUST stay byte-for-byte identical to infoSessionKey in
// v2/pkg/hub/hub_keys.go.
const INFO_SESSION_KEY = 'hive-session-v1';
function deriveSessionKey() {
  const explicit = (process.env.HIVE_SESSION_KEY || '').trim();
  if (explicit) return explicit;
  const master = (process.env.HIVE_HUB_SECRET || '').trim();
  if (!master) return '';
  return crypto.createHmac('sha256', master).update(INFO_SESSION_KEY).digest('hex');
}
const SESSION_KEY = deriveSessionKey();
// Boot-time "am I hub-hosted?" signal. Either the injected session key (modern)
// or a master secret (legacy) proves hosted mode, where identity comes from the
// hub cookie rather than a shared dashboard token.
const IS_HOSTED = SESSION_KEY !== '';

// HIVE_ID is this spoke's own hive identity, injected by the hub at provision
// time (mirrors the HIVE_ID env the Go dashboard reads). It is the anchor for
// per-hive terminal authorization: a hub-user cookie is authenticated hub-wide
// (the `hive_hub_user` cookie is scoped to .hive.kubestellar.io, so ANY hive's
// domain receives it), so the signature check alone proves only "some hub user",
// never "a user allowed on THIS hive".
const HIVE_ID = process.env.HIVE_ID || '';

// HIVE_AUTHORIZED_USERS is this hive's per-hive allowlist, injected by the hub
// (authorizedUsersForHive): a comma-separated list of `username` or
// `username:role` entries (owner + everyone with this hive in their SaaS
// Hives map). It is the proxy's INDEPENDENT, per-hive authorization source —
// the one defense that also holds on OpenShift Routes, which have no nginx
// auth-proxy in front of the pod to call /api/saas/auth-check.
//
// SECURITY (CWE-862, finding C3): without a per-hive check, one cookie from any
// free GitHub account that completed hub OAuth opened a shell in EVERY tenant's
// pod. We parse the allowlist into a lowercase username set (GitHub logins are
// case-insensitive) and require the verified cookie user to be a member.
const HIVE_AUTHORIZED_USERS = parseAuthorizedUsernames(process.env.HIVE_AUTHORIZED_USERS || '');

// HIVE_INGRESS_AUTHZ is set ("true") by the hub ONLY on the nginx-ingress lane,
// where an ingress auth-proxy per-hive-authorizes every /terminal request
// (auth-url=auth-check?hive=<id>) BEFORE it reaches this proxy. It is unset on
// the OpenShift-Route lane, where the Route forwards straight to the pod and
// this proxy is the ONLY per-hive gate.
//
// SECURITY (CWE-862, finding C3): this flag is what lets an empty allowlist fail
// CLOSED without breaking legitimate nginx access. With no ingress auth-proxy in
// front (OpenShift), an empty local allowlist can NOT be treated as "defer to
// the ingress" — there is no ingress to defer to — so the terminal must deny.
const HIVE_INGRESS_AUTHZ = (process.env.HIVE_INGRESS_AUTHZ || '') === 'true';

// parseAuthorizedUsernames turns "owner:owner,alice:read,bob" into a Set of
// lowercased usernames. It mirrors the Go parseAuthorizedUsers split (comma
// list, ":role" suffix optional) but keeps only the identity, which is all the
// terminal gate needs — role granularity stays with the dashboard/API layer.
function parseAuthorizedUsernames(raw) {
  const set = new Set();
  for (const entry of raw.split(',')) {
    const name = entry.split(':')[0].trim().toLowerCase();
    if (name) set.add(name);
  }
  return set;
}

// isAuthorizedForThisHive reports whether a verified hub username is allowed to
// open a terminal on THIS hive.
//
// Fail-CLOSED policy (finding C3 — no fail-open, even the narrow one):
//   - Populated allowlist (the normal hosted case — authorizedUsersForHive
//     always emits at least the owner): a user absent from it is DENIED.
//   - Empty allowlist (env unset/blank, e.g. an ownerless hive):
//       * nginx lane (HIVE_INGRESS_AUTHZ=true): an ingress auth-proxy already
//         per-hive-authorized this request before it reached the proxy, so the
//         proxy defers (returns true) rather than double-denying legitimate,
//         ingress-approved access.
//       * OpenShift-Route lane (HIVE_INGRESS_AUTHZ unset): there is NO ingress
//         auth-proxy — this proxy is the ONLY per-hive gate — so an empty
//         allowlist DENIES. A wide-open terminal on an ownerless OpenShift hive
//         is exactly the fail-open we refuse to ship.
function isAuthorizedForThisHive(username) {
  if (!username) return false;
  if (HIVE_AUTHORIZED_USERS.size === 0) {
    // No independent per-hive data. Defer to the ingress ONLY when an ingress
    // auth-proxy actually gates this hive; otherwise fail closed.
    return HIVE_INGRESS_AUTHZ;
  }
  return HIVE_AUTHORIZED_USERS.has(username.toLowerCase());
}

const HOSTED_SUFFIX = '.hive.kubestellar.io';

// isHostedHost decides whether the terminal auth gate applies to a request.
//
// SECURITY (CWE-862 bypass): the raw Host header can carry a :port suffix and a
// trailing FQDN dot, both of which a naive `host.endsWith('.hive.kubestellar.io')`
// would MISS — turning `hive-b.hive.kubestellar.io:443` or `hive-b.hive.kubestellar.io.`
// into a "non-hosted" request that skips the gate entirely and proxies straight
// to ttyd. Normalize first: lowercase, drop the port, strip a single trailing
// dot, THEN suffix-match.
function isHostedHost(rawHost) {
  let host = (rawHost || '').toLowerCase();
  const colon = host.indexOf(':');
  if (colon !== -1) host = host.slice(0, colon);
  if (host.endsWith('.')) host = host.slice(0, -1);
  return host.endsWith(HOSTED_SUFFIX);
}

// SECURITY (fail closed on empty dashboard token — CWE-306).
//
// The prior guard only fired when NODE_ENV === 'production', but NODE_ENV is
// never set in any deploy manifest, so it was dead code: every shipped
// self-hosted deploy ran with unauthenticated mutation endpoints. Reverse the
// polarity — the guard now fires in every real deploy and only the unit test
// (which sets NODE_ENV=test) opts out.
//
// Hosted hives INTENTIONALLY run with HIVE_DASHBOARD_TOKEN unset: identity is
// carried by the hub cookie, so a missing token is expected there and the proxy
// must still boot. A resolved SESSION_KEY (IS_HOSTED) proves hosted mode. Only a
// self-hosted deploy with neither a token nor a session key is truly
// unauthenticated, and that is what we refuse to start.
if (!DASHBOARD_TOKEN && !IS_HOSTED && process.env.NODE_ENV !== 'test') {
  console.error('[SECURITY] HIVE_DASHBOARD_TOKEN is not set and this hive is not hub-hosted (no HIVE_SESSION_KEY / HIVE_HUB_SECRET) — all mutation endpoints would be unauthenticated. Refusing to start. Set HIVE_DASHBOARD_TOKEN.');
  process.exit(1);
}

const app = express();
app.disable('x-powered-by');

function requireAuth(req, res, next) {
  if (!DASHBOARD_TOKEN) return next();
  const authHeader = req.headers.authorization || '';
  const match = authHeader.match(/^Bearer\s+(.+)$/i);
  if (!match) return res.status(401).json({ error: 'Unauthorized' });
  const supplied = Buffer.from(match[1]);
  const expected = Buffer.from(DASHBOARD_TOKEN);
  if (supplied.length !== expected.length || !crypto.timingSafeEqual(supplied, expected)) {
    return res.status(401).json({ error: 'Unauthorized' });
  }
  next();
}

// verifyHubUserCookie mirrors verifyHubUserCookieValue in v2/pkg/hub/hub_cookie.go
// EXACTLY. The `hive_hub_user` cookie value is `<username>.<sig>` where sig is
// base64url-unpadded HMAC-SHA256(key=SESSION_KEY, msg=username). The proxy holds
// no other secret and must not merely check that the cookie is non-empty (that
// was CWE-345: any `Cookie: hive_hub_user=x` yielded a shell in a
// credential-holding container). We recompute the HMAC and constant-time-compare
// the signature; a forged, edited, or legacy-unsigned cookie fails closed.
//
// Returns the verified username on success, or '' when the signature does not
// verify / the secret is unset / the value is malformed.
function verifyHubUserCookie(secret, value) {
  if (!secret || !value) return '';
  // SplitN from the right: the final segment is the signature.
  const idx = value.lastIndexOf('.');
  if (idx <= 0 || idx === value.length - 1) return '';
  const username = value.slice(0, idx);
  const sig = value.slice(idx + 1);
  const expected = crypto
    .createHmac('sha256', secret)
    .update(username)
    .digest('base64url'); // base64url is unpadded, matching Go's RawURLEncoding
  const suppliedBuf = Buffer.from(sig);
  const expectedBuf = Buffer.from(expected);
  if (suppliedBuf.length !== expectedBuf.length || !crypto.timingSafeEqual(suppliedBuf, expectedBuf)) {
    return '';
  }
  return username;
}

app.use((req, res, next) => {
  res.setHeader('Content-Security-Policy', [
    "default-src 'self'",
    "script-src 'self' 'unsafe-inline' https://cdn.redoc.ly",
    "style-src 'self' 'unsafe-inline' https://fonts.googleapis.com",
    "worker-src blob:",
    "img-src 'self' data: https:",
    "font-src 'self' https:",
    "connect-src 'self' https: ws: wss:",
    "object-src 'none'",
    "base-uri 'self'",
    "form-action 'self'",
    "frame-ancestors 'none'",
  ].join('; '));
  res.setHeader('X-Content-Type-Options', 'nosniff');
  res.setHeader('X-Frame-Options', 'DENY');
  res.setHeader('Referrer-Policy', 'strict-origin-when-cross-origin');
  res.setHeader('Cache-Control', 'no-store, no-cache, must-revalidate');
  res.setHeader('Pragma', 'no-cache');
  next();
});

const PUBLIC_POST_PATHS = ['/api/contribute/register'];
app.use((req, res, next) => {
  if (['POST', 'PUT', 'PATCH', 'DELETE'].includes(req.method)) {
    if (PUBLIC_POST_PATHS.some(p => req.url.startsWith(p))) return next();
    return requireAuth(req, res, next);
  }
  next();
});

const apiProxy = createProxyMiddleware({
  target: GO_API_URL,
  changeOrigin: true,
  pathRewrite: (path) => `/api${path}`,
  on: {
    proxyReq(proxyReq, req) {
      if (req.headers.upgrade) return;
      proxyReq.removeHeader('X-Hive-User');
      proxyReq.removeHeader('X-Hive-Role');
      if (DASHBOARD_TOKEN) {
        proxyReq.setHeader('X-Hive-Internal', DASHBOARD_TOKEN);
      } else {
        proxyReq.removeHeader('X-Hive-Internal');
      }
    },
    error(err, req, res) {
      console.error(`[proxy] ${req.method} ${req.url} → ${err.message}`);
      if (res.writeHead) {
        res.writeHead(502, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ error: 'Go API unavailable', detail: err.message }));
      }
    },
  },
});
// OpenAPI spec — serve from dashboard dir (bypasses Go API proxy)
const DASHBOARD_DIR = process.env.HIVE_DASHBOARD_DIR || path.join(__dirname, '..', 'dashboard');
app.get('/api/openapi.json', (_req, res) => {
  const specPath = path.join(DASHBOARD_DIR, 'openapi.json');
  try {
    res.type('json').send(fs.readFileSync(specPath, 'utf8'));
  } catch {
    res.status(404).json({ error: 'OpenAPI spec not found' });
  }
});

// Redoc API documentation (read-only)
app.get('/api-docs', (_req, res) => {
  res.type('html').send(`<!DOCTYPE html>
<html><head>
  <title>Hive API Reference</title>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <link href="https://fonts.googleapis.com/css?family=Montserrat:300,400,700|Roboto:300,400,700" rel="stylesheet">
  <style>body { margin: 0; padding: 0; }</style>
</head><body>
  <redoc spec-url="/api/openapi.json"></redoc>
  <script src="https://cdn.redoc.ly/redoc/latest/bundles/redoc.standalone.js"></script>
</body></html>`);
});

app.use('/api', apiProxy);

const ttydProxy = createProxyMiddleware({
  target: TTYD_URL,
  changeOrigin: true,
  pathRewrite: (p) => p.replace(/^\/terminal/, '') || '/',
  on: {
    error(err, req, res) {
      console.error(`[ttyd-proxy] ${req.method} ${req.url} → ${err.message}`);
      if (res.writeHead) {
        res.writeHead(502, { 'Content-Type': 'text/plain' });
        res.end('Terminal unavailable');
      }
    },
  },
});
app.use('/terminal', (req, res, next) => {
  const host = req.headers.host || '';
  const isHosted = isHostedHost(host);
  if (isHosted) {
    const cookies = (req.headers.cookie || '').split(';').reduce((acc, c) => {
      const [k, ...v] = c.trim().split('=');
      if (k) acc[k] = v.join('=');
      return acc;
    }, {});
    // SECURITY (CWE-345): the cookie is HMAC-signed by the hub. Verify the
    // signature — a non-empty value is NOT proof of authentication.
    const user = verifyHubUserCookie(SESSION_KEY, cookies['hive_hub_user']);
    if (!user) {
      if (req.headers.upgrade === 'websocket') {
        req.socket.destroy();
        return;
      }
      res.status(401).send('Terminal access requires authentication');
      return;
    }
    // SECURITY (CWE-862, finding C3): authentication is hub-WIDE — the signed
    // cookie only proves this is some real hub user, because it is scoped to
    // .hive.kubestellar.io and every hive's domain receives it. AUTHORIZATION is
    // per-hive: the user must be on THIS hive's allowlist. A cookie authorized on
    // hive A must not open a shell on hive B.
    if (!isAuthorizedForThisHive(user)) {
      console.warn(`[terminal] 403: hub user ${JSON.stringify(user)} not authorized for hive ${JSON.stringify(HIVE_ID)}`);
      if (req.headers.upgrade === 'websocket') {
        req.socket.destroy();
        return;
      }
      res.status(403).send('Not authorized for this hive');
      return;
    }
  }
  next();
}, ttydProxy);

app.use(express.static(STATIC_DIR, { index: false }));

const indexPath = path.join(STATIC_DIR, 'index.html');
let indexHtml = '';
try { indexHtml = fs.readFileSync(indexPath, 'utf8'); } catch { /* built at startup */ }

function serveIndex(_req, res) {
  if (!indexHtml) {
    try { indexHtml = fs.readFileSync(indexPath, 'utf8'); } catch { /* ignore */ }
  }
  // SECURITY: never inject the dashboard token into the served HTML. Doing so
  // handed the only API credential to every visitor who could reach this port,
  // reducing "authentication" to "can you open the page". The frontend now
  // obtains the token only from an operator who pastes it (stored in
  // localStorage), so the served page carries no secret.
  res.sendFile(indexPath);
}

const contributeProxy = createProxyMiddleware({
  target: GO_API_URL,
  changeOrigin: true,
  on: {
    proxyReq(proxyReq, req) {
      if (!req.headers.upgrade) {
        proxyReq.removeHeader('X-Hive-Internal');
        proxyReq.removeHeader('X-Hive-User');
        proxyReq.removeHeader('X-Hive-Role');
      }
      proxyReq.setHeader('X-Forwarded-Host', req.headers.host || '');
    },
  },
});
app.get('/contribute', contributeProxy);
app.get('/contribute/', contributeProxy);

const leaderboardProxy = createProxyMiddleware({
  target: GO_API_URL,
  changeOrigin: true,
});
app.get('/leaderboard', leaderboardProxy);
app.get('/leaderboard/', leaderboardProxy);

app.get('/snapshot/leaderboard', (_req, res) => res.redirect('/leaderboard'));

const snapshotProxy = createProxyMiddleware({
  target: GO_API_URL,
  changeOrigin: true,
});
app.get('/snapshot', snapshotProxy);

app.get('/', serveIndex);
app.get('/{*splat}', serveIndex);

const server = app.listen(PROXY_PORT, () => {
  console.log(`[hive-proxy] Dashboard proxy on :${PROXY_PORT} → Go API at ${GO_API_URL}`);
});

server.on('upgrade', (req, socket, head) => {
  if (req.url.startsWith('/api/contribute/ws')) {
    req.url = req.url.replace(/^\/api/, '');
    apiProxy.upgrade(req, socket, head);
    return;
  }
  if (req.url.startsWith('/terminal')) {
    const host = req.headers.host || '';
    const isHosted = isHostedHost(host);
    if (isHosted) {
      const cookies = (req.headers.cookie || '').split(';').reduce((acc, c) => {
        const [k, ...v] = c.trim().split('=');
        if (k) acc[k] = v.join('=');
        return acc;
      }, {});
      // SECURITY (CWE-345): verify the hub's HMAC signature, not mere existence.
      const wsUser = verifyHubUserCookie(SESSION_KEY, cookies['hive_hub_user']);
      if (!wsUser) {
        socket.destroy();
        return;
      }
      // SECURITY (CWE-862, finding C3): per-hive authorization on the WS upgrade,
      // mirroring the HTTP gate. A hub-authenticated user not on THIS hive's
      // allowlist gets the socket closed, not a shell.
      if (!isAuthorizedForThisHive(wsUser)) {
        console.warn(`[terminal-ws] 403: hub user ${JSON.stringify(wsUser)} not authorized for hive ${JSON.stringify(HIVE_ID)}`);
        socket.destroy();
        return;
      }
    } else if (DASHBOARD_TOKEN) {
      const params = new URL(req.url, `http://${req.headers.host}`).searchParams;
      const token = params.get('token') || '';
      const supplied = Buffer.from(token);
      const expected = Buffer.from(DASHBOARD_TOKEN);
      if (supplied.length !== expected.length || !crypto.timingSafeEqual(supplied, expected)) {
        socket.write('HTTP/1.1 401 Unauthorized\r\n\r\n');
        socket.destroy();
        return;
      }
    }
    ttydProxy.upgrade(req, socket, head);
  }
});
