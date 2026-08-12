// AUDIT F10: the hub session cookie's lifetime must be SIGNED and enforced by
// the verifier, not left to the browser's MaxAge.
//
// Run: node session_cookie_v3.test.js
//
// Why this file exists separately from the Go tests: the `hive_hub_user` cookie
// is minted by the hub (Go) and verified INDEPENDENTLY here, in every spoke's
// proxy. The two implementations share no code — only a wire format. If they
// disagree by one byte, every hosted login breaks at deploy, silently, and only
// in production, because nothing but a real hub-minted cookie exercises the
// disagreement.
//
// So the vectors below are FROZEN OUTPUT FROM THE REAL GO MINTER
// (mintHubUserCookieValueV3, seed and clock as noted). They are not
// Node-generated: a Node-generated vector would only prove Node agrees with
// itself, which is exactly the kind of green-but-meaningless test this audit
// kept finding.

import crypto from 'crypto';
import assert from 'assert';

const MARKER_V3 = '.v3.';
const MARKER_V2 = '.v2.';
const CLOCK_SKEW_SECONDS = 30;

// --- FROZEN VECTORS, produced by the Go hub -------------------------------
// Seed: 4f2c9a1b...5577 (test-only). Minted at Unix 1700000000 with ttl 1h.
const PUB = '1784545353abb533e166dad23a6363f573ff0ee6ce9c1d0cf7a1ce056640898b';
const MINT_TIME = 1700000000;
const SID = 'f19b65b4add0761dc7aa71fb49a3a3df9056faf0f8dee442fcc461653598e082';

// Valid: iat=1700000000, exp=1700003600, u=alice
const GO_V3_VALID =
  'eyJ1IjoiYWxpY2UiLCJpYXQiOjE3MDAwMDAwMDAsImV4cCI6MTcwMDAwMzYwMCwic2lkIjoiZjE5YjY1YjRhZGQwNzYxZGM3YWE3MWZiNDlhM2EzZGY5MDU2ZmFmMGY4ZGVlNDQyZmNjNDYxNjUzNTk4ZTA4MiJ9.v3.MCqKVIDZ4cEVs5mQ9Mve822nb5nSrXXAbRhr6FiH6sjtD3JyCIeTIj-O3ZeTempyxMDK721hgQJl23axvwiLAg';
// Minted 48h before MINT_TIME, so exp=1699830800 is long past.
const GO_V3_EXPIRED =
  'eyJ1IjoiYWxpY2UiLCJpYXQiOjE2OTk4MjcyMDAsImV4cCI6MTY5OTgzMDgwMCwic2lkIjoiZTIyZDY2YWM3ODllZDdmN2IyYzE5ODhmNzEzNThjZDUwYzcyYTdlOTBjZTY0MDJiMDg0NTIwNGE1Y2JiYjhkNiJ9.v3.liw-rbWHQR1_uVFlMpN_TigJ80E0N2g6CJt0o-TR1m-nRbvB7Y7oNA19FBdEmOrG9yQoIlJ33iR9Qte2CdFiCQ';
// Minted 2h AFTER MINT_TIME: iat=1700007200 is in the future at MINT_TIME.
const GO_V3_FUTURE =
  'eyJ1IjoiYWxpY2UiLCJpYXQiOjE3MDAwMDcyMDAsImV4cCI6MTcwMDAxMDgwMCwic2lkIjoiMzNjNzY2Y2E3ZmM5NzA5YWQzODM2YTg1N2UxOTg3OGIxNmZjNGU0NDNiMzQ1Y2U2YWJhNGVkZDYzYmUwOGY2NCJ9.v3.TH_eBfSf2dM8Y0GDs2RNo0oP3bg0TksrnT6zjWBgpUnd_Mp7OBL-nvEJwyeuzGX5ZhxDbxTqQGinacGcMEyxCA';
// A v2 cookie minted by the same Go hub, for the compatibility-lane control.
const GO_V2 =
  'v2user.v2.nNS44t2T-svaTHmr6dLBoAiaR0d9LP0VQTac8qHdXeinxw0pj-_HwpNKxbDYjAzFDDTlT2Xmhu5pt7IvgxQWDg';

// --- code under test (copied from server.js, kept in lockstep) -------------

function verifyHubUserCookieV3(pubHex, value, nowSeconds) {
  if (!pubHex || !value) return '';
  const idx = value.lastIndexOf(MARKER_V3);
  if (idx <= 0) return '';
  const body = value.slice(0, idx);
  const sigB64 = value.slice(idx + MARKER_V3.length);
  if (!body || !sigB64) return '';
  try {
    const raw = Buffer.from(pubHex, 'hex');
    if (raw.length !== 32) return '';
    const spki = Buffer.concat([Buffer.from('302a300506032b6570032100', 'hex'), raw]);
    const pub = crypto.createPublicKey({ key: spki, format: 'der', type: 'spki' });
    const sig = Buffer.from(sigB64, 'base64url');
    if (!crypto.verify(null, Buffer.from(body), pub, sig)) return '';
    const claims = JSON.parse(Buffer.from(body, 'base64url').toString('utf8'));
    if (!claims || typeof claims.u !== 'string' || !claims.u) return '';
    if (typeof claims.sid !== 'string' || !claims.sid) return '';
    if (typeof claims.iat !== 'number' || typeof claims.exp !== 'number') return '';
    const now = Number.isFinite(nowSeconds) ? nowSeconds : Math.floor(Date.now() / 1000);
    if (claims.iat > now + CLOCK_SKEW_SECONDS) return '';
    if (claims.exp < now - CLOCK_SKEW_SECONDS) return '';
    return claims.u;
  } catch (_) {
    return '';
  }
}

function verifyHubUserCookieV2(pubHex, value) {
  if (!pubHex || !value) return '';
  const idx = value.lastIndexOf(MARKER_V2);
  if (idx <= 0) return '';
  const username = value.slice(0, idx);
  const sigB64 = value.slice(idx + MARKER_V2.length);
  if (!username || !sigB64) return '';
  try {
    const raw = Buffer.from(pubHex, 'hex');
    if (raw.length !== 32) return '';
    const spki = Buffer.concat([Buffer.from('302a300506032b6570032100', 'hex'), raw]);
    const pub = crypto.createPublicKey({ key: spki, format: 'der', type: 'spki' });
    const sig = Buffer.from(sigB64, 'base64url');
    return crypto.verify(null, Buffer.from(username), pub, sig) ? username : '';
  } catch (_) {
    return '';
  }
}

function verifyHubUserCookieEither(pubHex, legacySecret, value, nowSeconds) {
  const v3 = verifyHubUserCookieV3(pubHex, value, nowSeconds);
  if (v3) return v3;
  return verifyHubUserCookieV2(pubHex, value);
}

// --- tests ----------------------------------------------------------------

let failures = 0;
function test(name, fn) {
  try {
    fn();
    console.log(`  ok  ${name}`);
  } catch (err) {
    failures++;
    console.error(`  FAIL ${name}\n       ${err.message}`);
  }
}

console.log('F10 v3 session cookie — Node/Go cross-language agreement');

// POSITIVE CONTROLS FIRST. Without these the whole file passes against a
// verifier that returns '' unconditionally.
test('a real Go-minted v3 cookie verifies in Node at mint time', () => {
  assert.strictEqual(verifyHubUserCookieV3(PUB, GO_V3_VALID, MINT_TIME), 'alice');
});

test('the frozen payload decodes to the exact claim keys Go wrote', () => {
  const body = GO_V3_VALID.slice(0, GO_V3_VALID.lastIndexOf(MARKER_V3));
  const claims = JSON.parse(Buffer.from(body, 'base64url').toString('utf8'));
  assert.deepStrictEqual(Object.keys(claims).sort(), ['exp', 'iat', 'sid', 'u']);
  assert.strictEqual(claims.u, 'alice');
  assert.strictEqual(claims.iat, MINT_TIME);
  assert.strictEqual(claims.exp, MINT_TIME + 3600);
  assert.strictEqual(claims.sid, SID);
});

test('valid just inside expiry still verifies', () => {
  assert.strictEqual(verifyHubUserCookieV3(PUB, GO_V3_VALID, MINT_TIME + 3599), 'alice');
});

test('clock skew tolerance: slightly past exp still verifies', () => {
  assert.strictEqual(verifyHubUserCookieV3(PUB, GO_V3_VALID, MINT_TIME + 3600 + 20), 'alice');
});

test('clock skew tolerance: iat slightly ahead still verifies', () => {
  // FUTURE has iat = MINT_TIME + 7200; check 20s before that.
  assert.strictEqual(verifyHubUserCookieV3(PUB, GO_V3_FUTURE, MINT_TIME + 7200 - 20), 'alice');
});

// REJECTIONS — the finding itself.
test('an expired Go-minted cookie is REJECTED (F10 arm 1)', () => {
  assert.strictEqual(verifyHubUserCookieV3(PUB, GO_V3_EXPIRED, MINT_TIME), '');
});

test('expired well past the skew window is rejected', () => {
  assert.strictEqual(verifyHubUserCookieV3(PUB, GO_V3_VALID, MINT_TIME + 7200), '');
});

test('not-yet-valid (iat in the future) is rejected', () => {
  assert.strictEqual(verifyHubUserCookieV3(PUB, GO_V3_FUTURE, MINT_TIME), '');
});

test('tampered payload (username swapped to the admin) is rejected', () => {
  const idx = GO_V3_VALID.lastIndexOf(MARKER_V3);
  const claims = JSON.parse(Buffer.from(GO_V3_VALID.slice(0, idx), 'base64url').toString('utf8'));
  claims.u = 'clubanderson';
  const forged = Buffer.from(JSON.stringify(claims)).toString('base64url') + GO_V3_VALID.slice(idx);
  assert.strictEqual(verifyHubUserCookieV3(PUB, forged, MINT_TIME), '');
});

test('tampered expiry (session extension) is rejected', () => {
  const idx = GO_V3_VALID.lastIndexOf(MARKER_V3);
  const claims = JSON.parse(Buffer.from(GO_V3_VALID.slice(0, idx), 'base64url').toString('utf8'));
  claims.exp = MINT_TIME + 10 * 365 * 24 * 3600;
  const forged = Buffer.from(JSON.stringify(claims)).toString('base64url') + GO_V3_VALID.slice(idx);
  assert.strictEqual(verifyHubUserCookieV3(PUB, forged, MINT_TIME), '');
});

test('tampered signature is rejected', () => {
  const idx = GO_V3_VALID.lastIndexOf(MARKER_V3);
  const sig = GO_V3_VALID.slice(idx + MARKER_V3.length);
  const flipped = (sig[0] === 'A' ? 'B' : 'A') + sig.slice(1);
  assert.strictEqual(
    verifyHubUserCookieV3(PUB, GO_V3_VALID.slice(0, idx + MARKER_V3.length) + flipped, MINT_TIME),
    ''
  );
});

test('a cookie under a different key is rejected', () => {
  const otherPub = '1111111111111111111111111111111111111111111111111111111111111111';
  assert.strictEqual(verifyHubUserCookieV3(otherPub, GO_V3_VALID, MINT_TIME), '');
});

test('wrong marker: a v3 body carrying .v2. is not parsed as v3', () => {
  assert.strictEqual(
    verifyHubUserCookieV3(PUB, GO_V3_VALID.replace(MARKER_V3, MARKER_V2), MINT_TIME),
    ''
  );
});

test('malformed values are rejected', () => {
  for (const bad of ['', '.v3.', 'abc.v3.', '.v3.sig', '!!!.v3.!!!', 'no-marker-at-all']) {
    assert.strictEqual(verifyHubUserCookieV3(PUB, bad, MINT_TIME), '', `accepted ${JSON.stringify(bad)}`);
  }
});

// COMPATIBILITY — the outage guard. The v2 lane must survive untouched.
test('the v2 lane still verifies through Either (no fleet 401)', () => {
  assert.strictEqual(verifyHubUserCookieEither(PUB, '', GO_V2, MINT_TIME), 'v2user');
});

test('an expired v3 does NOT fall through to the v2 lane and get accepted', () => {
  assert.strictEqual(verifyHubUserCookieEither(PUB, '', GO_V3_EXPIRED, MINT_TIME), '');
});

test('Either routes a valid v3 to the v3 lane', () => {
  assert.strictEqual(verifyHubUserCookieEither(PUB, '', GO_V3_VALID, MINT_TIME), 'alice');
});

if (failures > 0) {
  console.error(`\n${failures} F10 cookie test(s) failed`);
  process.exit(1);
}
console.log('\nall F10 v3 session cookie tests passed');
