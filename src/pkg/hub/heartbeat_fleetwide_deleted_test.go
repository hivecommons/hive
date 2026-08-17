package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// F2 (Critical, open across five audits): verifyHeartbeatBearer accepted a
// fleet-wide bearer — deriveDomainKey(hubSecret, infoHeartbeatKey) — alongside
// the per-hive one. That value is a pure function of the single hub master and
// was stamped IDENTICALLY into every spoke, so possession proved "some
// provisioned spoke" and never "THIS hive". handleHeartbeat then trusts the
// body-supplied hive_id, so any spoke could beat as any victim hive and receive
// victim-directed key material.
//
// This file pins the deletion of that lane. Every rejection assertion here is
// paired with a positive control, because a verifier that rejected EVERYTHING
// would satisfy all the rejections while 401ing the entire fleet — the exact
// failure mode a neutered test cannot distinguish from a fix.

func f2Hub() *HubServer {
	return &HubServer{logger: slog.Default(), hubSecret: "test-master-secret-f2-deleted"}
}

// TestF2PerHiveBearerVerifiesForItsOwnHive is the positive control for the whole
// file: the legitimate credential must still work.
func TestF2PerHiveBearerVerifiesForItsOwnHive(t *testing.T) {
	s := f2Hub()

	bearer := s.heartbeatKeyFor("hive-alpha")
	if bearer == "" {
		t.Fatal("per-hive derivation returned empty — no spoke could authenticate at all")
	}
	if !s.verifyHeartbeatBearer(bearer, "hive-alpha") {
		t.Fatal("hive-alpha's own per-hive bearer was rejected for hive-alpha — this " +
			"change would 401 the entire fleet")
	}
}

// TestF2PerHiveBearerRejectedForAnotherHive is the identity binding — the whole
// point of the finding.
func TestF2PerHiveBearerRejectedForAnotherHive(t *testing.T) {
	s := f2Hub()

	alpha := s.heartbeatKeyFor("hive-alpha")
	bravo := s.heartbeatKeyFor("hive-bravo")

	if alpha == bravo {
		t.Fatal("two hives derived the SAME bearer — identity cannot be bound and the " +
			"rejection below would be meaningless")
	}
	// Positive control.
	if !s.verifyHeartbeatBearer(alpha, "hive-alpha") {
		t.Fatal("positive control failed: hive-alpha cannot authenticate as itself")
	}
	// The finding.
	if s.verifyHeartbeatBearer(alpha, "hive-bravo") {
		t.Fatal("F2: hive-alpha's bearer authenticated a heartbeat CLAIMING to be " +
			"hive-bravo — hive-bravo's key material would be delivered to hive-alpha")
	}
}

// TestF2FleetWideBearerIsRejected is the regression: before this change the
// fleet-wide bearer was ACCEPTED for any hive ID.
func TestF2FleetWideBearerIsRejected(t *testing.T) {
	s := f2Hub()

	fleetWide := s.heartbeatKey()
	if fleetWide == "" {
		t.Fatal("test setup: fleet-wide derivation returned empty, so the rejections " +
			"below would pass for the wrong reason")
	}
	// The fleet-wide value must not coincide with any per-hive value, or the
	// rejection could not be distinguished from acceptance.
	if fleetWide == s.heartbeatKeyFor("hive-alpha") {
		t.Fatal("fleet-wide and per-hive derivations collided")
	}
	// Positive control: legitimate heartbeats still pass.
	if !s.verifyHeartbeatBearer(s.heartbeatKeyFor("hive-alpha"), "hive-alpha") {
		t.Fatal("positive control failed: the per-hive bearer was rejected, so the " +
			"rejections below prove nothing")
	}

	// The deleted lane, checked against several claimed identities: it must
	// authenticate NONE of them.
	for _, hiveID := range []string{"hive-alpha", "hive-bravo", "hive-victim"} {
		if s.verifyHeartbeatBearer(fleetWide, hiveID) {
			t.Errorf("F2: the fleet-wide bearer authenticated a heartbeat claiming to be "+
				"%q — every spoke holds this value, so any spoke can impersonate any hive",
				hiveID)
		}
	}
}

// TestF2BearerFailsClosedOnEmptyAndMalformed pins the fail-closed edges.
func TestF2BearerFailsClosedOnEmptyAndMalformed(t *testing.T) {
	s := f2Hub()

	// Positive control.
	if !s.verifyHeartbeatBearer(s.heartbeatKeyFor("hive-alpha"), "hive-alpha") {
		t.Fatal("positive control failed: the per-hive bearer was rejected")
	}

	valid := s.heartbeatKeyFor("hive-alpha")
	bad := []string{
		"",                      // empty bearer
		"   ",                   // whitespace
		"not-a-key",             // garbage
		strings.Repeat("a", 64), // right length, wrong bytes
		valid[:len(valid)-1],    // truncated
		valid + "0",             // extended
		strings.ToUpper(valid),  // case-mangled hex
	}
	for _, b := range bad {
		if s.verifyHeartbeatBearer(b, "hive-alpha") {
			t.Errorf("verifyHeartbeatBearer accepted malformed bearer %q", b)
		}
	}

	// An empty hive ID derives no key, so nothing authenticates for it — not even
	// the previously-accepted fleet-wide value.
	for _, b := range []string{"", "anything", valid, s.heartbeatKey()} {
		if s.verifyHeartbeatBearer(b, "") {
			t.Errorf("verifyHeartbeatBearer accepted %q for an EMPTY hive ID — an "+
				"identity-less caller must authenticate nothing", b)
		}
	}
}

// TestF2HeartbeatHandlerRejectsFleetWideBearer exercises the deletion through
// the actual HTTP handler, not just the verifier, so a caller that forgot to
// route through verifyHeartbeatBearer cannot hide behind the unit tests above.
func TestF2HeartbeatHandlerRejectsFleetWideBearer(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"hive_id":      "h1",
		"leaderboard":  []map[string]any{},
		"contributors": map[string]any{"active": 1, "registered": 1},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	post := func(bearer string) int {
		s := newTestHubServer("secret-abc")
		s.registry.Hives = []RegistryEntry{{ID: "h1", Name: "Hive One", Online: true}}
		req := httptest.NewRequest(http.MethodPost, "/api/task-status", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer "+bearer)
		rr := httptest.NewRecorder()
		s.handleTaskStatus(rr, req)
		return rr.Code
	}

	ref := newTestHubServer("secret-abc")

	// Positive control: the per-hive bearer for the hive named in the BODY works.
	if code := post(ref.heartbeatKeyFor("h1")); code != http.StatusOK {
		t.Fatalf("positive control failed: per-hive bearer got %d, want 200 — this "+
			"change would break real heartbeats", code)
	}
	// The deleted lane must not authenticate through the handler either.
	if code := post(ref.heartbeatKey()); code == http.StatusOK {
		t.Error("F2: the handler accepted the fleet-wide bearer (200) — the lane is " +
			"still reachable end-to-end")
	}
	// Nor may a per-hive bearer for a DIFFERENT hive claim h1.
	if code := post(ref.heartbeatKeyFor("h2")); code == http.StatusOK {
		t.Error("F2: the handler accepted h2's bearer for a body claiming hive_id=h1")
	}
}
