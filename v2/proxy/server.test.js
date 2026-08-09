import { createServer } from 'http';
import { WebSocketServer, WebSocket } from 'ws';
import { strict as assert } from 'assert';
import { spawn } from 'child_process';
import { fileURLToPath } from 'url';
import path from 'path';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PROXY_PORT = 19001;
const GO_PORT = 19002;
const TTYD_PORT = 19003;

let goServer, ttydServer, proxyProcess;
let mockSnapshotFrameAncestors = [];

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
    if (req.url === '/api/snapshot/frame-ancestors') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ origins: mockSnapshotFrameAncestors }));
      return;
    }
    if (req.url === '/snapshot') {
      res.writeHead(200, { 'Content-Type': 'text/html' });
      res.end('<!doctype html><title>snapshot</title>');
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

async function setup(extraEnv = {}) {
  goServer = await setupMockGoBackend();
  ttydServer = await setupMockTtyd();
  proxyProcess = await startProxy(extraEnv);
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

async function testDefaultFrameDeny() {
  const resp = await fetch(`http://localhost:${PROXY_PORT}/snapshot`);
  assert.equal(resp.status, 200);
  assert.equal(resp.headers.get('x-frame-options'), 'DENY');
  assert.match(resp.headers.get('content-security-policy') || '', /frame-ancestors 'none'/);
  console.log('  ✓ default /snapshot framing is denied');
}

async function testSnapshotFrameAllowlist() {
  const resp = await fetch(`http://localhost:${PROXY_PORT}/snapshot`);
  assert.equal(resp.status, 200);
  assert.equal(resp.headers.get('x-frame-options'), null);
  assert.match(resp.headers.get('content-security-policy') || '', /frame-ancestors https:\/\/docs\.projectbluefin\.io/);
  console.log('  ✓ /snapshot uses CSP frame allowlist without XFO');
}

async function testOtherRoutesStillDenyFraming() {
  const resp = await fetch(`http://localhost:${PROXY_PORT}/api/health`);
  assert.equal(resp.status, 200);
  assert.equal(resp.headers.get('x-frame-options'), 'DENY');
  assert.match(resp.headers.get('content-security-policy') || '', /frame-ancestors 'none'/);
  console.log('  ✓ non-snapshot routes still deny framing');
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

// Run tests
console.log('\nProxy WebSocket Tests\n');

try {
  mockSnapshotFrameAncestors = [];
  await setup();

  console.log('HTTP tests:');
  await testHTTPHealth();
  await testHTTPContributeStatus();
  await testDefaultFrameDeny();

  console.log('\nWebSocket tests:');
  await testWSContributeConnect();
  await testNoFINError();

  await teardown();
  mockSnapshotFrameAncestors = ['https://docs.projectbluefin.io'];
  await setup();

  console.log('\nFrame allowlist tests:');
  await testSnapshotFrameAllowlist();
  await testOtherRoutesStillDenyFraming();

  console.log('\n✓ All tests passed\n');
} catch (e) {
  console.error('\n✗ Test failed:', e.message, '\n');
  process.exitCode = 1;
} finally {
  await teardown();
}
