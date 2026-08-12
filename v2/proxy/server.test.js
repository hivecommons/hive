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
// `<username>.<base64url(HMAC-SHA256(key=SESSION_KEY, msg=username))>`.
//
// C2 domain separation (#2758): the proxy no longer verifies the cookie with the
// master HIVE_HUB_SECRET but with the derived SESSION sub-key —
// HMAC-SHA256(master, "hive-session-v1"). We are started with HIVE_HUB_SECRET
// (the legacy derivation lane), so mint through the SAME derivation the proxy's
// deriveSessionKey() uses, keeping the info string byte-for-byte identical.
function deriveSessionKey(secret) {
  return crypto.createHmac('sha256', secret).update('hive-session-v1').digest('hex');
}
function mintCookie(secret, username) {
  const sessionKey = deriveSessionKey(secret);
  const sig = crypto.createHmac('sha256', sessionKey).update(username).digest('base64url');
  return `${username}.${sig}`;
}

// mintTerminalAssertion mirrors Go hub.MintTerminalAssertion / the proxy's
// verifyTerminalAssertion: `<base64url(json{v,u,r,h,iat,exp})>.<base64url(HMAC)>`.
// It signs with the DERIVED terminal sub-key (deriveTerminalKey), not the raw
// master — matching the proxy's TERMINAL_SIGNING_KEY resolution for a spoke
// provisioned with only HIVE_HUB_SECRET (its default in these tests). ttlSec and
// version are overridable so tests can forge expired / wrong-version / wrong-hive
// assertions. Default: this hive, 15-min TTL, current version.
const TERMINAL_ASSERTION_VERSION = 'hive-terminal-v1';
const INFO_TERMINAL_KEY = 'hive-terminal-v1';
// deriveTerminalKey mirrors the Go deriveTerminalKeyFrom / proxy deriveTerminalKeyFrom.
function deriveTerminalKey(master) {
  return crypto.createHmac('sha256', master).update(INFO_TERMINAL_KEY).digest('hex');
}
function mintTerminalAssertion(secret, { username, role, hiveID, ttlSec = 900, version = TERMINAL_ASSERTION_VERSION, iatOffsetSec = 0 } = {}) {
  const key = deriveTerminalKey(secret);
  const now = Math.floor(Date.now() / 1000) + iatOffsetSec;
  const claims = { v: version, u: username, r: role, h: hiveID, iat: now, exp: now + ttlSec };
  const body = Buffer.from(JSON.stringify(claims)).toString('base64url');
  const sig = crypto.createHmac('sha256', key).update(body).digest('base64url');
  return `${body}.${sig}`;
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
function terminalHTTP(cookieVal, hostHeader, assertionVal) {
  return new Promise((resolve, reject) => {
    const headers = { Host: hostHeader };
    const cookieParts = [];
    if (cookieVal) cookieParts.push(`hive_hub_user=${cookieVal}`);
    if (assertionVal) cookieParts.push(`hive_terminal_assertion=${assertionVal}`);
    if (cookieParts.length) headers.Cookie = cookieParts.join('; ');
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
function terminalWS(cookieVal, hostHeader, assertionVal) {
  return new Promise((resolve) => {
    const headers = { Host: hostHeader };
    const cookieParts = [];
    if (cookieVal) cookieParts.push(`hive_hub_user=${cookieVal}`);
    if (assertionVal) cookieParts.push(`hive_terminal_assertion=${assertionVal}`);
    if (cookieParts.length) headers.Cookie = cookieParts.join('; ');
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

// ---------------------------------------------------------------------------
// Finding C3 FOLLOW-UP — short-lived signed {user,hive,role,exp} assertion.
//
// Reuses hive B (secret HIVE_B_SECRET, id HIVE_B_ID, static allowlist
// alice:owner,carol:read). `dave` is deliberately NOT on the static allowlist,
// so any decision that lets dave in must be coming from the SIGNED ASSERTION —
// the new PRIMARY gate — and any decision that keeps dave out proves the
// FALLBACK to the #2756 static allowlist still holds when the assertion is
// unusable. The hub cookie carries authentication; the assertion carries
// per-hive authorization.
// ---------------------------------------------------------------------------

// PRIMARY: a valid, this-hive, sufficient-role assertion admits a user who is
// NOT on the static allowlist — the whole point of the upgrade.
async function testC3F_ValidAssertionAdmitsNonAllowlistedUser() {
  const cookie = mintCookie(HIVE_B_SECRET, 'dave');
  const assertion = mintTerminalAssertion(HIVE_B_SECRET, { username: 'dave', role: 'owner', hiveID: HIVE_B_ID });
  const resp = await terminalHTTP(cookie, HOSTED_HOST, assertion);
  assert.equal(resp.status, 200, `dave with a valid owner assertion should reach terminal, got ${resp.status}`);
  const { opened } = await terminalWS(cookie, HOSTED_HOST, assertion);
  assert.ok(opened, 'dave with a valid owner assertion WS should open');
  console.log('  ✓ valid {user,hive,owner,exp} assertion → terminal opens (primary gate, beyond static list)');
}

// read-write is a sufficient terminal role too.
async function testC3F_ReadWriteRoleSufficient() {
  const cookie = mintCookie(HIVE_B_SECRET, 'dave');
  const assertion = mintTerminalAssertion(HIVE_B_SECRET, { username: 'dave', role: 'read-write', hiveID: HIVE_B_ID });
  const resp = await terminalHTTP(cookie, HOSTED_HOST, assertion);
  assert.equal(resp.status, 200, `dave with a read-write assertion should reach terminal, got ${resp.status}`);
  console.log('  ✓ read-write role assertion → terminal opens');
}

// EXPIRED assertion → not usable → falls back to static allowlist → dave denied.
async function testC3F_ExpiredAssertionDenied() {
  const cookie = mintCookie(HIVE_B_SECRET, 'dave');
  // iatOffset -1000s, ttl 600s → issued & expired well before now (beyond skew).
  const assertion = mintTerminalAssertion(HIVE_B_SECRET, { username: 'dave', role: 'owner', hiveID: HIVE_B_ID, ttlSec: 600, iatOffsetSec: -1000 });
  const resp = await terminalHTTP(cookie, HOSTED_HOST, assertion);
  assert.equal(resp.status, 403, `dave with an EXPIRED assertion must fall back and be 403, got ${resp.status}`);
  const { opened } = await terminalWS(cookie, HOSTED_HOST, assertion);
  assert.ok(!opened, 'dave with an EXPIRED assertion WS must be closed');
  console.log('  ✓ expired assertion → denied (fallback: dave not on static list)');
}

// WRONG-HIVE assertion (minted for another hive id) → not usable → denied.
async function testC3F_WrongHiveAssertionDenied() {
  const cookie = mintCookie(HIVE_B_SECRET, 'dave');
  const assertion = mintTerminalAssertion(HIVE_B_SECRET, { username: 'dave', role: 'owner', hiveID: 'some-other-hive' });
  const resp = await terminalHTTP(cookie, HOSTED_HOST, assertion);
  assert.equal(resp.status, 403, `dave with a WRONG-HIVE assertion must be 403, got ${resp.status}`);
  const { opened } = await terminalWS(cookie, HOSTED_HOST, assertion);
  assert.ok(!opened, 'dave with a WRONG-HIVE assertion WS must be closed');
  console.log('  ✓ wrong-hive assertion → denied (hive_id binding enforced)');
}

// INSUFFICIENT ROLE (read) assertion → not a sufficient terminal grant → falls
// back to static allowlist → dave denied.
async function testC3F_InsufficientRoleDenied() {
  const cookie = mintCookie(HIVE_B_SECRET, 'dave');
  const assertion = mintTerminalAssertion(HIVE_B_SECRET, { username: 'dave', role: 'read', hiveID: HIVE_B_ID });
  const resp = await terminalHTTP(cookie, HOSTED_HOST, assertion);
  assert.equal(resp.status, 403, `dave with a read-only assertion must be 403, got ${resp.status}`);
  console.log('  ✓ read-only role assertion → denied (owner/read-write required for a shell)');
}

// N4 (CWE-862/613) — THE TESTS THE SUITE WAS MISSING.
//
// Every C3F case above uses `dave`, who is deliberately NOT on the static
// allowlist. Their 403s are therefore produced by the FALLBACK denying a
// non-member — they prove nothing about role enforcement, and they passed
// against the vulnerable code.
//
// `carol:read` IS on the allowlist (see the HIVE_AUTHORIZED_USERS fixture). She
// is the case that mattered: authorizeTerminal treated "valid assertion, role
// too low" the same as "no assertion", fell through to the allowlist, and the
// allowlist parse discarded her ":read" suffix — so a read-only viewer got a
// shell in a credential-holding container.
async function testN4_AllowlistedReadOnlyDenied() {
  const cookie = mintCookie(HIVE_B_SECRET, 'carol');
  const assertion = mintTerminalAssertion(HIVE_B_SECRET, { username: 'carol', role: 'read', hiveID: HIVE_B_ID });
  const resp = await terminalHTTP(cookie, HOSTED_HOST, assertion);
  assert.equal(resp.status, 403,
    `N4: carol is allowlisted but read-only — a valid read assertion must DENY, got ${resp.status}`);
  const { opened } = await terminalWS(cookie, HOSTED_HOST, assertion);
  assert.ok(!opened, 'N4: carol read-only WS must be closed');
  console.log('  ✓ allowlisted READ-ONLY user + valid read assertion → denied (no fallback)');
}

// Same user with NO assertion at all. This one legitimately reaches the
// fallback — there is no authoritative role to read — and the role-aware
// allowlist is what must deny her now that ":read" is honoured.
async function testN4_AllowlistedReadOnlyNoAssertionDenied() {
  const cookie = mintCookie(HIVE_B_SECRET, 'carol');
  const resp = await terminalHTTP(cookie, HOSTED_HOST, null);
  assert.equal(resp.status, 403,
    `N4: carol with NO assertion must be denied by the role-aware allowlist, got ${resp.status}`);
  const { opened } = await terminalWS(cookie, HOSTED_HOST, null);
  assert.ok(!opened, 'N4: carol with no assertion WS must be closed');
  console.log('  ✓ allowlisted READ-ONLY user, no assertion → denied (:read honoured in allowlist)');
}

// An EXPIRED assertion is "tells us nothing", so it falls back — and the
// role-aware allowlist must still deny a read-only user. Under the old code this
// was a shell.
async function testN4_AllowlistedReadOnlyExpiredDenied() {
  const cookie = mintCookie(HIVE_B_SECRET, 'carol');
  const assertion = mintTerminalAssertion(HIVE_B_SECRET, { username: 'carol', role: 'owner', hiveID: HIVE_B_ID, ttlSec: 600, iatOffsetSec: -1000 });
  const resp = await terminalHTTP(cookie, HOSTED_HOST, assertion);
  assert.equal(resp.status, 403,
    `N4: carol with an EXPIRED owner assertion must be denied, got ${resp.status}`);
  console.log('  ✓ allowlisted READ-ONLY user + expired assertion → denied (revocation/expiry take effect)');
}

// CONTROL: the fix must not break legitimate access. alice:owner still gets in,
// both with a valid owner assertion and via the allowlist fallback.
async function testN4_OwnerStillAllowed() {
  const cookie = mintCookie(HIVE_B_SECRET, 'alice');
  const assertion = mintTerminalAssertion(HIVE_B_SECRET, { username: 'alice', role: 'owner', hiveID: HIVE_B_ID });
  const resp = await terminalHTTP(cookie, HOSTED_HOST, assertion);
  assert.equal(resp.status, 200, `alice:owner with a valid assertion must be 200, got ${resp.status}`);
  const noAssertion = await terminalHTTP(cookie, HOSTED_HOST, null);
  assert.equal(noAssertion.status, 200,
    `alice:owner must still pass via the allowlist fallback, got ${noAssertion.status}`);
  console.log('  ✓ owner still allowed (with assertion and via fallback) — fix is not over-broad');
}

// FORGED assertion (wrong signing secret) → signature fails → not usable → denied.
async function testC3F_ForgedAssertionDenied() {
  const cookie = mintCookie(HIVE_B_SECRET, 'dave');
  const assertion = mintTerminalAssertion('wrong-secret', { username: 'dave', role: 'owner', hiveID: HIVE_B_ID });
  const resp = await terminalHTTP(cookie, HOSTED_HOST, assertion);
  assert.equal(resp.status, 403, `dave with a FORGED-signature assertion must be 403, got ${resp.status}`);
  console.log('  ✓ forged-signature assertion → denied');
}

// WRONG-VERSION token (e.g. an SSO handoff "hive-sso-v1") is NOT a terminal grant.
async function testC3F_WrongVersionDenied() {
  const cookie = mintCookie(HIVE_B_SECRET, 'dave');
  const assertion = mintTerminalAssertion(HIVE_B_SECRET, { username: 'dave', role: 'owner', hiveID: HIVE_B_ID, version: 'hive-sso-v1' });
  const resp = await terminalHTTP(cookie, HOSTED_HOST, assertion);
  assert.equal(resp.status, 403, `dave with a wrong-version token must be 403, got ${resp.status}`);
  console.log('  ✓ wrong-version token (SSO handoff shape) → denied (token families not confusable)');
}

// USER MISMATCH: a valid assertion for alice presented alongside bob's hub
// cookie must NOT admit bob — the assertion user must equal the authenticated
// cookie user. (bob is also not on the static allowlist, so it falls through.)
// ── audit F4, final step: the assertion alone must authenticate ──────────────
//
// Both /terminal gates used to require a valid hive_hub_user cookie BEFORE
// authorization ran, so the hub-wide cookie was load-bearing for every terminal
// request — and that is exactly the cookie that must stop being delivered to
// sibling tenants. Dropping its Domain while the gates still demanded it would
// have locked every hosted tenant out of /terminal.
//
// A verified assertion is STRICTLY STRONGER than the hub cookie here: it is
// signed with THIS spoke's per-hive key and carries {user, role, hive_id, exp},
// whereas the hub cookie only proves "some hub user" and is handed to every
// sibling. These tests pin that the assertion alone opens a terminal, and that
// dropping the hub-cookie requirement did NOT weaken any existing denial.
async function testF4_AssertionAloneAdmits() {
  const assertion = mintTerminalAssertion(HIVE_B_SECRET, { username: 'dave', role: 'owner', hiveID: HIVE_B_ID });
  const resp = await terminalHTTP(null, HOSTED_HOST, assertion);
  assert.equal(resp.status, 200, `valid owner assertion with NO hub cookie should reach terminal, got ${resp.status}`);
  const { opened } = await terminalWS(null, HOSTED_HOST, assertion);
  assert.ok(opened, 'valid owner assertion with NO hub cookie should open WS');
  console.log('  ✓ F4: assertion alone (no hub cookie) → terminal opens, HTTP + WS');
}

// NEGATIVE CONTROLS. Removing the hub-cookie precondition must not turn the
// gate into a rubber stamp: every property the assertion itself asserts has to
// still be enforced when it is the ONLY credential presented.
async function testF4_NoCredentialsStillDenied() {
  const resp = await terminalHTTP(null, HOSTED_HOST, null);
  assert.equal(resp.status, 401, `no cookie and no assertion must be 401, got ${resp.status}`);
  const { opened } = await terminalWS(null, HOSTED_HOST, null);
  assert.ok(!opened, 'no credentials at all must not open a WS');
  console.log('  ✓ F4: no credentials → 401 / WS refused');
}

async function testF4_ExpiredAssertionAloneDenied() {
  const assertion = mintTerminalAssertion(HIVE_B_SECRET, { username: 'dave', role: 'owner', hiveID: HIVE_B_ID, ttlSec: -60 });
  const resp = await terminalHTTP(null, HOSTED_HOST, assertion);
  assert.notEqual(resp.status, 200, 'an EXPIRED assertion must not authenticate on its own');
  console.log('  ✓ F4: expired assertion alone → denied');
}

async function testF4_WrongHiveAssertionAloneDenied() {
  // Signed by hive B's key but claiming a different hive id.
  const assertion = mintTerminalAssertion(HIVE_B_SECRET, { username: 'dave', role: 'owner', hiveID: 'hosted-some-other-hive' });
  const resp = await terminalHTTP(null, HOSTED_HOST, assertion);
  assert.notEqual(resp.status, 200, 'an assertion for ANOTHER hive must not authenticate here');
  console.log('  ✓ F4: wrong-hive assertion alone → denied');
}

async function testF4_ForgedAssertionAloneDenied() {
  // Correct shape, wrong signing key — the cross-tenant forgery N3 closed.
  const assertion = mintTerminalAssertion('not-the-right-key', { username: 'dave', role: 'owner', hiveID: HIVE_B_ID });
  const resp = await terminalHTTP(null, HOSTED_HOST, assertion);
  assert.notEqual(resp.status, 200, 'a forged assertion must not authenticate on its own');
  const { opened } = await terminalWS(null, HOSTED_HOST, assertion);
  assert.ok(!opened, 'a forged assertion must not open a WS');
  console.log('  ✓ F4: forged-signature assertion alone → denied, HTTP + WS');
}

async function testF4_InsufficientRoleAloneDenied() {
  // carol:read — a VALID assertion that says "no". Must stay denied, and must
  // NOT fall through to the static allowlist (audit N4).
  const assertion = mintTerminalAssertion(HIVE_B_SECRET, { username: 'carol', role: 'read', hiveID: HIVE_B_ID });
  const resp = await terminalHTTP(null, HOSTED_HOST, assertion);
  assert.notEqual(resp.status, 200, 'a read-only assertion must not open a terminal, even as sole credential');
  console.log('  ✓ F4: read-only assertion alone → denied (N4 stays closed)');
}

async function testC3F_UserMismatchDenied() {
  const cookie = mintCookie(HIVE_B_SECRET, 'bob');
  const assertion = mintTerminalAssertion(HIVE_B_SECRET, { username: 'alice', role: 'owner', hiveID: HIVE_B_ID });
  const resp = await terminalHTTP(cookie, HOSTED_HOST, assertion);
  assert.equal(resp.status, 403, `bob presenting alice's assertion must be 403, got ${resp.status}`);
  console.log('  ✓ assertion-user ≠ cookie-user → denied (binding to authenticated user)');
}

// FALLBACK PRESERVED: an allowlisted user with NO assertion cookie still reaches
// the terminal via the #2756 static allowlist — the upgrade is additive.
async function testC3F_StaticAllowlistFallbackPreserved() {
  const cookie = mintCookie(HIVE_B_SECRET, 'alice');
  const resp = await terminalHTTP(cookie, HOSTED_HOST /* no assertion */);
  assert.equal(resp.status, 200, `alice (static allowlist) with no assertion must still reach terminal, got ${resp.status}`);
  console.log('  ✓ no assertion + static allowlist → terminal opens (fallback preserved)');
}

// ---------------------------------------------------------------------------
// Finding C3, empty-allowlist lane (no fail-open, even the narrow one).
//
// An OWNERLESS hive has an EMPTY per-hive allowlist (HIVE_AUTHORIZED_USERS
// unset). Behavior must be platform-aware:
//   - OpenShift Route (HIVE_INGRESS_AUTHZ unset): the proxy is the ONLY per-hive
//     gate — there is no ingress auth-proxy — so it must fail CLOSED: EVERY
//     signed cookie (cross-hive AND same-hive) is DENIED.
//   - nginx ingress (HIVE_INGRESS_AUTHZ=true): the ingress already
//     per-hive-authorized the request before it reached the proxy, so the proxy
//     DEFERS (allows) rather than double-denying legitimate access.
//
// Each scenario runs its own proxy on its own ports + its own mock ttyd.
// ---------------------------------------------------------------------------
const EMPTY_SECRET = 'test-hub-secret-empty';
const EMPTY_HIVE_ID = 'ownerless-hive';
const EMPTY_HOST = `${EMPTY_HIVE_ID}.hive.kubestellar.io`;

// startEmptyAllowlistProxy launches a proxy with NO HIVE_AUTHORIZED_USERS on the
// given ports; ingressAuthz toggles HIVE_INGRESS_AUTHZ (nginx vs OpenShift lane).
function startEmptyAllowlistProxy(proxyPort, apiPort, ttydPort, ingressAuthz) {
  const env = {
    ...process.env,
    HIVE_PROXY_PORT: String(proxyPort),
    HIVE_API_PORT: String(apiPort),
    HIVE_TTYD_PORT: String(ttydPort),
    HIVE_DASHBOARD_TOKEN: '',
    HIVE_HUB_SECRET: EMPTY_SECRET,
    HIVE_ID: EMPTY_HIVE_ID,
    // HIVE_AUTHORIZED_USERS intentionally omitted → empty allowlist.
    HIVE_STATIC_DIR: __dirname,
    NODE_ENV: 'test',
  };
  if (ingressAuthz) env.HIVE_INGRESS_AUTHZ = 'true';
  return new Promise((resolve, reject) => {
    const proc = spawn('node', ['server.js'], { cwd: __dirname, env, stdio: ['ignore', 'pipe', 'pipe'] });
    let started = false;
    proc.stdout.on('data', (d) => { if (!started && d.toString().includes('hive-proxy')) { started = true; resolve(proc); } });
    proc.stderr.on('data', (d) => { if (!started) console.error('empty-allowlist stderr:', d.toString()); });
    proc.on('error', reject);
    setTimeout(() => { if (!started) reject(new Error('empty-allowlist proxy start timeout')); }, 10000);
  });
}

function startMockTtydOn(port) {
  return new Promise(resolve => {
    const server = createServer((_req, res) => { res.writeHead(200); res.end('ttyd'); });
    const wss = new WebSocketServer({ server });
    wss.on('connection', (ws) => { ws.send('ttyd-ready'); });
    server.listen(port, () => resolve(server));
  });
}

// Port-parameterized request helpers (raw http + ws, explicit Host).
function terminalHTTPOn(port, cookieVal, hostHeader) {
  return new Promise((resolve, reject) => {
    const headers = { Host: hostHeader };
    if (cookieVal) headers.Cookie = `hive_hub_user=${cookieVal}`;
    const req = httpRequest({ host: '127.0.0.1', port, path: '/terminal/', method: 'GET', headers }, (res) => {
      let body = '';
      res.on('data', (c) => { body += c; });
      res.on('end', () => resolve({ status: res.statusCode, body }));
    });
    req.on('error', reject);
    req.end();
  });
}

function terminalWSOn(port, cookieVal, hostHeader) {
  return new Promise((resolve) => {
    const headers = { Host: hostHeader };
    if (cookieVal) headers.Cookie = `hive_hub_user=${cookieVal}`;
    const ws = new WebSocket(`ws://127.0.0.1:${port}/terminal/`, { headers });
    const done = (opened) => { try { ws.close(); } catch { /* ignore */ } resolve({ opened }); };
    const t = setTimeout(() => done(false), 4000);
    ws.on('open', () => { clearTimeout(t); done(true); });
    ws.on('error', () => { clearTimeout(t); done(false); });
    ws.on('unexpected-response', () => { clearTimeout(t); done(false); });
  });
}

// OpenShift lane: empty allowlist + no ingress authz → deny everything.
const OS_PROXY_PORT = 19021, OS_API_PORT = 19022, OS_TTYD_PORT = 19023;
let osTtyd, osProxy;

async function testC3_EmptyAllowlist_OpenShift_ForeignDenied() {
  const cookie = mintCookie(EMPTY_SECRET, 'bob');
  const resp = await terminalHTTPOn(OS_PROXY_PORT, cookie, EMPTY_HOST);
  assert.equal(resp.status, 403, `OpenShift empty-allowlist: foreign user must be 403, got ${resp.status}`);
  const { opened } = await terminalWSOn(OS_PROXY_PORT, cookie, EMPTY_HOST);
  assert.ok(!opened, 'OpenShift empty-allowlist: foreign user WS must be closed');
  console.log('  ✓ OpenShift + empty allowlist: foreign user → 403 / WS closed');
}

async function testC3_EmptyAllowlist_OpenShift_SameHiveDenied() {
  // The killer case: even a validly-signed cookie for THIS hive's own id-shaped
  // user must be denied — an ownerless OpenShift hive has no one authorized, and
  // the proxy is the only gate, so it must fail CLOSED rather than open a shell.
  const cookie = mintCookie(EMPTY_SECRET, 'anyone');
  const resp = await terminalHTTPOn(OS_PROXY_PORT, cookie, EMPTY_HOST);
  assert.equal(resp.status, 403, `OpenShift empty-allowlist: any user must be 403, got ${resp.status}`);
  const { opened } = await terminalWSOn(OS_PROXY_PORT, cookie, EMPTY_HOST);
  assert.ok(!opened, 'OpenShift empty-allowlist: any user WS must be closed');
  console.log('  ✓ OpenShift + empty allowlist: same-hive/any user → 403 / WS closed (no fail-open)');
}

// nginx lane: empty allowlist + ingress authz → proxy defers (allows).
const NGINX_PROXY_PORT = 19031, NGINX_API_PORT = 19032, NGINX_TTYD_PORT = 19033;
let nginxTtyd, nginxProxy;

async function testC3_EmptyAllowlist_Nginx_Deferred() {
  // The ingress auth-proxy already authorized this request; the proxy must NOT
  // double-deny a legitimate ingress-approved user just because the local
  // allowlist is empty.
  const cookie = mintCookie(EMPTY_SECRET, 'bob');
  const resp = await terminalHTTPOn(NGINX_PROXY_PORT, cookie, EMPTY_HOST);
  assert.equal(resp.status, 200, `nginx empty-allowlist: ingress-approved user must reach terminal, got ${resp.status}`);
  const { opened } = await terminalWSOn(NGINX_PROXY_PORT, cookie, EMPTY_HOST);
  assert.ok(opened, 'nginx empty-allowlist: ingress-approved user WS should open');
  console.log('  ✓ nginx + empty allowlist: ingress-approved user → deferred (200 / WS opens)');
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

  console.log('\nSigned terminal assertion (C3 follow-up):');
  await testC3F_ValidAssertionAdmitsNonAllowlistedUser();
  await testC3F_ReadWriteRoleSufficient();
  await testC3F_ExpiredAssertionDenied();
  await testC3F_WrongHiveAssertionDenied();
  await testC3F_InsufficientRoleDenied();
  await testN4_AllowlistedReadOnlyDenied();
  await testN4_AllowlistedReadOnlyNoAssertionDenied();
  await testN4_AllowlistedReadOnlyExpiredDenied();
  await testN4_OwnerStillAllowed();
  await testC3F_ForgedAssertionDenied();
  await testC3F_WrongVersionDenied();
  await testC3F_UserMismatchDenied();
  await testF4_AssertionAloneAdmits();
  await testF4_NoCredentialsStillDenied();
  await testF4_ExpiredAssertionAloneDenied();
  await testF4_WrongHiveAssertionAloneDenied();
  await testF4_ForgedAssertionAloneDenied();
  await testF4_InsufficientRoleAloneDenied();
  await testC3F_StaticAllowlistFallbackPreserved();

  console.log('\n✓ C3 tests passed\n');
} catch (e) {
  console.error('\n✗ C3 test failed:', e.message, '\n');
  process.exitCode = 1;
} finally {
  await teardownHiveB();
}

// C3 empty-allowlist lane: no fail-open, even the narrow one.
console.log('Finding C3 — empty-allowlist platform-aware fail-closed\n');
try {
  osTtyd = await startMockTtydOn(OS_TTYD_PORT);
  osProxy = await startEmptyAllowlistProxy(OS_PROXY_PORT, OS_API_PORT, OS_TTYD_PORT, false /* no ingress authz */);
  nginxTtyd = await startMockTtydOn(NGINX_TTYD_PORT);
  nginxProxy = await startEmptyAllowlistProxy(NGINX_PROXY_PORT, NGINX_API_PORT, NGINX_TTYD_PORT, true /* ingress authz */);
  await new Promise(r => setTimeout(r, 500));

  console.log('OpenShift-Route lane (no ingress auth-proxy):');
  await testC3_EmptyAllowlist_OpenShift_ForeignDenied();
  await testC3_EmptyAllowlist_OpenShift_SameHiveDenied();

  console.log('\nnginx-ingress lane (ingress auth-proxy in front):');
  await testC3_EmptyAllowlist_Nginx_Deferred();

  console.log('\n✓ C3 empty-allowlist tests passed\n');
} catch (e) {
  console.error('\n✗ C3 empty-allowlist test failed:', e.message, '\n');
  process.exitCode = 1;
} finally {
  if (osProxy) osProxy.kill();
  if (nginxProxy) nginxProxy.kill();
  if (osTtyd) osTtyd.close();
  if (nginxTtyd) nginxTtyd.close();
  await new Promise(r => setTimeout(r, 200));
}
