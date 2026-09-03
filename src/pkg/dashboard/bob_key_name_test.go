package dashboard

// Tests for the optional key NAME/label recorded alongside the bob API key
// (#3596). The name is safe-to-show metadata that lets managers tell WHICH key
// a hive uses without ever exposing the value. These tests pin the round-trip,
// the backwards-compatible absent-name state, and that the label is never
// confused with (nor allowed to leak) the secret.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"gopkg.in/yaml.v3"
)

// Setting a key WITH a name records the label and returns it in the response,
// while the key value itself never appears.
func TestHandleGovernorBobKey_SetWithNameRecordsLabel(t *testing.T) {
	pointBobKeyAtTempDir(t)
	s := newBobTestServer(t)

	const wantName = "Team inference key"
	body := `{"apiKey":"` + bobTestKey + `","keyName":"` + wantName + `"}`
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/bob", strings.NewReader(body))
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.handleGovernorBobKey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), bobTestKey) {
		t.Error("response body leaks the key value")
	}
	if got := s.deps.Config.Governor.Bob.KeyName; got != wantName {
		t.Errorf("stored KeyName = %q, want %q", got, wantName)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["keyName"] != wantName {
		t.Errorf("response keyName = %v, want %q", resp["keyName"], wantName)
	}
}

// A name is trimmed of surrounding whitespace (it is a display string) but
// interior spaces are preserved.
func TestHandleGovernorBobKey_NameIsTrimmed(t *testing.T) {
	pointBobKeyAtTempDir(t)
	s := newBobTestServer(t)

	body := `{"apiKey":"` + bobTestKey + `","keyName":"   spaced label   "}`
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/bob", strings.NewReader(body))
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.handleGovernorBobKey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Governor.Bob.KeyName; got != "spaced label" {
		t.Errorf("KeyName = %q, want %q", got, "spaced label")
	}
}

// An oversized name is rejected and nothing is stored.
func TestHandleGovernorBobKey_OversizedNameRejected(t *testing.T) {
	path := pointBobKeyAtTempDir(t)
	s := newBobTestServer(t)

	body := `{"apiKey":"` + bobTestKey + `","keyName":"` + strings.Repeat("x", bobKeyNameMaxLen+1) + `"}`
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/bob", strings.NewReader(body))
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.handleGovernorBobKey(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("key file written for a request rejected on name length")
	}
	if s.deps.Config.Governor.Bob.KeyName != "" {
		t.Error("a rejected oversized name was still recorded")
	}
}

// BACKWARDS COMPAT: setting a key with NO keyName field leaves the response and
// config name empty — never an error. This is how existing hives (which never
// recorded a name) behave.
func TestHandleGovernorBobKey_AbsentNameIsSafe(t *testing.T) {
	pointBobKeyAtTempDir(t)
	s := newBobTestServer(t)

	body := `{"apiKey":"` + bobTestKey + `"}`
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/bob", strings.NewReader(body))
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.handleGovernorBobKey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Governor.Bob.KeyName; got != "" {
		t.Errorf("KeyName = %q, want empty when no name is supplied", got)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["keyName"] != "" {
		t.Errorf("response keyName = %v, want empty string", resp["keyName"])
	}
}

// An absent (nil) keyName on a "Replace key" PUT keeps the existing name, so
// rotating a key without re-typing its label does not silently wipe it. An
// explicit empty string clears it.
func TestHandleGovernorBobKey_ReplaceKeepsThenClearsName(t *testing.T) {
	pointBobKeyAtTempDir(t)
	s := newBobTestServer(t)
	s.deps.Config.Governor.Bob.KeyName = "existing label"

	// Replace the key, sending no keyName field: the label must survive.
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/bob",
		strings.NewReader(`{"apiKey":"`+bobTestKey+`"}`))
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.handleGovernorBobKey(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("replace status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Governor.Bob.KeyName; got != "existing label" {
		t.Errorf("name after nameless replace = %q, want it preserved", got)
	}

	// Now send an explicit empty string: the label is cleared.
	req = httptest.NewRequest(http.MethodPut, "/api/config/governor/bob",
		strings.NewReader(`{"apiKey":"`+bobTestKey+`","keyName":""}`))
	rec = httptest.NewRecorder()
	markOwnerRequest(req)
	s.handleGovernorBobKey(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear-name status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Governor.Bob.KeyName; got != "" {
		t.Errorf("name after explicit empty = %q, want cleared", got)
	}
}

// The status endpoint surfaces the recorded name (safe metadata) and never the
// key value.
func TestHandleGovernorBobStatus_ReturnsKeyName(t *testing.T) {
	path := pointBobKeyAtTempDir(t)
	s := newBobTestServer(t)

	if err := writeBobKeyFile(bobTestKey); err != nil {
		t.Fatal(err)
	}
	s.deps.Config.Governor.Bob.APIKeyFile = path
	s.deps.Config.Governor.Bob.KeyName = "prod key"

	rec := httptest.NewRecorder()
	s.handleGovernorBobStatus(rec, httptest.NewRequest(http.MethodGet, "/api/config/governor/bob", nil))
	if strings.Contains(rec.Body.String(), bobTestKey) {
		t.Fatal("status response leaks the key value")
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["keyName"] != "prod key" {
		t.Errorf("status keyName = %v, want %q", resp["keyName"], "prod key")
	}
}

// Clearing the key drops the label too, so a stale name never sits beside an
// empty key slot.
func TestHandleGovernorBobKeyClear_DropsName(t *testing.T) {
	path := pointBobKeyAtTempDir(t)
	s := newBobTestServer(t)

	if err := writeBobKeyFile(bobTestKey); err != nil {
		t.Fatal(err)
	}
	s.deps.Config.Governor.Bob.APIKeyFile = path
	s.deps.Config.Governor.Bob.KeyName = "to be removed"

	req := httptest.NewRequest(http.MethodDelete, "/api/config/governor/bob", nil)
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.handleGovernorBobKeyClear(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Governor.Bob.KeyName; got != "" {
		t.Errorf("KeyName = %q, want cleared alongside the key", got)
	}
}

// BACKWARDS COMPAT at the config layer: a BobConfig with no key_name
// round-trips through YAML byte-identically (the omitempty tag), so existing
// hives that never recorded a name are not rewritten with a spurious field.
func TestBobConfig_KeyNameOmitemptyRoundTrip(t *testing.T) {
	// No name set: the field must NOT appear in the marshaled YAML.
	noName := config.BobConfig{APIKeyFile: "/data/secrets/bob_api_key"}
	out, err := yaml.Marshal(noName)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "key_name") {
		t.Errorf("empty KeyName leaked a key_name field into YAML:\n%s", out)
	}

	// Name set: it round-trips.
	withName := config.BobConfig{APIKeyFile: "/data/secrets/bob_api_key", KeyName: "Team key"}
	out, err = yaml.Marshal(withName)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "key_name: Team key") {
		t.Errorf("KeyName did not marshal as key_name:\n%s", out)
	}
	var back config.BobConfig
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if back.KeyName != "Team key" {
		t.Errorf("KeyName after round-trip = %q, want %q", back.KeyName, "Team key")
	}
}

// Boundary: a name at exactly bobKeyNameMaxLen is accepted; one char over is
// rejected. This pins the off-by-one boundary the oversized test alone cannot
// catch.
func TestHandleGovernorBobKey_ExactMaxLenNameAccepted(t *testing.T) {
	pointBobKeyAtTempDir(t)
	s := newBobTestServer(t)

	maxName := strings.Repeat("a", bobKeyNameMaxLen) // exactly 128
	body := `{"apiKey":"` + bobTestKey + `","keyName":"` + maxName + `"}`
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/bob", strings.NewReader(body))
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.handleGovernorBobKey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a name at exactly max len (body: %s)", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Governor.Bob.KeyName; got != maxName {
		t.Errorf("stored KeyName len = %d, want %d", len(got), bobKeyNameMaxLen)
	}
}

// When an admin pointed api_key_file at a custom path (not the writable PVC
// file), clear removes the PVC file but keeps the config pointer and the name
// — the key is still supplied by the admin path and the label still describes
// it. This exercises the "else" branch of the conditional in
// handleGovernorBobKeyClear.
func TestHandleGovernorBobKeyClear_CustomPathPreservesName(t *testing.T) {
	pointBobKeyAtTempDir(t)
	s := newBobTestServer(t)

	// Write a PVC key so clearBobKeyFile has something to remove.
	if err := writeBobKeyFile(bobTestKey); err != nil {
		t.Fatal(err)
	}
	// Point the config at a DIFFERENT path (admin-managed), not writableBobKeyFile.
	customPath := "/secrets/admin-bob-key"
	s.deps.Config.Governor.Bob.APIKeyFile = customPath
	s.deps.Config.Governor.Bob.KeyName = "admin key"

	req := httptest.NewRequest(http.MethodDelete, "/api/config/governor/bob", nil)
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.handleGovernorBobKeyClear(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	// The name must survive because the admin-managed key is still in place.
	if got := s.deps.Config.Governor.Bob.KeyName; got != "admin key" {
		t.Errorf("KeyName = %q, want preserved for custom api_key_file path", got)
	}
	if got := s.deps.Config.Governor.Bob.APIKeyFile; got != customPath {
		t.Errorf("APIKeyFile = %q, want custom path preserved", got)
	}
}

// The status endpoint with no key name set returns an empty keyName string —
// never null or absent — so the dashboard can unconditionally read it.
func TestHandleGovernorBobStatus_EmptyKeyNameWhenUnset(t *testing.T) {
	path := pointBobKeyAtTempDir(t)
	s := newBobTestServer(t)

	if err := writeBobKeyFile(bobTestKey); err != nil {
		t.Fatal(err)
	}
	s.deps.Config.Governor.Bob.APIKeyFile = path
	// KeyName deliberately left empty (zero value).

	rec := httptest.NewRecorder()
	s.handleGovernorBobStatus(rec, httptest.NewRequest(http.MethodGet, "/api/config/governor/bob", nil))

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// Must be present as "" (not null, not absent) for backwards compat.
	keyName, ok := resp["keyName"]
	if !ok {
		t.Fatal("status response missing keyName field entirely")
	}
	if keyName != "" {
		t.Errorf("status keyName = %v, want empty string when no name is set", keyName)
	}
}
