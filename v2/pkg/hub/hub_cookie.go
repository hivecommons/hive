package hub

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
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

// mintHubUserCookieValueV2 mints an Ed25519-signed session cookie. seedHex is
// the hub's PRIVATE session seed; a spoke cannot produce this value.
//
// Returns "" on empty inputs or seed material that is not a valid 32-byte
// Ed25519 seed (e.g. a public key passed by mistake) — fail closed rather than
// sign with junk, matching MintSSOToken.
func mintHubUserCookieValueV2(seedHex, username string) string {
	if seedHex == "" || username == "" {
		return ""
	}
	seed, err := hex.DecodeString(strings.TrimSpace(seedHex))
	if err != nil || len(seed) != ed25519.SeedSize {
		return ""
	}
	priv := ed25519.NewKeyFromSeed(seed)
	sig := hubCookieB64.EncodeToString(ed25519.Sign(priv, []byte(username)))
	return username + hubCookieV2Marker + sig
}

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
// are SHORT and FROZEN: the Node proxy (v2/proxy/server.js) parses these exact
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
// rollout note on verifyHubUserCookieEither.
//
// Returns "" on empty inputs, a bad seed, a non-positive ttl, or a CSPRNG
// failure — fail closed rather than sign junk, matching mintHubUserCookieValueV2.
func mintHubUserCookieValueV3(seedHex, username string, now time.Time, ttl time.Duration) (value, sid string) {
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
	payload, err := json.Marshal(hubCookieClaims{
		U:   username,
		IAT: now.Unix(),
		EXP: now.Add(ttl).Unix(),
		SID: sid,
	})
	if err != nil {
		return "", ""
	}
	body := hubCookieB64.EncodeToString(payload)
	priv := ed25519.NewKeyFromSeed(seed)
	sig := hubCookieB64.EncodeToString(ed25519.Sign(priv, []byte(body)))
	return body + hubCookieV3Marker + sig, sid
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

// mintHubUserCookieValue returns the signed, tamper-evident cookie value for
// username. It returns "" when no secret is configured or the username is empty
// — callers must treat that as "cannot establish a session" rather than emitting
// an unsigned (trusted-by-default) cookie.
func mintHubUserCookieValue(secret, username string) string {
	if secret == "" || username == "" {
		return ""
	}
	return username + "." + hubCookieSign(secret, username)
}

// verifyHubUserCookieValue parses a cookie value, recomputes its HMAC over the
// carried username, and constant-time-compares it against the presented
// signature. It returns (username, true) only when the signature verifies; on a
// legacy unsigned cookie, a forged value, or a missing secret it returns
// ("", false) so the caller treats the request as unauthenticated.
func verifyHubUserCookieValue(secret, value string) (string, bool) {
	if secret == "" || value == "" {
		return "", false
	}
	// SplitN from the right: usernames are validated to exclude ".", but be
	// defensive and treat the final segment as the signature regardless.
	idx := strings.LastIndex(value, ".")
	if idx <= 0 || idx == len(value)-1 {
		return "", false
	}
	username, sig := value[:idx], value[idx+1:]
	expected := hubCookieSign(secret, username)
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return "", false
	}
	return username, true
}

// hubCookieSign returns the URL-safe base64 HMAC-SHA256 of username under
// secret.
func hubCookieSign(secret, username string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(username))
	return hubCookieB64.EncodeToString(mac.Sum(nil))
}

// verifyHubUserCookieEither accepts a v2 (Ed25519) cookie OR a legacy HMAC one,
// so live sessions survive the N2 cutover.
//
// Rollout shape, mirroring the SSO migration: the hub MINTS only v2 while
// verifying both. Browsers holding a legacy cookie keep working until it is
// re-minted at their next login; spokes running an older image keep verifying
// the legacy format until they re-provision.
//
// The legacy lane is the vulnerability — anyone holding the symmetric key can
// forge it — so it is explicitly TEMPORARY. It must be removed once the fleet
// has rolled and existing cookies have aged out (the cookie's own MaxAge bounds
// that window). hubCookieIsV2 gives an operator the signal for when that is
// safe.
//
// legacySecret may be "" to disable the legacy lane entirely, which is what the
// removal PR will pass.
// F10 adds a THIRD lane, tried first: v3 → v2 → legacy HMAC. Strictly additive.
// The v2 and legacy lanes are the N2/F1/F2 compatibility paths — removing either
// one 401s the part of the fleet that has not rolled, so neither goes away here.
//
// verifyHubUserCookieEither keeps its original 3-argument shape so no existing
// call site changes. It cannot consult the revocation store (it has no server
// handle), so it passes nil: a v3 cookie is still checked for signature and for
// signed expiry, just not for revocation. Call sites that CAN revoke use
// verifyHubUserCookieEitherAt via HubServer.verifyHubUserCookie.
func verifyHubUserCookieEither(pubHex, legacySecret, value string) (string, bool) {
	return verifyHubUserCookieEitherAt(pubHex, legacySecret, value, time.Now(), nil)
}

// verifyHubUserCookieEitherAt is verifyHubUserCookieEither with the verifier's
// clock and revocation lookup made explicit, so expiry and revocation are
// testable without touching global state.
func verifyHubUserCookieEitherAt(pubHex, legacySecret, value string, now time.Time, revoked hubSessionRevokedFunc) (string, bool) {
	if u, ok := verifyHubUserCookieValueV3(pubHex, value, now, revoked); ok {
		return u, true
	}
	if u, ok := verifyHubUserCookieValueV2(pubHex, value); ok {
		return u, true
	}
	return verifyHubUserCookieValue(legacySecret, value)
}

// hubCookieIsV3 reports whether a cookie value carries the F10 format. Rollout
// telemetry only — never an authorization decision on its own.
func hubCookieIsV3(value string) bool {
	return strings.Contains(value, hubCookieV3Marker)
}

// hubCookieIsV2 reports whether a cookie value carries the asymmetric format.
// Rollout telemetry only — never an authorization decision on its own.
func hubCookieIsV2(value string) bool {
	return strings.Contains(value, hubCookieV2Marker)
}
