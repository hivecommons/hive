package hub

import (
	"testing"
	"time"
)

// F10 on v2 — VERIFIER ONLY. These pin the v3 lane's behaviour so a later mint
// flip has something to flip against, and so the lane cannot silently rot.
//
// Nothing in production mints v3 on this branch; TestF10_V2_MintingHasNotFlipped
// is the tripwire that makes any future change to that deliberate and visible.

func testSeedHex(t *testing.T) (seed, pub string) {
	t.Helper()
	s := &HubServer{hubSecret: testHubSecret}
	return s.sessionSigningSeed(), s.sessionPublicKey()
}

func TestF10_V2_V3CookieVerifiesWithSignedLifetime(t *testing.T) {
	seed, pub := testSeedHex(t)
	now := time.Now()
	value, sid := mintHubUserCookieValueV3(seed, "clubanderson", now, time.Hour)
	if value == "" || sid == "" {
		t.Fatal("mint returned empty — the v3 lane is not usable")
	}
	got, ok := verifyHubUserCookieValueV3(pub, value, now, nil)
	if !ok || got != "clubanderson" {
		t.Fatalf("valid v3 cookie failed to verify: got=%q ok=%v", got, ok)
	}
}

// The whole point of F10: a captured cookie must stop working.
func TestF10_V2_ExpiredV3CookieIsRejected(t *testing.T) {
	seed, pub := testSeedHex(t)
	issued := time.Now().Add(-2 * time.Hour)
	value, _ := mintHubUserCookieValueV3(seed, "clubanderson", issued, time.Hour)
	if _, ok := verifyHubUserCookieValueV3(pub, value, time.Now(), nil); ok {
		t.Fatal("F10: an EXPIRED v3 cookie still verified — the signed lifetime is not enforced")
	}
}

// The other half: logout must be able to kill a session server-side.
func TestF10_V2_RevokedSessionIsRejected(t *testing.T) {
	seed, pub := testSeedHex(t)
	now := time.Now()
	value, sid := mintHubUserCookieValueV3(seed, "clubanderson", now, time.Hour)
	revoked := func(s string) bool { return s == sid }
	if _, ok := verifyHubUserCookieValueV3(pub, value, now, revoked); ok {
		t.Fatal("F10: a REVOKED session id still verified — logout cannot invalidate a cookie")
	}
	// Control: a different sid must not be collaterally revoked.
	other := func(s string) bool { return s == "some-other-sid" }
	if _, ok := verifyHubUserCookieValueV3(pub, value, now, other); !ok {
		t.Fatal("revoking one session invalidated an unrelated one")
	}
}

func TestF10_V2_TamperedV3PayloadIsRejected(t *testing.T) {
	seed, pub := testSeedHex(t)
	now := time.Now()
	value, _ := mintHubUserCookieValueV3(seed, "alice", now, time.Hour)
	// Flip a byte in the signed body.
	b := []byte(value)
	b[0] ^= 0xFF
	if _, ok := verifyHubUserCookieValueV3(pub, string(b), now, nil); ok {
		t.Fatal("F10: a tampered v3 payload verified")
	}
}

// verifyHubUserDual must PREFER v3 and must NOT have broken the older lanes —
// they are the F1/F2 compatibility paths and removing them 401s the fleet.
func TestF10_V2_DualStillAcceptsV2AndLegacy(t *testing.T) {
	s := &HubServer{hubSecret: testHubSecret}

	v3, _ := mintHubUserCookieValueV3(s.sessionSigningSeed(), "clubanderson", time.Now(), time.Hour)
	if u, ok := s.verifyHubUserDual(v3); !ok || u != "clubanderson" {
		t.Fatalf("v3 cookie not accepted by verifyHubUserDual: %q %v", u, ok)
	}
	v2 := mintHubUserCookieValueV2(s.sessionSigningSeed(), "clubanderson")
	if u, ok := s.verifyHubUserDual(v2); !ok || u != "clubanderson" {
		t.Fatalf("REGRESSION: the v2 lane stopped verifying: %q %v", u, ok)
	}
	legacy := mintHubUserCookieValue(s.sessionCookieKey(), "clubanderson")
	if u, ok := s.verifyHubUserDual(legacy); !ok || u != "clubanderson" {
		t.Fatalf("REGRESSION: the legacy HMAC lane stopped verifying: %q %v", u, ok)
	}
}

// TRIPWIRE. Nothing may mint v3 until every verifier in the fleet — including
// each spoke's Node proxy — accepts it. If this fails, someone flipped minting;
// that is incident #2773's shape and must be a deliberate, reviewed change.
func TestF10_V2_MintingHasNotFlipped(t *testing.T) {
	s := &HubServer{hubSecret: testHubSecret}
	minted := mintHubUserCookieValueV2(s.sessionSigningSeed(), "clubanderson")
	if hubCookieIsV3(minted) {
		t.Fatal("F10: production minting has flipped to v3 — every spoke proxy must verify v3 FIRST")
	}
}
