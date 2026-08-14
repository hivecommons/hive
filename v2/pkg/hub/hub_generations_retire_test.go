package hub

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Tests for follow-on PR #7: automatic retirement past verify_until, and the
// alert that fires when a generation is pinned open.
//
// The properties under test, in order of how badly they hurt if broken:
//
//  1. RETIREMENT IS UNCONDITIONAL ON CONVERGENCE. A generation past
//     VerifyUntil is retired EVEN WITH SPOKES STILL ON IT. Gating retirement on
//     spokes_on_previous would let one unconverged spoke pin a superseded
//     master live forever — the "unversioned and permanent compat lane" failure
//     that took five audits to remove. This is the point of the PR.
//  2. VerifyUntil.IsZero() STILL MEANS ALREADY EXPIRED, on every path this PR
//     adds. A hand-edited file must fail closed, not pin a key open.
//  3. THE WALL-CLOCK GUARANTEE AND THE READINESS SIGNAL STAY SEPARATE.
//     SafeToRetirePrevious still fails closed on zero observations and on
//     non-zero spokes_unattributed; retirement ignores both.
//  4. RETIREMENT SURVIVES A RESTART, and a failed persist is a clean no-op.
//  5. THE FOUR MERGED READERS reject — not error — against a just-retired
//     generation.

const (
	retSecretA = "retire-test-master-alpha"
	retSecretB = "retire-test-master-bravo"
)

// newRetireTestHub builds a hub with an explicit two-generation set.
//
// It goes through setHubSecret rather than assigning hubSecret directly. That
// is the trap #2 (2234750a) left behind: 97 test sites assigned srv.hubSecret
// after NewHubServer, leaving keyGenerations derived from a DISCARDED secret,
// so a generation-set reader silently tested against the wrong material.
// retireExpiredGenerations is a new generation-set reader, so it would be
// exposed to exactly that. setHubSecret sets both in lockstep; the explicit set
// below then REPLACES the derived one, which is the only assignment this file
// makes and it assigns the field the readers actually consult.
func newRetireTestHub(t *testing.T, gens []keyGeneration, current int) *HubServer {
	t.Helper()
	s := &HubServer{logger: quietLogger()}
	s.setHubSecret(retSecretA)
	gs := newGenerationSet(current, gens)
	if gs == nil {
		t.Fatalf("fixture: newGenerationSet(%d, %d gens) returned nil", current, len(gens))
	}
	s.keyGenerations = gs
	return s
}

// seedObservations installs Deployment-sourced spoke observations, which is
// what PerHiveEnvSnapshot counts. Deliberately NOT heartbeat-sourced: a paused
// spoke still has a Deployment and must still block readiness.
// seedObservations stands in for a completed sweep, so it must seed what a
// sweep records: the observations AND the selection accounting. Considered is
// set to the number of hives observed with zero unreachable — i.e. a FULLY
// observed fleet — because that is the only state in which the readiness
// signals are permitted to say "yes". Tests that want a partially-observed
// fleet use seedObservationsWithUnreachable.
func seedObservations(s *HubServer, onGeneration map[string]int) {
	seedObservationsWithUnreachable(s, onGeneration, 0, nil)
}

// seedObservationsWithUnreachable seeds a sweep that could not read
// unreachable hives on the named clusters, on top of the ones it did observe.
func seedObservationsWithUnreachable(s *HubServer, onGeneration map[string]int, unreachable int, clusters []string) {
	s.perHiveEnvMu.Lock()
	defer s.perHiveEnvMu.Unlock()
	s.perHiveEnvSeen = map[string]perHiveEnvObservation{}
	for id, gen := range onGeneration {
		s.perHiveEnvSeen[id] = perHiveEnvObservation{Generation: gen}
	}
	s.perHiveEnvConsidered = len(onGeneration) + unreachable
	s.perHiveEnvSkippedByStatus = 0
	s.perHiveEnvUnreachable = unreachable
	s.perHiveEnvUnreachableClusters = append([]string(nil), clusters...)
}

// TestRetireExpiredGenerationsIsPositiveControl is the two-directional positive
// control. A retirement lane can fail two opposite ways — never firing, or
// firing on everything — and a test that only asserts one direction passes
// under the other. Both are asserted here, against the SAME hub.
func TestRetireExpiredGenerationsIsPositiveControl(t *testing.T) {
	withTempGenerationsPath(t)
	now := time.Now().UTC()

	// Direction 1: an UNEXPIRED previous generation must NOT be retired. Fails
	// an implementation that retires unconditionally.
	t.Run("unexpired generation is kept", func(t *testing.T) {
		s := newRetireTestHub(t, []keyGeneration{
			{ID: 2, Secret: retSecretB, Created: now},
			{ID: 1, Secret: retSecretA, VerifyUntil: now.Add(time.Hour)},
		}, 2)
		retired, err := s.retireExpiredGenerations(now)
		if err != nil {
			t.Fatalf("retire: %v", err)
		}
		if len(retired) != 0 {
			t.Errorf("retired %v, want none — an unexpired generation must survive", retired)
		}
		if got := len(s.currentGenerations().Generations); got != 2 {
			t.Errorf("live generations = %d, want 2", got)
		}
	})

	// Direction 2: an EXPIRED previous generation MUST be retired. Fails an
	// implementation that never retires.
	t.Run("expired generation is retired", func(t *testing.T) {
		s := newRetireTestHub(t, []keyGeneration{
			{ID: 2, Secret: retSecretB, Created: now},
			{ID: 1, Secret: retSecretA, VerifyUntil: now.Add(-time.Hour)},
		}, 2)
		retired, err := s.retireExpiredGenerations(now)
		if err != nil {
			t.Fatalf("retire: %v", err)
		}
		if len(retired) != 1 || retired[0] != 1 {
			t.Fatalf("retired %v, want [1] — an expired generation must be dropped", retired)
		}
		gs := s.currentGenerations()
		if len(gs.Generations) != 1 {
			t.Fatalf("live generations = %d, want 1", len(gs.Generations))
		}
		if gs.Current != 2 {
			t.Errorf("current = %d, want 2 — retirement must never move the minting key", gs.Current)
		}
	})

	// Direction 3: the CURRENT generation is never retired, however the clock
	// reads. Fails an implementation that applies the expiry rule to current.
	t.Run("current generation is never retired", func(t *testing.T) {
		s := newRetireTestHub(t, []keyGeneration{
			{ID: 2, Secret: retSecretB, Created: now.Add(-100 * 24 * time.Hour)},
		}, 2)
		retired, err := s.retireExpiredGenerations(now)
		if err != nil {
			t.Fatalf("retire: %v", err)
		}
		if len(retired) != 0 {
			t.Fatalf("retired %v, want none — the minting key must never be retired", retired)
		}
	})
}

// TestRetireTreatsZeroVerifyUntilAsExpired pins the invariant this whole design
// rests on, on the code path THIS PR adds.
//
// A zero VerifyUntil means ALREADY EXPIRED, not "never expires". Reading it the
// other way would make a hand-edited or malformed generations file pin the old
// master open permanently — which is the F1/F2 failure mode restated.
func TestRetireTreatsZeroVerifyUntilAsExpired(t *testing.T) {
	withTempGenerationsPath(t)
	now := time.Now().UTC()
	s := newRetireTestHub(t, []keyGeneration{
		{ID: 2, Secret: retSecretB, Created: now},
		// No VerifyUntil at all — the hand-edited-file case.
		{ID: 1, Secret: retSecretA},
	}, 2)

	// The read path already refuses it...
	if got := len(s.currentGenerations().acceptableGenerations(now)); got != 1 {
		t.Fatalf("acceptableGenerations = %d, want 1 — a zero VerifyUntil must not be accepted", got)
	}
	// ...and the retirement path must agree, or the dead secret stays on disk.
	retired, err := s.retireExpiredGenerations(now)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if len(retired) != 1 || retired[0] != 1 {
		t.Fatalf("retired %v, want [1] — a zero VerifyUntil is ALREADY EXPIRED, not never-expires", retired)
	}
}

// TestRetireHappensEvenWithSpokesStillOnGeneration is the ANTI-PIN TEST and the
// reason this PR exists.
//
// Every spoke in the fleet is on the outgoing generation and none has
// converged. Retirement must fire ANYWAY, and the alert must say so. An
// implementation that waits for spokes_on_previous == 0 passes every other test
// in this file and fails this one.
func TestRetireHappensEvenWithSpokesStillOnGeneration(t *testing.T) {
	withTempGenerationsPath(t)
	now := time.Now().UTC()
	s := newRetireTestHub(t, []keyGeneration{
		{ID: 2, Secret: retSecretB, Created: now},
		{ID: 1, Secret: retSecretA, VerifyUntil: now.Add(-time.Minute)},
	}, 2)
	// The entire fleet is still on generation 1. Nothing has converged.
	seedObservations(s, map[string]int{"hive-a": 1, "hive-b": 1, "hive-c": 1})

	snap := s.PerHiveEnvSnapshot()
	if snap.KeyGenerations.SpokesOnPrevious != 3 {
		t.Fatalf("fixture: spokes_on_previous = %d, want 3", snap.KeyGenerations.SpokesOnPrevious)
	}

	// The alert must fire, at STRANDED severity: the window has closed and
	// spokes are still on it.
	alert := evaluateGenerationPin(
		keyGeneration{ID: 1, VerifyUntil: now.Add(-time.Minute)}, false, now,
		snap.KeyGenerations.SpokesOnPrevious, snap.KeyGenerations.SpokesUnattributed, snap.ObservedHives)
	if alert.Severity != pinSeverityStranded {
		t.Errorf("severity = %v, want stranded — a closed window with spokes on it must alert loudly",
			alert.Severity)
	}

	// And retirement must happen regardless. This is the assertion that fails
	// if retirement is ever made conditional on convergence.
	retired, err := s.retireExpiredGenerations(now)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if len(retired) != 1 || retired[0] != 1 {
		t.Fatalf("retired %v, want [1] — retirement is a WALL-CLOCK guarantee and must NOT wait for "+
			"convergence; one unconverged spoke must never pin the old master open", retired)
	}
	if got := len(s.currentGenerations().acceptableGenerations(now)); got != 1 {
		t.Errorf("acceptable after retire = %d, want 1", got)
	}
}

// TestGenerationPinAlertSeverities covers the alert policy across the timeline.
func TestGenerationPinAlertSeverities(t *testing.T) {
	now := time.Now().UTC()
	prev := func(until time.Duration) keyGeneration {
		return keyGeneration{ID: 1, Secret: retSecretA, VerifyUntil: now.Add(until)}
	}

	cases := []struct {
		name         string
		gen          keyGeneration
		isCurrent    bool
		onPrevious   int
		unattributed int
		observed     int
		wantSeverity generationPinSeverity
	}{
		{
			name: "converged fleet well before expiry is silent",
			gen:  prev(72 * time.Hour), onPrevious: 0, observed: 5,
			wantSeverity: pinSeverityNone,
		},
		{
			name: "spokes on previous but expiry far away is silent",
			gen:  prev(72 * time.Hour), onPrevious: 3, observed: 5,
			wantSeverity: pinSeverityNone,
		},
		{
			name: "spokes on previous inside the warn window warns",
			gen:  prev(2 * time.Hour), onPrevious: 3, observed: 5,
			wantSeverity: pinSeverityWarn,
		},
		{
			name: "window closed with spokes still on it is stranded",
			gen:  prev(-time.Minute), onPrevious: 3, observed: 5,
			wantSeverity: pinSeverityStranded,
		},
		{
			// spokes_unattributed must count as "still carrying the old key".
			// #3766 made it block retirement readiness deliberately; folding it
			// into "converged" here would reopen that hole from the alerting
			// side.
			name: "unattributed spokes alone still raise the alert",
			gen:  prev(-time.Minute), onPrevious: 0, unattributed: 2, observed: 5,
			wantSeverity: pinSeverityStranded,
		},
		{
			// Zero observations means the hub has read nothing, so the counts
			// are evidence of nothing. Do not claim stranding from no data.
			name: "zero observations does not manufacture an alert",
			gen:  prev(-time.Minute), onPrevious: 0, unattributed: 0, observed: 0,
			wantSeverity: pinSeverityNone,
		},
		{
			// A zero VerifyUntil is ALREADY EXPIRED, so it is stranded, not
			// silent. Suppressing here would hide the malformed-file case.
			name: "zero VerifyUntil with spokes on it is stranded",
			gen:  keyGeneration{ID: 1, Secret: retSecretA}, onPrevious: 1, observed: 5,
			wantSeverity: pinSeverityStranded,
		},
		{
			name: "current generation is never pinned open",
			gen:  keyGeneration{ID: 2, Secret: retSecretB}, isCurrent: true, onPrevious: 3, observed: 5,
			wantSeverity: pinSeverityNone,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateGenerationPin(tc.gen, tc.isCurrent, now, tc.onPrevious, tc.unattributed, tc.observed)
			if got.Severity != tc.wantSeverity {
				t.Errorf("severity = %v, want %v", got.Severity, tc.wantSeverity)
			}
		})
	}
}

// TestSafeToRetirePreviousStillFailsClosed pins the READINESS signal, which is
// a different condition from retirement and must stay that way.
//
// Retirement is the wall clock's call. SafeToRetirePrevious answers a narrower
// question — "does retiring right now cost nothing?" — and fails closed on zero
// observations and on any unattributed spoke. This PR must not have relaxed it.
func TestSafeToRetirePreviousStillFailsClosed(t *testing.T) {
	withTempGenerationsPath(t)
	now := time.Now().UTC()
	build := func(t *testing.T, obs map[string]int) PerHiveEnvStatus {
		t.Helper()
		s := newRetireTestHub(t, []keyGeneration{
			{ID: 2, Secret: retSecretB, Created: now},
			{ID: 1, Secret: retSecretA, VerifyUntil: now.Add(-time.Hour)},
		}, 2)
		seedObservations(s, obs)
		return s.PerHiveEnvSnapshot()
	}

	t.Run("zero observations is never safe", func(t *testing.T) {
		snap := build(t, map[string]int{})
		if snap.KeyGenerations.SafeToRetirePrevious {
			t.Error("safe_to_retire_previous = true from ZERO observations — must fail closed")
		}
	})

	t.Run("unattributed spokes are never safe", func(t *testing.T) {
		// Generation 0 == matches no live generation == unattributed.
		snap := build(t, map[string]int{"hive-a": 2, "hive-b": 0})
		if snap.KeyGenerations.SpokesUnattributed != 1 {
			t.Fatalf("fixture: spokes_unattributed = %d, want 1", snap.KeyGenerations.SpokesUnattributed)
		}
		if snap.KeyGenerations.SafeToRetirePrevious {
			t.Error("safe_to_retire_previous = true with an UNATTRIBUTED spoke — #3766 made this block " +
				"readiness deliberately; an unattributed spoke is broken now, not converged")
		}
	})

	t.Run("spokes on previous are never safe", func(t *testing.T) {
		snap := build(t, map[string]int{"hive-a": 2, "hive-b": 1})
		if snap.KeyGenerations.SafeToRetirePrevious {
			t.Error("safe_to_retire_previous = true with a spoke still on previous")
		}
	})

	t.Run("converged fleet past the window is safe", func(t *testing.T) {
		snap := build(t, map[string]int{"hive-a": 2, "hive-b": 2})
		if !snap.KeyGenerations.SafeToRetirePrevious {
			t.Error("safe_to_retire_previous = false for a fully converged fleet past verify_until — " +
				"the readiness signal must still be able to say yes, or it is not a signal")
		}
	})
}

// TestRetirementSurvivesRestart asserts persistence: a retirement must not come
// back at the next hub roll, which happens several times a day.
func TestRetirementSurvivesRestart(t *testing.T) {
	path := withTempGenerationsPath(t)
	now := time.Now().UTC()

	// Seed a persisted two-generation set, one of them expired.
	seed := &generationSet{Current: 2, Generations: []keyGeneration{
		{ID: 2, Secret: retSecretB, Created: now},
		{ID: 1, Secret: retSecretA, VerifyUntil: now.Add(-time.Hour)},
	}}
	if err := saveGenerations(seed, now.Add(-8*24*time.Hour)); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	s := newRetireTestHub(t, seed.Generations, 2)
	s.lastKeyRotation = now.Add(-8 * 24 * time.Hour)
	retired, err := s.retireExpiredGenerations(now)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if len(retired) != 1 {
		t.Fatalf("retired %v, want [1]", retired)
	}

	// Simulate the restart: read the file back the way loadGenerations does.
	reloaded, rotatedAt := loadGenerations(retSecretA, quietLogger())
	if reloaded == nil {
		t.Fatal("reload returned nil set")
	}
	if len(reloaded.Generations) != 1 {
		t.Errorf("after restart, live generations = %d, want 1 — the retirement did not persist",
			len(reloaded.Generations))
	}
	if reloaded.Current != 2 {
		t.Errorf("after restart, current = %d, want 2", reloaded.Current)
	}
	for _, g := range reloaded.Generations {
		if g.ID == 1 {
			t.Error("retired generation 1 is still on disk after restart — its master secret is " +
				"retained in plaintext past the point where it protects anything")
		}
	}
	// Retirement is not a rotation and must not reset the cooldown, or an
	// operator could bypass it by waiting for a retirement.
	if rotatedAt.IsZero() {
		t.Error("rotated_at was cleared by retirement — the double-rotation cooldown would reset")
	}

	// And the file must still be 0600: it holds master secrets.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != hubGenerationsFileMode {
		t.Errorf("file mode = %o, want %o", perm, hubGenerationsFileMode)
	}
}

// TestFailedPersistLeavesRetirementUnapplied asserts the persist-before-install
// ordering #4 established: a failed write must be a clean NO-OP, never a
// half-applied state where memory and disk disagree.
func TestFailedPersistLeavesRetirementUnapplied(t *testing.T) {
	withTempGenerationsPath(t)
	now := time.Now().UTC()
	s := newRetireTestHub(t, []keyGeneration{
		{ID: 2, Secret: retSecretB, Created: now},
		{ID: 1, Secret: retSecretA, VerifyUntil: now.Add(-time.Hour)},
	}, 2)

	// Make the write fail by pointing the path at a location that cannot be
	// created: a directory component that is actually a file.
	blocker := hubGenerationsPath + ".blocker"
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("blocker: %v", err)
	}
	prev := hubGenerationsPath
	hubGenerationsPath = blocker + "/hub-generations.json"
	t.Cleanup(func() { hubGenerationsPath = prev })

	retired, err := s.retireExpiredGenerations(now)
	if err == nil {
		t.Fatal("retire returned no error despite an unwritable path")
	}
	if len(retired) != 0 {
		t.Errorf("retired %v on a failed persist, want none", retired)
	}
	// The in-memory set must be UNCHANGED — a retirement applied in memory only
	// would come back at the next roll.
	if got := len(s.currentGenerations().Generations); got != 2 {
		t.Errorf("in-memory generations = %d, want 2 — a failed persist must be a clean no-op", got)
	}
}

// TestMergedReadersRejectRetiredGeneration covers the four merged follow-ons
// that read the generation set. An artifact minted under a generation that has
// just been retired must be REJECTED — cleanly, not with an error or a panic —
// by every one of them.
func TestMergedReadersRejectRetiredGeneration(t *testing.T) {
	withTempGenerationsPath(t)
	now := time.Now().UTC()

	// Build the pre-retirement set and mint artifacts under generation 1.
	before := newGenerationSet(2, []keyGeneration{
		{ID: 2, Secret: retSecretB, Created: now},
		{ID: 1, Secret: retSecretA, VerifyUntil: now.Add(time.Hour)},
	})
	gen1 := keyGeneration{ID: 1, Secret: retSecretA}
	cookie, _ := mintHubUserCookieValueV3Gen(sessionSeedForGeneration(gen1), "alice", now, time.Hour, 1)
	bearer := derivePerHiveKey(retSecretA, infoHeartbeatKey, "hive-a")
	if cookie == "" || bearer == "" {
		t.Fatal("fixture: could not mint generation-1 artifacts")
	}

	// #1 session cookie and #2 heartbeat bearer both verify BEFORE retirement.
	if _, id, ok := verifyHubUserCookieAcrossGenerations(before, cookie, now, nil); !ok || id != 1 {
		t.Fatalf("fixture: cookie did not verify under generation 1 (ok=%v id=%d)", ok, id)
	}
	if id, ok := verifyHeartbeatBearerAcrossGenerations(before, bearer, "hive-a", now); !ok || id != 1 {
		t.Fatalf("fixture: bearer did not verify under generation 1 (ok=%v id=%d)", ok, id)
	}

	// Retire generation 1.
	s := newRetireTestHub(t, before.Generations, 2)
	retiredNow := now.Add(2 * time.Hour)
	if _, err := s.retireExpiredGenerations(retiredNow); err != nil {
		t.Fatalf("retire: %v", err)
	}
	after := s.currentGenerations()
	if len(after.Generations) != 1 {
		t.Fatalf("fixture: generation 1 was not retired (%d live)", len(after.Generations))
	}

	// #1 SESSION COOKIE — rejected, not errored. The cookie carries a `g:1`
	// claim naming a generation that no longer exists; that must fall through
	// to trial verification and simply fail, since only generation 2's key
	// remains and it did not sign this cookie.
	t.Run("session cookie is rejected", func(t *testing.T) {
		u, id, ok := verifyHubUserCookieAcrossGenerations(after, cookie, retiredNow, nil)
		if ok {
			t.Errorf("cookie minted under a RETIRED generation verified (user=%q id=%d)", u, id)
		}
		if u != "" || id != 0 {
			t.Errorf("rejected cookie leaked user=%q id=%d, want empty", u, id)
		}
	})

	// #2 HEARTBEAT BEARER — rejected. A bare derived string with no envelope,
	// so this is trial verification against the remaining generation only.
	t.Run("heartbeat bearer is rejected", func(t *testing.T) {
		id, ok := verifyHeartbeatBearerAcrossGenerations(after, bearer, "hive-a", retiredNow)
		if ok {
			t.Errorf("bearer derived from a RETIRED generation verified (id=%d)", id)
		}
		if id != 0 {
			t.Errorf("rejected bearer reported generation %d, want 0", id)
		}
	})

	// #3 PER-HIVE ENV — a spoke still carrying generation-1 material must
	// classify as UNATTRIBUTED, not as "on previous" and not as converged. It
	// is broken now, not lagging.
	t.Run("per-hive env classifies retired material as unattributed", func(t *testing.T) {
		live := []deploymentEnvVar{{Name: "HIVE_HEARTBEAT_KEY", Value: bearer}}
		gen, ok := perHiveEnvGeneration(after.acceptableGenerations(retiredNow), live, "hive-a")
		if ok || gen != 0 {
			t.Errorf("retired-generation material classified as generation %d (ok=%v), want unattributed",
				gen, ok)
		}
	})

	// #4 ROTATE ENDPOINT — the persisted file must no longer contain the
	// retired generation's secret, and the set must still be usable for a
	// subsequent rotation.
	t.Run("persisted set no longer holds the retired secret", func(t *testing.T) {
		data, err := os.ReadFile(hubGenerationsPath)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var p persistedGenerations
		if err := json.Unmarshal(data, &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(p.Generations) != 1 {
			t.Fatalf("persisted generations = %d, want 1", len(p.Generations))
		}
		for _, g := range p.Generations {
			if g.ID == 1 {
				t.Error("retired generation 1 remains in the persisted file")
			}
		}
		// A rotation must still work off the retired-down set.
		s.lastKeyRotation = time.Time{}
		next, _, err := s.rotateMasterSecret(retiredNow, false)
		if err != nil {
			t.Fatalf("rotate after retirement: %v", err)
		}
		if next.Current != 3 {
			t.Errorf("post-retirement rotation produced current = %d, want 3", next.Current)
		}
	})
}

// TestSweepGenerationRetirementIsIdempotent asserts the poller-facing entry
// point is safe to call every tick: the second sweep finds nothing to do.
func TestSweepGenerationRetirementIsIdempotent(t *testing.T) {
	withTempGenerationsPath(t)
	now := time.Now().UTC()
	s := newRetireTestHub(t, []keyGeneration{
		{ID: 2, Secret: retSecretB, Created: now},
		{ID: 1, Secret: retSecretA, VerifyUntil: now.Add(-time.Hour)},
	}, 2)
	seedObservations(s, map[string]int{"hive-a": 1})

	s.sweepGenerationRetirement(now)
	if got := len(s.currentGenerations().Generations); got != 1 {
		t.Fatalf("after first sweep, generations = %d, want 1", got)
	}
	s.sweepGenerationRetirement(now)
	if got := len(s.currentGenerations().Generations); got != 1 {
		t.Errorf("after second sweep, generations = %d, want 1 — the sweep must be idempotent", got)
	}
}
