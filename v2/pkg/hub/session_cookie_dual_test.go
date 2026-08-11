package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

// N2 (CWE-321/798) on v2, with DUAL-FORMAT minting.
//
// The hub cannot simply switch to Ed25519 here: a v2 spoke's proxy verifies
// HMAC and only HMAC (see F3/#3243), so an Ed25519-only cookie would 401 the
// terminal on every v2 spoke in the fleet. It also cannot stay HMAC-only,
// because that key is symmetric AND provisioned into every spoke — any spoke
// operator can mint `clubanderson.<sig>` and obtain hub admin.
//
// So the hub emits BOTH: `hive_hub_user` (HMAC, universally verifiable) and
// `hive_hub_user_v2` (Ed25519, unforgeable by a spoke). Verifiers prefer the
// asymmetric one; a spoke that does not know the name ignores it.

const n2v2Master = "test-master-n2-v2"

func n2v2Hub() *HubServer { return &HubServer{hubSecret: n2v2Master} }

// v2SpokeVerify mirrors the v2 proxy's verifier EXACTLY (HMAC over the
// username, base64url-unpadded) so this test fails if the hub ever mints
// something that proxy cannot read.
func v2SpokeVerify(sessionKey, value string) string {
	if sessionKey == "" || value == "" {
		return ""
	}
	idx := -1
	for i := len(value) - 1; i >= 0; i-- {
		if value[i] == '.' {
			idx = i
			break
		}
	}
	if idx <= 0 || idx == len(value)-1 {
		return ""
	}
	user, sig := value[:idx], value[idx+1:]
	mac := hmac.New(sha256.New, []byte(sessionKey))
	mac.Write([]byte(user))
	if base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) != sig {
		return ""
	}
	return user
}

// TestDualMint_V2SpokeCanVerifyTheHMACCookie is the compatibility guarantee.
// If this breaks, every v2 spoke's terminal 401s.
func TestDualMint_V2SpokeCanVerifyTheHMACCookie(t *testing.T) {
	s := n2v2Hub()
	hmacCookie := mintHubUserCookieValue(s.sessionCookieKey(), "alice")
	if got := v2SpokeVerify(s.sessionCookieKey(), hmacCookie); got != "alice" {
		t.Fatalf("a v2 spoke could not verify the hub's HMAC cookie (got %q) — "+
			"every v2 spoke's terminal would 401", got)
	}
}

// TestDualMint_Ed25519CookieIsUnforgeableByASpoke is the finding itself.
func TestDualMint_Ed25519CookieIsUnforgeableByASpoke(t *testing.T) {
	s := n2v2Hub()
	pub := s.sessionPublicKey()
	if pub == "" {
		t.Fatal("public key derivation returned empty")
	}
	// A spoke holding only the public key tries to mint an admin cookie.
	forged := mintHubUserCookieValueV2(pub, hubAdminUsername)
	if forged != "" {
		if u, ok := verifyHubUserCookieValueV2(pub, forged); ok {
			t.Fatalf("N2: a spoke forged a VERIFIABLE admin cookie (%q) from the public key", u)
		}
	}
	// The hub's own mint verifies.
	real := mintHubUserCookieValueV2(s.sessionSigningSeed(), "alice")
	if u, ok := verifyHubUserCookieValueV2(pub, real); !ok || u != "alice" {
		t.Fatalf("hub-minted Ed25519 cookie failed to verify: %q %v", u, ok)
	}
}

// TestDualMint_HubPrefersAsymmetric: when both cookies are present the
// asymmetric one must win, or the stronger credential is decorative.
func TestDualMint_HubPrefersAsymmetric(t *testing.T) {
	s := n2v2Hub()
	ed := mintHubUserCookieValueV2(s.sessionSigningSeed(), "alice")
	if u, ok := s.verifyHubUserDual(ed); !ok || u != "alice" {
		t.Fatalf("verifyHubUserDual rejected the Ed25519 value: %q %v", u, ok)
	}
	// And the legacy paths still work during rollout.
	if u, ok := s.verifyHubUserDual(mintHubUserCookieValue(s.sessionCookieKey(), "bob")); !ok || u != "bob" {
		t.Error("derived-key HMAC cookie rejected — existing sessions would break")
	}
	if u, ok := s.verifyHubUserDual(mintHubUserCookieValue(n2v2Master, "carol")); !ok || u != "carol" {
		t.Error("raw-master HMAC cookie rejected — v2 spokes mint against this")
	}
}

// TestDualMint_FormatsAreDistinguishable guards the reason for two cookie NAMES
// rather than one hybrid value: each verifier signs over the whole prefix, so no
// single concatenation satisfies both.
func TestDualMint_FormatsAreDistinguishable(t *testing.T) {
	s := n2v2Hub()
	hmacCookie := mintHubUserCookieValue(s.sessionCookieKey(), "alice")
	ed := mintHubUserCookieValueV2(s.sessionSigningSeed(), "alice")

	if hubCookieIsV2(hmacCookie) {
		t.Error("an HMAC cookie was misidentified as v2")
	}
	if !hubCookieIsV2(ed) {
		t.Error("an Ed25519 cookie was not identified as v2")
	}
	// The v2 spoke's HMAC verifier must REJECT the Ed25519 value rather than
	// mis-parsing a username out of it.
	if got := v2SpokeVerify(s.sessionCookieKey(), ed); got != "" {
		t.Errorf("a v2 spoke extracted %q from an Ed25519 cookie — it must reject it outright", got)
	}
}
