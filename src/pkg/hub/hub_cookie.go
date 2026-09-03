package hub

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// Hub session cookie integrity.
//
// The `hive_hub_user` cookie carries the logged-in GitHub username that the hub
// trusts for browser-originated API calls. Historically it stored the raw
// username, which meant anyone could hand-craft the cookie and impersonate any
// user — including the hub admin. To make the cookie tamper-evident it is now
// bound to the hub secret with an HMAC:
//
//	value = <username>.<base64url(HMAC-SHA256(username, hubSecret))>
//
// The username stays in the clear (it is not a secret and callers already know
// their own login); the appended signature is what proves the hub itself minted
// the value. Verification recomputes the HMAC and compares in constant time, so
// a forged or edited cookie fails closed.
//
// Legacy transition: existing sessions carry an UNSIGNED cookie (no "." + sig).
// Those fail verification and are treated as logged out, so the user simply
// re-authenticates through the normal login flow, which re-mints the new signed
// cookie. No stored data changes and nobody is permanently locked out.

// N2 (CWE-321/798): the session cookie is now ASYMMETRICALLY signed.
//
// The HMAC scheme below is verify-capable-implies-mint-capable, and the same key
// is provisioned into EVERY spoke so each spoke's proxy can check
// `hive_hub_user`. A spoke operator could therefore read the key out of their pod
// env and mint `clubanderson.<sig>` — a hub ADMIN cookie, accepted on ~21 admin
// routes including the cluster App-key writer. That is the whole of N2, and it is
// unfixable while the signer and the verifier hold the same bytes.
//
// The v2 format is Ed25519: the hub holds the private seed, spokes receive only
// the public key. Same split the SSO handoff already made (C2 follow-up, #2771).
//
//	v2 value = <username>.v2.<base64url(Ed25519-Sign(privateKey, username))>
//
// The ".v2." marker is what lets one verifier accept both formats during the
// rollout without ambiguity — a legacy HMAC signature can never contain a "."
// (base64url has no dot), so the two shapes are distinguishable by structure
// rather than by guessing.
const hubCookieV2Marker = ".v2."

// hubCookieB64 encodes the signature URL-safe and unpadded so the cookie value
// stays within the RFC 6265 cookie-octet set.
var hubCookieB64 = base64.RawURLEncoding

// verifyHubUserCookieValueV2 verifies an Ed25519-signed cookie against the hub's
// PUBLIC session key. Returns (username, true) only on a valid signature.
func verifyHubUserCookieValueV2(pubHex, value string) (string, bool) {
	if pubHex == "" || value == "" {
		return "", false
	}
	idx := strings.LastIndex(value, hubCookieV2Marker)
	if idx <= 0 {
		return "", false
	}
	username := value[:idx]
	sigB64 := value[idx+len(hubCookieV2Marker):]
	if username == "" || sigB64 == "" {
		return "", false
	}
	pub, err := hex.DecodeString(strings.TrimSpace(pubHex))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return "", false
	}
	sig, err := hubCookieB64.DecodeString(sigB64)
	if err != nil {
		return "", false
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), []byte(username), sig) {
		return "", false
	}
	return username, true
}

// AUDIT F10 (session lifetime is neither signed nor revocable).
//
// The v2 signature above covers ONLY the username. That leaves two holes:
//
//  1. The cookie's lifetime lives entirely in the browser's `MaxAge`. MaxAge is
//     a hint to the browser, not a server-side claim — a copied cookie value is
//     accepted forever, because nothing in the signed bytes says when it was
//     issued or when it stops being valid.
//  2. Logout only deletes the browser's copy. Anyone who captured the value
//     keeps a working session across logout, password change, or account
//     removal, for as long as the signing key stays accepted.
//
// v3 closes both by moving the claims INSIDE the signature:
//
//	v3 value = <base64url(JSON{u,iat,exp,sid})>.v3.<base64url(Ed25519-Sign(payload))>
//
// The signature covers the encoded payload bytes, so issued-at, expiry, and the
// session ID are all tamper-evident. Expiry is then enforced by the VERIFIER
// (hub and spoke alike) rather than trusted to the browser, and `sid` gives
// logout something concrete to revoke — see hub_session_revocation.go.
//
// Unlike v2, the username is no longer in the clear at the head of the value;
// it is a field of the signed payload. That is why v3 needs its own marker: the
// two shapes are not parseable by the same code, and the marker is what lets a
// single verifier route the value to the right parser without guessing.
const hubCookieV3Marker = ".v3."

// hubCookieClaims are the signed contents of a v3 session cookie. Field names
// are SHORT and FROZEN: the Node proxy (src/proxy/server.js) parses these exact
// JSON keys, and a rename on one side alone silently 401s every hosted login at
// deploy time. Do not "tidy" them.
type hubCookieClaims struct {
	// U is the GitHub login this session authenticates as.
	U string `json:"u"`
	// IAT is the issued-at time, Unix seconds.
	IAT int64 `json:"iat"`
	// EXP is the hard expiry, Unix seconds. Enforced server-side, unlike the
	// browser's MaxAge, so a copied cookie dies on schedule.
	EXP int64 `json:"exp"`
	// SID is the unguessable per-session ID that revocation is keyed on. It is
	// what makes logout mean something for a value that has already left the
	// browser.
	SID string `json:"sid"`
	// G names the master GENERATION whose derived seed signed this cookie
	// (hub_generations.go). It is a SELECTION HINT, not a claim of authority:
	// it lets the verifier check one generation instead of trying each, and it
	// lets telemetry answer "is anything still using the old key?" without
	// reading the code. It names a key; it is not a key, so carrying it in the
	// clear costs nothing.
	//
	// `omitempty` is load-bearing in BOTH directions:
	//
	//   - Omitted on the wire when zero, so a hub that has never rotated mints
	//     byte-identical cookies to the ones it mints today. Deploying this is
	//     therefore not observable at the cookie level.
	//   - Absent on every cookie already sitting in a browser, which is all of
	//     them. Verification treats a zero G as UNMARKED and falls back to
	//     trying each acceptable generation, so no live session is logged out.
	//
	// The Node proxy (src/proxy/server.js verifyHubUserCookieV3) parses u/iat/
	// exp/sid and ignores unrecognized fields, so adding this one is additive
	// and needs no spoke roll — which is the whole reason the marker lives in
	// the payload rather than as a "g<N>." prefix on the cookie value. A prefix
	// would sit in front of the base64url body and break the proxy's parse for
	// every hosted tenant, which is the flag day this mechanism exists to
	// avoid.
	G int `json:"g,omitempty"`
}

// hubCookieSessionIDBytes is the entropy behind a session ID. Matches
// oauthStateNonceBytes: a revocation key that can be guessed is a revocation
// key that can be exhausted by an attacker probing which sessions are live.
const hubCookieSessionIDBytes = 32

// newHubCookieSessionID mints an unguessable session ID, or "" if the system
// CSPRNG fails — callers must fail closed rather than mint a predictable SID.
func newHubCookieSessionID() string {
	buf := make([]byte, hubCookieSessionIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

// mintHubUserCookieValueV3 mints an Ed25519-signed session cookie carrying a
// signed lifetime and session ID. seedHex is the hub's PRIVATE session seed.
//
// NOTHING CALLS THIS IN PRODUCTION YET, deliberately. The cookie is verified by
// the hub (Go) AND independently by every spoke's Node proxy; minting a shape a
// not-yet-rolled spoke cannot parse breaks /terminal fleet-wide. That is
// incident #2773. Minting flips only once the whole fleet verifies v3 — see the
// rollout note on verifyHubUserCookieEitherAt.
//
// Returns "" on empty inputs, a bad seed, a non-positive ttl, or a CSPRNG
// failure — fail closed rather than sign junk, matching the older Ed25519 cookie minter.
func mintHubUserCookieValueV3(seedHex, username string, now time.Time, ttl time.Duration) (value, sid string) {
	// gen 0 => the G claim is omitted, so this mints exactly the bytes it
	// minted before generations existed.
	return mintHubUserCookieValueV3Gen(seedHex, username, now, ttl, 0)
}

// mintHubUserCookieValueV3Gen is mintHubUserCookieValueV3 with the master
// GENERATION that produced seedHex stamped into the signed claims.
//
// gen <= 0 omits the marker entirely and produces byte-identical output to the
// pre-generations mint, which is what makes the unmarked-legacy path testable
// against the real minter rather than against a hand-built fixture.
//
// The marker is INSIDE the signature, so it cannot be edited to steer generation
// selection: a value whose G was altered fails the Ed25519 check before the
// payload is ever parsed. That is a stronger property than the impersonation
// cookie's "g<N>." prefix has (the prefix is unauthenticated, which is why
// verifyWithGenerations treats a bad prefix as a hint that can only make
// verification cheaper, never make it fail).
func mintHubUserCookieValueV3Gen(seedHex, username string, now time.Time, ttl time.Duration, gen int) (value, sid string) {
	if seedHex == "" || username == "" || ttl <= 0 {
		return "", ""
	}
	seed, err := hex.DecodeString(strings.TrimSpace(seedHex))
	if err != nil || len(seed) != ed25519.SeedSize {
		return "", ""
	}
	sid = newHubCookieSessionID()
	if sid == "" {
		return "", ""
	}
	claims := hubCookieClaims{
		U:   username,
		IAT: now.Unix(),
		EXP: now.Add(ttl).Unix(),
		SID: sid,
	}
	if gen > 0 {
		claims.G = gen
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", ""
	}
	body := hubCookieB64.EncodeToString(payload)
	priv := ed25519.NewKeyFromSeed(seed)
	sig := hubCookieB64.EncodeToString(ed25519.Sign(priv, []byte(body)))
	return body + hubCookieV3Marker + sig, sid
}

// hubCookieClaimedGeneration reads the G marker off a v3 cookie WITHOUT
// verifying its signature, returning 0 for any other shape or an unmarked
// cookie.
//
// Reading an unauthenticated field is safe here for the same reason
// hubCookieSessionID's identical shortcut is: the result is used only to pick
// WHICH generation to verify against, and every candidate then has to pass a
// real Ed25519 check. The worst a forged G achieves is pointing the verifier at
// a key that will reject it — and the verifier falls back to trying every
// acceptable generation in that case, so a forged marker cannot even deny
// service to a legitimate cookie.
func hubCookieClaimedGeneration(value string) int {
	idx := strings.LastIndex(value, hubCookieV3Marker)
	if idx <= 0 {
		return 0
	}
	raw, err := hubCookieB64.DecodeString(value[:idx])
	if err != nil {
		return 0
	}
	var claims hubCookieClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return 0
	}
	return claims.G
}

// hubSessionRevokedFunc reports whether a session ID has been revoked. Passed in
// rather than reached for so the cookie layer stays a pure function of its
// inputs and the tests can drive revocation directly. A nil func means "nothing
// is revoked" — which is the correct behaviour for the spoke-side verifier,
// which has no revocation store of its own.
type hubSessionRevokedFunc func(sid string) bool

// verifyHubUserCookieValueV3 verifies a v3 cookie against the hub's PUBLIC
// session key AND enforces its signed lifetime and revocation state. Returns
// (username, true) only when the signature verifies, the clock window is
// satisfied, and the session ID has not been revoked.
//
// Order matters: the signature is checked BEFORE any payload byte is trusted,
// exactly as VerifyTerminalAssertion does. Parsing attacker-controlled JSON
// before authenticating it is how payload parsers become the attack surface.
func verifyHubUserCookieValueV3(pubHex, value string, now time.Time, revoked hubSessionRevokedFunc) (string, bool) {
	if pubHex == "" || value == "" {
		return "", false
	}
	idx := strings.LastIndex(value, hubCookieV3Marker)
	if idx <= 0 {
		return "", false
	}
	body := value[:idx]
	sigB64 := value[idx+len(hubCookieV3Marker):]
	if body == "" || sigB64 == "" {
		return "", false
	}
	pub, err := hex.DecodeString(strings.TrimSpace(pubHex))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return "", false
	}
	sig, err := hubCookieB64.DecodeString(sigB64)
	if err != nil {
		return "", false
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), []byte(body), sig) {
		return "", false
	}
	raw, err := hubCookieB64.DecodeString(body)
	if err != nil {
		return "", false
	}
	var claims hubCookieClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", false
	}
	if claims.U == "" || claims.SID == "" {
		return "", false
	}
	// Clock-skew tolerance mirrors VerifyTerminalAssertion: hub and spoke are
	// separate clusters and their clocks drift, so a strict comparison would
	// reject freshly minted cookies on a spoke running seconds ahead.
	nowUnix := now.Unix()
	skew := int64(ssoClockSkew / time.Second)
	if claims.IAT > nowUnix+skew {
		return "", false // not yet valid
	}
	if claims.EXP < nowUnix-skew {
		return "", false // expired
	}
	if revoked != nil && revoked(claims.SID) {
		return "", false
	}
	return claims.U, true
}

// hubCookieSessionID returns the session ID carried by a v3 cookie, or "" for
// any other shape. Used by logout to learn WHICH session to revoke.
//
// This deliberately does NOT verify the signature: it is only ever called on a
// value that a verifier already accepted, and its result is used solely to add
// an entry to the revocation set. The worst an unverified caller could achieve
// is revoking a session ID it already knows — which is not an escalation.
func hubCookieSessionID(value string) string {
	idx := strings.LastIndex(value, hubCookieV3Marker)
	if idx <= 0 {
		return ""
	}
	raw, err := hubCookieB64.DecodeString(value[:idx])
	if err != nil {
		return ""
	}
	var claims hubCookieClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return ""
	}
	return claims.SID
}

// hubCookieExpiry returns the signed expiry (Unix seconds) carried by a v3
// cookie, or 0 for any other shape. Like hubCookieSessionID this reads the
// payload without re-verifying; it bounds how long a revocation entry is kept,
// and an attacker who inflated it would only be lengthening the revocation of
// their OWN session.
func hubCookieExpiry(value string) int64 {
	idx := strings.LastIndex(value, hubCookieV3Marker)
	if idx <= 0 {
		return 0
	}
	raw, err := hubCookieB64.DecodeString(value[:idx])
	if err != nil {
		return 0
	}
	var claims hubCookieClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return 0
	}
	return claims.EXP
}

// verifyHubUserCookieEitherAt is the hub session-cookie verifier with
// explicit clock and revocation lookup, so expiry and revocation are
// testable without touching global state.
func verifyHubUserCookieEitherAt(pubHex, legacySecret, value string, now time.Time, revoked hubSessionRevokedFunc) (string, bool) {
	if u, ok := verifyHubUserCookieValueV3(pubHex, value, now, revoked); ok {
		return u, true
	}
	if u, ok := verifyHubUserCookieValueV2(pubHex, value); ok {
		return u, true
	}
	// AUDIT F1 (Critical, open across four audits): the legacy symmetric lane is
	// GONE. It verified the cookie against HIVE_SESSION_KEY, which provisioning
	// injects byte-identically into every spoke — so any spoke operator could mint
	// "username.HMAC(username)" and be accepted as a hub admin on ~21 admin routes.
	//
	// It survived four audits only because cutting it would have broken v2 spokes
	// mid-rollout. The whole fleet now runs v4 and production mints V2/Ed25519
	// (oauth.go mintSessionCookies), so the compatibility argument is void and the
	// lane is deleted rather than deprecated. legacySecret is retained in the
	// signature but is deliberately unused: callers still pass it, and accepting
	// and ignoring it is safer than a signature change that a merge could
	// mis-resolve into re-enabling the lane.
	_ = legacySecret
	return "", false
}
