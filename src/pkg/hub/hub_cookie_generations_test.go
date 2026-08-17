package hub

import (
	"testing"
	"time"
)

// Dual-generation acceptance for the HUB SESSION COOKIE — follow-on PR #1 of
// the master-key rotation design.
//
// The session cookie is the artifact that makes rotation visible to users: it
// is the longest-lived thing bound to a generation, so without dual acceptance
// a rotation logs every browser out at the instant it happens. These tests pin
// the four properties the design demands — accepted under current, accepted
// under an unexpired previous, REJECTED under an expired previous, and unmarked
// legacy cookies still accepted — each with a positive control so that a
// verifier which rejected everything could not pass.

// sessionSeedFor is the derived Ed25519 session seed for a raw master, i.e.
// exactly what the pre-generations code signed with.
func sessionSeedFor(master string) string {
	return deriveDomainKey(master, infoSessionEd25519Seed)
}

// TestSessionCookieSurvivesRotation is THE test this change exists for. A
// cookie minted before a rotation must keep working for its natural lifetime
// rather than dying at the instant the master changes.
func TestSessionCookieSurvivesRotation(t *testing.T) {
	now := time.Now()
	before := legacyGenerationSet(genSecretA)

	cookie, sid := mintHubUserCookieValueV3ForGeneration(before, "alice", now, time.Hour)
	if cookie == "" || sid == "" {
		t.Fatal("mint returned empty for valid inputs")
	}

	after := before.rotate(genSecretB, now, defaultVerifyWindow)
	if after == nil {
		t.Fatal("rotate returned nil")
	}

	user, gen, ok := verifyHubUserCookieAcrossGenerations(after, cookie, now.Add(time.Minute), nil)
	if !ok {
		t.Fatal("a session minted before rotation must still verify during the window")
	}
	if user != "alice" {
		t.Errorf("verified user = %q, want alice", user)
	}
	if gen != legacyGenerationID {
		t.Errorf("accepting generation = %d, want %d (the outgoing one)", gen, legacyGenerationID)
	}

	// POSITIVE CONTROL. The same cookie against a hub that knows ONLY the new
	// generation must FAIL. Without this, an implementation that accepted any
	// cookie regardless of key would pass the assertion above.
	onlyNew := legacyGenerationSet(genSecretB)
	if u, _, ok := verifyHubUserCookieAcrossGenerations(onlyNew, cookie, now.Add(time.Minute), nil); ok {
		t.Errorf("positive control: a pre-rotation cookie verified as %q against the new generation alone", u)
	}
}

// TestSessionCookieAcceptedUnderCurrentGeneration is the base case: a freshly
// minted cookie verifies, under the CURRENT generation, and carries that
// generation's marker inside its signed claims.
func TestSessionCookieAcceptedUnderCurrentGeneration(t *testing.T) {
	now := time.Now()
	rotated := legacyGenerationSet(genSecretA).rotate(genSecretB, now, defaultVerifyWindow)
	cur, _ := rotated.currentGeneration()

	cookie, _ := mintHubUserCookieValueV3ForGeneration(rotated, "bob", now, time.Hour)
	if cookie == "" {
		t.Fatal("mint returned empty")
	}

	if got := hubCookieClaimedGeneration(cookie); got != cur.ID {
		t.Errorf("claimed generation = %d, want current %d", got, cur.ID)
	}

	user, gen, ok := verifyHubUserCookieAcrossGenerations(rotated, cookie, now, nil)
	if !ok || user != "bob" || gen != cur.ID {
		t.Errorf("fresh cookie: user=%q gen=%d ok=%v; want bob/%d/true", user, gen, ok, cur.ID)
	}

	// POSITIVE CONTROL for the mint half of the contract: minting is
	// current-ONLY, so this must NOT verify under the outgoing key alone. If it
	// did, the rotation would never actually take effect.
	if _, _, ok := verifyHubUserCookieAcrossGenerations(legacyGenerationSet(genSecretA), cookie, now, nil); ok {
		t.Error("positive control: a post-rotation cookie must not verify under the outgoing key")
	}
}

// TestSessionCookieRejectedAfterGenerationExpires pins FINITENESS — the single
// property that keeps dual acceptance from rotting into the permanent compat
// lane F1/F2 took five audits to remove. Past verify_until the old key stops
// being accepted with no operator action.
func TestSessionCookieRejectedAfterGenerationExpires(t *testing.T) {
	now := time.Now()
	before := legacyGenerationSet(genSecretA)
	after := before.rotate(genSecretB, now, defaultVerifyWindow)

	// Mint under the OUTGOING generation at a moment past its verify window, so
	// the cookie's own signed expiry is not what rejects it — the generation
	// expiry is.
	late := now.Add(defaultVerifyWindow + time.Minute)
	lateCookie, _ := mintHubUserCookieValueV3Gen(
		sessionSeedFor(genSecretA), "dave", late, time.Hour, legacyGenerationID)
	if lateCookie == "" {
		t.Fatal("could not mint the late cookie")
	}

	if u, _, ok := verifyHubUserCookieAcrossGenerations(after, lateCookie, late, nil); ok {
		t.Errorf("a cookie under an EXPIRED generation was accepted as %q", u)
	}

	// POSITIVE CONTROL. The same construction verified INSIDE the window is
	// accepted, proving the rejection above is the generation expiry and not a
	// broken verifier or a malformed cookie.
	early := now.Add(time.Minute)
	earlyCookie, _ := mintHubUserCookieValueV3Gen(
		sessionSeedFor(genSecretA), "dave", early, time.Hour, legacyGenerationID)
	if _, _, ok := verifyHubUserCookieAcrossGenerations(after, earlyCookie, early, nil); !ok {
		t.Error("positive control: the same cookie inside the window must be accepted")
	}
}

// TestSessionCookieAcceptsUnmarkedLegacyV3 pins that deploying this is a
// NON-EVENT for cookies already in browsers. Every session cookie live today is
// v3 with NO `g` claim, and it must verify against the current generation
// rather than be rejected for lacking a field that did not exist when it was
// minted.
func TestSessionCookieAcceptsUnmarkedLegacyV3(t *testing.T) {
	now := time.Now()
	gs := legacyGenerationSet(genSecretA)

	// Mint EXACTLY the way production did before this change: the plain
	// three-argument minter, no generation involved.
	legacy, sid := mintHubUserCookieValueV3(sessionSeedFor(genSecretA), "carol", now, time.Hour)
	if legacy == "" || sid == "" {
		t.Fatal("legacy mint returned empty")
	}
	if got := hubCookieClaimedGeneration(legacy); got != 0 {
		t.Fatalf("the legacy mint carried generation %d — the test would not be testing the unmarked path", got)
	}

	user, gen, ok := verifyHubUserCookieAcrossGenerations(gs, legacy, now, nil)
	if !ok {
		t.Fatal("an UNMARKED pre-rotation session cookie must still verify")
	}
	if user != "carol" {
		t.Errorf("verified user = %q, want carol", user)
	}
	if gen != legacyGenerationID {
		t.Errorf("accepting generation = %d, want %d", gen, legacyGenerationID)
	}

	// And it survives a rotation too — the unmarked cookies in browsers at the
	// moment an operator rotates are precisely the population at risk.
	after := gs.rotate(genSecretB, now, defaultVerifyWindow)
	if u, g2, ok := verifyHubUserCookieAcrossGenerations(after, legacy, now.Add(time.Minute), nil); !ok || u != "carol" {
		t.Errorf("unmarked cookie across rotation: user=%q gen=%d ok=%v; want carol/true", u, g2, ok)
	}
}

// TestSessionCookieAcceptsUnmarkedLegacyV2 covers the OTHER live shape. A v2
// cookie has no payload to put a marker in, so it can only ever be verified by
// bounded trial across generations — this pins that the trial lane exists and
// is itself bounded by the generation expiry.
func TestSessionCookieAcceptsUnmarkedLegacyV2(t *testing.T) {
	now := time.Now()
	gs := legacyGenerationSet(genSecretA)

	v2 := mintHubUserCookieValueV2(sessionSeedFor(genSecretA), "erin")
	if v2 == "" {
		t.Fatal("v2 mint returned empty")
	}
	if !hubCookieIsV2(v2) {
		t.Fatal("precondition: minted value is not v2")
	}

	if u, gen, ok := verifyHubUserCookieAcrossGenerations(gs, v2, now, nil); !ok || u != "erin" || gen != legacyGenerationID {
		t.Errorf("v2 under current generation: user=%q gen=%d ok=%v; want erin/%d/true", u, gen, ok, legacyGenerationID)
	}

	// Survives rotation (trial verification finds the previous generation)...
	after := gs.rotate(genSecretB, now, defaultVerifyWindow)
	if u, _, ok := verifyHubUserCookieAcrossGenerations(after, v2, now.Add(time.Minute), nil); !ok || u != "erin" {
		t.Errorf("v2 across rotation: user=%q ok=%v; want erin/true", u, ok)
	}

	// ...and stops being accepted once that generation expires. A v2 cookie
	// cannot carry a marker, so this is the ONLY thing that bounds it.
	late := now.Add(defaultVerifyWindow + time.Minute)
	if u, _, ok := verifyHubUserCookieAcrossGenerations(after, v2, late, nil); ok {
		t.Errorf("a v2 cookie under an EXPIRED generation was accepted as %q", u)
	}
}

// TestSessionCookieGenerationsPreserveRevocation pins that adding generations
// did not route around F10 revocation. A revoked session must stay revoked
// under EVERY generation — otherwise a rotation would silently un-revoke every
// logged-out session, which is worse than the bug F10 fixed.
func TestSessionCookieGenerationsPreserveRevocation(t *testing.T) {
	now := time.Now()
	gs := legacyGenerationSet(genSecretA).rotate(genSecretB, now, defaultVerifyWindow)

	cookie, sid := mintHubUserCookieValueV3ForGeneration(gs, "frank", now, time.Hour)
	if cookie == "" || sid == "" {
		t.Fatal("mint returned empty")
	}

	revoked := func(s string) bool { return s == sid }
	if u, _, ok := verifyHubUserCookieAcrossGenerations(gs, cookie, now, revoked); ok {
		t.Errorf("a REVOKED session verified as %q under generations", u)
	}

	// Also revoked when it is the PREVIOUS generation doing the accepting: the
	// pre-rotation cookie is the one an attacker would replay across a
	// rotation hoping the new code forgot to check.
	old, oldSID := mintHubUserCookieValueV3ForGeneration(legacyGenerationSet(genSecretA), "frank", now, time.Hour)
	if _, _, ok := verifyHubUserCookieAcrossGenerations(gs, old, now, func(s string) bool { return s == oldSID }); ok {
		t.Error("a revoked PRE-ROTATION session verified under the previous generation")
	}

	// POSITIVE CONTROL: with nothing revoked, both cookies verify — so the two
	// rejections above are revocation and not a verifier that rejects all.
	if _, _, ok := verifyHubUserCookieAcrossGenerations(gs, cookie, now, nil); !ok {
		t.Error("positive control: the current-generation cookie must verify when not revoked")
	}
	if _, _, ok := verifyHubUserCookieAcrossGenerations(gs, old, now, nil); !ok {
		t.Error("positive control: the previous-generation cookie must verify when not revoked")
	}
}

// TestSessionCookieGenerationsDoNotWidenAcceptance is the risk any verify-both
// lane carries: adding a second acceptable key must not turn a forged or edited
// cookie into a valid one, and must not resurrect the deleted F1 symmetric lane.
func TestSessionCookieGenerationsDoNotWidenAcceptance(t *testing.T) {
	now := time.Now()
	gs := legacyGenerationSet(genSecretA).rotate(genSecretB, now, defaultVerifyWindow)

	cookie, _ := mintHubUserCookieValueV3ForGeneration(gs, "grace", now, time.Hour)

	// Edited payload, original signature.
	tampered := cookie[:len(cookie)-4] + "AAAA"
	if _, _, ok := verifyHubUserCookieAcrossGenerations(gs, tampered, now, nil); ok {
		t.Error("a tampered cookie must not verify under ANY generation")
	}

	// AUDIT F1 must stay closed. A cookie minted with the SYMMETRIC session
	// sub-key — the value provisioned byte-identically into every spoke — must
	// not verify, under any generation. If it does, a rotation has re-opened a
	// Critical finding that took four audits to close.
	for _, master := range []string{genSecretA, genSecretB} {
		forged := mintHubUserCookieValue(deriveDomainKey(master, infoSessionKey), hubAdminUsername)
		if u, _, ok := verifyHubUserCookieAcrossGenerations(gs, forged, now, nil); ok {
			t.Errorf("F1 REGRESSION: a spoke-forgeable symmetric cookie verified as %q", u)
		}
	}

	// Signed with the RAW master rather than the derived session seed — domain
	// separation must survive the change.
	rawSeed := genSecretB
	if v := mintHubUserCookieValueV2(rawSeed, "grace"); v != "" {
		if _, _, ok := verifyHubUserCookieAcrossGenerations(gs, v, now, nil); ok {
			t.Error("a cookie signed with the raw master must not verify — domain separation regressed")
		}
	}

	// A cookie signed by a key that is NO generation at all, but which CLAIMS
	// the current generation. The marker is a hint, never an authorization.
	cur, _ := gs.currentGeneration()
	alien, _ := mintHubUserCookieValueV3Gen(
		sessionSeedFor("some-master-that-is-not-a-generation"), "grace", now, time.Hour, cur.ID)
	if _, _, ok := verifyHubUserCookieAcrossGenerations(gs, alien, now, nil); ok {
		t.Error("a cookie signed by a non-generation key must not verify however it is marked")
	}

	// POSITIVE CONTROL: the untampered cookie verifies, so the rejections above
	// are about the forgeries rather than a verifier that rejects everything.
	if u, _, ok := verifyHubUserCookieAcrossGenerations(gs, cookie, now, nil); !ok || u != "grace" {
		t.Errorf("positive control: the untampered cookie must verify; got %q ok=%v", u, ok)
	}
}

// TestSessionCookieStaleMarkerFallsThrough pins that the `g` claim can only
// ever make verification CHEAPER, never make it fail. A cookie naming a
// generation the hub no longer accepts must still be tried against the ones it
// does — otherwise retiring a generation would reject cookies that a live key
// can perfectly well verify.
func TestSessionCookieStaleMarkerFallsThrough(t *testing.T) {
	now := time.Now()
	gs := legacyGenerationSet(genSecretA)

	// Signed by the CURRENT generation's seed but marked with a generation ID
	// that does not exist in the set.
	const unknownGeneration = 99
	cookie, _ := mintHubUserCookieValueV3Gen(
		sessionSeedFor(genSecretA), "heidi", now, time.Hour, unknownGeneration)
	if cookie == "" {
		t.Fatal("mint returned empty")
	}
	if got := hubCookieClaimedGeneration(cookie); got != unknownGeneration {
		t.Fatalf("claimed generation = %d, want %d", got, unknownGeneration)
	}

	u, gen, ok := verifyHubUserCookieAcrossGenerations(gs, cookie, now, nil)
	if !ok || u != "heidi" {
		t.Errorf("a cookie with an unknown marker but a valid signature must verify; got %q ok=%v", u, ok)
	}
	if gen != legacyGenerationID {
		t.Errorf("accepting generation = %d, want %d", gen, legacyGenerationID)
	}
}

// TestSessionCookieGenerationsFailClosed pins that a hub with no usable
// generation set cannot mint or verify a session. Fail-closed here means
// "cannot establish a session", which is the safe direction.
func TestSessionCookieGenerationsFailClosed(t *testing.T) {
	now := time.Now()

	if v, sid := mintHubUserCookieValueV3ForGeneration(nil, "alice", now, time.Hour); v != "" || sid != "" {
		t.Error("mint with no generation set must return empty")
	}
	if _, _, ok := verifyHubUserCookieAcrossGenerations(nil, "anything", now, nil); ok {
		t.Error("verify with no generation set must reject")
	}

	real, _ := mintHubUserCookieValueV3ForGeneration(legacyGenerationSet(genSecretA), "alice", now, time.Hour)
	if _, _, ok := verifyHubUserCookieAcrossGenerations(nil, real, now, nil); ok {
		t.Error("a valid cookie must not verify against a nil generation set")
	}
	// POSITIVE CONTROL: the same cookie verifies against its real set.
	if _, _, ok := verifyHubUserCookieAcrossGenerations(legacyGenerationSet(genSecretA), real, now, nil); !ok {
		t.Error("positive control: the cookie must verify against its own generation set")
	}
}

// TestSessionCookieUnrotatedHubIsByteIdentical pins the no-migration promise at
// the wire level: on a hub that has never rotated, the generation-aware minter
// stamps g=1 and the resulting cookie is still verifiable by the plain
// single-key verifier the spokes and the Node proxy run. The `g` claim is
// additive, not a format change.
func TestSessionCookieUnrotatedHubIsByteIdentical(t *testing.T) {
	now := time.Now()
	gs := legacyGenerationSet(genSecretA)

	cookie, _ := mintHubUserCookieValueV3ForGeneration(gs, "ivan", now, time.Hour)
	if cookie == "" {
		t.Fatal("mint returned empty")
	}

	// The SPOKE-side verifier — which knows nothing about generations and only
	// has the one public key — must still accept it. This is the assertion that
	// says no spoke roll is required.
	pub := ssoPublicKeyFromSeed(sessionSeedFor(genSecretA))
	if u, ok := verifyHubUserCookieEitherAt(pub, "", cookie, now, nil); !ok || u != "ivan" {
		t.Errorf("a generation-marked cookie must verify with the plain single-key verifier "+
			"(this is what every spoke's Node proxy does); got %q ok=%v", u, ok)
	}
}
