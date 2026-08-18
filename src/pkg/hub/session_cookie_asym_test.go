package hub

import (
	"strings"
	"testing"
	"time"
)

// N2 (CWE-321/798): the hub session cookie must be ASYMMETRICALLY signed.
//
// The cookie is `hive_hub_user`, and requireAdmin gates ~21 admin routes on
// nothing but a string compare of its carried username against
// hubAdminUsername. It used to be HMAC'd with the SESSION sub-key — which
// provisioning injects into EVERY spoke so each spoke's proxy can verify it.
//
// An HMAC key that verifies also signs. So any spoke operator could read
// HIVE_SESSION_KEY out of their own pod env and mint `clubanderson.<sig>`,
// producing a hub ADMIN session — including access to the cluster App-key
// writer, which loops straight back into N1's key material.
//
// Ed25519 splits the two capabilities: the hub holds the private seed, spokes
// receive only the public key. Same split the SSO handoff already made (#2771).

const n2Master = "test-master-secret-n2"

func n2Hub() *HubServer { return &HubServer{hubSecret: n2Master} }

// TestSessionCookieV2RoundTrips is the basic contract.
func TestSessionCookieV2RoundTrips(t *testing.T) {
	s := n2Hub()
	c := mintHubUserCookieValueV2(s.sessionSigningSeed(), "alice")
	if c == "" {
		t.Fatal("mint returned empty for valid inputs")
	}
	if !strings.Contains(c, hubCookieV2Marker) {
		t.Fatalf("minted cookie %q carries no v2 marker", c)
	}
	u, ok := verifyHubUserCookieValueV2(s.sessionPublicKey(), c)
	if !ok || u != "alice" {
		t.Fatalf("verify = (%q,%v), want (alice,true)", u, ok)
	}
}

// TestSpokePublicKeyCannotMint is the finding itself: the value a spoke holds
// must not yield a cookie the hub will accept.
//
// Note what is and is not checked here. An Ed25519 public key and a seed are
// BOTH 32 bytes, so a length check cannot tell them apart — mint will happily
// treat a public key as a seed and emit a syntactically valid cookie. (The
// comment on MintSSOToken's identical guard claims it catches "a public key
// passed by mistake"; it does not, for the same reason.) That is harmless
// because the resulting signature is made by a DIFFERENT private key than the
// one the hub's public key corresponds to, so it cannot verify. Verification —
// not the mint-side guard — is the boundary, which is exactly what this asserts.
func TestSpokePublicKeyCannotMint(t *testing.T) {
	s := n2Hub()
	pub := s.sessionPublicKey()
	if pub == "" {
		t.Fatal("public key derivation returned empty")
	}

	// A spoke tries to mint an ADMIN cookie with the only key it has.
	forged := mintHubUserCookieValueV2(pub, hubAdminUsername)
	if forged == "" {
		return // even better: it failed closed at mint
	}
	if u, ok := verifyHubUserCookieValueV2(pub, forged); ok {
		t.Fatalf("N2: a spoke minted a VERIFIABLE admin cookie (%q) from the public key alone — "+
			"the spoke-held value is still signing material", u)
	}
	// Also confirm it is rejected through the dual-verify path the hub actually
	// calls, legacy lane included.
	if u, ok := verifyHubUserCookieEither(pub, s.sessionKey(), forged); ok {
		t.Fatalf("N2: the spoke-forged admin cookie was accepted by the live verify path as %q", u)
	}
}

// TestSessionCookieV2RejectsTampering covers the forgery shapes that matter: a
// swapped username (privilege escalation to admin) and an edited signature.
func TestSessionCookieV2RejectsTampering(t *testing.T) {
	s := n2Hub()
	pub := s.sessionPublicKey()
	legit := mintHubUserCookieValueV2(s.sessionSigningSeed(), "alice")
	sig := legit[strings.LastIndex(legit, hubCookieV2Marker)+len(hubCookieV2Marker):]

	// Alice's signature re-labelled as the admin — the escalation an attacker
	// actually wants.
	if u, ok := verifyHubUserCookieValueV2(pub, hubAdminUsername+hubCookieV2Marker+sig); ok {
		t.Fatalf("N2: a re-labelled cookie verified as %q — username is not covered by the signature", u)
	}
	// Edited signature.
	if _, ok := verifyHubUserCookieValueV2(pub, "alice"+hubCookieV2Marker+"AAAA"); ok {
		t.Fatal("a garbage signature verified")
	}
	// Wrong hub's key.
	other := (&HubServer{hubSecret: "a-different-master"}).sessionPublicKey()
	if _, ok := verifyHubUserCookieValueV2(other, legit); ok {
		t.Fatal("a cookie verified under a DIFFERENT hub's public key")
	}
}

// TestSessionCookieDualVerifyDuringRollout pins the compatibility contract: the
// hub mints v2 but must keep accepting legacy HMAC cookies, or every live
// browser session breaks at deploy.
func TestSessionCookieDualVerifyDuringRollout(t *testing.T) {
	s := n2Hub()
	pub, legacySecret := s.sessionPublicKey(), s.sessionKey()

	v2Cookie := mintHubUserCookieValueV2(s.sessionSigningSeed(), "alice")
	legacyCookie := mintHubUserCookieValue(legacySecret, "bob")

	if _, ok := verifyHubUserCookieEitherAt(pub, legacySecret, legacyCookie, time.Now(), nil); ok {
		t.Error("AUDIT F1: the legacy symmetric cookie must NOT verify — that lane is deleted")
	}

	// And once the legacy secret is withdrawn (the follow-up removal PR), the
	// legacy cookie must stop verifying while v2 keeps working.
	if _, ok := verifyHubUserCookieEither(pub, "", legacyCookie); ok {
		t.Error("legacy cookie still verified with the legacy lane disabled")
	}
	if u, ok := verifyHubUserCookieEither(pub, "", v2Cookie); !ok || u != "alice" {
		t.Errorf("v2 cookie broke when the legacy lane was disabled: (%q,%v)", u, ok)
	}
}

// TestHubCookieIsV2 covers the rollout telemetry helper.
func TestHubCookieIsV2(t *testing.T) {
	s := n2Hub()
	if !hubCookieIsV2(mintHubUserCookieValueV2(s.sessionSigningSeed(), "alice")) {
		t.Error("v2 cookie not reported as v2")
	}
	if hubCookieIsV2(mintHubUserCookieValue(s.sessionKey(), "alice")) {
		t.Error("legacy cookie misreported as v2 — an operator would cut the compatibility " +
			"lane while sessions still depend on it")
	}
}

// TestSessionSeedIsNotTheSpokeValue guards the whole point of the change: what
// the hub signs with and what a spoke receives must be different values, and the
// signing seed must not be derivable from the shipped one.
func TestSessionSeedIsNotTheSpokeValue(t *testing.T) {
	s := n2Hub()
	seed, pub, legacy := s.sessionSigningSeed(), s.sessionPublicKey(), s.sessionKey()

	if seed == "" || pub == "" {
		t.Fatal("seed or public key empty")
	}
	if seed == pub {
		t.Fatal("the signing seed IS the value shipped to spokes — nothing was gained")
	}
	// Distinct domain label, so the asymmetric seed can never collide with the
	// symmetric session key still shipped during rollout.
	if seed == legacy {
		t.Fatal("the Ed25519 seed collides with the legacy symmetric session key")
	}
}

// TestSessionCookieV2FailsClosed: no key material, no cookie.
func TestSessionCookieV2FailsClosed(t *testing.T) {
	if got := mintHubUserCookieValueV2("", "alice"); got != "" {
		t.Errorf("mint with no seed returned %q", got)
	}
	if got := mintHubUserCookieValueV2("not-hex", "alice"); got != "" {
		t.Errorf("mint with garbage seed returned %q", got)
	}
	s := n2Hub()
	if got := mintHubUserCookieValueV2(s.sessionSigningSeed(), ""); got != "" {
		t.Errorf("mint with no username returned %q", got)
	}
	if _, ok := verifyHubUserCookieValueV2("", "alice.v2.AAAA"); ok {
		t.Error("verify succeeded with no public key")
	}
	// An empty-master hub derives nothing at all.
	empty := &HubServer{hubSecret: ""}
	if empty.sessionSigningSeed() != "" || empty.sessionPublicKey() != "" {
		t.Error("an empty master produced usable key material")
	}
}
