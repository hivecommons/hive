// Issue #7695 — X-Hive-Internal must be injected ONLY for requests that
// actually authenticated with the shared dashboard token, for EVERY method.
//
// The Go API grants a request carrying X-Hive-Internal == HIVE_DASHBOARD_TOKEN
// verified-OWNER trust (pkg/dashboard/server.go authenticate()), on the stated
// assumption that "the local gateway authenticates the browser with the same
// token, strips client identity headers, then injects X-Hive-Internal". This
// proxy used to hold up that assumption only for mutating methods: the auth
// gate skipped GET/HEAD, while the proxyReq hook injected the header on every
// request. Every anonymous GET to /api/* therefore executed on the Go side as
// a verified owner — including GET /api/config/download, which returns the raw
// hive.yaml (secrets) verbatim.
//
// These checks pin the fixed contract:
//   1. anonymous GET  → forwarded WITHOUT X-Hive-Internal (Go fails closed)
//   2. bearer GET     → forwarded WITH X-Hive-Internal
//   3. bearer POST    → forwarded WITH X-Hive-Internal (unchanged)
//   4. anonymous POST → 401 at the proxy (unchanged)
//   5. a client-forged X-Hive-Internal never reaches the Go API
//   6. hosted mode (no token): header absent, reads still pass through

import { createServer, request as httpRequest } from 'http';
import { strict as assert } from 'assert';
import { spawn } from 'child_process';
import { fileURLToPath } from 'url';
import path from 'path';

const __dirname = path.dirname(fileURLToPath(import.meta.url));

const PROXY_PORT = 19071;
const GO_PORT = 19072;
const TTYD_PORT = 19073;
const TOKEN = 'internal-header-test-token';

let goServer = null;
const children = [];

// Echoes back the trust-material headers the Go API would have seen.
function startMockGoApi() {
  const server = createServer((req, res) => {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({
      url: req.url,
      internal: req.headers['x-hive-internal'] ?? null,
      user: req.headers['x-hive-user'] ?? null,
      role: req.headers['x-hive-role'] ?? null,
    }));
  });
  return new Promise(resolve => server.listen(GO_PORT, () => resolve(server)));
}

function startProxy(extraEnv = {}) {
  return new Promise((resolve, reject) => {
    const proc = spawn('node', ['server.js'], {
      cwd: __dirname,
      env: {
        ...process.env,
        HIVE_PROXY_PORT: String(PROXY_PORT),
        HIVE_API_PORT: String(GO_PORT),
        HIVE_TTYD_PORT: String(TTYD_PORT),
        HIVE_DASHBOARD_TOKEN: '',
        HIVE_STATIC_DIR: __dirname,
        NODE_ENV: 'test',
        ...extraEnv,
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    children.push(proc);
    let started = false;
    proc.stdout.on('data', (d) => {
      if (!started && d.toString().includes('hive-proxy')) {
        started = true;
        resolve(proc);
      }
    });
    proc.stderr.on('data', (d) => { if (!started) console.error('proxy stderr:', d.toString()); });
    proc.on('error', reject);
    setTimeout(() => { if (!started) reject(new Error('proxy start timeout')); }, 10000);
  });
}

function stopProxy(proc) {
  return new Promise((resolve) => {
    if (!proc || proc.exitCode !== null) return resolve();
    proc.on('exit', () => resolve());
    proc.kill('SIGKILL');
  });
}

function req(method, path, headers = {}) {
  return new Promise((resolve, reject) => {
    const r = httpRequest(
      { host: '127.0.0.1', port: PROXY_PORT, path, method, headers },
      (res) => {
        let body = '';
        res.on('data', (c) => { body += c; });
        res.on('end', () => resolve({ status: res.statusCode, body }));
      },
    );
    r.on('error', reject);
    r.end();
  });
}

const results = [];
async function check(name, fn) {
  try {
    await fn();
    results.push([true, name]);
    console.log(`  PASS  ${name}`);
  } catch (err) {
    results.push([false, name]);
    console.error(`  FAIL  ${name}: ${err.message}`);
  }
}

async function main() {
  goServer = await startMockGoApi();

  // ── token-secured self-hosted mode ──────────────────────────────────────
  let proxy = await startProxy({ HIVE_DASHBOARD_TOKEN: TOKEN });

  await check('anonymous GET /api/* is forwarded WITHOUT X-Hive-Internal (#7695)', async () => {
    const res = await req('GET', '/api/config/download');
    assert.equal(res.status, 200); // the mock Go API always answers; trust is in the headers
    const seen = JSON.parse(res.body);
    assert.equal(seen.internal, null,
      'unauthenticated read must carry no owner-equivalent trust material');
  });

  await check('client-forged X-Hive-Internal / identity headers never reach the Go API', async () => {
    const res = await req('GET', '/api/status', {
      'X-Hive-Internal': TOKEN,
      'X-Hive-User': 'attacker',
      'X-Hive-Role': 'owner',
    });
    const seen = JSON.parse(res.body);
    assert.equal(seen.internal, null);
    assert.equal(seen.user, null);
    assert.equal(seen.role, null);
  });

  await check('bearer-authenticated GET is forwarded WITH X-Hive-Internal', async () => {
    const res = await req('GET', '/api/config/download', { Authorization: `Bearer ${TOKEN}` });
    const seen = JSON.parse(res.body);
    assert.equal(seen.internal, TOKEN);
  });

  await check('wrong bearer on GET is forwarded WITHOUT X-Hive-Internal', async () => {
    const res = await req('GET', '/api/status', { Authorization: 'Bearer wrong-token' });
    const seen = JSON.parse(res.body);
    assert.equal(seen.internal, null);
  });

  await check('bearer-authenticated POST is forwarded WITH X-Hive-Internal (unchanged)', async () => {
    const res = await req('POST', '/api/pause', { Authorization: `Bearer ${TOKEN}` });
    assert.equal(res.status, 200);
    const seen = JSON.parse(res.body);
    assert.equal(seen.internal, TOKEN);
  });

  await check('anonymous POST is still 401 at the proxy (unchanged)', async () => {
    const res = await req('POST', '/api/pause');
    assert.equal(res.status, 401);
  });

  await check('public POST /api/contribute/register still bypasses the token gate', async () => {
    const res = await req('POST', '/api/contribute/register');
    assert.equal(res.status, 200);
    const seen = JSON.parse(res.body);
    // It is admitted without a credential, so it must carry no trust material.
    assert.equal(seen.internal, null);
  });

  await stopProxy(proxy);

  // ── hosted mode: no shared token exists at all ──────────────────────────
  proxy = await startProxy({ HIVE_SESSION_KEY: 'hosted-session-key' });

  await check('hosted mode: GET passes through with no X-Hive-Internal', async () => {
    const res = await req('GET', '/api/status');
    assert.equal(res.status, 200);
    const seen = JSON.parse(res.body);
    assert.equal(seen.internal, null);
  });

  await stopProxy(proxy);

  const failed = results.filter(([ok]) => !ok).length;
  console.log(`\n${results.length - failed}/${results.length} checks passed`);
  process.exit(failed ? 1 : 0);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
}).finally(() => {
  for (const c of children) { try { c.kill('SIGKILL'); } catch { /* ignore */ } }
  if (goServer) goServer.close();
});
