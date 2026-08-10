package hub

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
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

// hubCookieB64 encodes the signature URL-safe and unpadded so the cookie value
// stays within the RFC 6265 cookie-octet set.
var hubCookieB64 = base64.RawURLEncoding

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

// hubUserCookieV2Name is the SECOND cookie the hub emits, carrying the
// Ed25519-signed value. It is a distinct NAME rather than a second format in
// the same cookie because each verifier signs over the whole prefix — no single
// concatenated value satisfies both the HMAC and Ed25519 verifiers (checked
// both orderings; both fail both). A spoke that does not know this name simply
// ignores it, which is what makes the mixed fleet work.
const hubUserCookieV2Name = "hive_hub_user_v2"

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
func verifyHubUserCookieEither(pubHex, legacySecret, value string) (string, bool) {
	if u, ok := verifyHubUserCookieValueV2(pubHex, value); ok {
		return u, true
	}
	return verifyHubUserCookieValue(legacySecret, value)
}

// hubCookieIsV2 reports whether a cookie value carries the asymmetric format.
// Rollout telemetry only — never an authorization decision on its own.
func hubCookieIsV2(value string) bool {
	return strings.Contains(value, hubCookieV2Marker)
}
