// N2 (CWE-321/798): the proxy must verify Ed25519-signed hub session cookies,
// and a spoke must NOT be able to forge one.
//
// Run: node session_cookie_v2.test.js
//
// The hub session cookie (`hive_hub_user`) used to be HMAC'd with the SESSION
// sub-key, which provisioning injects into EVERY spoke so each spoke's proxy can
// verify it. An HMAC key that verifies also signs, so any spoke operator could
// read HIVE_SESSION_KEY from their pod env and mint `clubanderson.<sig>` — a hub
// ADMIN cookie honoured on ~21 admin routes.
//
// These tests are self-contained (no proxy process): they exercise the exact
// verification logic server.js uses, against vectors produced the way the Go hub
// produces them. The cross-language agreement is the risky part — if Go and Node
// disagree on the encoding, every hosted login breaks at deploy.

import crypto from 'crypto';
import assert from 'assert';
import { readFileSync } from 'fs';

const INFO_SESSION_ED25519_SEED = 'hive-session-ed25519-v1';
const INFO_SESSION_KEY = 'hive-session-v1';
const MARKER = '.v2.';

// --- helpers mirroring the hub (Go) side -----------------------------------

// seedFromMaster mirrors deriveDomainKey(master, infoSessionEd25519Seed).
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

// mintV2 mirrors mintHubUserCookieValueV2 — HUB-ONLY, needs the private seed.
function mintV2(seedHex, username) {
  const sig = crypto.sign(null, Buffer.from(username), privFromSeed(seedHex));
  return username + MARKER + sig.toString('base64url');
}

// --- the code under test (copied from server.js, kept in lockstep) ---------

function verifyHubUserCookieV2(pubHex, value) {
  if (!pubHex || !value) return '';
  const idx = value.lastIndexOf(MARKER);
  if (idx <= 0) return '';
  const username = value.slice(0, idx);
  const sigB64 = value.slice(idx + MARKER.length);
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

function verifyHubUserCookie(secret, value) {
  if (!secret || !value) return '';
  const idx = value.lastIndexOf('.');
  if (idx <= 0 || idx === value.length - 1) return '';
  const username = value.slice(0, idx);
  const sig = value.slice(idx + 1);
  const expected = crypto.createHmac('sha256', secret).update(username).digest('base64url');
  const a = Buffer.from(sig);
  const b = Buffer.from(expected);
  if (a.length !== b.length || !crypto.timingSafeEqual(a, b)) return '';
  return username;
}

// AUDIT F23: kept in lockstep with server.js — the legacy symmetric lane is
// GONE. legacySecret is accepted and ignored, mirroring the real function, so
// this copy cannot drift back into verifying symmetric material.
function verifyHubUserCookieEither(pubHex, legacySecret, value) {
  void legacySecret;
  return verifyHubUserCookieV2(pubHex, value);
}

// --- tests ------------------------------------------------------------------

const tests = [];
const test = (name, fn) => tests.push([name, fn]);

const MASTER = 'test-master-secret-n2';
const SEED = seedFromMaster(MASTER);
const PUB = pubHexFromSeed(SEED);
const LEGACY = crypto.createHmac('sha256', MASTER).update(INFO_SESSION_KEY).digest('hex');

test('a hub-minted v2 cookie verifies against the public key', () => {
  const c = mintV2(SEED, 'alice');
  assert.strictEqual(verifyHubUserCookieV2(PUB, c), 'alice');
});

test('THE FINDING: a spoke holding only the public key cannot forge an admin cookie', () => {
  // The spoke's only key material is PUB. Treating it as a seed yields a
  // syntactically valid cookie (a public key and a seed are both 32 bytes, so
  // nothing can tell them apart by length) — but it is signed by a DIFFERENT
  // private key, so it must not verify.
  const forged = mintV2(PUB, 'clubanderson');
  assert.strictEqual(
    verifyHubUserCookieV2(PUB, forged), '',
    'a spoke forged a verifiable hub ADMIN cookie from the public key alone');
  assert.strictEqual(
    verifyHubUserCookieEither(PUB, LEGACY, forged), '',
    'the spoke-forged admin cookie was accepted by the live verify path');
});

test('a re-labelled cookie (alice signature, admin username) is rejected', () => {
  const c = mintV2(SEED, 'alice');
  const sig = c.slice(c.lastIndexOf(MARKER) + MARKER.length);
  assert.strictEqual(verifyHubUserCookieV2(PUB, 'clubanderson' + MARKER + sig), '',
    'username is not covered by the signature');
});

test('a cookie from a different hub is rejected', () => {
  const otherPub = pubHexFromSeed(seedFromMaster('a-different-master'));
  assert.strictEqual(verifyHubUserCookieV2(otherPub, mintV2(SEED, 'alice')), '');
});

test('garbage and malformed values fail closed', () => {
  for (const v of ['', 'alice', 'alice.v2.', '.v2.AAAA', 'alice.v2.!!!!']) {
    assert.strictEqual(verifyHubUserCookieV2(PUB, v), '', `accepted ${JSON.stringify(v)}`);
  }
  assert.strictEqual(verifyHubUserCookieV2('', mintV2(SEED, 'alice')), '', 'accepted with no public key');
  assert.strictEqual(verifyHubUserCookieV2('abcd', mintV2(SEED, 'alice')), '', 'accepted a short public key');
});

// AUDIT F23 — INVERTED. This test previously asserted "legacy HMAC cookies still
// verify alongside v2", i.e. it asserted the vulnerability. The legacy lane is
// symmetric and fleet-uniform, so verify-capable implied mint-capable: any spoke
// operator could forge `clubanderson.<sig>`. It is inverted rather than deleted
// so the file keeps documenting the decision and a re-added lane fails here.
test('F23: a correctly-HMACd legacy cookie is REJECTED even with the right secret', () => {
  const legacyCookie = 'bob.' + crypto.createHmac('sha256', LEGACY).update('bob').digest('base64url');

  // PRECONDITION: the legacy primitive itself is still well-formed and the
  // secret is the CORRECT one — so a rejection below is the lane being gone,
  // not a typo'd fixture. This is the mirror of the Go-side
  // TestF1LegacyLaneRejectsEvenTheCorrectSecret.
  assert.strictEqual(verifyHubUserCookie(LEGACY, legacyCookie), 'bob',
    'precondition: the legacy HMAC primitive must still compute — otherwise this test proves nothing');

  // THE ASSERTION: the live verify path refuses it, even handed the correct secret.
  assert.strictEqual(verifyHubUserCookieEither(PUB, LEGACY, legacyCookie), '',
    'F23 REGRESSION: the legacy symmetric lane still verifies — a spoke can forge an admin cookie');
  assert.strictEqual(verifyHubUserCookieEither(PUB, '', legacyCookie), '');

  // POSITIVE CONTROL: v2 must STILL verify. Without this, a verifier that
  // rejected everything would satisfy the assertions above while breaking every
  // hosted sign-in.
  assert.strictEqual(verifyHubUserCookieEither(PUB, LEGACY, mintV2(SEED, 'alice')), 'alice',
    'positive control FAILED: v2 cookies must still verify');
  assert.strictEqual(verifyHubUserCookieEither(PUB, '', mintV2(SEED, 'alice')), 'alice');
});

// AUDIT F23 SOURCE GUARD. The behavioural tests above exercise a COPY of the
// verifier kept in this file, and the end-to-end tests in server.test.js only
// catch a lane that is REACHED. heartbeatBearerOK (F24) showed that a
// dead-but-armed acceptor is invisible to both. This asserts the symmetric
// verifier has not returned to the shipped server.js at all.
test('F23: server.js contains no symmetric cookie-verification lane', () => {
  const src = readFileSync(new URL('./server.js', import.meta.url), 'utf8');

  // Positive control: the tombstone naming the finding must be present, so a
  // wholesale rewrite cannot make this assertion vacuously true.
  assert.ok(src.includes('AUDIT F23 TOMBSTONE'),
    'positive control FAILED: the F23 tombstone is missing from server.js');

  assert.ok(!/function\s+verifyHubUserCookie\s*\(\s*secret\s*,\s*value\s*\)/.test(src),
    'F23 REGRESSION: the symmetric verifier verifyHubUserCookie has been reintroduced in server.js');
  assert.ok(!/return\s+verifyHubUserCookie\s*\(\s*legacySecret\s*,\s*value\s*\)/.test(src),
    'F23 REGRESSION: a legacy symmetric fallback has been reintroduced in server.js');
});

test('INTEROP: the public key derived here matches the Go hub byte-for-byte', () => {
  // Vector generated from the Go implementation with master "interop-master":
  //   s := &HubServer{hubSecret: "interop-master"}; s.sessionPublicKey()
  const GO_MASTER = 'interop-master';
  const GO_PUB = 'fd5a5ba6610c75a66deb729adba31b2b5768d2e72f3621fcab6e16ad3b9a6520';
  const GO_COOKIE =
    'clubanderson.v2.plC1ZL1n1nxCIwemNsm0SQ4eZXfDhzY63b-QfaQs7gy2Pllc3V4d86bnVcWmsFExzAMo9ZMqJucVU0mRQY0UCg';

  assert.strictEqual(pubHexFromSeed(seedFromMaster(GO_MASTER)), GO_PUB,
    'Node and Go derive DIFFERENT public keys from the same master — every hosted login would break');
  assert.strictEqual(verifyHubUserCookieV2(GO_PUB, GO_COOKIE), 'clubanderson',
    'Node cannot verify a real Go-minted cookie');
});

let failed = 0;
for (const [name, fn] of tests) {
  try {
    fn();
    console.log(`ok   ${name}`);
  } catch (e) {
    failed++;
    console.error(`FAIL ${name}\n     ${e.message}`);
  }
}
console.log(`\n${tests.length - failed}/${tests.length} passed`);
process.exit(failed ? 1 : 0);
