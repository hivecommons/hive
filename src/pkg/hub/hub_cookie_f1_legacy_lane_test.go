package hub

import (
	"testing"
	"time"
)

// Audit F1 (Critical, open across four audits). The hub session cookie used to
// fall back to a symmetric HMAC lane keyed on HIVE_SESSION_KEY. Provisioning
// injects that key byte-identically into EVERY spoke, so any spoke operator
// could mint "username.HMAC(username)" and be accepted as a hub admin on ~21
// admin routes. The lane is now deleted.
//
// These tests pin the deletion. The forgery case is the regression; the V2/V3
// cases are the positive control — without them, a verifier that rejected
// everything would "pass" while breaking all sign-in.

const f1TestLegacySecret = "spoke-held-symmetric-session-key"

// TestF1LegacyHMACCookieIsRejected is the regression: a cookie minted with the
// fleet-shared symmetric key must NOT authenticate, even when the verifier is
// handed that exact key as legacySecret.
func TestF1LegacyHMACCookieIsRejected(t *testing.T) {
	forged := mintHubUserCookieValue(f1TestLegacySecret, "clubanderson")
	if forged == "" {
		t.Fatal("precondition: could not mint a legacy cookie to forge with")
	}

	seed := deriveDomainKey("test-master-for-f1", infoSessionEd25519Seed)
	pub := ssoPublicKeyFromSeed(seed)

	if user, ok := verifyHubUserCookieEitherAt(pub, f1TestLegacySecret, forged, time.Now(), nil); ok {
		t.Fatalf("legacy symmetric cookie was ACCEPTED as %q — any spoke holding "+
			"HIVE_SESSION_KEY can forge hub admin (audit F1)", user)
	}
}

// TestF1LegacyLaneRejectsEvenTheCorrectSecret removes the last doubt: it is the
// LANE that is gone, not merely a key mismatch. Verifying with the same secret
// the cookie was minted under must still fail.
func TestF1LegacyLaneRejectsEvenTheCorrectSecret(t *testing.T) {
	value := mintHubUserCookieValue(f1TestLegacySecret, "alice")
	if _, ok := verifyHubUserCookieValue(f1TestLegacySecret, value); !ok {
		t.Fatal("precondition: the legacy primitive itself should still verify its own output")
	}
	if _, ok := verifyHubUserCookieEitherAt("", f1TestLegacySecret, value, time.Now(), nil); ok {
		t.Error("the legacy lane is still reachable through verifyHubUserCookieEitherAt")
	}
}

// TestF1V2CookieStillVerifies is the positive control for the asymmetric lane
// production actually mints (oauth.go mintSessionCookies).
func TestF1V2CookieStillVerifies(t *testing.T) {
	seed := deriveDomainKey("test-master-for-f1", infoSessionEd25519Seed)
	pub := ssoPublicKeyFromSeed(seed)

	value := mintHubUserCookieValueV2(seed, "alice")
	if value == "" {
		t.Fatal("precondition: could not mint a V2 cookie")
	}
	user, ok := verifyHubUserCookieEitherAt(pub, f1TestLegacySecret, value, time.Now(), nil)
	if !ok {
		t.Fatal("a V2 (Ed25519) cookie must still verify — this is what production mints")
	}
	if user != "alice" {
		t.Errorf("V2 cookie verified as %q, want alice", user)
	}
}

// TestF1V3CookieStillVerifies is the same control for the revocable V3 format,
// so deleting the legacy lane cannot be mistaken for "cookies are broken".
func TestF1V3CookieStillVerifies(t *testing.T) {
	seed := deriveDomainKey("test-master-for-f1", infoSessionEd25519Seed)
	pub := ssoPublicKeyFromSeed(seed)
	now := time.Now()

	value, sid := mintHubUserCookieValueV3(seed, "bob", now, time.Hour)
	if value == "" || sid == "" {
		t.Fatal("precondition: could not mint a V3 cookie")
	}
	user, ok := verifyHubUserCookieEitherAt(pub, f1TestLegacySecret, value, now, nil)
	if !ok {
		t.Fatal("a V3 cookie must still verify")
	}
	if user != "bob" {
		t.Errorf("V3 cookie verified as %q, want bob", user)
	}
}
