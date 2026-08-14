package hub

import (
	"testing"
	"time"
)

// Dual-generation acceptance for the impersonation cookie — the pilot adoption
// of hub_generations.go.
//
// The impersonation cookie is the lowest-risk verifier on the platform: hub-only
// (no spoke, no Node proxy, no Deployment env), 30-minute TTL, and it grants
// nothing — impersonation is read-only. So it exercises the full marker path
// without any tenant-visible risk.

// TestImpersonationSurvivesRotation is THE test this whole change exists for.
// Before it, rotating the master invalidated every live impersonation grant at
// the instant of rotation. After it, a grant minted under the outgoing
// generation keeps working for its natural lifetime.
func TestImpersonationSurvivesRotation(t *testing.T) {
	now := time.Now()
	before := legacyGenerationSet(genSecretA)

	// A grant minted BEFORE the rotation.
	cookie := mintImpersonateCookieValueForGeneration(before, hubAdminUsername, "alice", now)
	if cookie == "" {
		t.Fatal("mint returned empty for valid inputs")
	}

	// Rotate the master.
	after := before.rotate(genSecretB, now, defaultVerifyWindow)
	if after == nil {
		t.Fatal("rotate returned nil")
	}

	// The pre-rotation grant STILL VERIFIES, and reports the generation that
	// accepted it — so an operator can see the old key is still in use.
	grant, gen, ok := verifyImpersonateCookieValueWithGenerations(after, cookie, now.Add(time.Minute))
	if !ok {
		t.Fatal("a grant minted before rotation must still verify during the window")
	}
	if gen != legacyGenerationID {
		t.Errorf("accepting generation = %d, want %d (the outgoing one)", gen, legacyGenerationID)
	}
	if grant.Admin != hubAdminUsername || grant.Target != "alice" {
		t.Errorf("grant payload corrupted across rotation: %+v", grant)
	}

	// POSITIVE CONTROL — this is the regression the test guards. Verifying the
	// SAME cookie against a hub that only knows the NEW generation must FAIL.
	// Without this assertion the test above would pass against an
	// implementation that accepts any cookie regardless of key.
	onlyNew := legacyGenerationSet(genSecretB)
	if _, _, ok := verifyImpersonateCookieValueWithGenerations(onlyNew, cookie, now.Add(time.Minute)); ok {
		t.Error("positive control: a pre-rotation cookie must NOT verify against the new generation alone")
	}
}

// TestImpersonationMintsOnlyWithCurrentGeneration pins the mint half of the
// contract: after rotation, NEW grants use the NEW key. Dual acceptance is for
// verification only — if minting also used the old key the rotation would never
// actually take effect.
func TestImpersonationMintsOnlyWithCurrentGeneration(t *testing.T) {
	now := time.Now()
	rotated := legacyGenerationSet(genSecretA).rotate(genSecretB, now, defaultVerifyWindow)

	cookie := mintImpersonateCookieValueForGeneration(rotated, hubAdminUsername, "bob", now)
	if cookie == "" {
		t.Fatal("mint returned empty")
	}

	// It carries the CURRENT generation's marker.
	cur, _ := rotated.currentGeneration()
	id, _, marked := splitGenerationMarker(cookie)
	if !marked {
		t.Fatal("a freshly minted cookie must carry a generation marker")
	}
	if id != cur.ID {
		t.Errorf("marker generation = %d, want current %d", id, cur.ID)
	}

	// It verifies, and does so under the CURRENT generation.
	_, gen, ok := verifyImpersonateCookieValueWithGenerations(rotated, cookie, now)
	if !ok || gen != cur.ID {
		t.Errorf("fresh cookie: ok=%v gen=%d, want ok=true gen=%d", ok, gen, cur.ID)
	}

	// POSITIVE CONTROL: it must NOT verify against the OLD generation alone —
	// proving the new key really was used rather than the old one.
	if _, _, ok := verifyImpersonateCookieValueWithGenerations(legacyGenerationSet(genSecretA), cookie, now); ok {
		t.Error("positive control: a post-rotation cookie must not verify under the outgoing key")
	}
}

// TestImpersonationAcceptsUnmarkedLegacyCookie pins that deploying this change
// is a NON-EVENT for cookies already sitting in browsers. Every impersonation
// cookie minted before this code shipped is unmarked and signed with the
// derived impersonate sub-key of the single master.
func TestImpersonationAcceptsUnmarkedLegacyCookie(t *testing.T) {
	now := time.Now()
	gs := legacyGenerationSet(genSecretA)

	// Mint EXACTLY the way the pre-generation code did: no marker, signed with
	// the derived sub-key.
	legacy := mintImpersonateCookieValue(
		deriveDomainKey(genSecretA, infoImpersonateKey), hubAdminUsername, "carol", now)
	if legacy == "" {
		t.Fatal("legacy mint returned empty")
	}
	if _, _, marked := splitGenerationMarker(legacy); marked {
		t.Fatal("the legacy mint must not produce a marker — the test would not be testing the legacy path")
	}

	grant, gen, ok := verifyImpersonateCookieValueWithGenerations(gs, legacy, now)
	if !ok {
		t.Fatal("an unmarked pre-rotation cookie must still verify")
	}
	if gen != legacyGenerationID {
		t.Errorf("accepting generation = %d, want %d", gen, legacyGenerationID)
	}
	if grant.Target != "carol" {
		t.Errorf("grant target = %q, want carol", grant.Target)
	}
}

// TestImpersonationRejectsAfterGenerationExpires pins finiteness at the
// artifact level: once the outgoing generation's window closes, cookies minted
// under it stop being accepted — with no operator action.
func TestImpersonationRejectsAfterGenerationExpires(t *testing.T) {
	now := time.Now()
	before := legacyGenerationSet(genSecretA)
	cookie := mintImpersonateCookieValueForGeneration(before, hubAdminUsername, "dave", now)
	after := before.rotate(genSecretB, now, defaultVerifyWindow)

	// Note the cookie's own 30-minute TTL would expire long before the 7-day
	// window, so verify at a time inside the cookie TTL but past the generation
	// window by minting a fresh legacy-signed cookie at that later moment.
	late := now.Add(defaultVerifyWindow + time.Minute)
	lateCookie := formatGenerationMarker(legacyGenerationID) + mintImpersonateCookieValue(
		deriveDomainKey(genSecretA, infoImpersonateKey), hubAdminUsername, "dave", late)

	if _, _, ok := verifyImpersonateCookieValueWithGenerations(after, lateCookie, late); ok {
		t.Error("a cookie under an EXPIRED generation must be rejected")
	}

	// POSITIVE CONTROL: the same cookie, verified INSIDE the window, is
	// accepted. This proves the rejection above is the expiry and not a broken
	// verifier or a bad cookie.
	early := now.Add(time.Minute)
	earlyCookie := formatGenerationMarker(legacyGenerationID) + mintImpersonateCookieValue(
		deriveDomainKey(genSecretA, infoImpersonateKey), hubAdminUsername, "dave", early)
	if _, _, ok := verifyImpersonateCookieValueWithGenerations(after, earlyCookie, early); !ok {
		t.Error("positive control: the same cookie inside the window must be accepted")
	}

	_ = cookie
}

// TestImpersonationGenerationFailsClosedWithoutKeys pins that a hub with no
// generation set cannot mint or verify an impersonation grant. Fail-closed here
// means "cannot impersonate", which is the safe direction.
func TestImpersonationGenerationFailsClosedWithoutKeys(t *testing.T) {
	now := time.Now()
	if v := mintImpersonateCookieValueForGeneration(nil, hubAdminUsername, "eve", now); v != "" {
		t.Error("mint with no generation set must return empty")
	}
	if _, _, ok := verifyImpersonateCookieValueWithGenerations(nil, "anything", now); ok {
		t.Error("verify with no generation set must reject")
	}
	// A well-formed cookie from a real generation must not verify against a nil
	// set either.
	real := mintImpersonateCookieValueForGeneration(legacyGenerationSet(genSecretA), hubAdminUsername, "eve", now)
	if _, _, ok := verifyImpersonateCookieValueWithGenerations(nil, real, now); ok {
		t.Error("a valid cookie must not verify against a nil generation set")
	}
	// POSITIVE CONTROL: that same cookie DOES verify against its real set, so
	// the rejection above is about the nil set, not a malformed cookie.
	if _, _, ok := verifyImpersonateCookieValueWithGenerations(legacyGenerationSet(genSecretA), real, now); !ok {
		t.Error("positive control: the cookie must verify against its own generation set")
	}
}

// TestImpersonationTamperStillFailsUnderGenerations pins that dual acceptance
// did NOT widen what verifies. Adding a second acceptable key must not turn a
// forged or edited cookie into a valid one — the risk any verify-both lane
// carries.
func TestImpersonationTamperStillFailsUnderGenerations(t *testing.T) {
	now := time.Now()
	gs := legacyGenerationSet(genSecretA).rotate(genSecretB, now, defaultVerifyWindow)
	cookie := mintImpersonateCookieValueForGeneration(gs, hubAdminUsername, "frank", now)

	// Edit the payload, keep the signature.
	tampered := cookie[:len(cookie)-4] + "AAAA"
	if _, _, ok := verifyImpersonateCookieValueWithGenerations(gs, tampered, now); ok {
		t.Error("a tampered cookie must not verify under ANY generation")
	}

	// A cookie signed with a key that is no generation at all.
	forged := formatGenerationMarker(gs.Current) + mintImpersonateCookieValue(
		"not-a-generation-key", hubAdminUsername, "frank", now)
	if _, _, ok := verifyImpersonateCookieValueWithGenerations(gs, forged, now); ok {
		t.Error("a cookie signed with a non-generation key must not verify")
	}

	// Signing with the RAW master rather than the derived impersonate sub-key
	// must also fail — domain separation is preserved across the change.
	rawMaster := formatGenerationMarker(gs.Current) + mintImpersonateCookieValue(
		genSecretB, hubAdminUsername, "frank", now)
	if _, _, ok := verifyImpersonateCookieValueWithGenerations(gs, rawMaster, now); ok {
		t.Error("a cookie signed with the raw master must not verify — domain separation regressed")
	}

	// POSITIVE CONTROL: the untampered cookie verifies, so the three rejections
	// above are about the tampering rather than a verifier that rejects all.
	if _, _, ok := verifyImpersonateCookieValueWithGenerations(gs, cookie, now); !ok {
		t.Error("positive control: the untampered cookie must verify")
	}
}
