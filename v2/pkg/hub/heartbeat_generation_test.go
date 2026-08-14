package hub

import (
	"testing"
	"time"
)

// Bounded trial verification of the HEARTBEAT BEARER across master generations
// — follow-on PR #2 of the master-key rotation design.
//
// The bearer is the class of artifact that CANNOT carry a generation marker: it
// IS the derived key, presented raw in the Authorization header, and its format
// is a contract with deployed spoke Deployments and with SpokeHeartbeatKey()'s
// self-derive lane. So the hub tries each live generation instead, bounded at
// maxLiveGenerations == 2.
//
// !! THE F2 TESTS BELOW ARE THE POINT OF THIS FILE. !!
//
// F2 (Critical, open across five audits) was closed by deleting the FLEET-WIDE
// heartbeat lane. Adding a second generation must add a second KEY, never a
// second LANE — so every generation's candidate must remain identity-bound, and
// TestHeartbeatBearerIsIdentityBoundUnderEveryGeneration asserts exactly that
// against both generations rather than only the current one.

const (
	hbHiveA = "hive-alpha"
	hbHiveB = "hive-bravo"
)

// hbBearerFor is the bearer a spoke presents: the per-hive derivation from a
// given master. This mirrors SpokeHeartbeatKey()'s self-derive lane and
// provisionHeartbeatKey exactly.
func hbBearerFor(master, hiveID string) string {
	return derivePerHiveKey(master, infoHeartbeatKey, hiveID)
}

// TestHeartbeatBearerSurvivesRotation is THE test this change exists for. A
// spoke that has not yet been reconciled onto the new master still holds a
// bearer derived from the old one. Without dual acceptance every one of ~66
// spokes 401s at the instant of rotation.
func TestHeartbeatBearerSurvivesRotation(t *testing.T) {
	now := time.Now()
	before := legacyGenerationSet(genSecretA)
	after := before.rotate(genSecretB, now, defaultVerifyWindow)
	if after == nil {
		t.Fatal("rotate returned nil")
	}

	old := hbBearerFor(genSecretA, hbHiveA)
	if old == "" {
		t.Fatal("precondition: could not derive the pre-rotation bearer")
	}

	gen, ok := verifyHeartbeatBearerAcrossGenerations(after, old, hbHiveA, now.Add(time.Minute))
	if !ok {
		t.Fatal("a bearer derived from the outgoing master must still verify during the window")
	}
	if gen != legacyGenerationID {
		t.Errorf("accepting generation = %d, want %d (the outgoing one)", gen, legacyGenerationID)
	}

	// POSITIVE CONTROL. Against a hub that knows ONLY the new generation the
	// same bearer must FAIL. Without this, an implementation that accepted any
	// bearer would pass the assertion above.
	onlyNew := legacyGenerationSet(genSecretB)
	if _, ok := verifyHeartbeatBearerAcrossGenerations(onlyNew, old, hbHiveA, now.Add(time.Minute)); ok {
		t.Error("positive control: a pre-rotation bearer must NOT verify against the new generation alone")
	}
}

// TestHeartbeatBearerAcceptedUnderCurrentGeneration is the base case, and the
// state of every already-reconciled spoke after a rotation.
func TestHeartbeatBearerAcceptedUnderCurrentGeneration(t *testing.T) {
	now := time.Now()
	rotated := legacyGenerationSet(genSecretA).rotate(genSecretB, now, defaultVerifyWindow)
	cur, _ := rotated.currentGeneration()

	fresh := hbBearerFor(genSecretB, hbHiveA)
	gen, ok := verifyHeartbeatBearerAcrossGenerations(rotated, fresh, hbHiveA, now)
	if !ok || gen != cur.ID {
		t.Errorf("current-generation bearer: gen=%d ok=%v, want %d/true", gen, ok, cur.ID)
	}

	// And on a hub that has never rotated, the single generation accepts.
	if gen, ok := verifyHeartbeatBearerAcrossGenerations(
		legacyGenerationSet(genSecretA), hbBearerFor(genSecretA, hbHiveA), hbHiveA, now); !ok || gen != legacyGenerationID {
		t.Errorf("un-rotated hub: gen=%d ok=%v, want %d/true", gen, ok, legacyGenerationID)
	}
}

// TestHeartbeatBearerRejectedAfterGenerationExpires pins FINITENESS. This is
// the property that stops dual acceptance from becoming the permanent compat
// lane F2 was: past verify_until the old bearer stops working with no operator
// action, whether or not anyone remembered the reconcile lane had stalled.
func TestHeartbeatBearerRejectedAfterGenerationExpires(t *testing.T) {
	now := time.Now()
	after := legacyGenerationSet(genSecretA).rotate(genSecretB, now, defaultVerifyWindow)
	old := hbBearerFor(genSecretA, hbHiveA)

	late := now.Add(defaultVerifyWindow + time.Minute)
	if _, ok := verifyHeartbeatBearerAcrossGenerations(after, old, hbHiveA, late); ok {
		t.Error("a bearer under an EXPIRED generation must be rejected")
	}

	// POSITIVE CONTROL 1: the same bearer INSIDE the window is accepted, so the
	// rejection above is the expiry and not a broken verifier.
	if _, ok := verifyHeartbeatBearerAcrossGenerations(after, old, hbHiveA, now.Add(time.Minute)); !ok {
		t.Error("positive control: the same bearer inside the window must be accepted")
	}
	// POSITIVE CONTROL 2: at that same LATE moment the CURRENT generation's
	// bearer still works — so the verifier has not simply stopped verifying.
	if _, ok := verifyHeartbeatBearerAcrossGenerations(after, hbBearerFor(genSecretB, hbHiveA), hbHiveA, late); !ok {
		t.Error("positive control: the current-generation bearer must still verify past the previous generation's expiry")
	}
}

// TestHeartbeatBearerRejectsMissingVerifyUntil pins the fail-closed reading of a
// hand-edited generations file. A previous generation with a ZERO VerifyUntil is
// treated as ALREADY EXPIRED, never as "never expires" — the latter is precisely
// the unbounded compat lane this design exists to prevent.
func TestHeartbeatBearerRejectsMissingVerifyUntil(t *testing.T) {
	now := time.Now()
	const currentID = 2
	handEdited := newGenerationSet(currentID, []keyGeneration{
		{ID: currentID, Secret: genSecretB},
		// No VerifyUntil — as a hand-edited file might have.
		{ID: legacyGenerationID, Secret: genSecretA},
	})
	if handEdited == nil {
		t.Fatal("precondition: could not build the hand-edited set")
	}

	if _, ok := verifyHeartbeatBearerAcrossGenerations(
		handEdited, hbBearerFor(genSecretA, hbHiveA), hbHiveA, now); ok {
		t.Error("a previous generation with NO verify_until must be treated as expired, not as never-expires")
	}

	// POSITIVE CONTROL: the current generation of that same set still verifies,
	// so the rejection is about the missing expiry and not about the set being
	// unusable.
	if _, ok := verifyHeartbeatBearerAcrossGenerations(
		handEdited, hbBearerFor(genSecretB, hbHiveA), hbHiveA, now); !ok {
		t.Error("positive control: the current generation of the hand-edited set must verify")
	}
}

// TestHeartbeatBearerIsIdentityBoundUnderEveryGeneration IS THE F2 TEST.
//
// F2 was closed by deleting the fleet-wide lane, whose possession proved "some
// provisioned spoke" and never "THIS hive" — and because handleHeartbeat trusts
// the body-supplied hive_id, that let any spoke beat as any victim. Adding a
// generation must not re-open it by ANY route, so this asserts the binding holds
// for EVERY generation independently, not merely for the current one.
func TestHeartbeatBearerIsIdentityBoundUnderEveryGeneration(t *testing.T) {
	now := time.Now()
	gs := legacyGenerationSet(genSecretA).rotate(genSecretB, now, defaultVerifyWindow)

	acceptable := gs.acceptableGenerations(now)
	if len(acceptable) != 2 {
		t.Fatalf("precondition: want 2 acceptable generations, got %d — the cross-generation "+
			"case this test exists for would not be exercised", len(acceptable))
	}

	for _, g := range acceptable {
		bearerA := hbBearerFor(g.Secret, hbHiveA)
		if bearerA == "" {
			t.Fatalf("gen %d: could not derive hive A's bearer", g.ID)
		}

		// Hive A's bearer, from THIS generation, must be rejected for hive B.
		if _, ok := verifyHeartbeatBearerAcrossGenerations(gs, bearerA, hbHiveB, now); ok {
			t.Errorf("F2 REGRESSION: hive %q's bearer from generation %d authenticated as hive %q",
				hbHiveA, g.ID, hbHiveB)
		}

		// POSITIVE CONTROL: that same bearer IS accepted for hive A, so the
		// rejection above is the identity binding and not a dead verifier.
		if _, ok := verifyHeartbeatBearerAcrossGenerations(gs, bearerA, hbHiveA, now); !ok {
			t.Errorf("positive control: generation %d's bearer for hive %q must verify for hive %q",
				g.ID, hbHiveA, hbHiveA)
		}
	}
}

// TestHeartbeatFleetWideLaneStaysDeletedUnderGenerations is the second half of
// F2. The deleted lane was deriveDomainKey(master, infoHeartbeatKey) — no hive
// ID in the derivation, so one value worked everywhere. It must not verify under
// ANY generation. If it does, the rotation work re-opened a Critical finding.
func TestHeartbeatFleetWideLaneStaysDeletedUnderGenerations(t *testing.T) {
	now := time.Now()
	gs := legacyGenerationSet(genSecretA).rotate(genSecretB, now, defaultVerifyWindow)

	for _, master := range []string{genSecretA, genSecretB} {
		fleetWide := deriveDomainKey(master, infoHeartbeatKey)
		if fleetWide == "" {
			t.Fatal("precondition: could not derive the fleet-wide value")
		}
		for _, hive := range []string{hbHiveA, hbHiveB} {
			if _, ok := verifyHeartbeatBearerAcrossGenerations(gs, fleetWide, hive, now); ok {
				t.Errorf("F2 REGRESSION: the FLEET-WIDE bearer authenticated hive %q", hive)
			}
		}
	}

	// The RAW master must not authenticate either — that was the even older
	// pre-N1 credential.
	for _, master := range []string{genSecretA, genSecretB} {
		if _, ok := verifyHeartbeatBearerAcrossGenerations(gs, master, hbHiveA, now); ok {
			t.Error("the RAW master authenticated a heartbeat — pre-N1 lane resurrected")
		}
	}

	// POSITIVE CONTROL: the per-hive bearer from each generation still verifies,
	// so the rejections above are about the LANE and not about a verifier that
	// rejects everything.
	for _, master := range []string{genSecretA, genSecretB} {
		if _, ok := verifyHeartbeatBearerAcrossGenerations(gs, hbBearerFor(master, hbHiveA), hbHiveA, now); !ok {
			t.Error("positive control: the per-hive bearer must verify under its own generation")
		}
	}
}

// TestHeartbeatBearerFailsClosed pins the fail-closed edges: no bearer, no hive
// ID, no generation set. An identity-less caller must authenticate nothing no
// matter how many generations exist — derivePerHiveKey returns "" for an empty
// hive ID rather than silently falling back to a shared key, and that is what
// keeps the F2 fix load-bearing rather than incidental.
func TestHeartbeatBearerFailsClosed(t *testing.T) {
	now := time.Now()
	gs := legacyGenerationSet(genSecretA).rotate(genSecretB, now, defaultVerifyWindow)

	cases := []struct {
		name           string
		gs             *generationSet
		bearer, hiveID string
	}{
		{"empty bearer", gs, "", hbHiveA},
		{"empty hive id", gs, hbBearerFor(genSecretB, hbHiveA), ""},
		{"both empty", gs, "", ""},
		{"nil generation set", nil, hbBearerFor(genSecretB, hbHiveA), hbHiveA},
	}
	for _, tc := range cases {
		if _, ok := verifyHeartbeatBearerAcrossGenerations(tc.gs, tc.bearer, tc.hiveID, now); ok {
			t.Errorf("%s: must not authenticate", tc.name)
		}
	}

	// POSITIVE CONTROL: the fully-populated call succeeds.
	if _, ok := verifyHeartbeatBearerAcrossGenerations(gs, hbBearerFor(genSecretB, hbHiveA), hbHiveA, now); !ok {
		t.Error("positive control: a well-formed bearer must verify")
	}
}

// TestHeartbeatTrialIsBounded pins the cost argument. Trial verification is only
// acceptable because maxLiveGenerations caps it — an unbounded set would turn
// every heartbeat into unbounded HMAC work and hand an attacker a cheap
// asymmetric cost. Stack more rotations than the cap allows and the set must
// still never exceed it.
func TestHeartbeatTrialIsBounded(t *testing.T) {
	now := time.Now()
	gs := legacyGenerationSet(genSecretA)
	for i := 0; i < 5; i++ {
		gs = gs.rotate(genSecretB, now, defaultVerifyWindow)
		if gs == nil {
			t.Fatal("rotate returned nil")
		}
		if n := len(gs.acceptableGenerations(now)); n > maxLiveGenerations {
			t.Fatalf("after %d rotations the verifier would try %d generations, cap is %d",
				i+1, n, maxLiveGenerations)
		}
	}
}

// TestHubServerHeartbeatBearerUsesGenerations pins the SERVER wiring, not just
// the free function — a correct helper that nothing calls would not have fixed
// anything. It drives HubServer.verifyHeartbeatBearer, which is what
// handleHeartbeat actually calls.
func TestHubServerHeartbeatBearerUsesGenerations(t *testing.T) {
	now := time.Now()
	s := &HubServer{
		hubSecret:      genSecretB,
		keyGenerations: legacyGenerationSet(genSecretA).rotate(genSecretB, now, defaultVerifyWindow),
	}

	// Current-generation bearer.
	if !s.verifyHeartbeatBearer(hbBearerFor(genSecretB, hbHiveA), hbHiveA) {
		t.Error("the current-generation bearer must authenticate through the server")
	}
	// Previous-generation bearer — the not-yet-reconciled spoke.
	if !s.verifyHeartbeatBearer(hbBearerFor(genSecretA, hbHiveA), hbHiveA) {
		t.Error("a pre-rotation bearer must authenticate through the server during the window")
	}
	// F2 through the server: cross-hive must fail under BOTH generations.
	for _, master := range []string{genSecretA, genSecretB} {
		if s.verifyHeartbeatBearer(hbBearerFor(master, hbHiveA), hbHiveB) {
			t.Error("F2 REGRESSION via HubServer: hive A's bearer authenticated as hive B")
		}
	}
	// The telemetry accessor agrees that both are per-hive, so the auth-rollout
	// surface does not report a phantom regression mid-rotation.
	for _, master := range []string{genSecretA, genSecretB} {
		if !s.heartbeatBearerIsPerHive(hbBearerFor(master, hbHiveA), hbHiveA) {
			t.Error("heartbeatBearerIsPerHive must recognize a per-hive bearer from EITHER generation")
		}
	}

	// A server with NO generation set falls back to the single-master path
	// unchanged — hand-built test servers must not start failing closed.
	legacy := &HubServer{hubSecret: genSecretA}
	if !legacy.verifyHeartbeatBearer(hbBearerFor(genSecretA, hbHiveA), hbHiveA) {
		t.Error("a server with no generation set must still verify on the single-master path")
	}
	if legacy.verifyHeartbeatBearer(hbBearerFor(genSecretA, hbHiveA), hbHiveB) {
		t.Error("F2 REGRESSION on the single-master fallback: cross-hive authenticated")
	}
}
