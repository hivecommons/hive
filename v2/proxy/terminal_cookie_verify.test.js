// F3 (CWE-345): the /terminal gate must VERIFY the hub cookie's signature.
//
// Run: node terminal_cookie_verify.test.js
//
// Before this fix both terminal gates — the HTTP route and the WebSocket
// upgrade — checked only that `hive_hub_user` was NON-EMPTY:
//
//     const user = cookies['hive_hub_user'];
//     if (!user) { ...401... }
//     next();                       // <-- any value proceeded
//
// So `curl -H 'Cookie: hive_hub_user=x' https://<hive>.hive.kubestellar.io/terminal`
// opened a shell in a container holding the GitHub App private key, every agent
// token, and the hub secret — with no hub account required at all.
//
// Both gates are covered here. Fixing only the HTTP one would be security
// theatre: a WebSocket upgrade reaches the same ttyd.
//
// PORTED FROM v2 (#3657): v4's /terminal gate is layered signature verification
// (this file, unchanged from v2) PLUS a PER-HIVE authorization step
// (isAuthorizedForThisHive / authorizeTerminal, audit #2756 + N4 + F4) that v2
// does not have at all. A hub-wide-valid signed cookie is necessary but no
// longer sufficient on v4: the caller must also be on THIS hive's authorized
// list (or hold a signed per-hive terminal assertion). The proxy itself is NOT
// changed for this port — only the "valid cookie" positive control below is
// adapted to also supply HIVE_AUTHORIZED_USERS/HIVE_ID, i.e. the config a real
// hosted hive is provisioned with, so it exercises v4's actual (correct, more
// restrictive) authorization rather than failing on a layer v2 never had.

import { WebSocket, WebSocketServer } from 'ws';
import { createServer, request as httpRequest } from 'http';
import { spawn } from 'child_process';
import path from 'path';
import crypto from 'crypto';
import { fileURLToPath } from 'url';
import assert from 'assert';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PROXY_PORT = 19101;
const GO_PORT = 19102;
const TTYD_PORT = 19103;
const HOSTED_HOST = 'hive-f3.hive.kubestellar.io';
const HIVE_ID = 'hive-f3';

// The master this spoke is provisioned with; the proxy derives its session
// PUBLIC key from it exactly as Go's provisioning does.
const MASTER = 'f3-test-master-secret';
const SESSION_KEY = crypto.createHmac('sha256', MASTER).update('hive-session-v1').digest('hex');

// AUDIT F23: mintCookie now mints v3 (Ed25519 + signed expiry), the format
// production actually issues. It previously minted the legacy symmetric HMAC
// format, whose verification lane is deleted from server.js — so the "valid
// cookie" positive control below would otherwise be minting a cookie no spoke
// can verify, and would fail for a reason that has nothing to do with F3.
const INFO_SESSION_ED25519_SEED = 'hive-session-ed25519-v1';

function privFromSeed(seedHex) {
  const pkcs8 = Buffer.concat([
    Buffer.from('302e020100300506032b657004220420', 'hex'),
    Buffer.from(seedHex, 'hex'),
  ]);
  return crypto.createPrivateKey({ key: pkcs8, format: 'der', type: 'pkcs8' });
}

// mintCookie mirrors the hub's mintHubUserCookieValueV3.
function mintCookie(username) {
  const seed = crypto.createHmac('sha256', MASTER).update(INFO_SESSION_ED25519_SEED).digest('hex');
  const iat = Math.floor(Date.now() / 1000);
  const claims = { u: username, iat, exp: iat + 3600, sid: crypto.randomBytes(32).toString('hex') };
  const body = Buffer.from(JSON.stringify(claims)).toString('base64url');
  const sig = crypto.sign(null, Buffer.from(body), privFromSeed(seed));
  return `${body}.v3.${sig.toString('base64url')}`;
}

// mintCookieLegacy mints the DELETED symmetric format, for the F23 rejection
// test only.
function mintCookieLegacy(username) {
  const sig = crypto.createHmac('sha256', SESSION_KEY).update(username).digest('base64url');
  return `${username}.${sig}`;
}

function mockBackend(port, body) {
  return new Promise(resolve => {
    const s = createServer((req, res) => { res.writeHead(200); res.end(body); });
    s.listen(port, () => resolve(s));
  });
}

// The ttyd mock must speak WebSocket, not just HTTP: the terminal is a WS
// upgrade, so a plain HTTP mock makes the "valid cookie opens a shell" control
// fail for the wrong reason and hides whether the gate actually passed.
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
      env: {
        ...process.env,
        HIVE_PROXY_PORT: String(PROXY_PORT),
        HIVE_API_PORT: String(GO_PORT),
        HIVE_TTYD_PORT: String(TTYD_PORT),
        HIVE_DASHBOARD_TOKEN: '',
        HIVE_STATIC_DIR: __dirname,
        HIVE_HUB_SECRET: MASTER,
        HIVE_ID: HIVE_ID,
        // v4 per-hive authorization (#2756/N4/F4): the static allowlist a real
        // hosted hive is provisioned with. 'alice' is the positive control below
        // (a valid signature alone must no longer be sufficient); 'mallory' is
        // deliberately OMITTED so the re-labelled-cookie case at the bottom is
        // denied on signature, not merely on authorization.
        HIVE_AUTHORIZED_USERS: 'alice:owner',
        NODE_ENV: 'test',
      },
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

// NOTE: uses http.request, not fetch. fetch() silently DROPS a custom Host
// header, so the proxy would see 127.0.0.1 and take the non-hosted branch —
// the terminal gate would never run and the test would pass vacuously against
// vulnerable code. That is exactly the failure mode this file exists to catch.
function terminalHTTP(cookie) {
  return new Promise((resolve, reject) => {
    const headers = { Host: HOSTED_HOST };
    if (cookie !== null) headers.Cookie = `hive_hub_user=${cookie}`;
    const req = httpRequest(
      { host: '127.0.0.1', port: PROXY_PORT, path: '/terminal', method: 'GET', headers },
      res => { res.resume(); resolve(res.statusCode); },
    );
    req.on('error', reject);
    req.end();
  });
}

function terminalWS(cookie) {
  return new Promise(resolve => {
    const headers = { Host: HOSTED_HOST };
    if (cookie !== null) headers.Cookie = `hive_hub_user=${cookie}`;
    const ws = new WebSocket(`ws://127.0.0.1:${PROXY_PORT}/terminal`, { headers });
    const done = opened => { try { ws.close(); } catch {} resolve(opened); };
    ws.on('open', () => done(true));
    ws.on('error', () => done(false));
    ws.on('close', () => done(false));
    setTimeout(() => done(false), 4000);
  });
}

let proxy, go, ttyd;
try {
  go = await mockBackend(GO_PORT, 'go');
  ttyd = await mockTtydWS(TTYD_PORT);
  proxy = await startProxy();
  await new Promise(r => setTimeout(r, 400));

  // --- THE VULNERABILITY: a garbage cookie must NOT open a shell -------------
  for (const forged of ['x', 'anything-at-all', 'clubanderson.not-a-real-signature']) {
    const status = await terminalHTTP(forged);
    assert.equal(status, 401,
      `F3: HTTP /terminal accepted the forged cookie ${JSON.stringify(forged)} (got ${status}) — ` +
      `this is a shell in a credential-holding container for anyone who can set a header`);
    const opened = await terminalWS(forged);
    assert.ok(!opened,
      `F3: WebSocket /terminal accepted the forged cookie ${JSON.stringify(forged)} — ` +
      `the upgrade path reaches the SAME ttyd`);
  }
  console.log('  ✓ forged / unsigned cookies denied on BOTH the HTTP and WS gates');

  // --- No cookie at all still denied (unchanged behaviour) -------------------
  assert.equal(await terminalHTTP(null), 401, 'missing cookie must be 401');
  assert.ok(!(await terminalWS(null)), 'missing cookie WS must be refused');
  console.log('  ✓ absent cookie denied');

  // --- CONTROL: a genuinely hub-signed AND per-hive-authorized cookie works --
  // 'alice' is both correctly signed (mintCookie) and on HIVE_AUTHORIZED_USERS
  // above — i.e. the config a real hosted hive is provisioned with. This proves
  // the signature-verification fix does not lock out a legitimately authorized
  // user, without asserting anything about the separate per-hive authorization
  // layer (covered by the fallback's own unit coverage, not this F3 test).
  const good = mintCookie('alice');
  assert.equal(await terminalHTTP(good), 200,
    'a VALID hub-signed, per-hive-authorized cookie must still reach the terminal — the fix must not lock out real users');
  assert.ok(await terminalWS(good), 'a VALID hub-signed, per-hive-authorized cookie must still open the WS');
  console.log('  ✓ valid hub-signed + authorized cookie still granted (HTTP + WS)');

  // --- Tampering: right shape, wrong signature ------------------------------
  // Swap the v3 CLAIMS BODY for one naming a different user while keeping
  // alice's signature. The signature covers the body, so this must fail.
  const goodSig = good.slice(good.lastIndexOf('.v3.') + 4);
  const malloryIat = Math.floor(Date.now() / 1000);
  const malloryBody = Buffer.from(JSON.stringify({
    u: 'mallory', iat: malloryIat, exp: malloryIat + 3600,
    sid: crypto.randomBytes(32).toString('hex'),
  })).toString('base64url');
  const tampered = `${malloryBody}.v3.${goodSig}`;
  assert.equal(await terminalHTTP(tampered), 401,
    'a re-labelled cookie (alice signature, different username) must be denied');
  console.log('  ✓ re-labelled cookie denied (signature covers the username)');

  // --- AUDIT F23: the legacy symmetric lane is deleted -----------------------
  // `alice` is signed with the CORRECT fleet SESSION_KEY and IS on this hive's
  // allowlist, so if the lane were live she would get 200. A 401 is therefore
  // attributable to the cookie lane alone, not to authorization. The v3 control
  // for the same user passed above, so this is not a reject-everything proxy.
  const legacyAlice = mintCookieLegacy('alice');
  assert.equal(await terminalHTTP(legacyAlice), 401,
    'F23 REGRESSION: a correctly-HMACd legacy symmetric cookie reached the terminal — ' +
    'SESSION_KEY is fleet-uniform, so any spoke operator could forge this');
  assert.ok(!(await terminalWS(legacyAlice)),
    'F23 REGRESSION: a correctly-HMACd legacy symmetric cookie opened the WS terminal');
  console.log('  ✓ F23: legacy symmetric cookie denied on BOTH gates (v3 for same user still 200)');

  console.log('\n✓ F3 terminal cookie verification tests passed\n');
} catch (e) {
  console.error('\n✗ F3 test failed:', e.message, '\n');
  process.exitCode = 1;
} finally {
  if (proxy) proxy.kill();
  if (go) go.close();
  if (ttyd) ttyd.close();
  await new Promise(r => setTimeout(r, 200));
}
