package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ============================================================
// digest_pin.go — handleGetDigestPin + digestPinCORS branches
// ============================================================
//
// handleGetDigestPin is the read side of the rollback runbook: a script (or
// the dashboard) asks the hub "is this hive pinned, and has the pin landed on
// the Deployment?". These tests pin through the same handler the operators
// use, then read the state back, so the GET view is checked against the real
// pin lifecycle rather than hand-assembled records.

// getPin issues GET /api/saas/hives/{id}/digest-pin as username for hiveID.
func getPin(t *testing.T, s *HubServer, username, hiveID string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodGet, "/digest-pin", "", username), "id", hiveID)
	s.handleGetDigestPin(rec, req)
	return rec
}

func decodePinBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return out
}

// TestGetDigestPin_Unpinned: an unpinned hive reports pinned:false with the
// spoke's reported image, and carries no pin/image/landed keys.
func TestGetDigestPin_Unpinned(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newPinHub()
	seedPinHive(t, s, false)

	rec := getPin(t, s, testPinOwner, testPinHiveID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decodePinBody(t, rec)
	if body["ok"] != true || body["pinned"] != false {
		t.Errorf("body = %v, want ok:true pinned:false", body)
	}
	if body["reported_image"] != "ghcr.io/hivecommons/hive:stable" {
		t.Errorf("reported_image = %v, want the registry's stable tag", body["reported_image"])
	}
	for _, k := range []string{"pin", "image", "landed"} {
		if _, present := body[k]; present {
			t.Errorf("unpinned response carries %q: %v", k, body[k])
		}
	}
}

// TestGetDigestPin_PinnedThenLanded: after a real pin the GET reports the pin
// with its provenance and landed:false while the spoke still reports the old
// tag; once the registry carries the pinned digest, landed flips to true —
// the hub-side witness the rollback runbook asks operators to verify.
func TestGetDigestPin_PinnedThenLanded(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	stubSpokeImageExists(t)
	installDigestPinKubectl(t, "ghcr.io/hivecommons/hive:stable")
	s := newPinHub()
	seedPinHive(t, s, false)

	if rec := postPin(t, s, testPinOwner, `{"digest":"`+testPinDigest+`","reason":"rollback"}`); rec.Code != http.StatusOK {
		t.Fatalf("pin status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec := getPin(t, s, testPinOwner, testPinHiveID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decodePinBody(t, rec)
	if body["pinned"] != true {
		t.Fatalf("pinned = %v, want true", body["pinned"])
	}
	wantImage := "ghcr.io/hivecommons/hive@" + testPinDigest
	if body["image"] != wantImage {
		t.Errorf("image = %v, want %q", body["image"], wantImage)
	}
	if body["landed"] != false {
		t.Errorf("landed = %v, want false while the spoke still reports the stable tag", body["landed"])
	}
	pin, ok := body["pin"].(map[string]any)
	if !ok {
		t.Fatalf("pin = %v, want the pin record", body["pin"])
	}
	if pin["digest"] != testPinDigest || pin["by"] != testPinOwner {
		t.Errorf("pin provenance = %v, want digest=%q by=%q", pin, testPinDigest, testPinOwner)
	}

	// The spoke's next heartbeat reports the pinned image: landed flips.
	s.mu.Lock()
	s.registry.Hives[0].ImageRef = wantImage
	s.mu.Unlock()
	body = decodePinBody(t, getPin(t, s, testPinOwner, testPinHiveID))
	if body["landed"] != true {
		t.Errorf("landed = %v after the registry carries the pinned digest, want true", body["landed"])
	}
}

// TestGetDigestPin_NotFound: an unknown hive id is a 404, not a zero-value
// "unpinned" answer a rollback script could mistake for success.
func TestGetDigestPin_NotFound(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newPinHub()
	seedPinHive(t, s, false)

	rec := getPin(t, s, testPinOwner, "no-such-hive")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d body=%s, want 404", rec.Code, rec.Body.String())
	}
}

// TestGetDigestPin_NonOwnerRefused: pin state (who pinned, why, and whether a
// rollback landed) is owner-only, like the timeline.
func TestGetDigestPin_NonOwnerRefused(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newPinHub()
	seedPinHive(t, s, false)

	rec := getPin(t, s, testPinStranger, testPinHiveID)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d body=%s, want 403", rec.Code, rec.Body.String())
	}
}

// TestDigestPinCORS_SameOriginPreflight: an OPTIONS preflight from the hub's
// own origin gets the CORS grant and a 204, and tells the handler to stop.
func TestDigestPinCORS_SameOriginPreflight(t *testing.T) {
	req := httptest.NewRequest(http.MethodOptions, "/pin", nil)
	req.Header.Set("Origin", "http://localhost:8080")
	rec := httptest.NewRecorder()

	if digestPinCORS(rec, req) {
		t.Error("digestPinCORS returned true for OPTIONS, want false (preflight handled)")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:8080" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the localhost origin echoed", got)
	}
	if rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Error("Access-Control-Allow-Credentials missing for the same-origin grant")
	}
}

// TestDigestPinCORS_ForeignOriginNoGrant: a sibling-tenant (or hostile) origin
// gets no CORS headers — the browser, not the hub, then blocks the response.
// A non-OPTIONS request still proceeds, because cookie-auth is the real gate.
func TestDigestPinCORS_ForeignOriginNoGrant(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/pin", nil)
	req.Header.Set("Origin", "https://tenant.hive.example.test")
	rec := httptest.NewRecorder()

	if !digestPinCORS(rec, req) {
		t.Error("digestPinCORS returned false for POST, want true (continue to handler)")
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q for a foreign origin, want none", got)
	}
}
