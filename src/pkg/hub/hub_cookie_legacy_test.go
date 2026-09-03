// Package hub: LEGACY session-cookie machinery, retained for tests only.
//
// These functions are the dead legacy/superseded cookie lanes (#5812): the
// symmetric-HMAC mint/verify pair F1 flagged as a forgery primitive, the v2
// mint superseded by v3 generation-aware minting, the 3-arg
// verifyHubUserCookieEither wrapper, and the hubCookieIsV2/V3 telemetry
// helpers. deadcode reported all seven unreachable from every binary under
// ./cmd/..., but the F1/legacy-lane regression tests still need them to FORGE
// legacy and v2 cookies and prove verification rejects them. Living in a
// _test.go file, they compile into test binaries only and are excluded from
// every shipped binary — do not move them back into hub_cookie.go.

package hub

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

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
