package hub

import (
	"testing"
	"time"
)

// Tests for follow-on PR #3: the per-hive env reconcile derives from the
// CURRENT generation, and PerHiveEnvStatus reports a per-generation breakdown.
//
// Two properties are load-bearing across this file:
//
//   - Provision-time and reconcile-time derivation must agree EXACTLY. They now
//     both resolve their master through provisionGenerationSet(), so a rotation
//     cannot move one without the other. Divergence would make the sweep fight
//     provisioning and roll a pod every cycle forever.
//   - A ZERO VerifyUntil means ALREADY EXPIRED, never "never expires". This is
//     the property that stops a previous generation being pinned live forever
//     by a hand-edited file, and it is asserted here on the retirement surface
//     as well as in hub_generations_test.go on the acceptance surface.

// genTestSecretA / B are fixed non-production masters. Only ever compared to
// each other, never to any real value.
const (
	perHiveGenSecretA = "perhive-generation-test-master-alpha"
	perHiveGenSecretB = "perhive-generation-test-master-bravo"
)

// withProvisionGenerations points provisionGenerationSet() at an explicit set
// for the duration of a test, and restores it afterwards. Without this the
// rotated case is untestable — see provisionGenerationsOverride.
func withProvisionGenerations(t *testing.T, gs *generationSet) {
	t.Helper()
	prev := provisionGenerationsOverride
	provisionGenerationsOverride = gs
	t.Cleanup(func() { provisionGenerationsOverride = prev })
}

// TestDesiredPerHiveEnvUsesCurrentNotPrevious is the test that can actually
// FAIL if desiredPerHiveEnv reverts to the raw master.
//
// It is the only one that can. With a single generation — the state of every
// hub today — provisionCurrentSecret() and provisionMasterSecret() return the
// same string, so swapping one for the other changes nothing observable. This
// test therefore installs a ROTATED set whose current secret differs from the
// raw master, and asserts the derived material follows the CURRENT generation
// and specifically is NOT the previous generation's.
func TestDesiredPerHiveEnvUsesCurrentNotPrevious(t *testing.T) {
	// The raw master stays on generation A. The rotated set's current is B.
	withTestMaster(t, perHiveGenSecretA)
	now := time.Now()
	rotated := legacyGenerationSet(perHiveGenSecretA).rotate(perHiveGenSecretB, now, defaultVerifyWindow)
	if rotated == nil || rotated.currentSecret() != perHiveGenSecretB {
		t.Fatal("fixture: rotate did not make secret B current")
	}
	withProvisionGenerations(t, rotated)

	const hiveID = "hive-alpha"
	got := desiredPerHiveEnv(hiveID)
	if got == nil {
		t.Fatal("desiredPerHiveEnv returned nil")
	}

	fromCurrent := map[string]string{
		EnvHeartbeatKey:     derivePerHiveKey(perHiveGenSecretB, infoHeartbeatKey, hiveID),
		EnvTerminalKey:      derivePerHiveKey(perHiveGenSecretB, infoTerminalKey, hiveID),
		EnvInviteKey:        derivePerHiveKey(perHiveGenSecretB, infoInviteKey, hiveID),
		EnvSSOPublicKey:     ssoPublicKeyFromSeed(deriveDomainKey(perHiveGenSecretB, infoSSOEd25519Seed)),
		envSessionPublicKey: ssoPublicKeyFromSeed(deriveDomainKey(perHiveGenSecretB, infoSessionEd25519Seed)),
	}
	fromPrevious := map[string]string{
		EnvHeartbeatKey:     derivePerHiveKey(perHiveGenSecretA, infoHeartbeatKey, hiveID),
		EnvTerminalKey:      derivePerHiveKey(perHiveGenSecretA, infoTerminalKey, hiveID),
		EnvInviteKey:        derivePerHiveKey(perHiveGenSecretA, infoInviteKey, hiveID),
		EnvSSOPublicKey:     ssoPublicKeyFromSeed(deriveDomainKey(perHiveGenSecretA, infoSSOEd25519Seed)),
		envSessionPublicKey: ssoPublicKeyFromSeed(deriveDomainKey(perHiveGenSecretA, infoSessionEd25519Seed)),
	}

	// COUNT FLOOR. The loop below iterates perHiveEnvNames(), so a change that
	// DROPS a var from that list would also drop it from every assertion and
	// this test would pass while the var silently stopped being rotated. Pin the
	// expected set explicitly against the fixture so narrowing the list is a
	// failure rather than a no-op. (Observed: removing EnvInviteKey from the
	// reconcile made this test pass until this guard was added.)
	if len(fromCurrent) != len(perHiveEnvNames()) {
		t.Fatalf("perHiveEnvNames() has %d vars but the fixture covers %d — a reconciled var is unasserted",
			len(perHiveEnvNames()), len(fromCurrent))
	}
	for name := range fromCurrent {
		if _, ok := got[name]; !ok {
			t.Errorf("%s: absent from desiredPerHiveEnv — it would never converge after a rotation", name)
		}
	}

	for _, name := range perHiveEnvNames() {
		if got[name] != fromCurrent[name] {
			t.Errorf("%s: not derived from the CURRENT generation", name)
		}
		if got[name] == fromPrevious[name] {
			t.Errorf("%s: still derived from the PREVIOUS generation — rotation would never converge", name)
		}
	}

	// The provisioning template must move in lockstep. If provisioning stayed
	// on the old master while the reconcile moved, the sweep would see drift on
	// every freshly provisioned hive and roll its pod every cycle forever.
	if provisionHeartbeatKey(hiveID) != fromCurrent[EnvHeartbeatKey] {
		t.Error("provisionHeartbeatKey did not follow the current generation — provision/reconcile would fight")
	}
	if provisionTerminalKey(hiveID) != fromCurrent[EnvTerminalKey] {
		t.Error("provisionTerminalKey did not follow the current generation")
	}
	if provisionInviteKey(hiveID) != fromCurrent[EnvInviteKey] {
		t.Error("provisionInviteKey did not follow the current generation")
	}
	if provisionSessionPublicKey() != fromCurrent[envSessionPublicKey] {
		t.Error("provisionSessionPublicKey did not follow the current generation")
	}

	// POSITIVE CONTROL: the fixture is only meaningful if the two generations
	// actually derive different material. If A and B collided every assertion
	// above would be vacuous.
	for _, name := range perHiveEnvNames() {
		if fromCurrent[name] == fromPrevious[name] {
			t.Fatalf("fixture is degenerate: %s derives identically from both generations", name)
		}
	}
}

// TestDesiredPerHiveEnvDerivesFromCurrentGeneration is the core PR #3
// assertion: the reconcile's desired values are a function of the CURRENT
// generation's secret, not of the raw master file.
//
// Today there is only ever one generation (nothing can create a second until
// PR #4), so this test does the derivation by hand rather than by rotating the
// hub: it asserts that desiredPerHiveEnv's output equals what the generation
// set's currentSecret() produces, which is the invariant PR #4 will start
// exercising for real.
func TestDesiredPerHiveEnvDerivesFromCurrentGeneration(t *testing.T) {
	withTestMaster(t, perHiveGenSecretA)
	const hiveID = "hive-alpha"

	gs := provisionGenerationSet()
	if gs == nil {
		t.Fatal("provisionGenerationSet returned nil for a configured master")
	}
	if gs.currentSecret() != perHiveGenSecretA {
		t.Fatalf("current generation secret does not match the configured master")
	}

	want := desiredPerHiveEnv(hiveID)
	if want == nil {
		t.Fatal("desiredPerHiveEnv returned nil for a valid master + hive ID")
	}

	cur := gs.currentSecret()
	expect := map[string]string{
		EnvHeartbeatKey:     derivePerHiveKey(cur, infoHeartbeatKey, hiveID),
		EnvTerminalKey:      derivePerHiveKey(cur, infoTerminalKey, hiveID),
		EnvSSOPublicKey:     ssoPublicKeyFromSeed(deriveDomainKey(cur, infoSSOEd25519Seed)),
		envSessionPublicKey: ssoPublicKeyFromSeed(deriveDomainKey(cur, infoSessionEd25519Seed)),
	}
	for name, v := range expect {
		if v == "" {
			t.Fatalf("generation-side derivation for %s is empty; test setup is wrong", name)
		}
		if want[name] != v {
			t.Errorf("%s: desiredPerHiveEnv does not derive from the current generation", name)
		}
	}
}

// TestPerHiveEnvGenerationIsAReadPathChangeToday is the "no behaviour change"
// proof the task asks for explicitly.
//
// Until PR #4 lands there is no way to create a second generation, so the
// generation set is always legacyGenerationSet(provisionMasterSecret()) and
// every derived value must be BYTE-IDENTICAL to what the pre-generation code
// produced. The pre-generation expressions are reproduced literally on the
// right-hand side.
func TestPerHiveEnvGenerationIsAReadPathChangeToday(t *testing.T) {
	withTestMaster(t, perHiveGenSecretA)
	const hiveID = "hive-alpha"

	if got, want := provisionCurrentSecret(), provisionMasterSecret(); got != want {
		t.Fatalf("current-generation secret differs from the raw master before any rotation exists")
	}

	master := provisionMasterSecret() // the PRE-generation expression
	preGeneration := map[string]string{
		EnvHeartbeatKey:     derivePerHiveKey(master, infoHeartbeatKey, hiveID),
		EnvTerminalKey:      derivePerHiveKey(master, infoTerminalKey, hiveID),
		EnvSSOPublicKey:     ssoPublicKeyFromSeed(deriveDomainKey(master, infoSSOEd25519Seed)),
		envSessionPublicKey: ssoPublicKeyFromSeed(deriveDomainKey(master, infoSessionEd25519Seed)),
	}

	got := desiredPerHiveEnv(hiveID)
	if got == nil {
		t.Fatal("desiredPerHiveEnv returned nil")
	}
	for name, v := range preGeneration {
		if got[name] != v {
			t.Errorf("%s changed value: this PR must be a read-path change with no fleet-visible effect", name)
		}
	}

	// Issue #3234: HIVE_SESSION_KEY must be ABSENT, not merely re-derived.
	// TestSessionKeyIsPermanentlyRemoved (perhive_env_reconcile_test.go) covers
	// the removal itself; this only guards against it silently reappearing in
	// the generation-aware map this test exercises.
	if _, present := got[EnvSessionKey]; present {
		t.Error("desiredPerHiveEnv wants HIVE_SESSION_KEY under a rotated generation — issue #3234 removal regressed")
	}
}

// TestPerHiveEnvEmptyGuardStillFailsSafe re-asserts the guard the task singles
// out, against the NEW master expression.
//
// The reason it matters is not obvious from the code: HIVE_HEARTBEAT_KEY=""
// falls back to the master exactly like an ABSENT var, so the spoke keeps
// working — while making the readiness count report that hive as CONVERGED. A
// silently-wrong "converged" is worse than a visible drift, which is why
// desiredPerHiveEnv must return nil rather than a map with an empty value in
// it.
func TestPerHiveEnvEmptyGuardStillFailsSafe(t *testing.T) {
	t.Run("nil generation set derives nothing", func(t *testing.T) {
		var gs *generationSet
		if gs.currentSecret() != "" {
			t.Fatal("nil generation set must yield an empty current secret")
		}
		if deriveDomainKey(gs.currentSecret(), infoSessionKey) != "" {
			t.Fatal("an empty current secret must derive to \"\" (fail closed)")
		}
	})

	t.Run("empty master short-circuits before deriving", func(t *testing.T) {
		withTestMaster(t, "")
		if provisionGenerationSet() != nil {
			t.Fatal("provisionGenerationSet with no master must be nil, not an empty-secret set")
		}
		if got := desiredPerHiveEnv("hive-alpha"); got != nil {
			t.Fatalf("desiredPerHiveEnv with no master = %v, want nil", got)
		}
	})

	t.Run("empty hive ID short-circuits", func(t *testing.T) {
		withTestMaster(t, perHiveGenSecretA)
		if got := desiredPerHiveEnv(""); got != nil {
			t.Fatalf("desiredPerHiveEnv(\"\") = %v, want nil", got)
		}
	})

	// POSITIVE CONTROL for the three sub-tests above. "always return nil" would
	// satisfy every one of them, so assert the opposite direction: a valid
	// master and hive ID DO produce a complete map with no empty value in it.
	t.Run("positive control: valid inputs produce all five non-empty", func(t *testing.T) {
		withTestMaster(t, perHiveGenSecretA)
		got := desiredPerHiveEnv("hive-alpha")
		if got == nil {
			t.Fatal("valid master + hive ID returned nil — the guard is too aggressive")
		}
		if len(got) != len(perHiveEnvNames()) {
			t.Fatalf("got %d vars, want %d", len(got), len(perHiveEnvNames()))
		}
		for _, name := range perHiveEnvNames() {
			if got[name] == "" {
				t.Errorf("%s is empty — exactly the value that must never reach a Deployment", name)
			}
		}
	})
}

// perHiveEnvForGeneration builds a live Deployment env list for hiveID as it
// would look if provisioned from the given generation's secret.
func perHiveEnvForGeneration(secret, hiveID string) []deploymentEnvVar {
	return []deploymentEnvVar{
		{Name: "HIVE_ID", Value: hiveID},
		{Name: EnvHeartbeatKey, Value: derivePerHiveKey(secret, infoHeartbeatKey, hiveID)},
		{Name: EnvTerminalKey, Value: derivePerHiveKey(secret, infoTerminalKey, hiveID)},
		{Name: EnvSessionKey, Value: derivePerHiveKey(secret, infoSessionKey, hiveID)},
		{Name: EnvSSOPublicKey, Value: ssoPublicKeyFromSeed(deriveDomainKey(secret, infoSSOEd25519Seed))},
		{Name: envSessionPublicKey, Value: ssoPublicKeyFromSeed(deriveDomainKey(secret, infoSessionEd25519Seed))},
	}
}

// TestPerHiveEnvGenerationAttribution covers the classifier that decides which
// generation a spoke is on.
func TestPerHiveEnvGenerationAttribution(t *testing.T) {
	now := time.Now()
	gs := legacyGenerationSet(perHiveGenSecretA).rotate(perHiveGenSecretB, now, defaultVerifyWindow)
	if gs == nil {
		t.Fatal("rotate returned nil")
	}
	acceptable := gs.acceptableGenerations(now)
	if len(acceptable) != 2 {
		t.Fatalf("expected current + previous acceptable, got %d", len(acceptable))
	}
	// Deliberately NOT the "hive-alpha" used elsewhere in this file. The probe
	// must depend on the hiveID ARGUMENT; a classifier that ignored it (or
	// substituted any fixed ID) would still pass if every fixture in the test
	// shared one hive name, which is exactly the test-passing-for-the-wrong-
	// reason this avoids.
	const hiveID = "hive-classifier-subject"

	t.Run("spoke on the new current", func(t *testing.T) {
		live := perHiveEnvForGeneration(perHiveGenSecretB, hiveID)
		id, ok := perHiveEnvGeneration(acceptable, live, hiveID)
		if !ok || id != gs.Current {
			t.Fatalf("got (%d,%v), want current generation %d", id, ok, gs.Current)
		}
	})

	t.Run("spoke still on the previous", func(t *testing.T) {
		live := perHiveEnvForGeneration(perHiveGenSecretA, hiveID)
		id, ok := perHiveEnvGeneration(acceptable, live, hiveID)
		if !ok || id != legacyGenerationID {
			t.Fatalf("got (%d,%v), want previous generation %d", id, ok, legacyGenerationID)
		}
	})

	t.Run("spoke on a key from no live generation is UNATTRIBUTED", func(t *testing.T) {
		live := perHiveEnvForGeneration("some-third-retired-master", hiveID)
		if id, ok := perHiveEnvGeneration(acceptable, live, hiveID); ok {
			t.Fatalf("attributed a retired key to generation %d — must fail closed", id)
		}
	})

	t.Run("stale hive ID is not mistaken for a generation match", func(t *testing.T) {
		// Correct generation, WRONG hive: the heartbeat key is per-hive, so
		// this must not attribute. If it did, a stale-hive-ID drift would read
		// as converged.
		live := perHiveEnvForGeneration(perHiveGenSecretB, "hive-bravo")
		if id, ok := perHiveEnvGeneration(acceptable, live, hiveID); ok {
			t.Fatalf("attributed another hive's key to generation %d", id)
		}
	})

	t.Run("absent heartbeat key is unattributed", func(t *testing.T) {
		live := []deploymentEnvVar{{Name: "HIVE_ID", Value: hiveID}}
		if _, ok := perHiveEnvGeneration(acceptable, live, hiveID); ok {
			t.Fatal("attributed a hive with no heartbeat key")
		}
	})

	t.Run("classification is a function of the hiveID argument", func(t *testing.T) {
		// Same live env, two different subject hive IDs: at most one can match.
		// This is the positive control for the stale-hive-ID case above — that
		// one only proves a NEGATIVE, which "never attribute anything" would
		// also satisfy.
		live := perHiveEnvForGeneration(perHiveGenSecretB, hiveID)
		if id, ok := perHiveEnvGeneration(acceptable, live, hiveID); !ok || id != gs.Current {
			t.Fatalf("subject hive did not attribute: (%d,%v)", id, ok)
		}
		if id, ok := perHiveEnvGeneration(acceptable, live, "hive-some-other"); ok {
			t.Fatalf("a DIFFERENT hive attributed to generation %d from the same env — "+
				"the classifier ignores its hiveID argument", id)
		}
	})

	t.Run("no live generations attributes nothing", func(t *testing.T) {
		live := perHiveEnvForGeneration(perHiveGenSecretB, hiveID)
		if _, ok := perHiveEnvGeneration(nil, live, hiveID); ok {
			t.Fatal("attributed with an empty generation set")
		}
	})
}

// TestPerHiveEnvStatusMixedFleetCounts is the observability assertion: a fleet
// split across generations must be counted correctly, and the counts must come
// from Deployment-sourced observations so a PAUSED spoke still blocks
// retirement rather than dropping out of the denominator.
func TestPerHiveEnvStatusMixedFleetCounts(t *testing.T) {
	now := time.Now()
	gs := legacyGenerationSet(perHiveGenSecretA).rotate(perHiveGenSecretB, now, defaultVerifyWindow)
	s := &HubServer{keyGenerations: gs, perHiveEnvSeen: map[string]perHiveEnvObservation{}}

	// 3 on the new current, 2 still on the previous, 1 on neither.
	for _, id := range []string{"c1", "c2", "c3"} {
		s.perHiveEnvSeen[id] = perHiveEnvObservation{Generation: gs.Current, Observed: now}
	}
	for _, id := range []string{"p1", "p2"} {
		s.perHiveEnvSeen[id] = perHiveEnvObservation{Generation: legacyGenerationID, Observed: now}
	}
	s.perHiveEnvSeen["x1"] = perHiveEnvObservation{Generation: 0, Observed: now}

	got := s.PerHiveEnvSnapshot().KeyGenerations
	if got.Current != gs.Current {
		t.Errorf("Current = %d, want %d", got.Current, gs.Current)
	}
	if got.SpokesOnCurrent != 3 {
		t.Errorf("SpokesOnCurrent = %d, want 3", got.SpokesOnCurrent)
	}
	if got.SpokesOnPrevious != 2 {
		t.Errorf("SpokesOnPrevious = %d, want 2", got.SpokesOnPrevious)
	}
	if got.SpokesUnattributed != 1 {
		t.Errorf("SpokesUnattributed = %d, want 1", got.SpokesUnattributed)
	}
	if len(got.LiveGenerations) != 2 {
		t.Errorf("LiveGenerations = %v, want two entries", got.LiveGenerations)
	}
	if got.PreviousVerifyUntil == "" {
		t.Error("PreviousVerifyUntil is empty; a demoted generation must publish its expiry")
	}
	// Still converging AND the window is still open: not retirable.
	if got.SafeToRetirePrevious {
		t.Error("SafeToRetirePrevious with 2 spokes still on the previous generation")
	}

	// The counts must NEVER expose secret material. LiveGenerations holds IDs.
	for _, id := range got.LiveGenerations {
		if id <= 0 {
			t.Errorf("live generation id %d is not a valid generation number", id)
		}
	}
}

// TestPerHiveEnvSafeToRetireIsGated walks every clause of
// SafeToRetirePrevious. Each sub-test flips exactly one input, so a clause
// deleted from the predicate turns exactly one sub-test red.
func TestPerHiveEnvSafeToRetireIsGated(t *testing.T) {
	now := time.Now()
	// A rotation whose verify window has ALREADY closed.
	expired := legacyGenerationSet(perHiveGenSecretA).
		rotate(perHiveGenSecretB, now.Add(-2*defaultVerifyWindow), defaultVerifyWindow)

	// perHiveEnvConsidered mirrors what a sweep that read every admitted hive
	// would record, with perHiveEnvUnreachable left at zero: a FULLY observed
	// fleet. Retirement readiness is gated on that, so a fixture that omitted
	// it would report "not retirable" for reasons unrelated to what each
	// subtest is actually asserting.
	newHub := func(gs *generationSet, seen map[string]perHiveEnvObservation) *HubServer {
		return &HubServer{
			keyGenerations:       gs,
			perHiveEnvSeen:       seen,
			perHiveEnvConsidered: len(seen),
		}
	}
	allOnCurrent := map[string]perHiveEnvObservation{
		"c1": {Generation: expired.Current, Observed: now},
	}

	t.Run("retirable: window closed, everything on current", func(t *testing.T) {
		got := newHub(expired, allOnCurrent).PerHiveEnvSnapshot().KeyGenerations
		if !got.SafeToRetirePrevious {
			t.Fatalf("want retirable; got %+v", got)
		}
	})

	t.Run("not retirable with zero observations (fails closed)", func(t *testing.T) {
		got := newHub(expired, map[string]perHiveEnvObservation{}).PerHiveEnvSnapshot().KeyGenerations
		if got.SafeToRetirePrevious {
			t.Fatal("reported retirable from zero evidence")
		}
	})

	t.Run("not retirable while a spoke is still on the previous", func(t *testing.T) {
		seen := map[string]perHiveEnvObservation{
			"c1": {Generation: expired.Current, Observed: now},
			"p1": {Generation: legacyGenerationID, Observed: now},
		}
		if newHub(expired, seen).PerHiveEnvSnapshot().KeyGenerations.SafeToRetirePrevious {
			t.Fatal("reported retirable with a spoke still on the previous generation")
		}
	})

	t.Run("not retirable while a spoke is unattributed", func(t *testing.T) {
		seen := map[string]perHiveEnvObservation{
			"c1": {Generation: expired.Current, Observed: now},
			"x1": {Generation: 0, Observed: now},
		}
		if newHub(expired, seen).PerHiveEnvSnapshot().KeyGenerations.SafeToRetirePrevious {
			t.Fatal("reported retirable with an unattributed spoke")
		}
	})

	t.Run("not retirable while the verify window is still open", func(t *testing.T) {
		open := legacyGenerationSet(perHiveGenSecretA).rotate(perHiveGenSecretB, now, defaultVerifyWindow)
		seen := map[string]perHiveEnvObservation{"c1": {Generation: open.Current, Observed: now}}
		if newHub(open, seen).PerHiveEnvSnapshot().KeyGenerations.SafeToRetirePrevious {
			t.Fatal("reported retirable before verify_until passed")
		}
	})

	t.Run("never-rotated hub has no previous to retire", func(t *testing.T) {
		single := legacyGenerationSet(perHiveGenSecretA)
		seen := map[string]perHiveEnvObservation{"c1": {Generation: legacyGenerationID, Observed: now}}
		got := newHub(single, seen).PerHiveEnvSnapshot().KeyGenerations
		if got.SafeToRetirePrevious {
			t.Fatal("a never-rotated hub reported a previous generation as retirable")
		}
		if got.PreviousVerifyUntil != "" {
			t.Errorf("PreviousVerifyUntil = %q on a never-rotated hub", got.PreviousVerifyUntil)
		}
		if got.SpokesOnCurrent != 1 {
			t.Errorf("SpokesOnCurrent = %d, want 1", got.SpokesOnCurrent)
		}
	})

	// THE ZERO-VerifyUntil CASE. A hand-edited generations file that strips
	// verify_until must read as ALREADY EXPIRED, never as "never expires".
	// Reading it the other way would pin the previous generation live forever —
	// the exact F1/F2 failure mode. acceptableGenerations already refuses to
	// ACCEPT such a generation; this asserts the retirement surface agrees.
	t.Run("zero verify_until reads as already expired, not never-expires", func(t *testing.T) {
		hand := &generationSet{
			Current: 2,
			Generations: []keyGeneration{
				{ID: 2, Secret: perHiveGenSecretB},
				{ID: 1, Secret: perHiveGenSecretA}, // VerifyUntil deliberately zero
			},
		}
		seen := map[string]perHiveEnvObservation{"c1": {Generation: 2, Observed: now}}
		got := newHub(hand, seen).PerHiveEnvSnapshot().KeyGenerations
		if !got.SafeToRetirePrevious {
			t.Fatal("a zero verify_until must read as already expired (retirable), not as never-expires")
		}
		// And it must not be advertised as live, since it is not accepted.
		for _, id := range got.LiveGenerations {
			if id == 1 {
				t.Fatal("an expired generation is advertised as live")
			}
		}
	})
}

// TestPerHiveEnvGenerationSnapshotNilSafe: the snapshot must not panic on a hub
// with no generation set, and must fail closed.
func TestPerHiveEnvGenerationSnapshotNilSafe(t *testing.T) {
	s := &HubServer{perHiveEnvSeen: map[string]perHiveEnvObservation{
		"c1": {Generation: 0, Observed: time.Now()},
	}}
	got := s.PerHiveEnvSnapshot().KeyGenerations
	if got.Current != 0 {
		t.Errorf("Current = %d on a hub with no generation set", got.Current)
	}
	if got.SpokesOnCurrent != 0 || got.SpokesOnPrevious != 0 {
		t.Errorf("counted spokes onto generations with no generation set: %+v", got)
	}
	if got.SpokesUnattributed != 1 {
		t.Errorf("SpokesUnattributed = %d, want 1", got.SpokesUnattributed)
	}
	if got.SafeToRetirePrevious {
		t.Error("reported retirable with no generation set")
	}
}
