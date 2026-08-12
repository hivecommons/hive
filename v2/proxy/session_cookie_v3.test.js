// Cross-language session-cookie vectors, frozen from the REAL Go minter.
//
// WHY THIS FILE EXISTS
// The hub session cookie is MINTED in Go (v2/pkg/hub/hub_cookie.go) and
// VERIFIED here in JavaScript. If the two disagree by a single byte — base64
// alphabet, JSON field order, marker placement — every hosted login breaks at
// deploy, silently, and only in production. A Node-generated vector would only
// prove Node agrees with itself, so every value below was minted by the actual
// Go code and pasted verbatim.
//
// If one of these fails, do NOT edit the expected value. Find out which side
// changed its encoding.
//
// Run: node session_cookie_v3.test.js

import { createServer, request as httpRequest } from 'http';
import { WebSocketServer } from 'ws';
import { spawn } from 'child_process';
import path from 'path';
import { fileURLToPath } from 'url';
import assert from 'assert';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PROXY_PORT = 19111;
const GO_PORT = 19112;
const TTYD_PORT = 19113;
const HOSTED_HOST = 'hive-v3.hive.kubestellar.io';
const MASTER = 'f3-test-master-secret';

// Minted by Go with the master above at iat=1700000000 (in the PAST, so the vector is valid now; a future iat is correctly rejected as not-yet-valid). V3_VALID carries a
// deliberately LONG ttl so the frozen vector cannot rot out of its validity
// window and start failing for a reason unrelated to encoding agreement;
// V3_EXPIRED keeps a 1h ttl and is therefore always in the past.
const V3_VALID   = 'eyJ1IjoiY2x1YmFuZGVyc29uIiwiaWF0IjoxNzAwMDAwMDAwLCJleHAiOjM1MDAwMDAwMDAsInNpZCI6Ijk3NjMzYjI1MWI0YmUzMjUwMmY2ZTQ0MjkyZjBlYzI3ZDk3Y2UxY2FjNzJlZGFmNjliOTBkYTdmMGUxMzBiZTUifQ.v3.ijJ_P2d5GrZqFOSnd5-c4ImJ_ItpPx_7ts_YI9lcV665ATC7EpTmGLD-awrcjkX7DtMs3GhE3cVOt2AvgKV7Ag';
const V2_VALID   = 'clubanderson.v2.qcKTjay3VjXCiaQ8qRUebT0ElrS4T0G0wiuegEZ6acIC3f8QNDG33K1o4tTZ7GKa_RQQtZH0Tsii1i8Tu0JdAw';
const V3_EXPIRED = 'eyJ1IjoiY2x1YmFuZGVyc29uIiwiaWF0IjoxNzAwMDAwMDAwLCJleHAiOjE3MDAwMDM2MDAsInNpZCI6Ijg3YzI4MDVhZjg1NTA2YzI1YTExNWQ0MDBmMGM4ZmM0OTQwMDM1NTJhOWUwYTM4ODIyNjk0Yjk0MDhlZDVlZmIifQ.v3.2wu1P0WD70mVNev2AGfsISvz58sU3MpQ_00Ga4eqtB2wvOWcz9pKZ3ane8P2xQfgH7PXKeBd27TT0AScUMrmDg';

function mockBackend(port, body) {
  return new Promise(resolve => {
    const s = createServer((req, res) => { res.writeHead(200); res.end(body); });
    s.listen(port, () => resolve(s));
  });
}
// ttyd must speak WebSocket: a plain HTTP mock makes the "valid cookie opens a
// shell" control fail for the wrong reason.
function mockTtydWS(port) {
  return new Promise(resolve => {
    const s = createServer((req, res) => { res.writeHead(200); res.end('ttyd'); });
    const wss = new WebSocketServer({ server: s });
    wss.on('connection', ws => ws.send('shell'));
    s.listen(port, () => resolve(s));
  });
}
function startProxy() {
  return new Promise((resolve, reject) => {
    const proc = spawn('node', ['server.js'], {
      cwd: __dirname,
      env: { ...process.env,
        HIVE_PROXY_PORT: String(PROXY_PORT), HIVE_API_PORT: String(GO_PORT),
        HIVE_TTYD_PORT: String(TTYD_PORT), HIVE_DASHBOARD_TOKEN: '',
        HIVE_STATIC_DIR: __dirname, HIVE_HUB_SECRET: MASTER, NODE_ENV: 'test' },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let started = false;
    proc.stdout.on('data', d => {
      if (!started && d.toString().includes('hive-proxy')) { started = true; resolve(proc); }
    });
    proc.on('error', reject);
    setTimeout(() => { if (!started) reject(new Error('proxy start timeout')); }, 10000);
  });
}
// http.request, not fetch(): fetch silently DROPS a custom Host header, so the
// proxy would take the non-hosted branch and the gate would never run.
function terminalHTTP(cookieName, cookieVal) {
  return new Promise((resolve, reject) => {
    const headers = { Host: HOSTED_HOST };
    if (cookieVal !== null) headers.Cookie = `${cookieName}=${cookieVal}`;
    const req = httpRequest({ host: '127.0.0.1', port: PROXY_PORT, path: '/terminal',
      method: 'GET', headers }, res => { res.resume(); resolve(res.statusCode); });
    req.on('error', reject);
    req.end();
  });
}

let go, ttyd, proxy, pass = 0, fail = 0;
async function check(name, fn) {
  try { await fn(); console.log('  \u2713 ' + name); pass++; }
  catch (e) { console.error('  \u2717 ' + name + ': ' + e.message); fail++; }
}

try {
  go = await mockBackend(GO_PORT, 'go');
  ttyd = await mockTtydWS(TTYD_PORT);
  proxy = await startProxy();
  await new Promise(r => setTimeout(r, 400));

  // A Go-minted v3 value must be ACCEPTED by the Node verifier. Before this
  // change the proxy understood only the HMAC format and would 401 it.
  await check('Go-minted v3 cookie is accepted (hive_hub_user)', async () => {
    const s = await terminalHTTP('hive_hub_user', V3_VALID);
    assert.notStrictEqual(s, 401, `expected the v3 vector to authenticate, got ${s}`);
  });

  // The v2 hub emits the Ed25519 value under a SECOND cookie name. Dispatch is
  // on the marker inside the value, so this must work too.
  await check('Go-minted v2 cookie is accepted (hive_hub_user_v2)', async () => {
    const s = await terminalHTTP('hive_hub_user_v2', V2_VALID);
    assert.notStrictEqual(s, 401, `expected the v2 vector to authenticate, got ${s}`);
  });

  // NEGATIVE CONTROLS — the verifier must not have become a rubber stamp.
  await check('EXPIRED v3 is rejected (signed lifetime enforced)', async () => {
    const s = await terminalHTTP('hive_hub_user', V3_EXPIRED);
    assert.strictEqual(s, 401, `an expired v3 cookie authenticated (got ${s})`);
  });
  await check('tampered v3 payload is rejected', async () => {
    const bad = 'X' + V3_VALID.slice(1);
    const s = await terminalHTTP('hive_hub_user', bad);
    assert.strictEqual(s, 401, `a tampered v3 cookie authenticated (got ${s})`);
  });
  await check('no cookie is rejected', async () => {
    const s = await terminalHTTP('hive_hub_user', null);
    assert.strictEqual(s, 401, `no cookie authenticated (got ${s})`);
  });
  await check('garbage value is rejected', async () => {
    const s = await terminalHTTP('hive_hub_user', 'x');
    assert.strictEqual(s, 401, `a garbage cookie authenticated (got ${s})`);
  });

  console.log(`\n${pass} passed, ${fail} failed`);
} finally {
  if (proxy) proxy.kill();
  if (go) go.close();
  if (ttyd) ttyd.close();
}
process.exit(fail ? 1 : 0);
