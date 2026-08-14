// Rotation follow-on #6: the proxy must accept TWO hub session public keys.
//
// Run: node session_pubkey_rotation.test.js
//
// WHY THIS MATTERS ON THE PROXY SPECIFICALLY. The hub mints `hive_hub_user`
// under the CURRENT master generation the instant an operator rotates, but the
// reconcile lane patches spokes at 3 per 15-minute cycle — about six hours for
// 65 spokes. A proxy that knows only one public key cannot verify anything the
// hub mints during those hours, so every hosted terminal session on a
// not-yet-reconciled spoke fails its cookie check. Holding both keys removes
// that window.
//
// ON THE `dave` PRECEDENT, WHICH THIS FILE DELIBERATELY DOES NOT FOLLOW.
// terminal_cookie_verify.test.js notes that its `dave` fixture is "deliberately
// NOT on the static allowlist", which means several of its assertions would hold
// even if the code under test did nothing — they pass for a reason other than
// the one they name. Every test below is therefore written so that it FAILS for
// the specific reason it claims:
//
//   - The two generations' public keys are asserted DISTINCT before anything
//     else runs, so "verified under the previous key" can never be satisfied by
//     the current key.
//   - Every acceptance assertion is paired with a REJECTION assertion from the
//     same fixture, so a verifier that accepted everything is caught too.
//   - The cookies are minted from real Ed25519 seeds derived exactly as the Go
//     hub derives them, so a Node-only encoding agreement cannot hide.

import crypto from 'crypto';
import assert from 'assert';

const INFO_SESSION_ED25519_SEED = 'hive-session-ed25519-v1';
const MARKER_V2 = '.v2.';
const MARKER_V3 = '.v3.';
const CLOCK_SKEW_SECONDS = 30;

// --- helpers mirroring the hub (Go) side -----------------------------------

function seedFromMaster(master) {
  return crypto.createHmac('sha256', master).update(INFO_SESSION_ED25519_SEED).digest('hex');
}

function privFromSeed(seedHex) {
  const pkcs8 = Buffer.concat([
    Buffer.from('302e020100300506032b657004220420', 'hex'),
    Buffer.from(seedHex, 'hex'),
  ]);
  return crypto.createPrivateKey({ key: pkcs8, format: 'der', type: 'pkcs8' });
}

function pubHexFromSeed(seedHex) {
  const spki = crypto.createPublicKey(privFromSeed(seedHex)).export({ format: 'der', type: 'spki' });
  return Buffer.from(spki.subarray(spki.length - 32)).toString('hex');
}

// mintV2 / mintV3 mirror the hub's minters. HUB-ONLY: they need the private seed.
function mintV2(seedHex, username) {
  const sig = crypto.sign(null, Buffer.from(username), privFromSeed(seedHex));
  return username + MARKER_V2 + sig.toString('base64url');
}

function mintV3(seedHex, username, iat, exp) {
  const body = Buffer.from(
    JSON.stringify({ u: username, iat, exp, sid: 'a'.repeat(64) }),
  ).toString('base64url');
  const sig = crypto.sign(null, Buffer.from(body), privFromSeed(seedHex));
  return body + MARKER_V3 + sig.toString('base64url');
}

// --- code under test (copied from server.js, kept in lockstep) -------------

function isValidEd25519PublicKeyHex(v) {
  return typeof v === 'string' && /^[0-9a-fA-F]{64}$/.test(v);
}

function sessionPublicKeysFrom(primary, prev) {
  const out = [];
  for (const k of [primary, prev]) {
    const v = (k || '').trim();
    if (!isValidEd25519PublicKeyHex(v)) continue;
    if (out.includes(v)) continue;
    out.push(v);
  }
  return out;
}

function verifyHubUserCookie(secret, value) {
  if (!secret || !value) return '';
  const idx = value.lastIndexOf('.');
  if (idx <= 0 || idx === value.length - 1) return '';
  const username = value.slice(0, idx);
  const sig = value.slice(idx + 1);
  const expected = crypto.createHmac('sha256', secret).update(username).digest('base64url');
  const suppliedBuf = Buffer.from(sig);
  const expectedBuf = Buffer.from(expected);
  if (suppliedBuf.length !== expectedBuf.length || !crypto.timingSafeEqual(suppliedBuf, expectedBuf)) {
    return '';
  }
  return username;
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
    return crypto.verify(null, Buffer.from(username), pub, Buffer.from(sigB64, 'base64url')) ? username : '';
  } catch (_) {
    return '';
  }
}

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
    if (!crypto.verify(null, Buffer.from(body), pub, Buffer.from(sigB64, 'base64url'))) return '';
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

// AUDIT F23: kept in lockstep with server.js — no trailing legacy symmetric
// lane. legacySecret is accepted and ignored.
function verifyHubUserCookieAcrossKeys(pubKeys, legacySecret, value) {
  if (!value) return '';
  const keys = Array.isArray(pubKeys) ? pubKeys : [];
  for (const pubHex of keys) {
    const v3 = verifyHubUserCookieV3(pubHex, value);
    if (v3) return v3;
    const v2 = verifyHubUserCookieV2(pubHex, value);
    if (v2) return v2;
  }
  void legacySecret;
  return '';
}

// --- fixtures ---------------------------------------------------------------

const MASTER_CUR = 'pubkey-rotation-test-master-bravo';
const MASTER_PREV = 'pubkey-rotation-test-master-alpha';

const SEED_CUR = seedFromMaster(MASTER_CUR);
const SEED_PREV = seedFromMaster(MASTER_PREV);
const PUB_CUR = pubHexFromSeed(SEED_CUR);
const PUB_PREV = pubHexFromSeed(SEED_PREV);

// NOW is the REAL clock, not a frozen vector, because
// verifyHubUserCookieAcrossKeys mirrors server.js's call shape exactly — it
// passes no `nowSeconds` through to the v3 lane, so v3 enforces its signed
// lifetime against Date.now(). Freezing the clock here would make every v3
// cookie in this file expired-by-decades and each assertion would then pass or
// fail for the wrong reason. The expiry cases in test 6 call
// verifyHubUserCookieV3 directly, where an explicit clock IS accepted.
const NOW = Math.floor(Date.now() / 1000);

// FIXTURE VALIDITY. Every "verified under the previous key" assertion below is
// meaningless if the two generations happen to derive the same public key, so
// assert they differ before anything else runs. This is the guard the `dave`
// precedent lacked.
assert.notStrictEqual(PUB_CUR, PUB_PREV, 'fixture: both generations derived the SAME public key');
assert.strictEqual(PUB_CUR.length, 64, 'fixture: current public key is not 32 bytes of hex');
assert.strictEqual(PUB_PREV.length, 64, 'fixture: previous public key is not 32 bytes of hex');

const COOKIE_CUR_V3 = mintV3(SEED_CUR, 'alice', NOW, NOW + 3600);
const COOKIE_PREV_V3 = mintV3(SEED_PREV, 'bob', NOW, NOW + 3600);
const COOKIE_CUR_V2 = mintV2(SEED_CUR, 'carol');
const COOKIE_PREV_V2 = mintV2(SEED_PREV, 'erin');

// --- tests ------------------------------------------------------------------

// 1. TWO KEYS: a spoke mid-rotation holds both and must verify cookies minted
//    under either generation.
{
  const keys = sessionPublicKeysFrom(PUB_CUR, PUB_PREV);
  assert.strictEqual(keys.length, 2, 'two distinct keys should yield two candidates');

  assert.strictEqual(
    verifyHubUserCookieAcrossKeys(keys, '', COOKIE_CUR_V3), 'alice',
    'two keys: a v3 cookie minted under the CURRENT generation must verify');
  assert.strictEqual(
    verifyHubUserCookieAcrossKeys(keys, '', COOKIE_PREV_V3), 'bob',
    'two keys: a v3 cookie minted under the PREVIOUS generation must verify');
  assert.strictEqual(
    verifyHubUserCookieAcrossKeys(keys, '', COOKIE_CUR_V2), 'carol',
    'two keys: a v2 cookie under the CURRENT generation must verify');
  assert.strictEqual(
    verifyHubUserCookieAcrossKeys(keys, '', COOKIE_PREV_V2), 'erin',
    'two keys: a v2 cookie under the PREVIOUS generation must verify');

  // POSITIVE CONTROL — the failing direction, from the SAME fixture. A cookie
  // signed by neither generation must be rejected, so the four assertions above
  // cannot be satisfied by a verifier that accepts anything well-formed.
  const strangerSeed = seedFromMaster('pubkey-rotation-test-master-stranger');
  assert.strictEqual(
    verifyHubUserCookieAcrossKeys(keys, '', mintV3(strangerSeed, 'mallory', NOW, NOW + 3600)), '',
    'positive control: a cookie signed by an unknown key was ACCEPTED');
  assert.strictEqual(
    verifyHubUserCookieAcrossKeys(keys, '', mintV2(strangerSeed, 'mallory')), '',
    'positive control: a v2 cookie signed by an unknown key was ACCEPTED');
}

// 2. ONE KEY (legacy): every spoke today. HIVE_SESSION_PUBLIC_KEY_PREV absent.
//    Behaviour must be byte-identical to the single-key path.
{
  const keys = sessionPublicKeysFrom(PUB_CUR, undefined);
  assert.strictEqual(keys.length, 1, 'legacy config must yield exactly one candidate');
  assert.strictEqual(keys[0], PUB_CUR, 'legacy config must yield the PRIMARY key');

  assert.strictEqual(
    verifyHubUserCookieAcrossKeys(keys, '', COOKIE_CUR_V3), 'alice',
    'legacy config: a cookie under the current generation must still verify');

  // THE ASSERTION THAT MAKES THIS TEST MEAN SOMETHING. A one-key spoke must
  // REJECT the previous generation's cookie — that rejection is exactly the
  // ~6h outage this PR removes, and if it did not hold here, test 1 would be
  // proving nothing about key plurality.
  assert.strictEqual(
    verifyHubUserCookieAcrossKeys(keys, '', COOKIE_PREV_V3), '',
    'legacy config: a one-key spoke must NOT verify a previous-generation cookie');
  assert.strictEqual(
    verifyHubUserCookieAcrossKeys(keys, '', COOKIE_PREV_V2), '',
    'legacy config: a one-key spoke must NOT verify a previous-generation v2 cookie');

  // Empty-string and empty-array _PREV are the same as absent.
  assert.strictEqual(sessionPublicKeysFrom(PUB_CUR, '').length, 1, 'empty _PREV must be treated as absent');
  assert.strictEqual(sessionPublicKeysFrom(PUB_CUR, '   ').length, 1, 'whitespace _PREV must be treated as absent');
}

// 3. MALFORMED SECOND KEY must not disable verification of the first.
{
  const malformed = [
    ['not hex', 'z'.repeat(64)],
    ['too short', PUB_PREV.slice(0, 62)],
    ['too long', PUB_PREV + 'ab'],
    // The delimited-list encoding this PR rejected. Node's Buffer.from would
    // silently truncate it to the first 32 bytes and "work"; the strict pattern
    // check must DROP it, so that Node and Go agree that this is malformed.
    ['comma-joined pair', PUB_CUR + ',' + PUB_PREV],
    ['space-joined pair', PUB_CUR + ' ' + PUB_PREV],
    ['garbage', 'not-a-key'],
  ];

  for (const [name, bad] of malformed) {
    const keys = sessionPublicKeysFrom(PUB_CUR, bad);
    assert.strictEqual(keys.length, 1, `malformed _PREV (${name}) must be dropped, leaving one key`);
    assert.strictEqual(keys[0], PUB_CUR, `malformed _PREV (${name}) must not displace the primary`);
    assert.strictEqual(
      verifyHubUserCookieAcrossKeys(keys, '', COOKIE_CUR_V3), 'alice',
      `malformed _PREV (${name}) broke verification of the PRIMARY key`);
  }

  // Node's silent hex truncation, demonstrated rather than asserted from
  // memory. This is the measurement that ruled out the delimited-list env
  // encoding: Node reads "<hex>,<hex>" as 32 valid bytes while Go's
  // hex.DecodeString errors, so the two verifiers would disagree about whether
  // hosted SSO still works on an un-rolled spoke.
  assert.strictEqual(
    Buffer.from(PUB_CUR + ',' + PUB_PREV, 'hex').length, 32,
    'expected Node to silently truncate a comma-joined hex pair to 32 bytes');
  assert.strictEqual(
    Buffer.from(PUB_CUR + ',' + PUB_PREV, 'hex').toString('hex'), PUB_CUR,
    'expected the silent truncation to yield exactly the FIRST key');
}

// 4. EMPTY: no public key at all. Ed25519 verification must fail closed, and
//    (audit F23) there is no longer any symmetric lane to fall through to, so
//    such a spoke now authenticates NOBODY through this path.
{
  assert.strictEqual(sessionPublicKeysFrom('', '').length, 0, 'no keys configured must yield no candidates');
  assert.strictEqual(sessionPublicKeysFrom(undefined, undefined).length, 0, 'undefined vars must yield no candidates');

  assert.strictEqual(
    verifyHubUserCookieAcrossKeys([], '', COOKIE_CUR_V3), '',
    'an empty key set must verify NOTHING through the Ed25519 lanes');
  assert.strictEqual(
    verifyHubUserCookieAcrossKeys(undefined, '', COOKIE_CUR_V3), '',
    'a non-array key set must fail closed rather than throw');

  // AUDIT F23 — INVERTED. These two assertions previously demanded the legacy
  // symmetric lane "must still be reachable". That lane is the cross-tenant
  // forgery primitive: SESSION_KEY is byte-identical fleet-wide, so any spoke
  // operator could mint a cookie every other tenant's proxy accepted. It is now
  // deleted, and these assert it stays deleted.
  const legacySecret = 'legacy-symmetric-session-key';
  const legacyCookie = 'frank.' + crypto.createHmac('sha256', legacySecret).update('frank').digest('base64url');

  // PRECONDITION: the legacy primitive still computes and the secret is the
  // CORRECT one, so the rejections below mean "lane gone", not "bad fixture".
  assert.strictEqual(
    verifyHubUserCookie(legacySecret, legacyCookie), 'frank',
    'precondition: the legacy HMAC primitive must still compute — otherwise this proves nothing');

  assert.strictEqual(
    verifyHubUserCookieAcrossKeys([], legacySecret, legacyCookie), '',
    'F23 REGRESSION: the legacy symmetric lane is reachable with no public keys');
  assert.strictEqual(
    verifyHubUserCookieAcrossKeys(sessionPublicKeysFrom(PUB_CUR, PUB_PREV), legacySecret, legacyCookie), '',
    'F23 REGRESSION: the legacy symmetric lane is reachable after the public keys');

  // POSITIVE CONTROL: the Ed25519 lanes must STILL verify across keys, so the
  // rejections above are not satisfied by a verifier that rejects everything.
  assert.strictEqual(
    verifyHubUserCookieAcrossKeys(sessionPublicKeysFrom(PUB_CUR, PUB_PREV), legacySecret, COOKIE_CUR_V3), 'alice',
    'positive control FAILED: a current-generation v3 cookie must still verify');
}

// 5. DE-DUPLICATION: before the first rotation the hub may briefly hand out a
//    _PREV equal to the primary. It must not double the verification work.
{
  const keys = sessionPublicKeysFrom(PUB_CUR, PUB_CUR);
  assert.strictEqual(keys.length, 1, 'a _PREV identical to the primary must be de-duplicated');
  assert.strictEqual(
    verifyHubUserCookieAcrossKeys(keys, '', COOKIE_CUR_V3), 'alice',
    'de-duplication must not break verification');
}

// 6. EXPIRY STILL BINDS across both keys. Key plurality must add a second KEY,
//    never a second LANE, and must not weaken the v3 signed-lifetime check
//    (audit F10) under either generation.
{
  const keys = sessionPublicKeysFrom(PUB_CUR, PUB_PREV);
  const expiredCur = mintV3(SEED_CUR, 'alice', NOW - 7200, NOW - 3600);
  const expiredPrev = mintV3(SEED_PREV, 'bob', NOW - 7200, NOW - 3600);
  assert.strictEqual(
    verifyHubUserCookieV3(PUB_CUR, expiredCur, NOW), '',
    'an expired v3 cookie under the current generation must be rejected');
  assert.strictEqual(
    verifyHubUserCookieV3(PUB_PREV, expiredPrev, NOW), '',
    'an expired v3 cookie under the previous generation must be rejected');
  // Control: the same minter with a live window is accepted, so the rejections
  // above are about EXPIRY and not about the minting helper being broken.
  assert.strictEqual(
    verifyHubUserCookieV3(PUB_PREV, mintV3(SEED_PREV, 'bob', NOW, NOW + 3600), NOW), 'bob',
    'control: a live v3 cookie under the previous generation must verify');
  void keys;
}

console.log('session_pubkey_rotation.test.js: all assertions passed');
