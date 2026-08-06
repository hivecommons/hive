import { createServer, request as httpRequest } from 'http';
import { WebSocketServer, WebSocket } from 'ws';
import { strict as assert } from 'assert';
import { spawn } from 'child_process';
import { fileURLToPath } from 'url';
import path from 'path';
import crypto from 'crypto';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PROXY_PORT = 19001;
const GO_PORT = 19002;
const TTYD_PORT = 19003;

let goServer, ttydServer, proxyProcess;

async function waitForPort(port, timeoutMs = 10000) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    try {
      await new Promise((resolve, reject) => {
        const req = createServer().listen(port, () => {
          req.close();
          reject(new Error('port free'));
        });
        req.on('error', () => resolve());
      });
      return;
    } catch {
      await new Promise(r => setTimeout(r, 200));
    }
  }
  throw new Error(`Port ${port} not ready after ${timeoutMs}ms`);
}

function setupMockGoBackend() {
  const server = createServer((req, res) => {
    if (req.url === '/api/health') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end('{"status":"ok"}');
      return;
    }
    if (req.url === '/api/contribute/status') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end('{"hub":"online","active_contributors":0,"total_registered":0,"actionable_items":0}');
      return;
    }
    res.writeHead(404);
    res.end('not found');
  });

  const wss = new WebSocketServer({ server, path: '/api/contribute/ws' });
  wss.on('connection', (ws) => {
    ws.send(JSON.stringify({ type: 'auth_challenge', seq: 1, nonce: 'test123' }));
    ws.on('message', (data) => {
      const msg = JSON.parse(data.toString());
      if (msg.type === 'ping') {
        ws.send(JSON.stringify({ type: 'pong', seq: msg.seq }));
      }
    });
  });

  return new Promise(resolve => {
    server.listen(GO_PORT, () => resolve(server));
  });
}

function setupMockTtyd() {
  const server = createServer((req, res) => {
    res.writeHead(200);
    res.end('ttyd');
  });
  return new Promise(resolve => {
    server.listen(TTYD_PORT, () => resolve(server));
  });
}

function startProxy() {
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
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let started = false;
    proc.stdout.on('data', (d) => {
      if (!started && d.toString().includes('hive-proxy')) {
        started = true;
        resolve(proc);
      }
    });
    proc.stderr.on('data', (d) => {
      if (!started) {
        console.error('proxy stderr:', d.toString());
      }
    });
    proc.on('error', reject);
    setTimeout(() => { if (!started) reject(new Error('proxy start timeout')); }, 10000);
  });
}

async function setup() {
  goServer = await setupMockGoBackend();
  ttydServer = await setupMockTtyd();
  proxyProcess = await startProxy();
  await new Promise(r => setTimeout(r, 500));
}

async function teardown() {
  if (proxyProcess) proxyProcess.kill();
  if (goServer) goServer.close();
  if (ttydServer) ttydServer.close();
  await new Promise(r => setTimeout(r, 200));
}

async function testWSContributeConnect() {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(`ws://localhost:${PROXY_PORT}/api/contribute/ws`);
    const timeout = setTimeout(() => { ws.close(); reject(new Error('WS timeout')); }, 5000);
    ws.on('open', () => console.log('  ✓ WS opened'));
    ws.on('message', (data) => {
      clearTimeout(timeout);
      const msg = JSON.parse(data.toString());
      console.log('  ✓ Received:', msg.type);
      assert.equal(msg.type, 'auth_challenge', 'Expected auth_challenge');
      assert.ok(msg.nonce, 'Expected nonce');
      ws.close();
      resolve();
    });
    ws.on('error', (e) => {
      clearTimeout(timeout);
      reject(new Error('WS error: ' + e.message));
    });
  });
}

async function testHTTPContributeStatus() {
  const resp = await fetch(`http://localhost:${PROXY_PORT}/api/contribute/status`);
  assert.equal(resp.status, 200);
  const data = await resp.json();
  assert.equal(data.hub, 'online');
  console.log('  ✓ /api/contribute/status returns 200');
}

async function testHTTPHealth() {
  const resp = await fetch(`http://localhost:${PROXY_PORT}/api/health`);
  assert.equal(resp.status, 200);
  const data = await resp.json();
  assert.equal(data.status, 'ok');
  console.log('  ✓ /api/health returns 200');
}

async function testNoFINError() {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(`ws://localhost:${PROXY_PORT}/api/contribute/ws`);
    const timeout = setTimeout(() => { ws.close(); reject(new Error('WS timeout')); }, 5000);
    let messageCount = 0;
    ws.on('message', (data) => {
      messageCount++;
      const msg = JSON.parse(data.toString());
      if (messageCount === 1) {
        assert.equal(msg.type, 'auth_challenge');
        ws.send(JSON.stringify({ type: 'ping', seq: 2 }));
      } else if (messageCount === 2) {
        assert.equal(msg.type, 'pong');
      }
    });
    ws.on('error', (e) => {
      clearTimeout(timeout);
      if (e.message.includes('FIN')) {
        reject(new Error('FIN error still present: ' + e.message));
      } else {
        reject(e);
      }
    });
    setTimeout(() => {
      clearTimeout(timeout);
      assert.ok(messageCount >= 2, 'Should have exchanged auth_challenge + ping/pong');
      ws.close();
      console.log('  ✓ No FIN error — WS frames valid (' + messageCount + ' messages exchanged)');
      resolve();
    }, 2000);
  });
}

// ---------------------------------------------------------------------------
// Finding C3 (CWE-862): per-hive terminal authorization.
//
// A hosted proxy is configured as "hive B" with HIVE_HUB_SECRET + an allowlist
// that contains `alice` but NOT `bob`. We mint a validly-signed hub cookie for
// each and prove:
//   - alice (on B's allowlist)   → terminal reachable (proxied to ttyd, 200)
//   - bob   (a real hub user, but authorized only on some OTHER hive)
//                                → 403 on HTTP, socket closed on WS
// exercising the exact cross-tenant bug: a valid cookie for a user with no
// access to THIS hive must not open a shell here. We also cover the
// port-suffixed and trailing-dot Host header variants of the hosted-host match.
// ---------------------------------------------------------------------------
const HIVE_B_PROXY_PORT = 19011;
const HIVE_B_GO_PORT = 19012;
const HIVE_B_TTYD_PORT = 19013;
const HIVE_B_SECRET = 'test-hub-secret-B';
const HIVE_B_ID = 'hosted-hive-b';
const HOSTED_HOST = `${HIVE_B_ID}.hive.kubestellar.io`;

let hiveBTtyd, hiveBProxy;

// mintCookie mirrors mintHubUserCookieValue / verifyHubUserCookie:
// `<username>.<base64url(HMAC-SHA256(key=secret, msg=username))>`.
function mintCookie(secret, username) {
  const sig = crypto.createHmac('sha256', secret).update(username).digest('base64url');
  return `${username}.${sig}`;
}

function startHiveBProxy() {
  return new Promise((resolve, reject) => {
    const proc = spawn('node', ['server.js'], {
      cwd: __dirname,
      env: {
        ...process.env,
        HIVE_PROXY_PORT: String(HIVE_B_PROXY_PORT),
        HIVE_API_PORT: String(HIVE_B_GO_PORT),
        HIVE_TTYD_PORT: String(HIVE_B_TTYD_PORT),
        HIVE_DASHBOARD_TOKEN: '',
        HIVE_HUB_SECRET: HIVE_B_SECRET,
        HIVE_ID: HIVE_B_ID,
        HIVE_AUTHORIZED_USERS: 'alice:owner,carol:read',
        HIVE_STATIC_DIR: __dirname,
        NODE_ENV: 'test',
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let started = false;
    proc.stdout.on('data', (d) => {
      if (!started && d.toString().includes('hive-proxy')) { started = true; resolve(proc); }
    });
    proc.stderr.on('data', (d) => { if (!started) console.error('hiveB stderr:', d.toString()); });
    proc.on('error', reject);
    setTimeout(() => { if (!started) reject(new Error('hiveB proxy start timeout')); }, 10000);
  });
}

async function setupHiveB() {
  hiveBTtyd = await new Promise(resolve => {
    const server = createServer((_req, res) => { res.writeHead(200); res.end('ttyd-B'); });
    // Mock ttyd must accept WS upgrades so an AUTHORIZED terminal upgrade can
    // actually complete an open() (the denial path never reaches ttyd).
    const wss = new WebSocketServer({ server });
    wss.on('connection', (ws) => { ws.send('ttyd-ready'); });
    server.listen(HIVE_B_TTYD_PORT, () => resolve(server));
  });
  hiveBProxy = await startHiveBProxy();
  await new Promise(r => setTimeout(r, 500));
}

async function teardownHiveB() {
  if (hiveBProxy) hiveBProxy.kill();
  if (hiveBTtyd) hiveBTtyd.close();
  await new Promise(r => setTimeout(r, 200));
}

// HTTP terminal request with a given cookie + explicit Host header.
//
// We use the raw http module, NOT fetch(): Node's fetch() overrides the Host
// header to match the URL authority, which would defeat the whole point — the
// gate keys off the Host header to decide `isHosted`, and we must be able to
// present an arbitrary hosted / port-suffixed / trailing-dot Host.
function terminalHTTP(cookieVal, hostHeader) {
  return new Promise((resolve, reject) => {
    const headers = { Host: hostHeader };
    if (cookieVal) headers.Cookie = `hive_hub_user=${cookieVal}`;
    const req = httpRequest({
      host: '127.0.0.1', port: HIVE_B_PROXY_PORT, path: '/terminal/',
      method: 'GET', headers,
    }, (res) => {
      let body = '';
      res.on('data', (c) => { body += c; });
      res.on('end', () => resolve({ status: res.statusCode, body }));
    });
    req.on('error', reject);
    req.end();
  });
}

// WS terminal upgrade. Resolves {opened:true} if the socket opens (allowed) or
// {opened:false} if it is closed/errored before/at open (denied).
function terminalWS(cookieVal, hostHeader) {
  return new Promise((resolve) => {
    const headers = { Host: hostHeader };
    if (cookieVal) headers.Cookie = `hive_hub_user=${cookieVal}`;
    const ws = new WebSocket(`ws://127.0.0.1:${HIVE_B_PROXY_PORT}/terminal/`, { headers });
    const done = (opened) => { try { ws.close(); } catch { /* ignore */ } resolve({ opened }); };
    const t = setTimeout(() => done(false), 4000);
    ws.on('open', () => { clearTimeout(t); done(true); });
    ws.on('error', () => { clearTimeout(t); done(false); });
    ws.on('unexpected-response', () => { clearTimeout(t); done(false); });
  });
}

async function testC3_AuthorizedUserHTTP() {
  const cookie = mintCookie(HIVE_B_SECRET, 'alice');
  const resp = await terminalHTTP(cookie, HOSTED_HOST);
  assert.equal(resp.status, 200, `alice (allowlisted) should reach terminal, got ${resp.status}`);
  console.log('  ✓ authorized user (alice) → terminal 200');
}

async function testC3_ForeignUserHTTP() {
  // bob's cookie is validly SIGNED (a real hub user) but bob is NOT on hive B's
  // allowlist — this is the cross-tenant bug. Must be 403, not a shell.
  const cookie = mintCookie(HIVE_B_SECRET, 'bob');
  const resp = await terminalHTTP(cookie, HOSTED_HOST);
  assert.equal(resp.status, 403, `bob (not on this hive) must be 403, got ${resp.status}`);
  console.log('  ✓ foreign hub user (bob) → terminal 403');
}

async function testC3_ForeignUserHTTP_PortSuffixHost() {
  const cookie = mintCookie(HIVE_B_SECRET, 'bob');
  const resp = await terminalHTTP(cookie, `${HOSTED_HOST}:443`);
  assert.equal(resp.status, 403, `bob with port-suffixed Host must be 403, got ${resp.status}`);
  console.log('  ✓ foreign user, port-suffixed Host → 403');
}

async function testC3_ForeignUserHTTP_TrailingDotHost() {
  const cookie = mintCookie(HIVE_B_SECRET, 'bob');
  const resp = await terminalHTTP(cookie, `${HOSTED_HOST}.`);
  // Trailing-dot FQDN must NOT be treated as non-hosted (which would skip the
  // gate and proxy straight to ttyd). It must still be gated and 403 a foreign
  // user — this pins the host-normalization bypass fix.
  assert.equal(resp.status, 403,
    `bob with trailing-dot Host must be 403, got ${resp.status}`);
  console.log('  ✓ foreign user, trailing-dot Host → 403 (bypass closed)');
}

async function testC3_AuthorizedUserWS() {
  const cookie = mintCookie(HIVE_B_SECRET, 'alice');
  const { opened } = await terminalWS(cookie, HOSTED_HOST);
  assert.ok(opened, 'alice (allowlisted) WS terminal should open');
  console.log('  ✓ authorized user (alice) → WS opens');
}

async function testC3_ForeignUserWS() {
  const cookie = mintCookie(HIVE_B_SECRET, 'bob');
  const { opened } = await terminalWS(cookie, HOSTED_HOST);
  assert.ok(!opened, 'bob (not on this hive) WS terminal must be closed');
  console.log('  ✓ foreign hub user (bob) → WS closed');
}

async function testC3_ForeignUserWS_PortSuffixHost() {
  const cookie = mintCookie(HIVE_B_SECRET, 'bob');
  const { opened } = await terminalWS(cookie, `${HOSTED_HOST}:443`);
  assert.ok(!opened, 'bob with port-suffixed Host WS must be closed');
  console.log('  ✓ foreign user, port-suffixed Host → WS closed');
}

async function testC3_ForgedSigStillRejected() {
  // Belt-and-suspenders: a cookie whose signature does not verify (username
  // alice but wrong secret) is rejected regardless of the allowlist.
  const cookie = mintCookie('wrong-secret', 'alice');
  const resp = await terminalHTTP(cookie, HOSTED_HOST);
  assert.equal(resp.status, 401, `forged-signature cookie must be 401, got ${resp.status}`);
  console.log('  ✓ forged-signature cookie (even for allowlisted name) → 401');
}

// Run tests
console.log('\nProxy WebSocket Tests\n');

try {
  await setup();

  console.log('HTTP tests:');
  await testHTTPHealth();
  await testHTTPContributeStatus();

  console.log('\nWebSocket tests:');
  await testWSContributeConnect();
  await testNoFINError();

  console.log('\n✓ Base tests passed\n');
} catch (e) {
  console.error('\n✗ Test failed:', e.message, '\n');
  process.exitCode = 1;
} finally {
  await teardown();
}

// C3 per-hive terminal authorization suite (own proxy lifecycle).
console.log('Finding C3 — per-hive terminal authorization\n');
try {
  await setupHiveB();

  console.log('HTTP terminal gate:');
  await testC3_AuthorizedUserHTTP();
  await testC3_ForeignUserHTTP();
  await testC3_ForeignUserHTTP_PortSuffixHost();
  await testC3_ForeignUserHTTP_TrailingDotHost();
  await testC3_ForgedSigStillRejected();

  console.log('\nWebSocket terminal gate:');
  await testC3_AuthorizedUserWS();
  await testC3_ForeignUserWS();
  await testC3_ForeignUserWS_PortSuffixHost();

  console.log('\n✓ C3 tests passed\n');
} catch (e) {
  console.error('\n✗ C3 test failed:', e.message, '\n');
  process.exitCode = 1;
} finally {
  await teardownHiveB();
}
