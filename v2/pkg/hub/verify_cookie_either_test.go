package hub

// Tests for issue #3784: verifyHubUserCookieEither — the dual-format verifier
// that accepts both Ed25519 (v2) and legacy HMAC cookies during the N2 cutover.
// This function had zero direct test coverage.

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestVerifyCookieEitherAcceptsV2(t *testing.T) {
	// Generate a fresh Ed25519 keypair.
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	seedHex := hex.EncodeToString(seed)
	pubHex := hex.EncodeToString(pub)

	cookie := mintHubUserCookieValueV2(seedHex, "alice")
	if cookie == "" {
		t.Fatal("mintHubUserCookieValueV2 returned empty")
	}

	got, ok := verifyHubUserCookieEither(pubHex, "some-legacy-secret", cookie)
	if !ok {
		t.Fatal("verifyHubUserCookieEither rejected a valid v2 cookie")
	}
	if got != "alice" {
		t.Errorf("username = %q, want %q", got, "alice")
	}
}

func TestVerifyCookieEitherAcceptsLegacy(t *testing.T) {
	secret := "test-legacy-secret"
	cookie := mintHubUserCookieValue(secret, "bob")
	if cookie == "" {
		t.Fatal("mintHubUserCookieValue returned empty")
	}

	// Use an invalid pubHex so v2 verification fails; legacy should succeed.
	got, ok := verifyHubUserCookieEither("0000", secret, cookie)
	if !ok {
		t.Fatal("verifyHubUserCookieEither rejected a valid legacy HMAC cookie")
	}
	if got != "bob" {
		t.Errorf("username = %q, want %q", got, "bob")
	}
}

func TestVerifyCookieEitherRejectsBoth(t *testing.T) {
	_, ok := verifyHubUserCookieEither("0000", "wrong-secret", "garbage.value")
	if ok {
		t.Error("verifyHubUserCookieEither accepted an invalid cookie")
	}
}

func TestVerifyCookieEitherPrefersV2(t *testing.T) {
	// Mint a v2 cookie; pass a legacy secret that could also verify an HMAC
	// cookie for the same username — the function should return via the v2 path.
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 42)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	seedHex := hex.EncodeToString(seed)
	pubHex := hex.EncodeToString(pub)

	v2Cookie := mintHubUserCookieValueV2(seedHex, "charlie")
	got, ok := verifyHubUserCookieEither(pubHex, "any-secret", v2Cookie)
	if !ok || got != "charlie" {
		t.Errorf("expected v2 path to verify, got (%q, %v)", got, ok)
	}
}

func TestVerifyCookieEitherLegacyDisabled(t *testing.T) {
	// When legacySecret is empty, the legacy lane must be disabled.
	secret := "real-secret"
	cookie := mintHubUserCookieValue(secret, "dave")

	// With empty legacySecret and no valid v2, should reject.
	_, ok := verifyHubUserCookieEither("0000", "", cookie)
	if ok {
		t.Error("expected rejection when legacy is disabled (empty secret)")
	}
}

func TestVerifyCookieEitherEmptyValue(t *testing.T) {
	_, ok := verifyHubUserCookieEither("anything", "anything", "")
	if ok {
		t.Error("empty cookie value should be rejected")
	}
}

func TestHubCookieIsV2(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 7)
	}
	seedHex := hex.EncodeToString(seed)

	v2 := mintHubUserCookieValueV2(seedHex, "eve")
	if !hubCookieIsV2(v2) {
		t.Error("hubCookieIsV2 should return true for a v2 cookie")
	}

	// Legacy HMAC cookie should not match.
	legacy := mintHubUserCookieValue("secret", "eve")
	if hubCookieIsV2(legacy) {
		t.Error("hubCookieIsV2 should return false for a legacy HMAC cookie")
	}
}

func TestVerifyCookieEitherForgedV2(t *testing.T) {
	// A forged v2 cookie with a different key must fail.
	seed1 := make([]byte, ed25519.SeedSize)
	seed2 := make([]byte, ed25519.SeedSize)
	for i := range seed1 {
		seed1[i] = byte(i + 1)
		seed2[i] = byte(i + 99)
	}
	priv2 := ed25519.NewKeyFromSeed(seed2)
	pub2 := priv2.Public().(ed25519.PublicKey)
	pub2Hex := hex.EncodeToString(pub2)

	// Mint with seed1, verify with pub2 — should fail v2.
	seedHex1 := hex.EncodeToString(seed1)
	forged := mintHubUserCookieValueV2(seedHex1, "mallory")

	_, ok := verifyHubUserCookieEither(pub2Hex, "", forged)
	if ok {
		t.Error("forged v2 cookie with wrong key should be rejected")
	}
}

func TestVerifyCookieEitherCrossFormatNoCollision(t *testing.T) {
	// Verify that an HMAC cookie never accidentally passes v2 verification.
	_ = hmac.New(sha256.New, []byte("key"))
	legacy := mintHubUserCookieValue("test-key", "frank")

	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 3)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	pubHex := hex.EncodeToString(pub)

	// v2 verify alone should fail for a legacy cookie.
	if _, ok := verifyHubUserCookieValueV2(pubHex, legacy); ok {
		t.Error("legacy HMAC cookie must never pass Ed25519 verification")
	}

	// But verifyEither with a valid legacy secret should still accept it.
	got, ok := verifyHubUserCookieEither(pubHex, "test-key", legacy)
	if !ok || got != "frank" {
		t.Errorf("verifyEither should accept via legacy path, got (%q, %v)", got, ok)
	}
}
