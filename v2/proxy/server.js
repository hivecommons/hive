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
const SNAPSHOT_FRAME_ANCESTORS_FALLBACK = parseSnapshotFrameAncestors(process.env.HIVE_SNAPSHOT_FRAME_ANCESTORS || '');

if (!DASHBOARD_TOKEN && process.env.NODE_ENV === 'production') {
  console.error('[SECURITY] HIVE_DASHBOARD_TOKEN is not set — all mutations are unauthenticated!');
  process.exit(1);
}

const app = express();
app.disable('x-powered-by');

function parseSnapshotFrameAncestors(raw) {
  const origins = raw.split(/[\s,]+/).map(v => v.trim()).filter(Boolean);
  const seen = new Set();
  const dnsHostPattern = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*\.?$/i;
  const ipHostPattern = /^(?:\d{1,3}\.){3}\d{1,3}$|^\[[0-9a-f:.]+\]$/i;
  return origins.map((origin) => {
    let parsed;
    try {
      parsed = new URL(origin);
    } catch {
      throw new Error(`invalid HIVE_SNAPSHOT_FRAME_ANCESTORS entry ${origin}: expected an https origin`);
    }
    if (
      parsed.protocol !== 'https:' ||
      !parsed.hostname ||
      parsed.username ||
      parsed.password ||
      parsed.pathname !== '/' ||
      parsed.search ||
      parsed.hash ||
      parsed.hostname.includes('*') ||
      (!dnsHostPattern.test(parsed.hostname) && !ipHostPattern.test(parsed.hostname)) ||
      (parsed.port && (Number(parsed.port) < 1 || Number(parsed.port) > 65535))
    ) {
      throw new Error(`invalid HIVE_SNAPSHOT_FRAME_ANCESTORS entry ${origin}: expected exact https://host[:port] origin`);
    }
    const normalized = `https://${parsed.host}`;
    if (seen.has(normalized)) return '';
    seen.add(normalized);
    return normalized;
  }).filter(Boolean);
}

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

async function snapshotFrameAncestors() {
  try {
    const headers = {};
    if (DASHBOARD_TOKEN) headers['X-Hive-Internal'] = DASHBOARD_TOKEN;
    const resp = await fetch(`${GO_API_URL}/api/snapshot/frame-ancestors`, { headers });
    if (!resp.ok) return SNAPSHOT_FRAME_ANCESTORS_FALLBACK;
    const data = await resp.json();
    return parseSnapshotFrameAncestors((data.origins || []).join(' '));
  } catch {
    return SNAPSHOT_FRAME_ANCESTORS_FALLBACK;
  }
}

app.use(async (req, res, next) => {
  const frameAllowlist = req.path === '/snapshot' ? await snapshotFrameAncestors() : [];
  const snapshotFramingAllowed = frameAllowlist.length > 0;
  const frameAncestors = snapshotFramingAllowed ? frameAllowlist.join(' ') : "'none'";
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
    `frame-ancestors ${frameAncestors}`,
  ].join('; '));
  res.setHeader('X-Content-Type-Options', 'nosniff');
  // X-Frame-Options has no allowlist form; rely on CSP frame-ancestors for the
  // explicitly allowlisted public /snapshot document, keep DENY everywhere else.
  if (!snapshotFramingAllowed) res.setHeader('X-Frame-Options', 'DENY');
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
  const isHosted = host.endsWith('.hive.kubestellar.io');
  if (isHosted) {
    const cookies = (req.headers.cookie || '').split(';').reduce((acc, c) => {
      const [k, ...v] = c.trim().split('=');
      if (k) acc[k] = v.join('=');
      return acc;
    }, {});
    const user = cookies['hive_hub_user'];
    if (!user) {
      if (req.headers.upgrade === 'websocket') {
        req.socket.destroy();
        return;
      }
      res.status(401).send('Terminal access requires authentication');
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
  if (DASHBOARD_TOKEN && indexHtml) {
    const injection = `<script>if(!localStorage.getItem('hive-token'))localStorage.setItem('hive-token',${JSON.stringify(DASHBOARD_TOKEN)});</script>`;
    res.type('html').send(indexHtml.replace('</head>', injection + '</head>'));
  } else {
    res.sendFile(indexPath);
  }
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
    const isHosted = host.endsWith('.hive.kubestellar.io');
    if (isHosted) {
      const cookies = (req.headers.cookie || '').split(';').reduce((acc, c) => {
        const [k, ...v] = c.trim().split('=');
        if (k) acc[k] = v.join('=');
        return acc;
      }, {});
      if (!cookies['hive_hub_user']) {
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
