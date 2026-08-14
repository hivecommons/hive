package hub

import (
	"errors"
	"os"
	"testing"
	"time"
)

// AUDIT 8 / F19 — the spoke-bound derivation path reads the hub's LIVE
// generation set.
//
// WHAT WENT WRONG, AND WHY NO TEST CAUGHT IT. provisionGenerationSet() used to
// consult a package-level override that only a test helper ever wrote, then fall
// through to legacyGenerationSet(provisionMasterSecret()) — generation 1 rebuilt
// from a file rotateMasterSecret never rewrites. The hub's MINTING side used the
// real persisted set; the SPOKE-BOUND side used that frozen one. A rotation
// changed what the hub minted and nothing about what the fleet held.
//
// The existing coverage could not see it because it was split across two test
// files that never referenced each other: hub_generations_store_test.go drove
// rotateMasterSecret, perhive_env_generation_test.go drove desiredPerHiveEnv
// through the test-only seam. Each half passed. The join — "a real rotation
// changes what a spoke would be given" — was the thing nobody asserted, and it
// was the only thing that was false.
//
// So the load-bearing test in this file is TestRotationReachesDesiredPerHiveEnv:
// it spans rotateMasterSecret -> desiredPerHiveEnv with NO seam in between.

const (
	f19SecretA = "f19-wiring-test-master-alpha"
	f19SecretB = "f19-wiring-test-master-bravo"
)

// withCleanLiveGenerations isolates the package-level live-generation pointer
// for a test and restores it afterwards.
//
// Every test in this file must take it. The pointer is process-global by
// design (see liveGenerations), so a test that installed a set and did not
// restore it would silently change what every LATER test in the package
// derives — the kind of cross-test bleed that makes a suite pass for the wrong
// reason. Restoring rather than nil-ing keeps that honest under -count=N.
func withCleanLiveGenerations(t *testing.T) {
	t.Helper()
	prevSet, prevUntrusted := liveGenerations, liveGenerationsUntrusted
	setLiveGenerations(nil, false)
	t.Cleanup(func() { setLiveGenerations(prevSet, prevUntrusted) })
}

// TestRotationReachesDesiredPerHiveEnv is THE test whose absence let F19 ship.
//
// It spans rotateMasterSecret -> desiredPerHiveEnv with no test seam in the
// middle: provisionGenerationsOverride is explicitly left nil, so the only way
// desiredPerHiveEnv can see the new generation is if the production wiring
// actually connects them. Against the pre-fix code every assertion in the
// "after" block fails, because desiredPerHiveEnv keeps deriving from the raw
// master file that rotation never touches.
func TestRotationReachesDesiredPerHiveEnv(t *testing.T) {
	withTempGenerationsPath(t)
	withCleanLiveGenerations(t)
	// The raw master file stays on A forever — rotateMasterSecret never rewrites
	// hub-secret.key, which is exactly why reading it is wrong after a rotation.
	withTestMaster(t, f19SecretA)
	// Assert the seam is NOT in play. If a future edit made this file rely on
	// it, this test would silently go back to proving only what the old one did.
	if provisionGenerationsOverride != nil {
		t.Fatal("provisionGenerationsOverride must be nil here: this test exists to prove the PRODUCTION path is wired")
	}

	const hiveID = "hive-f19"
	s := newRotationTestHub(t, f19SecretA)
	// Install the pre-rotation set the way NewHubServer does.
	setLiveGenerations(s.keyGenerations, false)

	before := desiredPerHiveEnv(hiveID)
	if before == nil {
		t.Fatal("desiredPerHiveEnv returned nil before rotation")
	}
	// PRECONDITION / POSITIVE CONTROL for the un-rotated direction: before any
	// rotation the wiring must be byte-identical to the raw-master derivation.
	// Without this the test could pass by breaking the un-rotated case.
	for _, name := range perHiveEnvNames() {
		if before[name] != rawMasterDerivation(f19SecretA, hiveID)[name] {
			t.Fatalf("%s: pre-rotation derivation already differs from the raw master — "+
				"this change must be invisible on a never-rotated hub", name)
		}
	}

	next, _, err := s.rotateMasterSecret(time.Now(), false)
	if err != nil {
		t.Fatalf("rotateMasterSecret: %v", err)
	}
	newSecret := next.currentSecret()
	if newSecret == "" || newSecret == f19SecretA {
		t.Fatal("fixture: rotation did not produce a new current secret")
	}

	after := desiredPerHiveEnv(hiveID)
	if after == nil {
		t.Fatal("desiredPerHiveEnv returned nil after rotation")
	}

	// THE ASSERTION. Every mandatory var must now be derived from the NEW
	// generation's secret, computed here independently of the code under test.
	wantAfter := map[string]string{
		EnvHeartbeatKey:     derivePerHiveKey(newSecret, infoHeartbeatKey, hiveID),
		EnvTerminalKey:      derivePerHiveKey(newSecret, infoTerminalKey, hiveID),
		EnvInviteKey:        derivePerHiveKey(newSecret, infoInviteKey, hiveID),
		EnvSessionKey:       deriveDomainKey(newSecret, infoSessionKey),
		EnvSSOPublicKey:     ssoPublicKeyFromSeed(deriveDomainKey(newSecret, infoSSOEd25519Seed)),
		envSessionPublicKey: ssoPublicKeyFromSeed(deriveDomainKey(newSecret, infoSessionEd25519Seed)),
	}
	// COUNT FLOOR, same discipline as TestDesiredPerHiveEnvUsesCurrentNotPrevious:
	// a var dropped from perHiveEnvNames() must fail here rather than silently
	// stop being asserted.
	if len(wantAfter) != len(perHiveEnvNames()) {
		t.Fatalf("perHiveEnvNames() has %d vars but this test covers %d — a reconciled var is unasserted after rotation",
			len(perHiveEnvNames()), len(wantAfter))
	}
	for _, name := range perHiveEnvNames() {
		if after[name] != wantAfter[name] {
			t.Errorf("%s: NOT derived from the new generation after rotation — "+
				"the reconcile sweep would see no drift and patch nothing (F19)", name)
		}
		if after[name] == before[name] {
			t.Errorf("%s: unchanged by rotation — rotation is inert on the spoke-bound path (F19)", name)
		}
	}

	// And specifically NOT the raw master file's derivation, which is the exact
	// value the pre-fix code returned.
	stale := rawMasterDerivation(provisionMasterSecret(), hiveID)
	for _, name := range perHiveEnvNames() {
		if after[name] == stale[name] {
			t.Errorf("%s: still derived from the raw hub-secret.key master after a rotation — "+
				"this is the F19 inertness verbatim", name)
		}
	}

	// The provisioning template must move in lockstep with the reconcile, or the
	// sweep would fight provisioning and roll a freshly created pod every cycle.
	if provisionHeartbeatKey(hiveID) != wantAfter[EnvHeartbeatKey] {
		t.Error("provisionHeartbeatKey did not follow the rotation — provision and reconcile would fight")
	}
	if provisionTerminalKey(hiveID) != wantAfter[EnvTerminalKey] {
		t.Error("provisionTerminalKey did not follow the rotation")
	}
	if provisionInviteKey(hiveID) != wantAfter[EnvInviteKey] {
		t.Error("provisionInviteKey did not follow the rotation")
	}

	// The hub's own minting set and the spoke-bound set must be the SAME object
	// after a rotation. Two equal-but-separate sets would pass every assertion
	// above and still drift on the next rotation.
	if provisionGenerationSet() != s.currentGenerations() {
		t.Error("the derivation set and the hub's minting set are different objects — they can drift")
	}
}

// rawMasterDerivation reproduces the PRE-generation expressions literally, the
// same way TestPerHiveEnvGenerationIsAReadPathChangeToday does. Used as the
// byte-identity reference in both directions.
func rawMasterDerivation(master, hiveID string) map[string]string {
	return map[string]string{
		EnvHeartbeatKey:     derivePerHiveKey(master, infoHeartbeatKey, hiveID),
		EnvTerminalKey:      derivePerHiveKey(master, infoTerminalKey, hiveID),
		EnvInviteKey:        derivePerHiveKey(master, infoInviteKey, hiveID),
		EnvSessionKey:       deriveDomainKey(master, infoSessionKey),
		EnvSSOPublicKey:     ssoPublicKeyFromSeed(deriveDomainKey(master, infoSSOEd25519Seed)),
		envSessionPublicKey: ssoPublicKeyFromSeed(deriveDomainKey(master, infoSessionEd25519Seed)),
	}
}

// TestUnrotatedHubDerivesByteIdenticallyAfterWiring is the positive control in
// the OTHER direction, and the reason this change is safe to ship to a fleet
// that has never rotated.
//
// The whole fleet is on generationsNeverRotated today. If wiring the live set
// changed ANY derived byte on such a hub, the next reconcile sweep would see
// drift on all 70 spokes and roll every tenant pod. So: with a single
// generation installed, every var must equal the literal pre-generation
// expression, exactly as TestPerHiveEnvGenerationIsAReadPathChangeToday pins it.
func TestUnrotatedHubDerivesByteIdenticallyAfterWiring(t *testing.T) {
	withCleanLiveGenerations(t)
	withTestMaster(t, f19SecretA)
	const hiveID = "hive-f19"

	want := rawMasterDerivation(f19SecretA, hiveID)

	t.Run("no hub constructed: falls back to the legacy set", func(t *testing.T) {
		// liveGenerations is nil here — the state of every unit test that
		// derives keys without building a HubServer.
		got := desiredPerHiveEnv(hiveID)
		if got == nil {
			t.Fatal("desiredPerHiveEnv returned nil with no live set installed")
		}
		for _, name := range perHiveEnvNames() {
			if got[name] != want[name] {
				t.Errorf("%s changed with no live set installed — the fallback is not byte-identical", name)
			}
		}
	})

	t.Run("never-rotated hub: live set installed, still byte-identical", func(t *testing.T) {
		setLiveGenerations(legacyGenerationSet(f19SecretA), false)
		got := desiredPerHiveEnv(hiveID)
		if got == nil {
			t.Fatal("desiredPerHiveEnv returned nil on a never-rotated hub")
		}
		for _, name := range perHiveEnvNames() {
			if got[name] != want[name] {
				t.Errorf("%s changed on a never-rotated hub — this would roll all 70 tenant pods on the next sweep", name)
			}
		}
		// No _PREV vars on a hub with one generation: emitting them would add a
		// var to every Deployment in the fleet.
		for _, name := range perHiveEnvOptionalNames() {
			if _, present := got[name]; present {
				t.Errorf("%s present on a never-rotated hub — there is no previous generation", name)
			}
		}
	})

	// POSITIVE CONTROL. Everything above is an equality check, which "always
	// return the raw-master derivation" would also satisfy — that is precisely
	// the pre-fix behaviour. Prove the installed set is actually consulted.
	t.Run("positive control: an installed ROTATED set does change the output", func(t *testing.T) {
		rotated := legacyGenerationSet(f19SecretA).rotate(f19SecretB, time.Now(), defaultVerifyWindow)
		setLiveGenerations(rotated, false)
		got := desiredPerHiveEnv(hiveID)
		if got == nil {
			t.Fatal("desiredPerHiveEnv returned nil with a rotated set installed")
		}
		for _, name := range perHiveEnvNames() {
			if got[name] == want[name] {
				t.Fatalf("%s is unchanged by an installed ROTATED set — the live set is being ignored, "+
					"which means the byte-identity assertions above are vacuous", name)
			}
		}
	})
}

// TestUntrustedGenerationsRefusesDerivation is the F20 handoff.
//
// When the loader cannot establish which generation is current, the derivation
// path must REFUSE rather than re-derive its own notion from the raw master
// file. That file holds SUPERSEDED generation-1 material once a rotation has
// happened, so falling back would write keys from a retired generation onto
// live Deployments and drag the whole fleet backwards — F20's silent
// un-rotation, reintroduced on the side F20 did not cover.
func TestUntrustedGenerationsRefusesDerivation(t *testing.T) {
	withCleanLiveGenerations(t)
	withTestMaster(t, f19SecretA)
	const hiveID = "hive-f19"

	// PRECONDITION: with a trusted set this hive derives fine. Without this the
	// test could pass because desiredPerHiveEnv is broken for some other reason.
	setLiveGenerations(legacyGenerationSet(f19SecretA), false)
	if desiredPerHiveEnv(hiveID) == nil {
		t.Fatal("precondition: desiredPerHiveEnv returned nil for a TRUSTED set")
	}

	setLiveGenerations(nil, true)

	if gs := provisionGenerationSet(); gs != nil {
		t.Error("provisionGenerationSet returned a set while the generation state is UNTRUSTED — " +
			"it re-derived from the raw master, which is superseded material after a rotation")
	}
	if got := provisionCurrentSecret(); got != "" {
		t.Error("provisionCurrentSecret returned a non-empty secret while UNTRUSTED (value withheld)")
	}
	if got := desiredPerHiveEnv(hiveID); got != nil {
		t.Errorf("desiredPerHiveEnv returned %d vars while UNTRUSTED — the sweep would patch spokes "+
			"with keys derived from a generation the hub cannot vouch for", len(got))
	}
	// The per-hive provisioning helpers must fail closed the same way, or a hive
	// created during the untrusted window would be handed stale keys.
	if provisionHeartbeatKey(hiveID) != "" {
		t.Error("provisionHeartbeatKey derived a key while UNTRUSTED")
	}
	if provisionTerminalKey(hiveID) != "" {
		t.Error("provisionTerminalKey derived a key while UNTRUSTED")
	}
	if provisionInviteKey(hiveID) != "" {
		t.Error("provisionInviteKey derived a key while UNTRUSTED")
	}

	// NOT-nil control: untrusted must be distinguishable from "no hub in this
	// process". Same nil set, untrusted cleared — derivation resumes. If these
	// two collapsed into one state, every unit test that never builds a hub
	// would take the refuse path.
	setLiveGenerations(nil, false)
	if desiredPerHiveEnv(hiveID) == nil {
		t.Error("clearing the untrusted flag did not restore derivation — " +
			"\"untrusted\" and \"no hub constructed\" have been collapsed into one state")
	}
}

// TestRotationRefusedWhileFleetNotFullyObserved is the F19/F21 stranding
// interlock: the guarantee that wiring F19 today cannot strand the 44 spokes on
// pull-only clusters.
//
// Retirement at verify_until is UNCONDITIONAL by design, so a spoke the sweep
// never converged is a spoke that hard-fails heartbeat 7 days after a rotation.
// The only safe place to stop that is before the rotation starts.
func TestRotationRefusedWhileFleetNotFullyObserved(t *testing.T) {
	withTempGenerationsPath(t)
	withCleanLiveGenerations(t)
	withTestMaster(t, f19SecretA)

	// The shape of the production hub: hives on a pull-only cluster the sweep
	// counted but could not read. 26 reachable, 44 not — the F21 census.
	unobservedHub := func() *HubServer {
		s := newRotationTestHub(t, f19SecretA)
		s.perHiveEnvConsidered = 70
		s.perHiveEnvUnreachable = 44
		s.perHiveEnvUnreachableClusters = []string{"a-ks-wec2", "vllm-d"}
		return s
	}

	t.Run("refused with unreachable spokes", func(t *testing.T) {
		s := unobservedHub()
		next, decision, err := s.rotateMasterSecret(time.Now(), false)
		if !errors.Is(err, errRotationWouldStrandSpokes) {
			t.Fatalf("err = %v, want errRotationWouldStrandSpokes", err)
		}
		if next != nil {
			t.Error("a refused rotation returned a new generation set")
		}
		if decision.Allowed {
			t.Error("decision.Allowed = true on a refused rotation")
		}
		// Nothing may have changed: not the in-memory set, not the file.
		if s.currentGenerations().Current != legacyGenerationID {
			t.Error("the refused rotation still advanced the current generation")
		}
		if _, statErr := os.Stat(hubGenerationsPath); statErr == nil {
			t.Error("the refused rotation persisted a generations file")
		}
	})

	// force must NOT override this. It exists to override the convergence
	// COOLDOWN — "the current generation is compromised, accept the cost". With
	// 44 spokes structurally unreachable the cost is not lag, it is a
	// guaranteed outage with no in-band way to avert it.
	t.Run("force does NOT override the stranding interlock", func(t *testing.T) {
		s := unobservedHub()
		if _, _, err := s.rotateMasterSecret(time.Now(), true); !errors.Is(err, errRotationWouldStrandSpokes) {
			t.Fatalf("force bypassed the stranding interlock: err = %v", err)
		}
	})

	// A sweep that observed NOTHING is not a fully observed fleet. This is the
	// fail-closed direction: a hub that has not run a sweep yet must not be
	// treated as having a clean bill of health.
	// Built inline rather than via newRotationTestHub, which now DECLARES a
	// fully observed fleet so the rotation-mechanics tests can run. This case
	// needs the opposite: a hub whose sweep has never populated anything.
	t.Run("a hub that has never swept is refused", func(t *testing.T) {
		s := &HubServer{
			logger:          quietLogger(),
			hubSecret:       f19SecretA,
			keyGenerations:  legacyGenerationSet(f19SecretA),
			lastKeyRotation: time.Time{},
			// perHiveEnvConsidered and perHiveEnvUnreachable both zero: no sweep
			// has run. "Nothing observed" is not "everything observed".
		}
		if _, _, err := s.rotateMasterSecret(time.Now(), false); !errors.Is(err, errRotationWouldStrandSpokes) {
			t.Fatalf("a never-swept hub was allowed to rotate: err = %v", err)
		}
	})

	// POSITIVE CONTROL. Every case above asserts a REFUSAL, and "always refuse"
	// would satisfy all of them — which would make F19's wiring permanently
	// unreachable and this whole change a no-op. Prove a fully observed fleet
	// still rotates, and that the rotation reaches the derivation path.
	t.Run("positive control: a fully observed fleet DOES rotate", func(t *testing.T) {
		withCleanLiveGenerations(t)
		s := newRotationTestHub(t, f19SecretA)
		setLiveGenerations(s.keyGenerations, false)
		next, _, err := s.rotateMasterSecret(time.Now(), false)
		if err != nil {
			t.Fatalf("a fully observed fleet was refused: %v", err)
		}
		if next.Current == legacyGenerationID {
			t.Fatal("rotation did not advance the generation")
		}
		if provisionCurrentSecret() != next.currentSecret() {
			t.Error("the rotation did not reach the derivation path even though it was allowed")
		}
	})
}

// TestRotationInterlockMatchesFleetFullyObserved pins fleetObservation()'s
// predicate to PerHiveEnvSnapshot().FleetFullyObserved.
//
// There are now two expressions of "did the sweep see everything" — the
// operator-facing one on the readiness surface and the enforcing one in the
// rotate path. Two copies of one predicate is exactly the drift hazard the four
// VerifyUntil readers are held against, so assert them equal across the cases
// that distinguish them rather than trusting the copies to stay in step.
func TestRotationInterlockMatchesFleetFullyObserved(t *testing.T) {
	cases := []struct {
		name        string
		considered  int
		unreachable int
	}{
		{"nothing swept", 0, 0},
		{"fully observed", 22, 0},
		{"one unreachable", 70, 44},
		{"all unreachable", 44, 44},
		{"unreachable with nothing considered", 0, 3},
	}
	sawTrue, sawFalse := false, false
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &HubServer{
				keyGenerations:        legacyGenerationSet(f19SecretA),
				perHiveEnvSeen:        map[string]perHiveEnvObservation{},
				perHiveEnvConsidered:  tc.considered,
				perHiveEnvUnreachable: tc.unreachable,
			}
			want := s.PerHiveEnvSnapshot().FleetFullyObserved
			got := s.fleetObservation().FullyObserved
			if got != want {
				t.Errorf("fleetObservation().FullyObserved = %v but PerHiveEnvSnapshot().FleetFullyObserved = %v — "+
					"the rotate interlock and the readiness surface disagree about what a fully observed fleet is",
					got, want)
			}
			if got {
				sawTrue = true
			} else {
				sawFalse = true
			}
		})
	}
	// POSITIVE CONTROL: the table must exercise both outcomes, or "always equal"
	// would be proven by a table that only ever produces one of them.
	if !sawTrue || !sawFalse {
		t.Fatalf("degenerate table: sawTrue=%v sawFalse=%v — the equality is only meaningful across both outcomes",
			sawTrue, sawFalse)
	}
}

// TestRetirementKeepsDerivationSetInLockstep. Retirement replaces the
// generation set without changing the CURRENT generation, so the six mandatory
// vars are unaffected — but it does drop PREVIOUS generations, and those are
// what the two _PREV public-key vars are derived from. If the derivation set
// were not updated, the sweep would keep advertising a _PREV public key for a
// generation the hub has just stopped accepting.
func TestRetirementKeepsDerivationSetInLockstep(t *testing.T) {
	withTempGenerationsPath(t)
	withCleanLiveGenerations(t)
	// The ambient master is B, the generation that SURVIVES the retirement.
	//
	// This mirrors the real post-rotation hub rather than the fixture's
	// convenience: generationSetDescendsFrom requires the installed set to still
	// contain the ambient master, and retirement drops generation A. Pinning the
	// ambient master to A would make the set legitimately stale the instant A is
	// retired, and the test would be asserting the staleness fallback rather than
	// the lockstep it is named for.
	withTestMaster(t, f19SecretB)
	const hiveID = "hive-f19"

	now := time.Now()
	// A rotation whose verify window has already closed: the previous
	// generation is retirable right now.
	expired := legacyGenerationSet(f19SecretA).
		rotate(f19SecretB, now.Add(-2*defaultVerifyWindow), defaultVerifyWindow)
	s := newRotationTestHub(t, f19SecretB)
	s.keyGenerations = expired
	setLiveGenerations(expired, false)

	// PRECONDITION: while the previous generation is still in the set, the
	// _PREV vars are absent because the window has closed (they track
	// ACCEPTANCE, not mere presence). Assert the current-generation vars are
	// present so the post-retirement comparison is meaningful.
	beforeEnv := desiredPerHiveEnv(hiveID)
	if beforeEnv == nil {
		t.Fatal("precondition: desiredPerHiveEnv returned nil before retirement")
	}

	retired, err := s.retireExpiredGenerations(now)
	if err != nil {
		t.Fatalf("retireExpiredGenerations: %v", err)
	}
	if len(retired) == 0 {
		t.Fatal("fixture: nothing was retired, so this test asserts nothing")
	}

	// The derivation set must be the retired-from set, not the stale one.
	if provisionGenerationSet() != s.currentGenerations() {
		t.Error("retirement left the derivation set pointing at the pre-retirement object")
	}
	for _, g := range provisionGenerationSet().Generations {
		for _, id := range retired {
			if g.ID == id {
				t.Errorf("generation %d was retired but is still in the derivation set — "+
					"the hub would keep handing spokes a public key it no longer accepts", id)
			}
		}
	}
	// The CURRENT generation did not move, so the mandatory vars must not have.
	afterEnv := desiredPerHiveEnv(hiveID)
	if afterEnv == nil {
		t.Fatal("desiredPerHiveEnv returned nil after retirement")
	}
	for _, name := range perHiveEnvNames() {
		if afterEnv[name] != beforeEnv[name] {
			t.Errorf("%s changed on RETIREMENT — retirement does not move the current generation "+
				"and must not roll a single tenant pod", name)
		}
	}
	// And no _PREV var may survive the retirement of its generation.
	for _, name := range perHiveEnvOptionalNames() {
		if _, present := afterEnv[name]; present {
			t.Errorf("%s still emitted after the previous generation was retired", name)
		}
	}
}
