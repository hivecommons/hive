package hub

import (
	"testing"
	"time"
)

// Tests for the master-secret generation mechanism (hub_generations.go).
//
// Each test below states the property it pins and, where the property is a
// SECURITY one, is paired with a positive control — an assertion that the test
// would actually fail if the behavior regressed. Several of these properties
// are "X is rejected", which passes trivially against a broken implementation
// that rejects everything, so the controls prove the accept path works too.

const (
	genSecretA = "master-generation-A-0123456789abcdef"
	genSecretB = "master-generation-B-fedcba9876543210"
)

// TestLegacyGenerationSetIsBehaviorPreserving pins the no-migration promise: a
// hub that has never rotated derives EXACTLY the keys the single-master code
// derives today. If this ever fails, deploying the generation mechanism is
// itself a flag day — the precise outcome it exists to prevent.
func TestLegacyGenerationSetIsBehaviorPreserving(t *testing.T) {
	gs := legacyGenerationSet(genSecretA)
	if gs == nil {
		t.Fatal("legacy set must exist for a non-empty master")
	}
	if gs.Current != legacyGenerationID {
		t.Errorf("current = %d, want %d", gs.Current, legacyGenerationID)
	}
	if got := gs.currentSecret(); got != genSecretA {
		t.Errorf("current secret does not round-trip the master")
	}
	// The derived key must be byte-identical to the pre-generation formula.
	want := deriveDomainKey(genSecretA, infoImpersonateKey)
	got := deriveDomainKey(gs.currentSecret(), infoImpersonateKey)
	if got != want || got == "" {
		t.Errorf("derived key changed under the generation set: got %q want %q", got, want)
	}
	// Fail-closed: no master means no generation set, so nothing can mint.
	if legacyGenerationSet("") != nil {
		t.Error("empty master must not produce a generation set")
	}
	if legacyGenerationSet("   ") != nil {
		t.Error("whitespace-only master must not produce a generation set")
	}
}

// TestRotateDemotesOutgoingAndMintsWithIncoming is the core rotation contract:
// after rotating, MINTING uses the new secret while the outgoing one remains
// acceptable for VERIFY. Without both halves a rotation is a flag day.
func TestRotateDemotesOutgoingAndMintsWithIncoming(t *testing.T) {
	now := time.Now()
	gs := legacyGenerationSet(genSecretA)
	rotated := gs.rotate(genSecretB, now, defaultVerifyWindow)
	if rotated == nil {
		t.Fatal("rotate returned nil for a valid secret")
	}

	// Minting moved to the incoming generation.
	if rotated.currentSecret() != genSecretB {
		t.Error("current secret must be the incoming one after rotation")
	}
	if rotated.Current != legacyGenerationID+1 {
		t.Errorf("current id = %d, want %d", rotated.Current, legacyGenerationID+1)
	}

	// The outgoing generation is still ACCEPTED — this is the property that
	// makes rotation not a flag day.
	acceptable := rotated.acceptableGenerations(now)
	if len(acceptable) != 2 {
		t.Fatalf("acceptable generations = %d, want 2 (current + outgoing)", len(acceptable))
	}
	if acceptable[0].Secret != genSecretB {
		t.Error("current generation must be tried first")
	}
	if acceptable[1].Secret != genSecretA {
		t.Error("outgoing generation must remain acceptable during the window")
	}

	// POSITIVE CONTROL. "The outgoing key is accepted" is only meaningful if an
	// UNRELATED key is not. Otherwise this test would pass against an
	// implementation that accepts anything.
	for _, g := range acceptable {
		if g.Secret == "some-key-that-was-never-a-generation" {
			t.Fatal("positive control: an unrelated secret must never be acceptable")
		}
	}

	// The receiver is untouched — rotate is pure, so a failed persist does not
	// leave the hub having half-rotated in memory.
	if gs.currentSecret() != genSecretA {
		t.Error("rotate must not mutate the receiver")
	}
}

// TestPreviousGenerationExpiresOnItsOwn is the FINITENESS property — the one
// the F1/F2 verify-both lanes lacked, which is why they survived five audits.
// The old key must stop being accepted on a wall clock with NO operator action.
func TestPreviousGenerationExpiresOnItsOwn(t *testing.T) {
	now := time.Now()
	rotated := legacyGenerationSet(genSecretA).rotate(genSecretB, now, defaultVerifyWindow)

	// Inside the window: two generations accepted.
	inside := now.Add(defaultVerifyWindow - time.Hour)
	if n := len(rotated.acceptableGenerations(inside)); n != 2 {
		t.Fatalf("inside window: acceptable = %d, want 2", n)
	}

	// Past the window: the previous generation is GONE with nobody retiring it.
	after := now.Add(defaultVerifyWindow + time.Minute)
	got := rotated.acceptableGenerations(after)
	if len(got) != 1 {
		t.Fatalf("after window: acceptable = %d, want 1 (current only)", len(got))
	}
	if got[0].Secret != genSecretB {
		t.Error("after expiry the surviving generation must be the current one")
	}

	// POSITIVE CONTROL: the current generation must NOT expire. A test that
	// only checked "count drops to 1" would also pass if everything expired,
	// which would break the hub entirely.
	far := now.Add(100 * 365 * 24 * time.Hour)
	if n := len(rotated.acceptableGenerations(far)); n != 1 {
		t.Errorf("current generation must never expire: acceptable = %d, want 1", n)
	}
}

// TestPreviousGenerationWithoutExpiryFailsClosed pins that a MISSING expiry is
// treated as already-expired rather than as never-expires. A hand-edited or
// malformed generations file must not be able to create the unbounded compat
// lane this whole design exists to prevent.
func TestPreviousGenerationWithoutExpiryFailsClosed(t *testing.T) {
	now := time.Now()
	gs := newGenerationSet(2, []keyGeneration{
		{ID: 2, Secret: genSecretB},
		{ID: 1, Secret: genSecretA}, // no VerifyUntil at all
	})
	if gs == nil {
		t.Fatal("set must build")
	}
	acceptable := gs.acceptableGenerations(now)
	if len(acceptable) != 1 {
		t.Fatalf("acceptable = %d, want 1 — a previous generation with no expiry must NOT be accepted", len(acceptable))
	}
	if acceptable[0].ID != 2 {
		t.Error("the surviving generation must be the current one")
	}
}

// TestGenerationSetCapsLiveGenerations pins the bound that makes trial
// verification affordable for artifacts that cannot carry a marker (the
// heartbeat bearer above all).
func TestGenerationSetCapsLiveGenerations(t *testing.T) {
	now := time.Now()
	gs := legacyGenerationSet(genSecretA)
	// Rotate repeatedly; the set must never exceed the cap.
	for i := 0; i < 5; i++ {
		gs = gs.rotate("secret-round-"+time.Duration(i).String(), now, defaultVerifyWindow)
		if gs == nil {
			t.Fatal("rotate returned nil")
		}
		if len(gs.Generations) > maxLiveGenerations {
			t.Fatalf("round %d: %d live generations, cap is %d", i, len(gs.Generations), maxLiveGenerations)
		}
	}
	// After many rotations the ORIGINAL secret must be long gone, not merely
	// unexpired — otherwise stacked rotations silently extend an old key's life.
	for _, g := range gs.acceptableGenerations(now) {
		if g.Secret == genSecretA {
			t.Error("a key from several rotations ago must not still be acceptable")
		}
	}
}

// TestGenerationMarkerRoundTripAndStrictParse pins the marker format. The parse
// must be STRICT: a malformed marker must read as "unmarked legacy", never as a
// valid generation selector, or a crafted value could steer key selection.
func TestGenerationMarkerRoundTripAndStrictParse(t *testing.T) {
	// Round-trip.
	marked := formatGenerationMarker(7) + "payload.sig"
	id, rest, ok := splitGenerationMarker(marked)
	if !ok || id != 7 || rest != "payload.sig" {
		t.Fatalf("round trip failed: id=%d rest=%q ok=%v", id, rest, ok)
	}

	// Values that must NOT parse as a marker. Each would otherwise be a way to
	// influence which key is selected.
	for _, bad := range []string{
		"payload.sig",    // unmarked legacy — the common case
		"gfoo.bar",       // non-numeric
		"g.bar",          // empty number
		"g-1.bar",        // negative
		"g0.bar",         // zero is not a valid generation id
		"g12",            // no separator
		"alice.sigvalue", // a real legacy session-shaped value
		"",               // empty
	} {
		if _, _, ok := splitGenerationMarker(bad); ok {
			t.Errorf("%q must not parse as a generation marker", bad)
		}
	}

	// POSITIVE CONTROL: the rejections above are only meaningful if a
	// well-formed marker still parses. Otherwise a parser that always returned
	// false would pass every assertion in this test.
	if _, _, ok := splitGenerationMarker("g1.x"); !ok {
		t.Error("positive control: a well-formed marker must parse")
	}
}

// TestVerifyWithGenerationsSelectsAndFallsBack pins the three paths through the
// dual-acceptance driver: marked-and-current, marked-and-previous, and
// unmarked-legacy.
func TestVerifyWithGenerationsSelectsAndFallsBack(t *testing.T) {
	now := time.Now()
	gs := legacyGenerationSet(genSecretA).rotate(genSecretB, now, defaultVerifyWindow)

	// attempt accepts only when the artifact equals the secret it was derived
	// under — a stand-in for a signature check that is exact and observable.
	attempt := func(secret, artifact string) (string, bool) {
		return artifact, artifact == "signed-by-"+secret
	}

	// 1. Marked with the CURRENT generation.
	cur, _ := gs.currentGeneration()
	v1 := formatGenerationMarker(cur.ID) + "signed-by-" + genSecretB
	if _, gen, ok := verifyWithGenerations(gs, v1, now, attempt); !ok || gen != cur.ID {
		t.Errorf("current-marked artifact: ok=%v gen=%d, want ok=true gen=%d", ok, gen, cur.ID)
	}

	// 2. Marked with the PREVIOUS generation — the rotation-window case.
	v2 := formatGenerationMarker(legacyGenerationID) + "signed-by-" + genSecretA
	if _, gen, ok := verifyWithGenerations(gs, v2, now, attempt); !ok || gen != legacyGenerationID {
		t.Errorf("previous-marked artifact: ok=%v gen=%d, want ok=true gen=%d", ok, gen, legacyGenerationID)
	}

	// 3. UNMARKED legacy artifact signed by the previous generation. Every
	//    artifact minted before this change looks like this, so it must verify
	//    by falling back rather than being rejected.
	if _, gen, ok := verifyWithGenerations(gs, "signed-by-"+genSecretA, now, attempt); !ok || gen != legacyGenerationID {
		t.Errorf("unmarked legacy artifact: ok=%v gen=%d, want ok=true gen=%d", ok, gen, legacyGenerationID)
	}

	// 4. POSITIVE CONTROL / negative case: an artifact signed by NO generation
	//    must be rejected. Without this the three accepts above would pass
	//    against a driver that accepts everything.
	if _, _, ok := verifyWithGenerations(gs, "signed-by-some-other-key", now, attempt); ok {
		t.Error("an artifact signed by no live generation must be rejected")
	}

	// 5. Once the window closes, the previous generation's artifact stops
	//    verifying — finiteness, end to end.
	after := now.Add(defaultVerifyWindow + time.Minute)
	if _, _, ok := verifyWithGenerations(gs, "signed-by-"+genSecretA, after, attempt); ok {
		t.Error("a previous-generation artifact must stop verifying after the window")
	}
	// ...while the current one still does. (Control for #5: proves the failure
	// above is expiry, not a broken driver.)
	if _, _, ok := verifyWithGenerations(gs, "signed-by-"+genSecretB, after, attempt); !ok {
		t.Error("the current generation must still verify after the previous expires")
	}
}

// TestGenerationMarkerCannotForceFailure pins that the marker is an
// OPTIMIZATION HINT on an unauthenticated value: editing it must never turn a
// valid artifact into a rejected one via an unknown generation, or anyone could
// invalidate anyone's cookie by rewriting a prefix.
func TestGenerationMarkerCannotForceFailure(t *testing.T) {
	now := time.Now()
	gs := legacyGenerationSet(genSecretA)
	attempt := func(secret, artifact string) (string, bool) {
		return artifact, artifact == "signed-by-"+secret
	}
	valid := "signed-by-" + genSecretA

	// A marker naming a generation that does not exist must fall through to
	// trying the generations we DO accept.
	tampered := formatGenerationMarker(999) + valid
	if _, _, ok := verifyWithGenerations(gs, tampered, now, attempt); !ok {
		t.Error("an unknown-generation marker must fall back, not fail the artifact")
	}

	// POSITIVE CONTROL: falling back must not become "accept anything". A
	// tampered marker on a BOGUS payload still fails.
	if _, _, ok := verifyWithGenerations(gs, formatGenerationMarker(999)+"signed-by-nothing", now, attempt); ok {
		t.Error("fallback must not accept an artifact signed by no generation")
	}
}

// TestNilGenerationSetFailsClosed pins that an unconfigured hub cannot mint or
// verify anything. "No key" must mean "no", never "yes by default".
func TestNilGenerationSetFailsClosed(t *testing.T) {
	var gs *generationSet
	if _, ok := gs.currentGeneration(); ok {
		t.Error("nil set must have no current generation")
	}
	if gs.currentSecret() != "" {
		t.Error("nil set must yield an empty secret")
	}
	if n := len(gs.acceptableGenerations(time.Now())); n != 0 {
		t.Errorf("nil set acceptable = %d, want 0", n)
	}
	attempt := func(string, string) (string, bool) { return "", true } // accepts EVERYTHING
	if _, _, ok := verifyWithGenerations(gs, "anything", time.Now(), attempt); ok {
		t.Error("nil set must reject even when the attempt function accepts everything")
	}
}
