package hub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for follow-on PR #4: generation persistence and the admin rotate
// endpoint.
//
// The properties under test, in order of how badly they hurt if broken:
//
//  1. A rotation SURVIVES A RESTART. The hub rolls several times a day; a
//     rotation held only in memory means the hub comes back on the old
//     generation and rejects everything minted since.
//  2. A DOUBLE ROTATION IS REFUSED. maxLiveGenerations is 2, so a second
//     rotation before the first converges DROPS the generation most of the
//     fleet is still on.
//  3. A MALFORMED OR UNREADABLE FILE FAILS CLOSED to NO set at all, and is
//     never replaced by a fresh rotation and never quietly resolved to the
//     legacy single-generation set. Only an ABSENT file falls back to legacy
//     (audit 8, F20 — see TestMalformedGenerationsFileFailsClosed).
//  4. NO SECRET MATERIAL is ever logged or returned.

const (
	rotStoreSecretA = "rotation-store-test-master-alpha"
	rotStoreSecretB = "rotation-store-test-master-bravo"
)

// withTempGenerationsPath redirects hubGenerationsPath into a temp dir so tests
// never read or write the real PVC path.
func withTempGenerationsPath(t *testing.T) string {
	t.Helper()
	prev := hubGenerationsPath
	dir := t.TempDir()
	hubGenerationsPath = filepath.Join(dir, "hub-generations.json")
	t.Cleanup(func() { hubGenerationsPath = prev })
	return hubGenerationsPath
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newRotationTestHub builds a hub that is ALLOWED to rotate.
//
// AUDIT 8 / F19+F21. rotateMasterSecret now refuses unless the last reconcile
// sweep observed the ENTIRE fleet, because with 44 of 70 spokes on pull-only
// clusters a rotation would converge the reachable ones and strand the rest
// when the previous generation retires. So a hub with no sweep state — which is
// what this fixture used to be — is refused with errRotationWouldStrandSpokes.
//
// This fixture therefore declares a fully observed fleet: one hive considered,
// none unreachable. That is the PRECONDITION under test elsewhere, not the
// subject of the tests using this helper; they assert rotation mechanics
// (demotion, persistence, cooldown, double-submit) and would otherwise all fail
// for one unrelated reason. The interlock itself is asserted directly, in both
// directions, by TestRotationRefusedWhileFleetNotFullyObserved.
func newRotationTestHub(t *testing.T, master string) *HubServer {
	t.Helper()
	return &HubServer{
		logger:          quietLogger(),
		hubSecret:       master,
		keyGenerations:  legacyGenerationSet(master),
		lastKeyRotation: time.Time{},
		// A fully observed fleet: considered > 0 and unreachable == 0.
		perHiveEnvConsidered:  1,
		perHiveEnvUnreachable: 0,
		perHiveEnvSeen: map[string]perHiveEnvObservation{
			"hive-observed": {Generation: legacyGenerationID, Observed: time.Now()},
		},
	}
}

// TestRotateDemotesAndSetsVerifyUntil is the core mechanism assertion.
func TestRotateDemotesAndSetsVerifyUntil(t *testing.T) {
	withTempGenerationsPath(t)
	s := newRotationTestHub(t, rotStoreSecretA)
	before := s.currentGenerations()
	if before.Current != legacyGenerationID {
		t.Fatalf("fixture: current = %d, want %d", before.Current, legacyGenerationID)
	}
	beforeSecret := before.currentSecret()

	now := time.Now().UTC()
	next, _, err := s.rotateMasterSecret(now, false)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	if next.Current != legacyGenerationID+1 {
		t.Errorf("new current = %d, want %d", next.Current, legacyGenerationID+1)
	}
	if len(next.Generations) != 2 {
		t.Fatalf("live generations = %d, want current + one previous", len(next.Generations))
	}
	if next.currentSecret() == beforeSecret {
		t.Error("rotation did not change the minting secret")
	}
	if len(next.Generations) > maxLiveGenerations {
		t.Errorf("live generations %d exceeds maxLiveGenerations %d", len(next.Generations), maxLiveGenerations)
	}

	// The demoted generation must be the OLD current, carry the old secret, and
	// carry a NON-ZERO VerifyUntil — the whole finiteness promise.
	prev := next.Generations[1]
	if prev.ID != legacyGenerationID {
		t.Errorf("previous id = %d, want %d", prev.ID, legacyGenerationID)
	}
	if prev.Secret != beforeSecret {
		t.Error("previous generation does not carry the outgoing master — pre-rotation artifacts would not verify")
	}
	if prev.VerifyUntil.IsZero() {
		t.Fatal("demoted generation has a ZERO VerifyUntil, which means ALREADY EXPIRED — " +
			"every pre-rotation artifact would be rejected immediately")
	}
	if want := now.Add(defaultVerifyWindow); !prev.VerifyUntil.Equal(want) {
		t.Errorf("VerifyUntil = %v, want %v (now + defaultVerifyWindow)", prev.VerifyUntil, want)
	}

	// Both generations must be acceptable right now — that is what keeps
	// unconverged spokes authenticating during the walk.
	if got := len(next.acceptableGenerations(now)); got != 2 {
		t.Errorf("acceptable generations = %d immediately after rotation, want 2", got)
	}
	// And the previous one must STOP being acceptable once its window closes,
	// with no operator action.
	if got := len(next.acceptableGenerations(now.Add(defaultVerifyWindow + time.Minute))); got != 1 {
		t.Errorf("acceptable generations = %d after verify_until, want 1 — the window must close on the wall clock", got)
	}

	// The generated secret must be a full-length hex master, not a short read.
	if len(next.currentSecret()) != masterSecretBytes*2 {
		t.Errorf("new master is %d chars, want %d hex chars", len(next.currentSecret()), masterSecretBytes*2)
	}
}

// TestRotationSurvivesRestart is property (1): persist, then reload from disk
// exactly as NewHubServer does, and confirm the rotated state came back.
func TestRotationSurvivesRestart(t *testing.T) {
	path := withTempGenerationsPath(t)
	s := newRotationTestHub(t, rotStoreSecretA)

	now := time.Now().UTC().Truncate(time.Second)
	rotated, _, err := s.rotateMasterSecret(now, false)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("rotation did not persist a generations file: %v", err)
	}

	// SIMULATED RESTART: nothing survives but the PVC and hub-secret.key.
	reloaded, rotatedAt, _ := loadGenerations(rotStoreSecretA, quietLogger())
	if reloaded == nil {
		t.Fatal("reload returned no generation set")
	}
	if reloaded.Current != rotated.Current {
		t.Errorf("after restart current = %d, want %d — the hub forgot the rotation", reloaded.Current, rotated.Current)
	}
	if reloaded.currentSecret() != rotated.currentSecret() {
		t.Error("after restart the minting secret differs — artifacts minted before the roll would not verify")
	}
	if len(reloaded.Generations) != 2 {
		t.Fatalf("after restart live generations = %d, want 2", len(reloaded.Generations))
	}
	if reloaded.Generations[1].Secret != rotStoreSecretA {
		t.Error("after restart the previous generation lost the outgoing master")
	}
	if reloaded.Generations[1].VerifyUntil.IsZero() {
		t.Fatal("after restart the previous generation has a ZERO VerifyUntil — it would be treated as expired")
	}
	// The cooldown timestamp must survive too, or a hub roll would reset the
	// double-rotation guard.
	if rotatedAt.IsZero() {
		t.Fatal("RotatedAt did not survive the restart — the cooldown would reset on every hub roll")
	}
	if !rotatedAt.Equal(now) {
		t.Errorf("RotatedAt = %v, want %v", rotatedAt, now)
	}

	// The file must be 0600: it holds master secrets in plaintext.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != hubGenerationsFileMode {
		t.Errorf("generations file mode = %o, want %o — it contains master secrets", perm, hubGenerationsFileMode)
	}

	// POSITIVE CONTROL for this test. "reload always returns the legacy set"
	// would pass every assertion above if the legacy set happened to match, so
	// assert the reloaded state is NOT the pre-rotation state.
	legacy := legacyGenerationSet(rotStoreSecretA)
	if reloaded.Current == legacy.Current {
		t.Error("reloaded set is the pre-rotation legacy set — the rotation was not actually persisted")
	}
	if reloaded.currentSecret() == legacy.currentSecret() {
		t.Error("reloaded minting secret is the pre-rotation master")
	}
}

// TestSecondRotationIsRefused is property (2).
func TestSecondRotationIsRefused(t *testing.T) {
	withTempGenerationsPath(t)
	s := newRotationTestHub(t, rotStoreSecretA)
	now := time.Now().UTC()

	first, _, err := s.rotateMasterSecret(now, false)
	if err != nil {
		t.Fatalf("first rotate: %v", err)
	}

	t.Run("immediate second rotation refused", func(t *testing.T) {
		_, decision, err := s.rotateMasterSecret(now.Add(time.Minute), false)
		if err == nil {
			t.Fatal("second rotation was allowed — it would drop the generation most spokes are still on")
		}
		if decision.Allowed {
			t.Error("decision reports Allowed on a refusal")
		}
		if decision.RetryAfter <= 0 {
			t.Error("refusal did not report a RetryAfter")
		}
		// The refusal must be a NO-OP: the set is unchanged.
		if s.currentGenerations().Current != first.Current {
			t.Error("a refused rotation still changed the current generation")
		}
		if s.currentGenerations().currentSecret() != first.currentSecret() {
			t.Error("a refused rotation still changed the minting secret")
		}
	})

	t.Run("force overrides", func(t *testing.T) {
		forced, decision, err := s.rotateMasterSecret(now.Add(time.Minute), true)
		if err != nil {
			t.Fatalf("forced rotation refused: %v", err)
		}
		if !decision.Allowed {
			t.Error("forced decision reports not Allowed")
		}
		if decision.RetryAfter <= 0 {
			t.Error("a forced-within-cooldown rotation should still report the remaining cooldown")
		}
		if forced.Current != first.Current+1 {
			t.Errorf("forced current = %d, want %d", forced.Current, first.Current+1)
		}
		// maxLiveGenerations still holds, and the generation from two rotations
		// ago is GONE — which is exactly the hazard the cooldown guards.
		if len(forced.Generations) != maxLiveGenerations {
			t.Errorf("live generations = %d, want %d", len(forced.Generations), maxLiveGenerations)
		}
		for _, g := range forced.Generations {
			if g.Secret == rotStoreSecretA {
				t.Error("the original master is still live after two rotations — maxLiveGenerations is not holding")
			}
		}
	})

	t.Run("allowed again after the cooldown lapses", func(t *testing.T) {
		s2 := newRotationTestHub(t, rotStoreSecretA)
		base := time.Now().UTC()
		if _, _, err := s2.rotateMasterSecret(base, false); err != nil {
			t.Fatalf("first rotate: %v", err)
		}
		if _, _, err := s2.rotateMasterSecret(base.Add(rotationCooldown+time.Minute), false); err != nil {
			t.Fatalf("rotation after the cooldown was refused: %v", err)
		}
	})
}

// TestEvaluateRotationGuard walks the pure decision function directly.
func TestEvaluateRotationGuard(t *testing.T) {
	now := time.Now()

	t.Run("never rotated is allowed", func(t *testing.T) {
		if d := evaluateRotation(time.Time{}, now, false); !d.Allowed {
			t.Fatalf("a hub that has never rotated was refused: %+v", d)
		}
	})
	t.Run("inside the cooldown is refused", func(t *testing.T) {
		d := evaluateRotation(now.Add(-time.Hour), now, false)
		if d.Allowed {
			t.Fatal("allowed one hour after a rotation")
		}
		if d.Reason == "" {
			t.Error("refusal carries no reason for the operator")
		}
	})
	t.Run("exactly at the cooldown is allowed", func(t *testing.T) {
		if d := evaluateRotation(now.Add(-rotationCooldown), now, false); !d.Allowed {
			t.Fatal("refused exactly at the cooldown boundary")
		}
	})
	t.Run("force allows inside the cooldown", func(t *testing.T) {
		if d := evaluateRotation(now.Add(-time.Hour), now, true); !d.Allowed {
			t.Fatal("force did not override the cooldown")
		}
	})
	// A lastRotation in the FUTURE — clock skew or a hand-edited file — must
	// fail closed rather than compute a nonsense RetryAfter.
	t.Run("future lastRotation fails closed", func(t *testing.T) {
		d := evaluateRotation(now.Add(24*time.Hour), now, false)
		if d.Allowed {
			t.Fatal("allowed with a lastRotation in the future")
		}
		if d.RetryAfter > rotationCooldown {
			t.Errorf("RetryAfter = %v exceeds the cooldown %v", d.RetryAfter, rotationCooldown)
		}
	})
	// The cooldown must be sized to CONVERGENCE, not to the verify window. If
	// it were >= defaultVerifyWindow an emergency re-rotation would be
	// impossible for a week.
	t.Run("cooldown is shorter than the verify window", func(t *testing.T) {
		if rotationCooldown >= defaultVerifyWindow {
			t.Fatalf("rotationCooldown %v >= defaultVerifyWindow %v — emergency re-rotation would be blocked for the whole window",
				rotationCooldown, defaultVerifyWindow)
		}
	})
}

// TestConcurrentRotationsProduceOne is the double-submit guarantee, exercised
// concurrently rather than argued for in a comment.
func TestConcurrentRotationsProduceOne(t *testing.T) {
	withTempGenerationsPath(t)
	s := newRotationTestHub(t, rotStoreSecretA)
	now := time.Now().UTC()

	const attempts = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()
			if _, _, err := s.rotateMasterSecret(now, false); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if succeeded != 1 {
		t.Fatalf("%d of %d concurrent rotations succeeded, want exactly 1 — a double-submit must not rotate twice", succeeded, attempts)
	}
	if got := s.currentGenerations().Current; got != legacyGenerationID+1 {
		t.Errorf("current = %d after %d concurrent attempts, want %d", got, attempts, legacyGenerationID+1)
	}
}

// TestMalformedGenerationsFileFailsClosed is property (3).
//
// INVERTED FOR AUDIT 8 / F20. This test previously asserted that a malformed
// file falls back to the LEGACY single-generation set, and checked
// `gs.currentSecret() == rotStoreSecretA` to prove it. That assertion encoded
// the vulnerability: after a rotation, rotStoreSecretA is SUPERSEDED material,
// and "the malformed file made the hub mint on generation 1 again" is exactly
// the silent un-rotation F20 describes — the old test would have passed on the
// vulnerable code and failed on the fix. It is inverted, not relaxed: every
// case now asserts the STRONGER property that no set is returned at all, so a
// future regression back to the legacy fallback fails here.
func TestMalformedGenerationsFileFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		content string
		// quarantined is whether the bad file should be moved aside.
		quarantined bool
	}{
		{
			name:        "not JSON at all",
			content:     "this is not json {{{",
			quarantined: true,
		},
		{
			name: "current names a generation that is not present",
			content: `{"current": 99, "generations": [
				{"id": 1, "secret": "` + rotStoreSecretA + `"}
			]}`,
		},
		{
			name: "every generation has an empty secret",
			content: `{"current": 2, "generations": [
				{"id": 2, "secret": ""},
				{"id": 1, "secret": "   "}
			]}`,
		},
		{
			name:    "empty generation list",
			content: `{"current": 1, "generations": []}`,
		},
		{
			name:    "empty object",
			content: `{}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := withTempGenerationsPath(t)
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}

			gs, rotatedAt, outcome := loadGenerations(rotStoreSecretA, quietLogger())
			if outcome != generationsUntrusted {
				t.Fatalf("outcome = %v, want generationsUntrusted — a file that EXISTS but cannot be trusted "+
					"is not the same as one that is absent, and must not resolve to 'never rotated'", outcome)
			}
			// The inverted assertion. A malformed file must yield NO set: the
			// legacy set it used to yield is generation 1, which after any
			// rotation is superseded material, so returning it silently
			// un-rotates the hub.
			if gs != nil {
				t.Fatalf("a malformed file yielded a usable generation set (current=%d) — it must fail closed. "+
					"If this set is the legacy one, the hub has silently reverted to the superseded master (F20)", gs.Current)
			}
			// And it must certainly not mint a NEW key — that would be material
			// the fleet has never seen, replacing the generation it is on.
			if gs.currentSecret() != "" {
				t.Fatal("a malformed file produced a minting secret")
			}
			if !rotatedAt.IsZero() {
				t.Error("a malformed file yielded a non-zero RotatedAt")
			}
			if tc.quarantined {
				if _, err := os.Stat(path + hubGenerationsQuarantineSuffix); err != nil {
					t.Errorf("unparseable file was not quarantined for inspection: %v", err)
				}
			}
		})
	}

	// A generation whose VerifyUntil is absent must load, and then be REFUSED
	// at verify time — zero means ALREADY EXPIRED, never "never expires".
	t.Run("previous generation with no verify_until is loaded but not accepted", func(t *testing.T) {
		path := withTempGenerationsPath(t)
		content := `{"current": 2, "generations": [
			{"id": 2, "secret": "` + rotStoreSecretB + `"},
			{"id": 1, "secret": "` + rotStoreSecretA + `"}
		]}`
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		gs, _, _ := loadGenerations(rotStoreSecretA, quietLogger())
		if gs == nil || gs.Current != 2 {
			t.Fatalf("a well-formed file should load; got %+v", gs)
		}
		acceptable := gs.acceptableGenerations(time.Now())
		if len(acceptable) != 1 || acceptable[0].ID != 2 {
			t.Fatalf("acceptable = %+v, want only the current generation — a missing verify_until must read as ALREADY EXPIRED", acceptable)
		}
	})

	// POSITIVE CONTROL for the whole block. Every case above asserts a
	// FALLBACK, and "always fall back to legacy" would satisfy all of them. So
	// assert that a VALID file is honoured and does NOT fall back.
	t.Run("positive control: a valid file is honoured", func(t *testing.T) {
		path := withTempGenerationsPath(t)
		until := time.Now().Add(defaultVerifyWindow).UTC().Format(time.RFC3339)
		content := `{"current": 2, "rotated_at": "` + time.Now().UTC().Format(time.RFC3339) + `", "generations": [
			{"id": 2, "secret": "` + rotStoreSecretB + `"},
			{"id": 1, "secret": "` + rotStoreSecretA + `", "verify_until": "` + until + `"}
		]}`
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		gs, rotatedAt, _ := loadGenerations(rotStoreSecretA, quietLogger())
		if gs == nil {
			t.Fatal("valid file did not load")
		}
		if gs.Current != 2 {
			t.Fatalf("current = %d, want 2 — a VALID file must be honoured, not discarded", gs.Current)
		}
		if gs.currentSecret() != rotStoreSecretB {
			t.Error("valid file did not install its current secret")
		}
		if len(gs.acceptableGenerations(time.Now())) != 2 {
			t.Error("valid file did not yield two acceptable generations")
		}
		if rotatedAt.IsZero() {
			t.Error("valid file did not yield its rotated_at")
		}
		if _, err := os.Stat(path + hubGenerationsQuarantineSuffix); err == nil {
			t.Error("a VALID file was quarantined")
		}
	})
}

// TestMissingGenerationsFileIsNormal — the state of every hub in the fleet.
//
// This is the case F20's fix must NOT break. ENOENT is a positive fact ("no
// rotation has ever been persisted", because the file is only ever created by
// saveGenerations), not an absence of information, so the legacy fallback stays
// exactly as it was here.
func TestMissingGenerationsFileIsNormal(t *testing.T) {
	withTempGenerationsPath(t) // temp dir; the file does not exist
	gs, rotatedAt, outcome := loadGenerations(rotStoreSecretA, quietLogger())
	if outcome != generationsNeverRotated {
		t.Fatalf("outcome = %v, want generationsNeverRotated — an ABSENT file is the ONE case that may fall back to legacy", outcome)
	}
	if gs == nil {
		t.Fatal("a missing generations file must synthesize the legacy set, not return nil")
	}
	if gs.Current != legacyGenerationID || gs.currentSecret() != rotStoreSecretA {
		t.Errorf("got current=%d, want the single legacy generation from hub-secret.key", gs.Current)
	}
	if !rotatedAt.IsZero() {
		t.Error("a never-rotated hub reported a rotation time")
	}
	// An empty master must still fail closed, exactly as before.
	if gs, _, _ := loadGenerations("", quietLogger()); gs != nil {
		t.Error("an empty master produced a generation set")
	}
}

// TestSaveGenerationsRefusesEmpty — never persist a set with no minting key.
func TestSaveGenerationsRefusesEmpty(t *testing.T) {
	path := withTempGenerationsPath(t)
	if err := saveGenerations(nil, time.Now()); err == nil {
		t.Error("saveGenerations(nil) succeeded")
	}
	if err := saveGenerations(&generationSet{Current: 1}, time.Now()); err == nil {
		t.Error("saveGenerations with no generations succeeded")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a refused save still wrote a file")
	}
	// POSITIVE CONTROL: a real set DOES persist.
	if err := saveGenerations(legacyGenerationSet(rotStoreSecretA), time.Now()); err != nil {
		t.Fatalf("saveGenerations refused a valid set: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a valid save wrote nothing: %v", err)
	}
}

// --- Handler tests -------------------------------------------------------

// newRotationHandlerHub builds a hub with a real mux so the requireAdmin
// wrapper — and therefore isCSRFSafe — is actually exercised, rather than
// calling the handler directly and testing nothing about authorisation.
func newRotationHandlerHub(t *testing.T) *HubServer {
	t.Helper()
	withTempGenerationsPath(t)
	s := newRotationTestHub(t, rotStoreSecretA)
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("GET /api/saas/admin/key-generations", s.requireAdmin(s.handleKeyGenerations))
	s.mux.HandleFunc("POST /api/saas/admin/rotate-master-key", s.requireAdmin(s.handleRotateMasterKey))
	return s
}

// TestRotateEndpointRejectsNonAdmin: the endpoint must be unreachable without
// admin, and unreachable cross-site even WITH a session.
func TestRotateEndpointRejectsNonAdmin(t *testing.T) {
	s := newRotationHandlerHub(t)
	beforeCurrent := s.currentGenerations().Current
	beforeSecret := s.currentGenerations().currentSecret()

	post := func(t *testing.T, mutate func(*http.Request)) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/api/saas/admin/rotate-master-key", strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		if mutate != nil {
			mutate(r)
		}
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		return w
	}

	t.Run("unauthenticated is refused", func(t *testing.T) {
		if code := post(t, nil).Code; code == http.StatusOK {
			t.Fatal("an unauthenticated POST rotated the master key")
		}
	})

	t.Run("cross-site is refused by isCSRFSafe", func(t *testing.T) {
		w := post(t, func(r *http.Request) {
			r.Header.Set("Origin", "https://evil.example.com")
		})
		if w.Code != http.StatusForbidden {
			t.Errorf("cross-origin POST got %d, want 403 — requireAdmin must call isCSRFSafe", w.Code)
		}
	})

	// Nothing above may have changed the key material.
	if s.currentGenerations().Current != beforeCurrent {
		t.Error("a rejected request still rotated the current generation")
	}
	if s.currentGenerations().currentSecret() != beforeSecret {
		t.Error("a rejected request still changed the minting secret")
	}
	if _, err := os.Stat(hubGenerationsPath); err == nil {
		t.Error("a rejected request still persisted a generations file")
	}
}

// TestRotateResponseLeaksNoSecret is the secret-hygiene assertion, made by test
// rather than by review: no response body and no persisted-file-adjacent
// surface may contain any master secret.
func TestRotateResponseLeaksNoSecret(t *testing.T) {
	withTempGenerationsPath(t)
	s := newRotationTestHub(t, rotStoreSecretA)
	now := time.Now().UTC()
	next, _, err := s.rotateMasterSecret(now, false)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	newSecret := next.currentSecret()

	// Build the exact response the handler builds.
	w := httptest.NewRecorder()
	s.handleKeyGenerations(w, httptest.NewRequest(http.MethodGet, "/api/saas/admin/key-generations", nil))
	bodies := []string{w.Body.String()}

	rw := httptest.NewRecorder()
	rr := httptest.NewRequest(http.MethodPost, "/api/saas/admin/rotate-master-key", strings.NewReader(`{}`))
	s.handleRotateMasterKey(rw, rr) // refused by the cooldown; still a response body
	bodies = append(bodies, rw.Body.String())

	for i, body := range bodies {
		for _, secret := range []string{newSecret, rotStoreSecretA} {
			if strings.Contains(body, secret) {
				t.Fatalf("response %d contains master secret material", i)
			}
		}
	}

	// The read endpoint must still be USEFUL — a handler that returned nothing
	// would trivially leak nothing. Positive control.
	var view keyGenerationsResponse
	if err := json.Unmarshal([]byte(bodies[0]), &view); err != nil {
		t.Fatalf("key-generations response is not JSON: %v", err)
	}
	if view.Current != next.Current {
		t.Errorf("view current = %d, want %d", view.Current, next.Current)
	}
	if len(view.Generations) != 2 {
		t.Fatalf("view lists %d generations, want 2", len(view.Generations))
	}
	if view.PersistPath == "" {
		t.Error("view does not report where generations are persisted")
	}
	if view.LastRotation == "" {
		t.Error("view does not report the last rotation")
	}
	if view.RotateAvailableInSeconds <= 0 {
		t.Error("view does not report the remaining cooldown right after a rotation")
	}
	var sawCurrent, sawPrevious bool
	for _, g := range view.Generations {
		if g.Current {
			sawCurrent = true
			if !g.Acceptable {
				t.Error("the current generation is reported as not acceptable")
			}
			if g.VerifyUntil != "" {
				t.Error("the current generation carries a verify_until; it never expires")
			}
		} else {
			sawPrevious = true
			if g.VerifyUntil == "" {
				t.Error("the previous generation does not publish its verify_until")
			}
		}
	}
	if !sawCurrent || !sawPrevious {
		t.Error("the view does not distinguish current from previous")
	}
}

// ---------------------------------------------------------------------------
// AUDIT 8 / F20: a non-ENOENT read error must NOT silently un-rotate the hub.
//
// The bug: loadGenerations collapsed "file absent" and "file unreadable" into
// the same return — the legacy single-generation set. Before the first rotation
// those really are the same state. After one they are opposites: generation 1
// is SUPERSEDED material with a VerifyUntil, and re-installing it as the
// current minting generation means the hub re-mints on the old key, drops the
// post-rotation generation out of the accepted set entirely, and returns a zero
// RotatedAt that clears the 8-hour anti-stranding cooldown.
//
// Every test below seeds a ROTATED file first, because that is the only state
// in which the two cases differ — a test written against a never-rotated hub
// would pass on the vulnerable code.
// ---------------------------------------------------------------------------

// seedRotatedGenerationsFile writes a well-formed, ROTATED generations file:
// current is generation 2 on secret B, with generation 1 on secret A demoted
// and still inside its verify window. Returns the path and the rotated_at it
// recorded.
func seedRotatedGenerationsFile(t *testing.T) (string, time.Time) {
	t.Helper()
	path := withTempGenerationsPath(t)
	rotatedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	content := `{"current": 2, "rotated_at": "` + rotatedAt.Format(time.RFC3339) + `", "generations": [
		{"id": 2, "secret": "` + rotStoreSecretB + `", "created": "` + rotatedAt.Format(time.RFC3339) + `"},
		{"id": 1, "secret": "` + rotStoreSecretA + `", "verify_until": "` +
		rotatedAt.Add(defaultVerifyWindow).Format(time.RFC3339) + `"}
	]}`
	if err := os.WriteFile(path, []byte(content), hubGenerationsFileMode); err != nil {
		t.Fatal(err)
	}
	return path, rotatedAt
}

// makeUnreadable chmods the file to 000 so os.ReadFile returns EACCES —
// a real, unsynthesised non-ENOENT read error.
//
// Skips as root, where the mode is not enforced and the read would SUCCEED,
// turning this into a test that silently proves nothing. That is exactly the
// "test passing for the wrong reason" failure mode this repo has been bitten by
// before, so it is made explicit rather than left to chance.
func makeUnreadable(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 000 is not enforced, so the read would succeed and the test would pass vacuously")
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, hubGenerationsFileMode) })
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("this filesystem/user can still read a 0000 file; cannot produce a genuine EACCES here")
	} else if os.IsNotExist(err) {
		t.Fatalf("fixture is wrong: chmod produced ENOENT, not a read error: %v", err)
	}
}

// TestF20_UnreadableGenerationsFileDoesNotRevertToSupersededMaster is the
// central regression for F20.
func TestF20_UnreadableGenerationsFileDoesNotRevertToSupersededMaster(t *testing.T) {
	path, _ := seedRotatedGenerationsFile(t)

	// POSITIVE CONTROL, direction 1: while the file IS readable, the rotated
	// state loads. Without this, "always fail closed" would satisfy the
	// assertions below and the test would prove nothing about the fix.
	pre, preRotatedAt, preOutcome := loadGenerations(rotStoreSecretA, quietLogger())
	if preOutcome != generationsLoaded || pre == nil || pre.Current != 2 {
		t.Fatalf("positive control: a readable rotated file must load as generation 2; got outcome=%v set=%+v", preOutcome, pre)
	}
	if pre.currentSecret() != rotStoreSecretB {
		t.Fatal("positive control: readable rotated file did not mint on the POST-rotation secret")
	}
	if preRotatedAt.IsZero() {
		t.Fatal("positive control: readable rotated file did not yield its rotated_at")
	}

	makeUnreadable(t, path)

	gs, rotatedAt, outcome := loadGenerations(rotStoreSecretA, quietLogger())

	if outcome != generationsUntrusted {
		t.Errorf("outcome = %v, want generationsUntrusted — an unreadable file is NOT 'never rotated'", outcome)
	}
	// THE FINDING. The vulnerable code returned the legacy set here.
	if gs != nil {
		t.Fatalf("a non-ENOENT read error yielded a usable generation set (current=%d, minting on %s) — "+
			"the hub silently un-rotated to the superseded master (F20)",
			gs.Current, secretLabelForTest(gs.currentSecret()))
	}
	if gs.currentSecret() == rotStoreSecretA {
		t.Fatal("the hub is minting on the SUPERSEDED generation-1 master after a read error (F20)")
	}
	if gs.currentSecret() != "" {
		t.Fatal("the hub has a minting secret despite being unable to establish its generation state")
	}
	// Nothing may be accepted from a set the hub cannot vouch for.
	if n := len(gs.acceptableGenerations(time.Now())); n != 0 {
		t.Errorf("acceptable generations = %d on untrusted state, want 0", n)
	}
	// POSITIVE CONTROL, direction 2: restoring readability restores the
	// rotated state, proving the failure above is caused by the read error and
	// not by the fixture being broken.
	if err := os.Chmod(path, hubGenerationsFileMode); err != nil {
		t.Fatal(err)
	}
	post, _, postOutcome := loadGenerations(rotStoreSecretA, quietLogger())
	if postOutcome != generationsLoaded || post == nil || post.Current != 2 {
		t.Fatalf("positive control: the file became loadable again but did not load; outcome=%v set=%+v", postOutcome, post)
	}
	_ = rotatedAt
}

// TestF20_ReadErrorDoesNotClearRotationCooldown covers the second half of the
// finding: RotatedAt going to zero makes evaluateRotation say "never rotated",
// which clears the 8-hour anti-stranding cooldown.
func TestF20_ReadErrorDoesNotClearRotationCooldown(t *testing.T) {
	path, seededRotatedAt := seedRotatedGenerationsFile(t)

	// POSITIVE CONTROL: the seeded rotation is INSIDE the cooldown while the
	// file is readable, so a second rotation is refused.
	_, readableRotatedAt, _ := loadGenerations(rotStoreSecretA, quietLogger())
	if !readableRotatedAt.Equal(seededRotatedAt) {
		t.Fatalf("positive control: rotated_at = %v, want %v", readableRotatedAt, seededRotatedAt)
	}
	if d := evaluateRotation(readableRotatedAt, time.Now(), false); d.Allowed {
		t.Fatal("positive control: a rotation one hour old must be inside the 8h cooldown")
	}

	makeUnreadable(t, path)

	gs, rotatedAt, outcome := loadGenerations(rotStoreSecretA, quietLogger())
	if outcome != generationsUntrusted {
		t.Fatalf("outcome = %v, want generationsUntrusted", outcome)
	}

	// On the vulnerable code rotatedAt is zero here AND a legacy set is
	// installed, so evaluateRotation returns Allowed — the cooldown is gone.
	// The fix does return a zero rotatedAt (there is nothing to read it from),
	// but it returns NO SET, and rotateMasterSecret refuses outright on a nil
	// set. That is what has to be asserted: not the timestamp in isolation, but
	// that a rotation cannot proceed.
	s := &HubServer{logger: quietLogger(), hubSecret: rotStoreSecretA, keyGenerations: gs, lastKeyRotation: rotatedAt}
	next, decision, err := s.rotateMasterSecret(time.Now(), false)
	if err == nil {
		t.Fatal("a rotation was ALLOWED after a generations-file read error — the anti-stranding guard was cleared (F20)")
	}
	if !errors.Is(err, errGenerationsUntrusted) {
		t.Errorf("err = %v, want errGenerationsUntrusted", err)
	}
	if decision.Allowed {
		t.Error("rotationDecision.Allowed is true on untrusted generation state")
	}
	if next != nil {
		t.Fatal("a rotation produced a new generation set from untrusted state — it would strand the fleet's actual current generation")
	}

	// And force must NOT override it. force exists to beat the convergence
	// cooldown when the new generation is compromised, not to rotate from a
	// state the hub cannot read.
	if _, _, err := s.rotateMasterSecret(time.Now(), true); err == nil {
		t.Fatal("force=true rotated from untrusted generation state — force must not override 'I do not know what generation I am on'")
	}
}

// TestF20_TruncatedGenerationsFileFailsClosed covers the partial-write case: a
// rotated file cut short by a crash mid-rename or a PVC fault. It must not read
// as "empty" and therefore as "never rotated".
func TestF20_TruncatedGenerationsFileFailsClosed(t *testing.T) {
	path, _ := seedRotatedGenerationsFile(t)

	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// POSITIVE CONTROL: the untruncated bytes load as the rotated set.
	if gs, _, outcome := loadGenerations(rotStoreSecretA, quietLogger()); outcome != generationsLoaded || gs == nil || gs.Current != 2 {
		t.Fatalf("positive control: the full file must load as generation 2; got outcome=%v set=%+v", outcome, gs)
	}

	// Truncate to half. Valid JSON prefix, invalid JSON document.
	if err := os.WriteFile(path, full[:len(full)/2], hubGenerationsFileMode); err != nil {
		t.Fatal(err)
	}

	gs, rotatedAt, outcome := loadGenerations(rotStoreSecretA, quietLogger())
	if outcome != generationsUntrusted {
		t.Errorf("outcome = %v, want generationsUntrusted for a truncated file", outcome)
	}
	if gs != nil {
		t.Fatalf("a truncated file yielded a usable set (current=%d) — a partial write of a ROTATED file "+
			"must not resolve to the superseded generation 1", gs.Current)
	}
	if gs.currentSecret() == rotStoreSecretA {
		t.Fatal("a truncated file put the hub back on the superseded master (F20)")
	}
	if !rotatedAt.IsZero() {
		t.Error("a truncated file yielded a non-zero rotated_at")
	}
	// The bad bytes are preserved for the operator rather than overwritten.
	if _, err := os.Stat(path + hubGenerationsQuarantineSuffix); err != nil {
		t.Errorf("truncated file was not quarantined for inspection: %v", err)
	}
}

// TestF20_ReadIsRetriedBeforeFailingClosed asserts the availability half of the
// trade: a fault that clears must not take the hub out of minting. The file is
// unreadable on the first attempt and readable by the time the loader retries.
func TestF20_ReadIsRetriedBeforeFailingClosed(t *testing.T) {
	path, _ := seedRotatedGenerationsFile(t)
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 000 is not enforced")
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("this filesystem/user can still read a 0000 file")
	}

	// Restore readability right after attempt 0 is observed to have failed,
	// synchronized on the loader's own progress via afterGenerationsReadAttempt
	// instead of a wall-clock guess. The original version raced a fixed
	// generationsReadRetryDelay*1.5 sleep against readGenerationsFile's own
	// generationsReadRetryDelay sleep before its 3rd/final attempt (attempts
	// land at ~0/100/200ms): under load the restore goroutine's time.Sleep can
	// overshoot that ~50ms margin, so the final read still sees EACCES and the
	// loader correctly, but flakily, fails closed (#5080). Hooking the actual
	// attempt boundary removes the margin entirely — the restore is scheduled
	// the instant attempt 0 is known to have failed, not "probably by then".
	restored := make(chan struct{})
	t.Cleanup(func() {
		afterGenerationsReadAttempt = nil
		select {
		case <-restored:
		case <-time.After(time.Second):
			t.Error("restore goroutine never signalled completion")
		}
	})
	afterGenerationsReadAttempt = func(attempt int) {
		if attempt != 0 {
			return
		}
		afterGenerationsReadAttempt = nil // fire once
		go func() {
			defer close(restored)
			_ = os.Chmod(path, hubGenerationsFileMode)
		}()
	}

	gs, _, outcome := loadGenerations(rotStoreSecretA, quietLogger())
	if outcome != generationsLoaded {
		t.Fatalf("outcome = %v, want generationsLoaded — a TRANSIENT fault must be retried, not turned into a fail-closed hub", outcome)
	}
	if gs == nil || gs.Current != 2 {
		t.Fatalf("the rotated set did not load after the transient fault cleared; got %+v", gs)
	}
	// POSITIVE CONTROL: the retry must not be so generous that it papers over a
	// PERMANENT fault. An ENOENT is returned immediately with no retry at all.
	withTempGenerationsPath(t)
	start := time.Now()
	if _, _, o := loadGenerations(rotStoreSecretA, quietLogger()); o != generationsNeverRotated {
		t.Fatalf("outcome = %v, want generationsNeverRotated for an absent file", o)
	}
	if elapsed := time.Since(start); elapsed >= generationsReadRetryDelay {
		t.Errorf("an absent file took %v — ENOENT must not be retried; it is the fleet's normal state", elapsed)
	}
}

// TestF20_NewHubServerDoesNotServeSupersededMasterOnReadError is the end-to-end
// assertion at the only call site that matters: NewHubServer installs a
// provisional LEGACY set before loading, so a loader that merely returns nil
// would leave the superseded master in place anyway. The server must actively
// drop it.
func TestF20_NewHubServerDoesNotServeSupersededMasterOnReadError(t *testing.T) {
	path, _ := seedRotatedGenerationsFile(t)

	// POSITIVE CONTROL: with a readable file the hub comes up on generation 2.
	ok := newHubServerForGenerationsTest(t, rotStoreSecretA)
	if gs := ok.currentGenerations(); gs == nil || gs.Current != 2 {
		t.Fatalf("positive control: hub did not come up on the rotated generation; got %+v", gs)
	}

	makeUnreadable(t, path)

	s := newHubServerForGenerationsTest(t, rotStoreSecretA)
	gs := s.currentGenerations()
	if gs != nil {
		t.Fatalf("hub came up with a generation set (current=%d) after a generations-file read error; "+
			"if that is the legacy set the hub is minting on SUPERSEDED material (F20)", gs.Current)
	}
	if gs.currentSecret() != "" {
		t.Fatal("hub has a minting secret it cannot vouch for")
	}
	// The fleet must stay up: heartbeat verification runs off the single-master
	// path, which is untouched, so an existing spoke still authenticates.
	if s.hubSecret != rotStoreSecretA {
		t.Fatal("fixture: hubSecret was not preserved")
	}
	if key := s.heartbeatKeyFor(rotStoreHiveIDForTest); key == "" {
		t.Error("per-hive heartbeat key derivation broke — failing closed must not brick the fleet")
	}
	if !s.verifyHeartbeatBearer(s.heartbeatKeyFor(rotStoreHiveIDForTest), rotStoreHiveIDForTest) {
		t.Error("an existing spoke can no longer authenticate — this fix must hold minting, not take the fleet down")
	}
	// And rotation is held.
	if _, _, err := s.rotateMasterSecret(time.Now(), false); err == nil {
		t.Error("rotation was allowed on a hub with untrusted generation state")
	}
}

const rotStoreHiveIDForTest = "f20-test-hive"

// newHubServerForGenerationsTest exercises the NewHubServer generations-restore
// block without standing up the rest of the server, which reads the registry,
// clusters, and several other PVC paths. It mirrors server.go's switch exactly;
// if that block changes, this must change with it.
func newHubServerForGenerationsTest(t *testing.T, master string) *HubServer {
	t.Helper()
	s := &HubServer{
		logger:         quietLogger(),
		hubSecret:      master,
		keyGenerations: legacyGenerationSet(master),
	}
	switch gs, rotatedAt, outcome := loadGenerations(master, s.logger); outcome {
	case generationsLoaded:
		s.keyGenerations = gs
		s.lastKeyRotation = rotatedAt
	case generationsNeverRotated:
		if gs != nil {
			s.keyGenerations = gs
		}
	case generationsUntrusted:
		s.keyGenerations = nil
	}
	return s
}

// secretLabelForTest renders a secret as a length + short hash prefix so a
// failure message can identify WHICH master without ever printing one.
func secretLabelForTest(secret string) string {
	if secret == "" {
		return "<none>"
	}
	sum := sha256.Sum256([]byte(secret))
	return "len=" + strconv.Itoa(len(secret)) + " sha256=" + hex.EncodeToString(sum[:4])
}
