package hub

// Dead-code relocation (see hub_cookie.go for the live verify chain).
//
// Every function in this file is unreachable from every binary under ./cmd/...
// (`deadcode ./cmd/...`), but is load-bearing for the pkg/hub test suite —
// most importantly hub_cookie_f1_legacy_lane_test.go, which uses the legacy
// HMAC mint machinery as a FORGERY PRIMITIVE to prove that
// verifyHubUserCookieEitherAt rejects legacy cookies (AUDIT F1). Moving the
// functions into a _test.go file keeps those regression guards byte-identical
// while removing the forgery-capable code from every shipped binary.
//
// Nothing here may be re-wired into production. In particular, the legacy
// symmetric lane (mintHubUserCookieValue / hubCookieSign /
// verifyHubUserCookieValue) is the exact machinery F1 flagged: HIVE_SESSION_KEY
// is provisioned byte-identically into every spoke, so anyone holding it could
// mint an admin cookie. It exists below only so tests can construct such a
// forgery and assert it is refused.

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
//
// Production mints only v3 (oauth.go mintSessionCookies); tests use this to
// prove the v2 verify lane still accepts rollout-window cookies.
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

// mintHubUserCookieValue returns the legacy symmetric-HMAC cookie value for
// username. The verify side of this lane was deleted (AUDIT F1); tests use this
// mint to forge legacy cookies and assert they are rejected.
func mintHubUserCookieValue(secret, username string) string {
	if secret == "" || username == "" {
		return ""
	}
	return username + "." + hubCookieSign(secret, username)
}

// verifyHubUserCookieValue parses a legacy cookie value, recomputes its HMAC
// over the carried username, and constant-time-compares it against the
// presented signature. Retained for tests that characterize the legacy format.
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

// verifyHubUserCookieEither is the original 3-argument wrapper over
// verifyHubUserCookieEitherAt (implicit clock, no revocation lookup). All
// production call sites use verifyHubUserCookieEitherAt directly; the wrapper
// survives here for the tests written against its original shape.
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
