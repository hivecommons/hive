package hub

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
//  3. A MALFORMED FILE FAILS CLOSED to the legacy single-generation set, and is
//     never replaced by a fresh rotation.
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

func newRotationTestHub(t *testing.T, master string) *HubServer {
	t.Helper()
	return &HubServer{
		logger:          quietLogger(),
		hubSecret:       master,
		keyGenerations:  legacyGenerationSet(master),
		lastKeyRotation: time.Time{},
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
	reloaded, rotatedAt := loadGenerations(rotStoreSecretA, quietLogger())
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
func TestMalformedGenerationsFileFailsClosed(t *testing.T) {
	legacy := legacyGenerationSet(rotStoreSecretA)

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

			gs, rotatedAt := loadGenerations(rotStoreSecretA, quietLogger())
			if gs == nil {
				t.Fatal("loadGenerations returned nil — the hub would have no minting key at all")
			}
			// Fails closed to the LEGACY set, which is always correct because
			// hub-secret.key is authoritative for generation 1.
			if gs.Current != legacy.Current {
				t.Errorf("current = %d, want the legacy generation %d", gs.Current, legacy.Current)
			}
			if gs.currentSecret() != rotStoreSecretA {
				t.Error("did not fall back to the master from hub-secret.key")
			}
			if len(gs.Generations) != 1 {
				t.Errorf("live generations = %d, want exactly the single legacy generation", len(gs.Generations))
			}
			if !rotatedAt.IsZero() {
				t.Error("a malformed file yielded a non-zero RotatedAt")
			}
			// A malformed file must NOT trigger a fresh rotation — that would
			// mint material the fleet has never seen while forgetting the
			// generation it is actually on.
			if gs.currentSecret() != legacy.currentSecret() {
				t.Fatal("a malformed file caused the hub to mint on a NEW key — it must fall back, not rotate")
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
		gs, _ := loadGenerations(rotStoreSecretA, quietLogger())
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
		gs, rotatedAt := loadGenerations(rotStoreSecretA, quietLogger())
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
func TestMissingGenerationsFileIsNormal(t *testing.T) {
	withTempGenerationsPath(t) // temp dir; the file does not exist
	gs, rotatedAt := loadGenerations(rotStoreSecretA, quietLogger())
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
	if gs, _ := loadGenerations("", quietLogger()); gs != nil {
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
