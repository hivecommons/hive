package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Handler-level tests for handleRotateMasterKey (hub_generations_handler.go).
//
// The store-level mechanics (demotion, persistence, cooldown, double-submit)
// are asserted in hub_generations_store_test.go; the admin-gate and CSRF
// refusals in TestRotateEndpointRejectsNonAdmin; and the secret-hygiene
// property in TestRotateResponseLeaksNoSecret. What was NOT covered before this
// file is the handler's own branchwork: the malformed-body 400, the SUCCESS
// response shape, the forced-rotation path, the stranding 409 (which, unlike
// the cooldown 409, must carry NO RetryAfter), and the persist-failure 500
// (which must leave the in-memory set untouched so a retry is safe).

// rotatePOST drives the handler directly, the same way the existing
// TestRotateResponseLeaksNoSecret does. Authorisation is asserted separately by
// TestRotateEndpointRejectsNonAdmin through the real mux + requireAdmin.
func rotatePOST(t *testing.T, s *HubServer, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/saas/admin/rotate-master-key", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleRotateMasterKey(w, r)
	return w
}

// TestRotateHandlerMalformedBodyIs400 — only MALFORMED JSON is an error; it
// must be refused before any rotation logic runs.
func TestRotateHandlerMalformedBodyIs400(t *testing.T) {
	path := withTempGenerationsPath(t)
	s := newRotationTestHub(t, rotStoreSecretA)
	beforeSecret := s.currentGenerations().currentSecret()

	w := rotatePOST(t, s, `{not json`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed body got %d, want 400", w.Code)
	}
	if s.currentGenerations().currentSecret() != beforeSecret {
		t.Error("a malformed request still rotated the minting secret")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a malformed request still persisted a generations file")
	}
}

// TestRotateHandlerSuccessResponseShape asserts the 200 body carries exactly
// the operator-facing facts the doc comment promises: the new current, the
// demoted previous with its verify_until, every live generation, a parseable
// rotation time, forced=false for an unforced rotation, and the convergence
// note — and (re-asserting hygiene on the SUCCESS path, which
// TestRotateResponseLeaksNoSecret only exercised via a cooldown refusal) no
// secret material.
func TestRotateHandlerSuccessResponseShape(t *testing.T) {
	withTempGenerationsPath(t)
	s := newRotationTestHub(t, rotStoreSecretA)

	// An EMPTY body must behave as force=false: a plain POST with no body
	// performs an unforced rotation.
	w := rotatePOST(t, s, "")
	if w.Code != http.StatusOK {
		t.Fatalf("rotation got %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp rotateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("success response is not JSON: %v", err)
	}
	if !resp.OK {
		t.Error("ok = false on a successful rotation")
	}
	if resp.Current != legacyGenerationID+1 {
		t.Errorf("current = %d, want %d", resp.Current, legacyGenerationID+1)
	}
	if resp.Previous != legacyGenerationID {
		t.Errorf("previous = %d, want %d", resp.Previous, legacyGenerationID)
	}
	if resp.PreviousVerifyUntil == "" {
		t.Error("previous_verify_until is empty — the demoted generation's deadline is the fact an operator most needs")
	} else if _, err := time.Parse(time.RFC3339, resp.PreviousVerifyUntil); err != nil {
		t.Errorf("previous_verify_until %q is not RFC3339: %v", resp.PreviousVerifyUntil, err)
	}
	if len(resp.LiveGenerations) != 2 {
		t.Fatalf("live_generations = %v, want the new current and the demoted previous", resp.LiveGenerations)
	}
	seen := map[int]bool{}
	for _, id := range resp.LiveGenerations {
		seen[id] = true
	}
	if !seen[resp.Current] || !seen[resp.Previous] {
		t.Errorf("live_generations %v does not list current %d and previous %d", resp.LiveGenerations, resp.Current, resp.Previous)
	}
	if _, err := time.Parse(time.RFC3339, resp.RotatedAt); err != nil {
		t.Errorf("rotated_at %q is not RFC3339: %v", resp.RotatedAt, err)
	}
	if resp.Forced {
		t.Error("forced = true on an unforced rotation")
	}
	if resp.Note == "" {
		t.Error("note is empty — the operator is not told the rotation has not finished converging")
	}

	// Secret hygiene on the SUCCESS body.
	newSecret := s.currentGenerations().currentSecret()
	for _, secret := range []string{rotStoreSecretA, newSecret} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("success response contains master secret material")
		}
	}
}

// TestRotateHandlerForcedWithinCooldown — force=true overrides the convergence
// cooldown and the response must RECORD that it was forced, so the audit log
// and the operator cannot disagree about whether the override was used.
func TestRotateHandlerForcedWithinCooldown(t *testing.T) {
	withTempGenerationsPath(t)
	s := newRotationTestHub(t, rotStoreSecretA)

	if w := rotatePOST(t, s, `{}`); w.Code != http.StatusOK {
		t.Fatalf("first rotation got %d, want 200; body: %s", w.Code, w.Body.String())
	}

	// Inside the cooldown, an unforced second rotation is refused with the
	// retry hint...
	w := rotatePOST(t, s, `{}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("unforced second rotation got %d, want 409", w.Code)
	}
	var refused rotateRefusedResponse
	if err := json.Unmarshal(w.Body.Bytes(), &refused); err != nil {
		t.Fatalf("refusal response is not JSON: %v", err)
	}
	if refused.RetryAfterSeconds <= 0 {
		t.Error("cooldown refusal carries no retry_after_seconds — the operator cannot know when to retry")
	}
	if refused.Current != legacyGenerationID+1 {
		t.Errorf("refusal reports current = %d, want the unchanged %d", refused.Current, legacyGenerationID+1)
	}

	// ...and a forced one proceeds, flagged as forced.
	w = rotatePOST(t, s, `{"force": true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("forced rotation got %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp rotateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("forced response is not JSON: %v", err)
	}
	if !resp.Forced {
		t.Error("forced = false on a forced rotation — the override left no trace in the response")
	}
	if resp.Current != legacyGenerationID+2 {
		t.Errorf("current = %d after two rotations, want %d", resp.Current, legacyGenerationID+2)
	}
}

// TestRotateHandlerStrandingRefusalHasNoRetryAfter — the F19+F21 stranding 409
// is deliberately DIFFERENT from the cooldown 409: it carries no RetryAfter
// because it does not clear on its own, and force must not reach it.
func TestRotateHandlerStrandingRefusalHasNoRetryAfter(t *testing.T) {
	path := withTempGenerationsPath(t)
	s := newRotationTestHub(t, rotStoreSecretA)
	// Fleet NOT fully observed: one hive unreachable.
	s.perHiveEnvConsidered = 2
	s.perHiveEnvUnreachable = 1
	beforeSecret := s.currentGenerations().currentSecret()

	for _, body := range []string{`{}`, `{"force": true}`} {
		w := rotatePOST(t, s, body)
		if w.Code != http.StatusConflict {
			t.Fatalf("body %s: got %d, want 409 while the fleet is not fully observed", body, w.Code)
		}
		var refused rotateRefusedResponse
		if err := json.Unmarshal(w.Body.Bytes(), &refused); err != nil {
			t.Fatalf("refusal response is not JSON: %v", err)
		}
		if refused.RetryAfterSeconds != 0 {
			t.Errorf("body %s: stranding refusal carries retry_after_seconds=%d, want 0 — this refusal does not clear on its own", body, refused.RetryAfterSeconds)
		}
		if refused.Error == "" {
			t.Errorf("body %s: stranding refusal has no error message for the operator", body)
		}
		if refused.Current != legacyGenerationID {
			t.Errorf("body %s: refusal reports current = %d, want the unchanged %d", body, refused.Current, legacyGenerationID)
		}
	}

	if s.currentGenerations().currentSecret() != beforeSecret {
		t.Error("a stranding-refused request still rotated the minting secret")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a stranding-refused request still persisted a generations file")
	}
}

// TestRotateHandlerUntrustedStateIs500 — a nil generation set means the loader
// could not establish the hub's current generation; the handler must land in
// the generic-failure branch (500), not rotate, and not persist.
func TestRotateHandlerUntrustedStateIs500(t *testing.T) {
	path := withTempGenerationsPath(t)
	s := newRotationTestHub(t, rotStoreSecretA)
	s.keyGenerations = nil // generationsUntrusted at startup

	w := rotatePOST(t, s, `{"force": true}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("untrusted state got %d, want 500", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failure response is not JSON: %v", err)
	}
	if body["error"] == "" {
		t.Error("failure response carries no error message")
	}
	if s.currentGenerations() != nil {
		t.Error("a refused untrusted-state rotation still installed a generation set")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a refused untrusted-state rotation still persisted a generations file")
	}
}

// TestRotateHandlerPersistFailureIsCleanNoOp — persist-then-install means a
// failed save is a 500 AND a no-op: the in-memory set must be untouched so the
// operator can simply retry.
func TestRotateHandlerPersistFailureIsCleanNoOp(t *testing.T) {
	// Point the persistence path inside a read-only directory so
	// saveGenerations fails at write time.
	prev := hubGenerationsPath
	roDir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	hubGenerationsPath = filepath.Join(roDir, "hub-generations.json")
	t.Cleanup(func() {
		hubGenerationsPath = prev
		_ = os.Chmod(roDir, 0o755) // let TempDir cleanup succeed
	})
	if f, err := os.Create(hubGenerationsPath); err == nil {
		f.Close()
		t.Skip("directory permissions are not enforced here (running as root?)")
	}

	s := newRotationTestHub(t, rotStoreSecretA)
	before := s.currentGenerations()
	beforeSecret := before.currentSecret()

	w := rotatePOST(t, s, `{}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("persist failure got %d, want 500; body: %s", w.Code, w.Body.String())
	}

	after := s.currentGenerations()
	if after.Current != before.Current || after.currentSecret() != beforeSecret {
		t.Error("a failed persist still installed the new set in memory — the hub would mint on a key it forgets at its next roll")
	}

	// And the failure is retryable: with a writable path the same hub rotates.
	hubGenerationsPath = filepath.Join(t.TempDir(), "hub-generations.json")
	if w := rotatePOST(t, s, `{}`); w.Code != http.StatusOK {
		t.Errorf("retry after a persist failure got %d, want 200 — the failed attempt was not a clean no-op", w.Code)
	}
}
