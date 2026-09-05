import express from 'express';
import httpBase from 'http';
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
// SESSION_KEY was the symmetric key the `hive_hub_user` cookie used to be
// verified with. AUDIT F23 DELETED THAT LANE — see verifyHubUserCookieEither.
// The cookie is now verified ONLY against the Ed25519 SESSION_PUBLIC_KEY below,
// so this spoke can verify a hub-minted cookie but cannot mint one.
//
// SESSION_KEY is still resolved because it remains the IS_HOSTED boot signal
// (a self-hosted operator legitimately holds the master). It must NOT be
// reintroduced as a signature-verification comparand.
//
// SESSION_KEY resolution mirrors the Go SpokeSessionKey() helper:
//   1. HIVE_SESSION_KEY (modern least-privilege provisioning) if set, else
//   2. derive it from HIVE_HUB_SECRET via HMAC-SHA256(master, "hive-session-v1")
//      — the backward-compatible path for self-hosted/legacy spokes that still
//      hold the master. Both yield the identical key the hub signs with.
// The info string MUST stay byte-for-byte identical to infoSessionKey in
// src/pkg/hub/hub_keys.go.
const INFO_SESSION_KEY = 'hive-session-v1';
function deriveSessionKey() {
  const explicit = (process.env.HIVE_SESSION_KEY || '').trim();
  if (explicit) return explicit;
  const master = (process.env.HIVE_HUB_SECRET || '').trim();
  if (!master) return '';
  return crypto.createHmac('sha256', master).update(INFO_SESSION_KEY).digest('hex');
}
const SESSION_KEY = deriveSessionKey();

// SESSION_PUBLIC_KEY is the hub's Ed25519 PUBLIC key for session cookies (audit
// N2). The hub holds the private seed and is the only party that can MINT a
// cookie; this spoke can only VERIFY.
//
// SESSION_KEY above is symmetric, so a spoke able to verify was equally able to
// mint — any spoke operator could read it from the pod env and forge
// `clubanderson.<sig>`, a hub ADMIN cookie honoured on ~21 admin routes.
//
// AUDIT F23: that verification lane is now DELETED. SESSION_KEY is no longer a
// signature-verification comparand anywhere in this file; cookies verify ONLY
// against SESSION_PUBLIC_KEY through the asymmetric v3/v2 lanes, so a spoke can
// verify but can no longer mint. SESSION_KEY is retained solely as the IS_HOSTED
// boot signal (see below).
//
// Derived from HIVE_HUB_SECRET as a fallback for self-hosted spokes whose
// operator legitimately holds the master. The info string MUST stay byte-for-byte
// identical to infoSessionEd25519Seed in src/pkg/hub/hub_keys.go.
const INFO_SESSION_ED25519_SEED = 'hive-session-ed25519-v1';
function deriveSessionPublicKey() {
  const explicit = (process.env.HIVE_SESSION_PUBLIC_KEY || '').trim();
  if (explicit) return explicit;
  const master = (process.env.HIVE_HUB_SECRET || '').trim();
  if (!master) return '';
  const seedHex = crypto.createHmac('sha256', master).update(INFO_SESSION_ED25519_SEED).digest('hex');
  try {
    // Node cannot expand a raw Ed25519 seed directly, so wrap it in the PKCS#8
    // prefix for a 32-byte Ed25519 private key and export the public half. This
    // mirrors Go's ed25519.NewKeyFromSeed(seed).Public().
    const pkcs8 = Buffer.concat([
      Buffer.from('302e020100300506032b657004220420', 'hex'),
      Buffer.from(seedHex, 'hex'),
    ]);
    const priv = crypto.createPrivateKey({ key: pkcs8, format: 'der', type: 'pkcs8' });
    const spki = crypto.createPublicKey(priv).export({ format: 'der', type: 'spki' });
    // Strip the 12-byte SPKI header to get the raw 32-byte public key, matching
    // the hex encoding the hub ships in HIVE_SESSION_PUBLIC_KEY.
    return Buffer.from(spki.subarray(spki.length - 32)).toString('hex');
  } catch (_) {
    return '';
  }
}
const SESSION_PUBLIC_KEY = deriveSessionPublicKey();

// ROTATION (src/docs/design/master-key-rotation.md, follow-on #6): during a hub
// master rotation this spoke may hold TWO session public keys.
//
// The hub mints session cookies under the CURRENT generation the instant an
// operator rotates, but the reconcile lane walks the fleet at 3 patches per
// 15-minute cycle — about six hours for 65 spokes. For those hours a spoke that
// knows only one public key cannot verify any cookie the hub is now minting, so
// every hosted terminal session on that spoke fails its cookie check. Holding
// both keys and trying each is what removes that window.
//
// WHY A SECOND VAR RATHER THAN A DELIMITED LIST IN HIVE_SESSION_PUBLIC_KEY.
// Because the same value is read by an independently written Go verifier
// (hub.SpokeSSOPublicKey / VerifySSOToken), and the two languages do NOT agree
// on what "<hex>,<hex>" means:
//
//   Node  Buffer.from(v,'hex')  -> 32 bytes; silently truncates at the comma.
//   Go    hex.DecodeString(v)   -> error; the verifier fails closed.
//
// A list would therefore keep working here by accident and break SSO outright
// on any spoke still running the older Go image — which is the whole fleet for
// the first reconcile cycle. An extra var has a defined meaning to old readers:
// they do not read it, so they cannot misread it.
//
// ABSENT TODAY AND ABSENT UNTIL SOMEONE ROTATES. HIVE_SESSION_PUBLIC_KEY_PREV
// is set on no spoke in the fleet, so SESSION_PUBLIC_KEYS below is a
// one-element array whose single element is SESSION_PUBLIC_KEY, and every
// verification does exactly the one Ed25519 operation it does today. The
// legacy single-value configuration is not a compatibility branch here; it is
// the zero-previous case of the general one.
const SESSION_PUBLIC_KEY_PREV = (process.env.HIVE_SESSION_PUBLIC_KEY_PREV || '').trim();

// isValidEd25519PublicKeyHex rejects anything that is not exactly 32 bytes of
// hex.
//
// The length check cannot be skipped and cannot be done with Buffer alone:
// Buffer.from('zz','hex') returns an EMPTY buffer rather than throwing, and
// Buffer.from('<64 hex>,<64 hex>','hex') returns the FIRST 32 bytes rather than
// throwing — Node truncates silently at the first non-hex character. So a
// malformed value reaches crypto.createPublicKey looking plausible. Testing the
// string against a strict pattern first is what makes "malformed" mean
// malformed here and in Go alike.
function isValidEd25519PublicKeyHex(v) {
  return typeof v === 'string' && /^[0-9a-fA-F]{64}$/.test(v);
}

// sessionPublicKeys resolves the ordered list of public keys to try, current
// first. Malformed and duplicate entries are DROPPED rather than passed through.
//
// Dropping a malformed _PREV is the invariant that makes this safe to ship: a
// bad second key must cost one skipped candidate and must never prevent the
// FIRST key from being tried. Filtering here — rather than letting each verify
// call throw and be swallowed — also keeps the candidate count, and therefore
// the work done, a property of configuration rather than of the cookie.
function sessionPublicKeys() {
  const out = [];
  for (const k of [SESSION_PUBLIC_KEY, SESSION_PUBLIC_KEY_PREV]) {
    const v = (k || '').trim();
    if (!isValidEd25519PublicKeyHex(v)) continue;
    if (out.includes(v)) continue; // before the first rotation both can be equal
    out.push(v);
  }
  return out;
}
const SESSION_PUBLIC_KEYS = sessionPublicKeys();

// Boot-time "am I hub-hosted?" signal. Either the injected session key (modern)
// or a master secret (legacy) proves hosted mode, where identity comes from the
// hub cookie rather than a shared dashboard token.
// N2: either key proves hosted mode. Hosted provisioning no longer injects
// HIVE_SESSION_KEY at all (issue #3234), so a spoke provisioned or reconciled
// after that change holds only the public key — hence checking both.
//
// ROTATION: this deliberately keys off SESSION_PUBLIC_KEY — the PRIMARY key —
// exactly as before, NOT off SESSION_PUBLIC_KEYS. Two reasons, both about not
// moving a boundary this PR has no business moving. First, a spoke is never
// handed a _PREV without a primary, so the plural form could not make this true
// where the singular is false; switching to it would be a change with no effect
// that a later reader would have to re-derive. Second, and decisively, the
// plural form applies validation the singular does not, so keying off it would
// flip a spoke whose HIVE_SESSION_PUBLIC_KEY is malformed from hosted to
// SELF-HOSTED — silently swapping the whole identity model from hub cookies to
// a shared dashboard token. Hosted/self-hosted detection stays byte-identical.
const IS_HOSTED = SESSION_KEY !== '' || SESSION_PUBLIC_KEY !== '';

// Snapshot frame-ancestors allowlist (v2 #3032). Additive to the v4 C2/session
// hardening above: the CSP default stays frame-ancestors 'none' everywhere; only
// the explicit public /snapshot document may be framed by an allowlisted origin.
const SNAPSHOT_FRAME_ANCESTORS_FALLBACK = parseSnapshotFrameAncestors(process.env.HIVE_SNAPSHOT_FRAME_ANCESTORS || '');

// HIVE_ID is this spoke's own hive identity, injected by the hub at provision
// time (mirrors the HIVE_ID env the Go dashboard reads). It is the anchor for
// per-hive terminal authorization: a hub-user cookie is authenticated hub-wide
// (the `hive_hub_user` cookie is scoped to .kubestellar.io — widened from
// .hive.kubestellar.io for sibling-product SSO, hive#4171 — so ANY hive's
// domain receives it), so the signature check alone proves only "some hub user",
// never "a user allowed on THIS hive".
const HIVE_ID = process.env.HIVE_ID || '';

// TERMINAL_ROLES are the roles sufficient to open a shell: owner and read-write.
// A `read`-only grant authenticates and authorizes the DASHBOARD but is NOT
// enough for a terminal — the finding asks role sufficiency (owner/operator) be
// enforced, and a read-only user with shell access would be a privilege
// escalation. Role strings mirror pkg/config (RoleOwner, RoleReadWrite) and the
// hub saasRole* constants.
const TERMINAL_ROLES = new Set(['owner', 'read-write']);

// Declared ABOVE the HIVE_AUTHORIZED_USERS parse below, because
// parseAuthorizedUsernames now consults it (N4 role-aware allowlist). A `const`
// sits in a temporal dead zone until its declaration executes, and that parse
// runs at module load — with this further down, the proxy threw "Cannot access
// 'TERMINAL_ROLES' before initialization" and failed to start at all.

// HIVE_AUTHORIZED_USERS is this hive's per-hive allowlist, injected by the hub
// (authorizedUsersForHive): a comma-separated list of `username` or
// `username:role` entries (owner + everyone with this hive in their SaaS
// Hives map). It is the proxy's INDEPENDENT, per-hive authorization source —
// the one defense that also holds on OpenShift Routes, which have no nginx
// auth-proxy in front of the pod to call /api/saas/auth-check.
//
// SECURITY (CWE-862, finding C3): without a per-hive check, one cookie from any
// free GitHub account that completed hub OAuth opened a shell in EVERY tenant's
// pod. We parse the allowlist into a lowercase username set (GitHub logins are
// case-insensitive) and require the verified cookie user to be a member.
const HIVE_AUTHORIZED_USERS = parseAuthorizedUsernames(process.env.HIVE_AUTHORIZED_USERS || '');

// HIVE_INGRESS_AUTHZ is set ("true") by the hub ONLY on the nginx-ingress lane,
// where an ingress auth-proxy per-hive-authorizes every /terminal request
// (auth-url=auth-check?hive=<id>) BEFORE it reaches this proxy. It is unset on
// the OpenShift-Route lane, where the Route forwards straight to the pod and
// this proxy is the ONLY per-hive gate.
//
// SECURITY (CWE-862, finding C3): this flag is what lets an empty allowlist fail
// CLOSED without breaking legitimate nginx access. With no ingress auth-proxy in
// front (OpenShift), an empty local allowlist can NOT be treated as "defer to
// the ingress" — there is no ingress to defer to — so the terminal must deny.
const HIVE_INGRESS_AUTHZ = (process.env.HIVE_INGRESS_AUTHZ || '') === 'true';



// parseAuthorizedUsernames turns "owner:owner,alice:read,bob" into a Set of
// lowercased usernames. It mirrors the Go parseAuthorizedUsers split (comma
// list, ":role" suffix optional) but keeps only the identity, which is all the
// terminal gate needs — role granularity stays with the dashboard/API layer.
function parseAuthorizedUsernames(raw) {
  const set = new Set();
  for (const entry of raw.split(',')) {
    const parts = entry.split(':');
    const name = parts[0].trim().toLowerCase();
    if (!name) continue;
    // SECURITY (audit N4, CWE-862): the role suffix is NOT decoration — honour it.
    //
    // This used to keep only `parts[0]`, discarding ":read". authorizedUsersForHive
    // emits EVERY user granted the hive, as `owner:owner` plus `name:read` for the
    // rest, so dropping the suffix made a read-only viewer indistinguishable from
    // an owner in this set — and the terminal gate then handed them a shell in a
    // credential-holding container.
    //
    // A missing suffix means "no role stated"; those entries are kept, since the
    // list has historically carried bare names and the caller (isAuthorizedForThisHive)
    // is only reached when there is no authoritative assertion to read a role from.
    const role = (parts[1] || '').trim().toLowerCase();
    if (role && !TERMINAL_ROLES.has(role)) continue;
    set.add(name);
  }
  return set;
}

// isAuthorizedForThisHive reports whether a verified hub username is allowed to
// open a terminal on THIS hive.
//
// Fail-CLOSED policy (finding C3 — no fail-open, even the narrow one):
//   - Populated allowlist (the normal hosted case — authorizedUsersForHive
//     always emits at least the owner): a user absent from it is DENIED.
//   - Empty allowlist (env unset/blank, e.g. an ownerless hive):
//       * nginx lane (HIVE_INGRESS_AUTHZ=true): an ingress auth-proxy already
//         per-hive-authorized this request before it reached the proxy, so the
//         proxy defers (returns true) rather than double-denying legitimate,
//         ingress-approved access.
//       * OpenShift-Route lane (HIVE_INGRESS_AUTHZ unset): there is NO ingress
//         auth-proxy — this proxy is the ONLY per-hive gate — so an empty
//         allowlist DENIES. A wide-open terminal on an ownerless OpenShift hive
//         is exactly the fail-open we refuse to ship.
function isAuthorizedForThisHive(username) {
  if (!username) return false;
  if (HIVE_AUTHORIZED_USERS.size === 0) {
    // No independent per-hive data. Defer to the ingress ONLY when an ingress
    // auth-proxy actually gates this hive; otherwise fail closed.
    return HIVE_INGRESS_AUTHZ;
  }
  return HIVE_AUTHORIZED_USERS.has(username.toLowerCase());
}

// ── Signed terminal assertion (finding C3 follow-up) ──────────────────────────
//
// The static HIVE_AUTHORIZED_USERS allowlist above (from #2756) authorizes by a
// per-hive username list injected at PROVISION time: coarse (no expiry, no role
// granularity, a re-provision needed to change access). The PRIMARY gate is now
// a short-lived HMAC-signed assertion the spoke mints at LOGIN time, binding
// {user, hive_id, role, exp}. This block verifies it, mirroring the Go
// hub.VerifyTerminalAssertion EXACTLY (keep the two in lockstep).
//
// DECOUPLED FROM SSO SIGNING (C2 #2761): the SSO handoff token is now ASYMMETRIC
// (Ed25519 — only the hub mints, the spoke holds the public key). The terminal
// assertion is a DIFFERENT trust shape: SYMMETRIC and SPOKE-LOCAL — the spoke
// mints it (dashboard) and this proxy verifies it, both on the same spoke, with a
// key the spoke holds. So it has its OWN dedicated HMAC key (TERMINAL_SIGNING_KEY
// below), independent of the SSO key. This mirrors the Go hub.terminalSign /
// hub.TerminalSigningKey EXACTLY (keep the two in lockstep).

// TERMINAL_ASSERTION_VERSION must equal the Go terminalAssertionVersion. A token
// carrying any other version (e.g. an SSO handoff token) is rejected, so the two
// token families are never confusable.
const TERMINAL_ASSERTION_VERSION = 'hive-terminal-v1';

// INFO_TERMINAL_KEY is the domain-separation label for the terminal signing
// sub-key. Must equal the Go infoTerminalKey and match the C2 deriveDomainKey
// convention (HMAC-SHA256(master, info) as lowercase hex).
const INFO_TERMINAL_KEY = 'hive-terminal-v1';

// derivePerHiveTerminalKey mirrors the Go derivePerHiveKey(master,
// INFO_TERMINAL_KEY, hiveID): hex HMAC-SHA256 over info || 0x00 || hiveID.
//
// The 0x00 separator is load-bearing and must match Go byte for byte. Plain
// concatenation would be ambiguous — ("hive-terminal-v1", "a|b") and
// ("hive-terminal-v1|a", "b") would hash identically — and hive IDs are
// attacker-influenced.
//
// Returns '' when EITHER input is missing, so a spoke that cannot identify
// itself derives nothing rather than silently sharing a key.
function derivePerHiveTerminalKey(master, hiveID) {
  if (!master || !hiveID) return '';
  return crypto
    .createHmac('sha256', master)
    .update(INFO_TERMINAL_KEY)
    .update(Buffer.from([0]))
    .update(hiveID)
    .digest('hex');
}

// TERMINAL_SIGNING_KEY resolves the symmetric key the proxy verifies terminal
// assertions with, mirroring Go hub.TerminalSigningKey's order EXACTLY.
// EVERY lane is PER-HIVE:
//   1. HIVE_TERMINAL_KEY — the hub-injected per-hive sub-key. Present on every
//      spoke in the fleet today, so this is the lane in force in production.
//   2. Self-derived per-hive key from HIVE_HUB_SECRET + HIVE_ID.
//
// !! AUDIT N3 MUST NOT REGRESS: no lane may resolve to a FLEET-UNIFORM value. !!
//
// Two lanes used to sit here and both are DELETED in lockstep with the Go side:
//   - HIVE_SESSION_KEY, which is byte-identical across all 65 spokes. An
//     assertion minted on one tenant's spoke verified on every other, so any
//     tenant operator could forge a shell grant for an arbitrary user on an
//     arbitrary hive.
//   - deriveTerminalKeyFrom(HIVE_HUB_SECRET), which took no hive ID and so
//     derived ONE key for the whole fleet from a fleet-uniform master.
//
// Empty when none is configured → no assertion ever verifies → the terminal gate
// falls back to the #2756 static allowlist. Computed ONCE at startup like HUB_SECRET.
const TERMINAL_SIGNING_KEY =
  (process.env.HIVE_TERMINAL_KEY || '').trim() ||
  derivePerHiveTerminalKey(
    (process.env.HIVE_HUB_SECRET || '').trim(),
    (process.env.HIVE_ID || '').trim(),
  );

// TERMINAL_ASSERTION_COOKIE is where the spoke deposits the assertion (see
// dashboard setTerminalAssertionCookie). Path=/terminal-scoped, HttpOnly.
const TERMINAL_ASSERTION_COOKIE = 'hive_terminal_assertion';

// TERMINAL_CLOCK_SKEW_S tolerates minor hub/spoke clock drift on the exp / iat
// bounds. Matches the Go ssoClockSkew (30s).
const TERMINAL_CLOCK_SKEW_S = 30;

// verifyTerminalAssertion mirrors Go hub.VerifyTerminalAssertion. It returns
// { username, role } on success, or null on ANY failure (bad signature, wrong
// version, wrong hive, expired/not-yet-valid, malformed) — fail closed, never
// trusting payload bytes before the constant-time signature check passes.
function verifyTerminalAssertion(key, token, expectedHiveID, nowSec) {
  if (!key || !token || !expectedHiveID) return null;
  const idx = token.indexOf('.');
  if (idx <= 0 || idx === token.length - 1) return null;
  const body = token.slice(0, idx);
  const sig = token.slice(idx + 1);
  // Constant-time signature check over the base64url body BEFORE decoding it.
  const expected = crypto.createHmac('sha256', key).update(body).digest('base64url');
  const sigBuf = Buffer.from(sig);
  const expBuf = Buffer.from(expected);
  if (sigBuf.length !== expBuf.length || !crypto.timingSafeEqual(sigBuf, expBuf)) {
    return null;
  }
  let claims;
  try {
    claims = JSON.parse(Buffer.from(body, 'base64url').toString('utf8'));
  } catch {
    return null;
  }
  if (!claims || typeof claims !== 'object') return null;
  if (claims.v !== TERMINAL_ASSERTION_VERSION) return null;
  if (claims.h !== expectedHiveID) return null;
  if (!claims.u) return null;
  const iat = Number(claims.iat);
  const exp = Number(claims.exp);
  if (!Number.isFinite(iat) || !Number.isFinite(exp)) return null;
  if (iat > nowSec + TERMINAL_CLOCK_SKEW_S) return null; // not yet valid
  if (exp < nowSec - TERMINAL_CLOCK_SKEW_S) return null; // expired
  return { username: String(claims.u), role: String(claims.r || '') };
}

// authorizeTerminal is the single per-hive terminal decision, combining the new
// PRIMARY signed-assertion gate with the #2756 static-allowlist FALLBACK.
//
//   PRIMARY  — a valid, unexpired, this-hive assertion whose role is sufficient
//              (owner / read-write) → ALLOW. This is the principled gate: fresh,
//              expiring, role-checked, minted per-user-per-hive at login.
//   FALLBACK — no usable assertion (absent / expired / wrong hive / insufficient
//              role) → defer to isAuthorizedForThisHive(cookieUser): the static
//              per-hive allowlist plus the OpenShift fail-CLOSED behavior from
//              #2756. This is defense-in-depth and MUST NOT regress: a hive with
//              no assertion cookie (older client, direct navigation) still gets
//              exactly the #2756 decision, and an empty allowlist on an
//              OpenShift-Route hive still fails closed.
//
// cookieUser is the hub-wide-authenticated user from the verified hive_hub_user
// cookie (already checked by the caller); assertionCookie is the raw
// hive_terminal_assertion value (may be undefined).
// resolveTerminalIdentity decides WHO is asking, from either credential.
//
// SECURITY (audit F4, final step). Both /terminal gates used to require a valid
// hive_hub_user cookie before authorization ran at all, so the hub-wide cookie
// was load-bearing for every terminal request — and that is precisely the cookie
// that must stop being delivered to sibling tenants (Domain=.hive.kubestellar.io).
// Dropping the Domain while the gates still demanded it would have locked every
// hosted tenant out of /terminal.
//
// A verified terminal assertion is a STRICTLY STRONGER credential than the hub
// cookie for this purpose:
//
//   hub cookie   — proves "some real hub user", hub-wide, delivered to EVERY
//                  sibling tenant, and says nothing about THIS hive.
//   assertion    — signed with THIS spoke's per-hive key, carries {user, role,
//                  hive_id, exp}, is host-only + Path=/terminal, and is checked
//                  against this hive's own ID and a fresh expiry.
//
// So accepting a valid assertion on its own is not a relaxation: it removes a
// weaker requirement that sat in front of a stronger one. The assertion is still
// fully verified (signature, this-hive binding, expiry, role) inside
// authorizeTerminal — this only changes whether the hub cookie must ALSO be
// present.
//
// parseCookies is shared by the HTTP and WebSocket terminal gates so the two
// cannot drift — they are equally exploitable and must stay in lockstep.
function parseCookies(header) {
  return (header || '').split(';').reduce((acc, c) => {
    const [k, ...v] = c.trim().split('=');
    if (k) acc[k] = v.join('=');
    return acc;
  }, {});
}

// Returns the identity to authorize as, or null if neither credential is usable.
// Fails closed: an absent, expired, wrong-hive or badly-signed assertion with no
// valid hub cookie yields null and the caller denies.
function resolveTerminalIdentity(cookies) {
  const hubUser = verifyHubUserCookieAcrossKeys(
    SESSION_PUBLIC_KEYS, SESSION_KEY, cookies['hive_hub_user']);
  if (hubUser) return hubUser;

  const claim = verifyTerminalAssertion(
    TERMINAL_SIGNING_KEY, cookies[TERMINAL_ASSERTION_COOKIE], HIVE_ID,
    Math.floor(Date.now() / 1000));
  // Only the assertion's OWN username may stand in for the hub cookie. Returning
  // it here keeps authorizeTerminal's user-binding check meaningful rather than
  // silently disabling it: the comparison becomes claim.username === itself,
  // which is exactly the "this assertion authenticates this user" statement, and
  // the role and expiry checks there still decide the outcome.
  return claim && claim.username ? claim.username : null;
}

function authorizeTerminal(cookieUser, assertionCookie) {
  const nowSec = Math.floor(Date.now() / 1000);
  const claim = verifyTerminalAssertion(TERMINAL_SIGNING_KEY, assertionCookie, HIVE_ID, nowSec);

  if (claim) {
    // The assertion VERIFIED — a correctly signed, unexpired, this-hive grant.
    // Its role is therefore authoritative, and this function must answer from it
    // alone. It must NOT fall through to the allowlist.
    //
    // SECURITY (audit N4, CWE-862/613): the fallback below used to be reached
    // for every non-grant outcome, including a valid assertion whose role was
    // merely insufficient. That silently upgraded read-only users to a shell,
    // because the allowlist parse (parseAuthorizedUsernames) DISCARDS the
    // ":read" suffix and authorizedUsersForHive puts every granted user in the
    // list — so `carol:read` was indistinguishable from `alice:owner` there.
    //
    // Worse, the allowlist is a provision-time env snapshot: hub-side revocation
    // never reaches a running spoke, so a revoked user kept shell access
    // indefinitely on a stale list. Honouring the assertion's own role — and its
    // expiry — is what makes revocation and downgrade actually take effect.
    const roleOK = TERMINAL_ROLES.has((claim.role || '').toLowerCase());
    const userOK = !!claim.username && !!cookieUser &&
      claim.username.toLowerCase() === cookieUser.toLowerCase();
    // Bind the grant to the SAME user the hub-wide cookie authenticated: an
    // attacker must present BOTH a validly-signed hub cookie for user X AND a
    // validly-signed terminal assertion for user X on THIS hive.
    return roleOK && userOK;
  }

  // No USABLE assertion — absent, expired, wrong hive, or bad signature. There
  // is no authoritative role to read, so fall back to the #2756 static allowlist
  // (itself fail-closed). Note this is deliberately narrower than before: it is
  // now reached only when the assertion tells us NOTHING, never when it tells us
  // "no".
  return isAuthorizedForThisHive(cookieUser);
}

const HOSTED_SUFFIX = '.hive.kubestellar.io';

// isHostedHost decides whether the terminal auth gate applies to a request.
//
// SECURITY (CWE-862 bypass): the raw Host header can carry a :port suffix and a
// trailing FQDN dot, both of which a naive `host.endsWith('.hive.kubestellar.io')`
// would MISS — turning `hive-b.hive.kubestellar.io:443` or `hive-b.hive.kubestellar.io.`
// into a "non-hosted" request that skips the gate entirely and proxies straight
// to ttyd. Normalize first: lowercase, drop the port, strip a single trailing
// dot, THEN suffix-match.
function isHostedHost(rawHost) {
  let host = (rawHost || '').toLowerCase();
  const colon = host.indexOf(':');
  if (colon !== -1) host = host.slice(0, colon);
  if (host.endsWith('.')) host = host.slice(0, -1);
  return host.endsWith(HOSTED_SUFFIX);
}

// SECURITY (fail closed on empty dashboard token — CWE-306).
//
// The prior guard only fired when NODE_ENV === 'production', but NODE_ENV is
// never set in any deploy manifest, so it was dead code: every shipped
// self-hosted deploy ran with unauthenticated mutation endpoints. Reverse the
// polarity — the guard now fires in every real deploy and only the unit test
// (which sets NODE_ENV=test) opts out.
//
// Hosted hives INTENTIONALLY run with HIVE_DASHBOARD_TOKEN unset: identity is
// carried by the hub cookie, so a missing token is expected there and the proxy
// must still boot. A resolved SESSION_KEY (IS_HOSTED) proves hosted mode. Only a
// self-hosted deploy with neither a token nor a session key is truly
// unauthenticated, and that is what we refuse to start.
if (!DASHBOARD_TOKEN && !IS_HOSTED && process.env.NODE_ENV !== 'test') {
  console.error('[SECURITY] HIVE_DASHBOARD_TOKEN is not set and this hive is not hub-hosted (no HIVE_SESSION_KEY / HIVE_HUB_SECRET) — all mutation endpoints would be unauthenticated. Refusing to start. Set HIVE_DASHBOARD_TOKEN.');
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

// AUDIT F23 TOMBSTONE — verifyHubUserCookie (the legacy symmetric HMAC verifier)
// WAS HERE AND IS DELETED. Do not reintroduce it.
//
// It verified `<username>.<base64url(HMAC-SHA256(SESSION_KEY, username))>`.
// SESSION_KEY is symmetric and fleet-uniform, so any party able to VERIFY was
// equally able to MINT. The Go hub deleted its twin in F1 (#3725); this is the
// Node half, and with it the last consumer of SESSION_KEY as a verification
// input.
//
// Only the ASYMMETRIC lanes remain — verifyHubUserCookieV3 (F10, signed expiry)
// and verifyHubUserCookieV2 (Ed25519). A spoke holds only the PUBLIC key, so a
// spoke operator can verify a hub-minted cookie but can no longer forge one.
//
// SESSION_KEY itself is intentionally still resolved above: it remains the
// IS_HOSTED signal and the self-hosted boot check. What died is its use as a
// signature-verification comparand, not the variable.

// HUB_COOKIE_V2_MARKER separates the username from an Ed25519 signature. It must
// stay byte-identical to hubCookieV2Marker in src/pkg/hub/hub_cookie.go. A legacy
// HMAC signature is base64url and so can never contain a '.', which is what makes
// the two formats distinguishable by structure rather than by guessing.
const HUB_COOKIE_V2_MARKER = '.v2.';

// verifyHubUserCookieV2 mirrors verifyHubUserCookieValueV2 in
// src/pkg/hub/hub_cookie.go EXACTLY: value is `<username>.v2.<base64url(sig)>`
// where sig is Ed25519 over the username, verified against the hub's PUBLIC key.
// Returns the verified username, or '' on any failure.
function verifyHubUserCookieV2(pubHex, value) {
  if (!pubHex || !value) return '';
  const idx = value.lastIndexOf(HUB_COOKIE_V2_MARKER);
  if (idx <= 0) return '';
  const username = value.slice(0, idx);
  const sigB64 = value.slice(idx + HUB_COOKIE_V2_MARKER.length);
  if (!username || !sigB64) return '';
  try {
    const raw = Buffer.from(pubHex, 'hex');
    if (raw.length !== 32) return '';
    // Wrap the raw 32-byte key in its SPKI header so Node can import it.
    const spki = Buffer.concat([Buffer.from('302a300506032b6570032100', 'hex'), raw]);
    const pub = crypto.createPublicKey({ key: spki, format: 'der', type: 'spki' });
    const sig = Buffer.from(sigB64, 'base64url');
    return crypto.verify(null, Buffer.from(username), pub, sig) ? username : '';
  } catch (_) {
    return '';
  }
}

// HUB_COOKIE_V3_MARKER separates the signed claims payload from its Ed25519
// signature. Must stay byte-identical to hubCookieV3Marker in
// src/pkg/hub/hub_cookie.go.
const HUB_COOKIE_V3_MARKER = '.v3.';

// HUB_COOKIE_CLOCK_SKEW_SECONDS mirrors ssoClockSkew in src/pkg/hub/sso.go. The
// hub and this spoke are separate clusters with separate clocks; without a
// tolerance a freshly minted cookie is rejected by a spoke running seconds
// ahead of the hub.
const HUB_COOKIE_CLOCK_SKEW_SECONDS = 30;

// verifyHubUserCookieV3 mirrors verifyHubUserCookieValueV3 in
// src/pkg/hub/hub_cookie.go EXACTLY.
//
// AUDIT F10: the v2 cookie signs only the username, so its lifetime lives in the
// browser's MaxAge — which is a hint the browser may ignore and a copied cookie
// never had in the first place. v3 moves the claims inside the signature:
//
//   value = base64url(JSON{u,iat,exp,sid}) + '.v3.' + base64url(Ed25519 sig)
//
// so this proxy can enforce the expiry itself rather than trusting the browser.
//
// The JSON keys (u/iat/exp/sid) and the base64url alphabet are a FROZEN wire
// contract with the Go hub. If the two sides disagree by a single byte, every
// hosted login breaks at deploy — silently, and production-only, because nothing
// but a real hub-minted cookie exercises the disagreement. session_cookie_v3.test.js
// pins Go-minted vectors for exactly this reason.
//
// Revocation is deliberately NOT checked here: the spoke has no revocation store
// and asking the hub would put a network dependency on the terminal path. The
// spoke enforces signature + expiry; the hub additionally enforces revocation.
//
// Returns the verified username, or '' on any failure.
function verifyHubUserCookieV3(pubHex, value, nowSeconds) {
  if (!pubHex || !value) return '';
  const idx = value.lastIndexOf(HUB_COOKIE_V3_MARKER);
  if (idx <= 0) return '';
  const body = value.slice(0, idx);
  const sigB64 = value.slice(idx + HUB_COOKIE_V3_MARKER.length);
  if (!body || !sigB64) return '';
  try {
    const raw = Buffer.from(pubHex, 'hex');
    if (raw.length !== 32) return '';
    // Wrap the raw 32-byte key in its SPKI header so Node can import it.
    const spki = Buffer.concat([Buffer.from('302a300506032b6570032100', 'hex'), raw]);
    const pub = crypto.createPublicKey({ key: spki, format: 'der', type: 'spki' });
    const sig = Buffer.from(sigB64, 'base64url');
    // Verify the signature over the ENCODED body BEFORE parsing any payload
    // byte — never let attacker-controlled JSON reach the parser first.
    if (!crypto.verify(null, Buffer.from(body), pub, sig)) return '';
    const claims = JSON.parse(Buffer.from(body, 'base64url').toString('utf8'));
    if (!claims || typeof claims.u !== 'string' || !claims.u) return '';
    if (typeof claims.sid !== 'string' || !claims.sid) return '';
    if (typeof claims.iat !== 'number' || typeof claims.exp !== 'number') return '';
    const now = Number.isFinite(nowSeconds) ? nowSeconds : Math.floor(Date.now() / 1000);
    if (claims.iat > now + HUB_COOKIE_CLOCK_SKEW_SECONDS) return ''; // not yet valid
    if (claims.exp < now - HUB_COOKIE_CLOCK_SKEW_SECONDS) return ''; // expired
    return claims.u;
  } catch (_) {
    return '';
  }
}

// verifyHubUserCookieEither accepts a v3 (F10: signed lifetime) or a v2
// (Ed25519) cookie — mirroring the Go helper of the same name. Lanes are tried
// v3 → v2. Both are ASYMMETRIC: this spoke holds only the PUBLIC key, so it can
// verify but cannot mint.
//
// AUDIT F23 (Medium): the legacy symmetric HMAC lane is GONE, in lockstep with
// the Go hub's F1 deletion (hub_cookie.go, PR #3725). It verified the cookie
// against SESSION_KEY, which is byte-identical across the whole hosted fleet —
// measured live at 1 distinct value across 48/48 reachable spokes. Symmetric
// material means verify-capable implies MINT-capable, so any spoke operator who
// could read the pod env could forge `clubanderson.<sig>` and have it accepted
// by EVERY other tenant's proxy. That is a genuine cross-tenant forgery
// primitive, not a theoretical one.
//
// legacySecret is retained in the signature but is deliberately unused: callers
// still pass it, and accepting-and-ignoring is safer than a signature change a
// future merge could mis-resolve into re-enabling the lane. That is precisely
// how F14 and F18 regressed on the Go side.
function verifyHubUserCookieEither(pubHex, legacySecret, value) {
  const v3 = verifyHubUserCookieV3(pubHex, value);
  if (v3) return v3;
  const v2 = verifyHubUserCookieV2(pubHex, value);
  if (v2) return v2;
  void legacySecret;
  return '';
}

// verifyHubUserCookieAcrossKeys is BOUNDED TRIAL VERIFICATION of a session
// cookie against every public key this spoke holds, current-then-previous.
//
// This adds a second KEY, not a second FORMAT and not a second LANE. Each
// candidate runs the identical v3/v2 verification the single-key path runs, so
// the lane structure, the expiry enforcement and the signature checks are
// unchanged.
//
// AUDIT F23: the trailing legacy symmetric lane is DELETED (see
// verifyHubUserCookieEither). `legacySecret` is accepted and ignored so callers
// need no change and no merge can silently restore a symmetric fallback here.
//
// BOUNDED BY CONFIGURATION, NOT BY INPUT. `pubKeys` comes from
// sessionPublicKeys(), which is at most two entries because the hub holds at
// most maxLiveGenerations == 2 live generations and provisions at most one
// _PREV var. A cookie cannot lengthen this list, so it cannot be used as an
// asymmetric-cost lever.
//
// A MALFORMED SECOND KEY CANNOT DISABLE THE FIRST. Malformed entries never
// reach here — sessionPublicKeys() drops them — and a candidate that simply
// fails to verify is treated as a failed candidate, never as an error that
// aborts the loop. So a garbage HIVE_SESSION_PUBLIC_KEY_PREV costs nothing but
// a skipped entry.
//
// An empty list verifies NOTHING and returns '' — fail-closed. Before F23 it
// fell through to the symmetric lane, so a spoke with no public key still
// authenticated users; now such a spoke authenticates nobody through this path,
// which is the correct posture for a spoke that holds no verification material.
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

// snapshotFrameAncestors (v2 #3032): resolves the per-hive allowlist for framing
// the public /snapshot document from the Go API, falling back to the env-derived
// list. Only ever consulted for req.path === '/snapshot' below.
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

// ── CSP script-src scoping (#3848 part 1 / #3907, mirrors Go securityHeaders) ──
//
// script-src is split into its CSP3 halves, exactly like the Go spoke server:
//   script-src-elem 'self' cdn + sha256 hashes — inline <script> ELEMENTS,
//     CLOSED: only this proxy's own inline scripts (the SPA document's) can
//     execute; an injected inline <script> matches no hash and is blocked.
//   script-src-attr 'none' — inline on*= handler attributes, CLOSED by the
//     #3848 event-delegation refactor: the SPA and Go-generated HTML use
//     data-action / data-* dispatch, so no handler attribute remains and an
//     injected one never executes.
//   script-src (fallback) is 'self' (+ cdn) for pre-CSP3 browsers —
//     'unsafe-inline' dropped with #3848 — and must never carry the hashes: a
//     hash there makes hash-aware browsers ignore 'unsafe-inline' in the same
//     directive on browsers that know hashes but not -elem/-attr (Firefox < 108).
//
// Hashes are computed from the exact index.html bytes this proxy serves —
// lazily, because the dashboard file can be built after startup (see
// serveIndex). Documents this proxy does NOT render — the Go-rendered
// /contribute, /leaderboard and /snapshot pages, and ttyd's own UI under
// /terminal — keep the blanket CSP2 policy: the Go upstream stamps its own
// per-document hash policy on the pages it renders (http-proxy-middleware
// copies upstream headers over ours), and a hash allowlist computed from the
// WRONG document would blank exactly those pages if that copy ever changed.
function computeInlineScriptHashes(html) {
  const hashes = [];
  const scriptRe = /<script\b([^>]*)>([\s\S]*?)<\/script>/gi;
  for (const m of html.matchAll(scriptRe)) {
    if (/(^|\s)src\s*=/i.test(m[1])) continue; // external: covered by URL sources
    const h = `'sha256-${crypto.createHash('sha256').update(m[2], 'utf8').digest('base64')}'`;
    if (!hashes.includes(h)) hashes.push(h);
  }
  return hashes;
}

let indexScriptHashes = null;
function spaScriptElemHashes() {
  if (indexScriptHashes) return indexScriptHashes;
  if (!indexHtml) {
    try { indexHtml = fs.readFileSync(indexPath, 'utf8'); } catch { return []; }
  }
  indexScriptHashes = computeInlineScriptHashes(indexHtml);
  return indexScriptHashes;
}

// Paths whose HTML this proxy does not render itself; they keep the blanket
// CSP2 script-src (see the comment above).
const UPSTREAM_DOC_PATHS = ['/contribute', '/leaderboard', '/snapshot', '/terminal'];
function isUpstreamDocPath(p) {
  return UPSTREAM_DOC_PATHS.some((base) => p === base || p.startsWith(`${base}/`));
}

app.use(async (req, res, next) => {
  const frameAllowlist = req.path === '/snapshot' ? await snapshotFrameAncestors() : [];
  const snapshotFramingAllowed = frameAllowlist.length > 0;
  const frameAncestors = snapshotFramingAllowed ? frameAllowlist.join(' ') : "'none'";
  const scriptDirectives = isUpstreamDocPath(req.path)
    ? ["script-src 'self' 'unsafe-inline' https://cdn.redoc.ly"]
    : [
        "script-src 'self' https://cdn.redoc.ly",
        `script-src-elem ${["'self'", 'https://cdn.redoc.ly', ...spaScriptElemHashes()].join(' ')}`,
        "script-src-attr 'none'",
      ];
  res.setHeader('Content-Security-Policy', [
    "default-src 'self'",
    ...scriptDirectives,
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
      console.error(`[proxy] ${req.method} ${redactURL(req.url)} → ${err.message}`);
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
  <!-- Pinned to a specific Redoc version with a Subresource Integrity hash so a
       compromise of the CDN or a silent 'latest'-tag update cannot execute
       arbitrary script in the dashboard origin. crossorigin is required for SRI
       to be enforced on a cross-origin script. Ported from v2 (#3297/#3265). -->
  <script src="https://cdn.redoc.ly/redoc/v2.1.5/bundles/redoc.standalone.js"
          integrity="sha384-0GrsyTQc9Oqd8h+b2dbc4XdR2T/DYpy0tLNNstyx+LBMUyiBbcWPbEs9aRmUcaxD"
          crossorigin="anonymous"></script>
</body></html>`);
});

app.use('/api', apiProxy);

// GitHub App setup callback. After the operator installs the App, GitHub
// redirects their browser to /gh-setup?installation_id=...&setup_action=...,
// and the Go API's GET /gh-setup handler verifies the installation with the
// App key and persists installation_id. This route was missing here: the
// request fell through to the SPA fallback below, the dashboard rendered at
// the /gh-setup URL, and the documented setup flow silently did nothing on
// every gateway deployment — the operator had to find and paste the
// installation ID by hand. Proxy it to the Go API exactly like /api.
app.use('/gh-setup', createProxyMiddleware({
  target: GO_API_URL,
  changeOrigin: true,
  // The express mount strips the /gh-setup prefix (same as /api above);
  // re-prepend it, collapsing the bare "/" express leaves before a query
  // string so the Go mux pattern "GET /gh-setup" still matches.
  pathRewrite: (p) => '/gh-setup' + p.replace(/^\/(?=\?|$)/, ''),
  on: {
    error(err, req, res) {
      console.error(`[gh-setup-proxy] ${req.method} ${redactURL(req.url)} → ${err.message}`);
      if (res.writeHead) {
        res.writeHead(502, { 'Content-Type': 'text/plain' });
        res.end('GitHub App setup endpoint unavailable');
      }
    },
  },
}));

function stripSensitiveTerminalQuery(rawUrl) {
  try {
    const u = new URL(rawUrl, 'http://hive.local');
    u.searchParams.delete('token');
    u.searchParams.delete('code');
    return u.pathname + (u.search ? u.search : '');
  } catch {
    return String(rawUrl || '').replace(/([?&])(token|code)=[^&]*/gi, '$1$2=[redacted]');
  }
}

function redactURL(rawUrl) {
  try {
    const u = new URL(rawUrl, 'http://hive.local');
    if (u.searchParams.has('token')) u.searchParams.set('token', '[redacted]');
    if (u.searchParams.has('code')) u.searchParams.set('code', '[redacted]');
    return u.pathname + (u.search ? u.search : '');
  } catch {
    return String(rawUrl || '').replace(/([?&])(token|code)=[^&]*/gi, '$1$2=[redacted]');
  }
}


const terminalGoProxy = createProxyMiddleware({
  target: GO_API_URL,
  changeOrigin: true,
  on: {
    error(err, req, res) {
      console.error(`[terminal-go-proxy] ${req.method} ${redactURL(req.url)} → ${err.message}`);
      if (res.writeHead) {
        res.writeHead(502, { 'Content-Type': 'text/plain' });
        res.end('Terminal handoff endpoint unavailable');
      }
    },
  },
});

const ttydProxy = createProxyMiddleware({
  target: TTYD_URL,
  changeOrigin: true,
  // ttyd (libwebsockets) serves ONE request per TCP connection and hangs up
  // on any reuse. Without an explicit agent, httpxy (the proxy engine) pools
  // upstream sockets in its own keepAlive:true default agents, so the
  // browser's basic-auth flow — an unauthenticated request answered 401,
  // then an authenticated retry — deterministically rode a dead pooled
  // socket and surfaced as 502 "Terminal unavailable" with the CORRECT
  // password. A non-pooling agent gives every request a fresh connection;
  // pooling buys nothing here anyway, because the endpoint's real traffic
  // is a single long-lived websocket.
  agent: new httpBase.Agent({ keepAlive: false }),
  pathRewrite: (p) => stripSensitiveTerminalQuery(p.replace(/^\/terminal/, '') || '/'),
  on: {
    error(err, req, res) {
      console.error(`[ttyd-proxy] ${req.method} ${redactURL(req.url)} → ${err.message}`);
      if (res.writeHead) {
        res.writeHead(502, { 'Content-Type': 'text/plain' });
        res.end('Terminal unavailable');
      }
    },
  },
});
app.use('/terminal', (req, res, next) => {
  if (new URL(req.url, `http://${req.headers.host || 'hive.local'}`).searchParams.has('code')) {
    req.url = '/terminal' + (req.url.startsWith('/') ? req.url : '/' + req.url);
    terminalGoProxy(req, res, next);
    return;
  }
  const host = req.headers.host || '';
  const isHosted = isHostedHost(host);
  if (isHosted || DASHBOARD_TOKEN) {
    const cookies = parseCookies(req.headers.cookie);
    // SECURITY (CWE-345): terminal access must come from a verified hub cookie
    // or a spoke-minted terminal assertion. The shared dashboard token is never
    // accepted from ?token=; URLs are logged by browsers and ingress layers.
    const user = resolveTerminalIdentity(cookies);
    if (!user) {
      if (req.headers.upgrade === 'websocket') {
        req.socket.destroy();
        return;
      }
      res.status(401).send('Terminal access requires authentication; upgrade the dashboard client if it generated a ?token= terminal URL');
      return;
    }
    // SECURITY (CWE-862, finding C3 + follow-up): PRIMARY gate: a short-lived
    // signed {user,hive,role,exp} assertion for THIS hive with a sufficient
    // role. FALLBACK: hosted hub-cookie users go through the #2756 static
    // allowlist + OpenShift fail-closed path. Token-secured terminal opens are
    // prepared by /api/terminal/handoff, which mints the assertion cookie before
    // the browser navigates here.
    if (!authorizeTerminal(user, cookies[TERMINAL_ASSERTION_COOKIE])) {
      console.warn(`[terminal] 403: user ${JSON.stringify(user)} not authorized for hive ${JSON.stringify(HIVE_ID)}`);
      if (req.headers.upgrade === 'websocket') {
        req.socket.destroy();
        return;
      }
      res.status(403).send('Not authorized for this hive');
      return;
    }
  }
  req.url = stripSensitiveTerminalQuery(req.url);
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
  // SECURITY: never inject the dashboard token into the served HTML. Doing so
  // handed the only API credential to every visitor who could reach this port,
  // reducing "authentication" to "can you open the page". The frontend now
  // obtains the token only from an operator who pastes it (stored in
  // localStorage), so the served page carries no secret.
  res.sendFile(indexPath);
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
    if (new URL(req.url, `http://${req.headers.host || 'hive.local'}`).searchParams.has('code')) {
      terminalGoProxy.upgrade(req, socket, head);
      return;
    }
    const host = req.headers.host || '';
    const isHosted = isHostedHost(host);
    if (isHosted) {
      const cookies = parseCookies(req.headers.cookie);
      // SECURITY (CWE-345): verify the hub's HMAC signature, not mere existence.
      const wsUser = resolveTerminalIdentity(cookies);
      if (!wsUser) {
        socket.destroy();
        return;
      }
      // SECURITY (CWE-862, finding C3 + follow-up): per-hive authorization on the
      // WS upgrade, mirroring the HTTP gate. PRIMARY: signed {user,hive,role,exp}
      // assertion for THIS hive; FALLBACK: #2756 static allowlist + fail-closed.
      // A hub-authenticated user without a usable grant for THIS hive gets the
      // socket closed, not a shell.
      if (!authorizeTerminal(wsUser, cookies[TERMINAL_ASSERTION_COOKIE])) {
        console.warn(`[terminal-ws] 403: hub user ${JSON.stringify(wsUser)} not authorized for hive ${JSON.stringify(HIVE_ID)}`);
        socket.destroy();
        return;
      }
    } else if (DASHBOARD_TOKEN) {
      const cookies = parseCookies(req.headers.cookie);
      const user = resolveTerminalIdentity(cookies);
      if (!user || !authorizeTerminal(user, cookies[TERMINAL_ASSERTION_COOKIE])) {
        socket.write('HTTP/1.1 401 Unauthorized\r\n\r\n');
        socket.destroy();
        return;
      }
    }
    req.url = stripSensitiveTerminalQuery(req.url);
    ttydProxy.upgrade(req, socket, head);
  }
});
