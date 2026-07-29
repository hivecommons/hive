package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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
