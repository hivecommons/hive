package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// --- AUDIT F24: heartbeatBearerOK is DELETED; these tests are INVERTED ---
//
// These four tests previously asserted the VULNERABLE behaviour — literally
// "heartbeatBearerOK must accept raw master secret". heartbeatBearerOK accepted
// the raw fleet master and the fleet-wide heartbeatKey(), neither of which binds
// a hive identity, so any spoke holding either could claim ANY hive_id (N1 IDOR).
//
// They are INVERTED rather than deleted on purpose. Deleting them would leave
// nothing standing guard: a future refactor that re-introduced a fleet-wide
// acceptor would face a green suite. Inverted, they fail the moment such a lane
// comes back. Each now asserts REJECTION against the live verifier,
// verifyHeartbeatBearer, which is per-hive-only (F2).

const f24TestHiveID = "hive-f24"

// TestF24_FleetWideHeartbeatBearerIsRejected is the direct inversion of the old
// TestHeartbeatBearerOK_LegacyRawSecret and _DerivedKey. Both tokens the deleted
// acceptor honoured must now be refused.
func TestF24_FleetWideHeartbeatBearerIsRejected(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	// POSITIVE CONTROL FIRST: prove the per-hive bearer DOES verify, so that a
	// verifier which rejected everything could not pass this test. Without this
	// the rejection assertions below would be satisfied by a broken hub that
	// authenticates no spoke at all.
	perHive := srv.heartbeatKeyFor(f24TestHiveID)
	if perHive == "" {
		t.Fatal("precondition: heartbeatKeyFor returned empty — cannot distinguish reject-all from correct")
	}
	if !srv.verifyHeartbeatBearer(perHive, f24TestHiveID) {
		t.Fatal("positive control FAILED: the per-hive heartbeat bearer must still verify")
	}

	// The two tokens the deleted heartbeatBearerOK accepted.
	if srv.verifyHeartbeatBearer(srv.hubSecret, f24TestHiveID) {
		t.Error("F24 REGRESSION: the RAW fleet master must not authenticate a heartbeat")
	}
	fleetWide := deriveDomainKey(srv.hubSecret, infoHeartbeatKey)
	if fleetWide == "" {
		t.Fatal("precondition: fleet-wide derivation returned empty")
	}
	if srv.verifyHeartbeatBearer(fleetWide, f24TestHiveID) {
		t.Error("F24 REGRESSION: the FLEET-WIDE derived heartbeat key must not authenticate a heartbeat")
	}
}

// TestF24_PerHiveBearerDoesNotCrossHives is the F2 identity property: hive A's
// bearer must not authenticate hive B. A fleet-wide lane of any shape breaks
// this, so it guards the invariant rather than one deleted function.
func TestF24_PerHiveBearerDoesNotCrossHives(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	keyA := srv.heartbeatKeyFor("hive-a")
	keyB := srv.heartbeatKeyFor("hive-b")
	if keyA == "" || keyB == "" {
		t.Fatal("precondition: per-hive derivation returned empty")
	}
	if keyA == keyB {
		t.Fatal("per-hive heartbeat keys for different hives must differ — derivation is not hive-bound")
	}

	// Positive controls: each key works for its OWN hive.
	if !srv.verifyHeartbeatBearer(keyA, "hive-a") {
		t.Fatal("positive control FAILED: hive-a bearer must verify for hive-a")
	}
	if !srv.verifyHeartbeatBearer(keyB, "hive-b") {
		t.Fatal("positive control FAILED: hive-b bearer must verify for hive-b")
	}

	// The identity property itself, in both directions.
	if srv.verifyHeartbeatBearer(keyA, "hive-b") {
		t.Error("F2 REGRESSION: hive-a's bearer must be rejected for hive-b")
	}
	if srv.verifyHeartbeatBearer(keyB, "hive-a") {
		t.Error("F2 REGRESSION: hive-b's bearer must be rejected for hive-a")
	}
}

// TestF24_CrossHiveRejectedUnderEveryLiveGeneration extends the identity
// property across master generations. Rotation introduced trial verification
// over every live generation; a fleet-wide lane re-added to ANY generation would
// be invisible to a single-generation test.
func TestF24_CrossHiveRejectedUnderEveryLiveGeneration(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	if srv.keyGenerations == nil {
		t.Fatal("precondition: no generation set on a default hub server")
	}

	gens := srv.keyGenerations.acceptableGenerations(time.Now())
	if len(gens) == 0 {
		t.Fatal("precondition: no live generations to exercise")
	}

	for _, g := range gens {
		// Positive control for THIS generation: its per-hive key verifies.
		keyA := derivePerHiveKey(g.Secret, infoHeartbeatKey, "hive-a")
		if keyA == "" {
			t.Fatalf("gen %d: per-hive derivation returned empty", g.ID)
		}
		if !srv.verifyHeartbeatBearer(keyA, "hive-a") {
			t.Fatalf("gen %d: positive control FAILED — per-hive bearer must verify for its own hive", g.ID)
		}

		// Cross-hive must fail under this generation.
		if srv.verifyHeartbeatBearer(keyA, "hive-b") {
			t.Errorf("gen %d: F2 REGRESSION — hive-a's bearer accepted for hive-b", g.ID)
		}
		// Fleet-wide material from this generation must fail outright.
		if fw := deriveDomainKey(g.Secret, infoHeartbeatKey); fw != "" && srv.verifyHeartbeatBearer(fw, "hive-a") {
			t.Errorf("gen %d: F24 REGRESSION — fleet-wide derived key accepted", g.ID)
		}
		if srv.verifyHeartbeatBearer(g.Secret, "hive-a") {
			t.Errorf("gen %d: F24 REGRESSION — raw generation master accepted", g.ID)
		}
	}
}

// TestF24_MalformedAndEmptyBearersRejected preserves the old
// _InvalidTokenRejected coverage against the live verifier. The old cases were
// Authorization-header shaped because heartbeatBearerOK parsed the header;
// verifyHeartbeatBearer takes the token itself, so the header-parsing cases
// become token-shaped and the empty-hiveID fail-closed case is added.
func TestF24_MalformedAndEmptyBearersRejected(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	valid := srv.heartbeatKeyFor(f24TestHiveID)
	if valid == "" {
		t.Fatal("precondition: heartbeatKeyFor returned empty")
	}
	if !srv.verifyHeartbeatBearer(valid, f24TestHiveID) {
		t.Fatal("positive control FAILED: the valid per-hive bearer must verify")
	}

	cases := []struct {
		name      string
		presented string
		hiveID    string
	}{
		{"empty token", "", f24TestHiveID},
		{"wrong token", "totally-wrong-token", f24TestHiveID},
		{"valid token, empty hiveID", valid, ""},
		{"valid token, unknown hive", valid, "some-other-hive"},
		{"bearer prefix left on", "Bearer " + valid, f24TestHiveID},
		{"leading space", " " + valid, f24TestHiveID},
		{"truncated token", valid[:len(valid)-1], f24TestHiveID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if srv.verifyHeartbeatBearer(tc.presented, tc.hiveID) {
				t.Errorf("verifyHeartbeatBearer(%q, %q) = true, want false", tc.presented, tc.hiveID)
			}
		})
	}
}

// TestF24_NoFleetWideHeartbeatLaneInSource is a source-level invariant guard.
// The behavioural tests above can only catch a fleet-wide lane that is REACHED;
// heartbeatBearerOK was dead-but-armed and therefore invisible to them. This
// asserts the deleted acceptor has not returned to the file at all.
func TestF24_NoFleetWideHeartbeatLaneInSource(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	body := string(src)

	// Positive control: the tombstone naming the finding must be present, so a
	// wholesale rewrite of the file cannot make this test vacuously pass.
	if !strings.Contains(body, "AUDIT F24 TOMBSTONE") {
		t.Fatal("positive control FAILED: the F24 tombstone comment is missing from server.go")
	}

	if strings.Contains(body, "func (s *HubServer) heartbeatBearerOK(") {
		t.Error("F24 REGRESSION: heartbeatBearerOK has been reintroduced in server.go")
	}
}

// --- storePendingGitHubAppConfig / consumePendingGitHubAppConfig tests ---

func TestPendingGitHubAppConfig_StoreAndConsume(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	cfg := &HeartbeatGitHubAppConfig{AppID: 42, InstallationID: 99}

	srv.storePendingGitHubAppConfig("hive-1", cfg)

	got := srv.consumePendingGitHubAppConfig("hive-1")
	if got == nil {
		t.Fatal("consumePendingGitHubAppConfig returned nil after store")
	}
	if got.AppID != 42 || got.InstallationID != 99 {
		t.Errorf("got AppID=%d InstallationID=%d, want 42/99", got.AppID, got.InstallationID)
	}

	// Second consume must return nil (consumed)
	if got2 := srv.consumePendingGitHubAppConfig("hive-1"); got2 != nil {
		t.Error("second consumePendingGitHubAppConfig should return nil")
	}
}

func TestPendingGitHubAppConfig_ConsumeNonExistent(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	if got := srv.consumePendingGitHubAppConfig("no-such-hive"); got != nil {
		t.Errorf("consume on empty map returned %v, want nil", got)
	}
}

func TestPendingGitHubAppConfig_OverwritesPrevious(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.storePendingGitHubAppConfig("hive-1", &HeartbeatGitHubAppConfig{AppID: 1})
	srv.storePendingGitHubAppConfig("hive-1", &HeartbeatGitHubAppConfig{AppID: 2})

	got := srv.consumePendingGitHubAppConfig("hive-1")
	if got == nil || got.AppID != 2 {
		t.Errorf("store should overwrite: got AppID=%v, want 2", got)
	}
}

// --- handleLeaderboard tests ---

func TestHandleLeaderboard_EmptyRegistry(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	req := httptest.NewRequest("GET", "/api/leaderboard", nil)
	w := httptest.NewRecorder()

	srv.handleLeaderboard(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	lb, ok := resp["leaderboard"]
	if !ok {
		t.Fatal("response missing 'leaderboard' key")
	}
	arr, ok := lb.([]any)
	if !ok || len(arr) != 0 {
		t.Errorf("expected empty leaderboard array, got %v", lb)
	}
}

func TestHandleLeaderboard_WithEntries(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.registry.Hives = []RegistryEntry{
		{
			ID:       "hive-1",
			Name:     "Alpha",
			Online:   true,
			IsPublic: true,
			Leaderboard: []LeaderboardEntry{
				{GitHubUsername: "alice", HiveName: "Alpha", TasksCompleted: 5},
				{GitHubUsername: "bob", HiveName: "Alpha", TasksCompleted: 3},
			},
		},
		{
			ID:       "hive-2",
			Name:     "Beta",
			Online:   true,
			IsPublic: true,
			Leaderboard: []LeaderboardEntry{
				{GitHubUsername: "charlie", HiveName: "Beta", TasksCompleted: 2},
			},
		},
	}

	req := httptest.NewRequest("GET", "/api/leaderboard", nil)
	w := httptest.NewRecorder()
	srv.handleLeaderboard(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Leaderboard []LeaderboardEntry `json:"leaderboard"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp.Leaderboard) != 3 {
		t.Errorf("expected 3 leaderboard entries (alice, bob, charlie), got %d", len(resp.Leaderboard))
	}
}

func TestHandleLeaderboard_PrivateHivesExcluded(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.registry.Hives = []RegistryEntry{
		{
			ID:       "priv",
			Name:     "Private",
			Online:   true,
			IsPublic: false,
			Leaderboard: []LeaderboardEntry{
				{GitHubUsername: "secret-user", TasksCompleted: 99},
			},
		},
	}

	req := httptest.NewRequest("GET", "/api/leaderboard", nil)
	w := httptest.NewRecorder()
	srv.handleLeaderboard(w, req)

	var resp struct {
		Leaderboard []LeaderboardEntry `json:"leaderboard"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Leaderboard) != 0 {
		t.Errorf("private hive entries should not appear in leaderboard, got %d entries", len(resp.Leaderboard))
	}
}

// --- handleStats tests ---

func TestHandleStats_EmptyRegistry(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	req := httptest.NewRequest("GET", "/api/stats", nil)
	w := httptest.NewRecorder()
	srv.handleStats(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// Presence AND value. The registry is empty, so the expected answer for
	// every counter is 0 — asserting only that the key exists (#5388) let any
	// count through, including garbage, in the one test whose name states what
	// the numbers have to be.
	for _, key := range []string{"hives", "online", "agents", "contributors", "issues", "prs"} {
		v, ok := resp[key]
		if !ok {
			t.Errorf("response missing key %q", key)
			continue
		}
		n, isNum := v.(float64)
		if !isNum {
			t.Errorf("%s = %v (%T), want a JSON number", key, v, v)
			continue
		}
		if n != 0 {
			t.Errorf("%s = %v, want 0 — the registry is empty", key, n)
		}
	}
}

func TestHandleStats_OnlyCountsPublicHives(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.registry.Hives = []RegistryEntry{
		{ID: "pub1", IsPublic: true, Online: true, AgentCount: 3, ActiveContributors: 2, ActionableIssues: 5, ActionablePRs: 4},
		{ID: "priv1", IsPublic: false, Online: true, AgentCount: 10, ActiveContributors: 10, ActionableIssues: 10, ActionablePRs: 10},
		{ID: "pub2", IsPublic: true, Online: false, AgentCount: 1, ActiveContributors: 1, ActionableIssues: 1, ActionablePRs: 1},
	}

	req := httptest.NewRequest("GET", "/api/stats", nil)
	w := httptest.NewRecorder()
	srv.handleStats(w, req)

	var resp map[string]float64
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// Only public hives counted
	if resp["hives"] != 2 {
		t.Errorf("hives = %v, want 2 (only public)", resp["hives"])
	}
	if resp["online"] != 1 {
		t.Errorf("online = %v, want 1 (only pub1 is online+public)", resp["online"])
	}
	if resp["agents"] != 4 {
		t.Errorf("agents = %v, want 4 (3+1 from public hives)", resp["agents"])
	}
	if resp["contributors"] != 3 {
		t.Errorf("contributors = %v, want 3 (2+1 from public hives)", resp["contributors"])
	}
	if resp["issues"] != 6 {
		t.Errorf("issues = %v, want 6 (5+1 from public hives)", resp["issues"])
	}
	if resp["prs"] != 5 {
		t.Errorf("prs = %v, want 5 (4+1 from public hives)", resp["prs"])
	}
}

// --- handleTaskStatus tests ---

func TestHandleTaskStatus_Unauthorized(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	body := `{"hive_id":"hive-1","leaderboard":[],"contributors":{"registered":0,"active":0}}`
	req := httptest.NewRequest("POST", "/api/task-status", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()
	srv.handleTaskStatus(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleTaskStatus_ValidPayload(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.registry.Hives = []RegistryEntry{
		{ID: "hive-1", Name: "TestHive", Online: true},
	}

	body := `{"hive_id":"hive-1","leaderboard":[{"github_username":"alice"}],"contributors":{"registered":5,"active":3}}`
	req := httptest.NewRequest("POST", "/api/task-status", strings.NewReader(body))
	// Post-F2 task-status auth is fail-closed AND identity-bound: it accepts ONLY
	// the PER-HIVE bearer for the hive_id in the body. Supply that so the request
	// authenticates and we exercise the payload path this test targets.
	req.Header.Set("Authorization", "Bearer "+srv.heartbeatKeyFor("hive-1"))
	w := httptest.NewRecorder()
	srv.handleTaskStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	// Verify contributors were updated
	srv.mu.RLock()
	h := srv.registry.Hives[0]
	srv.mu.RUnlock()
	if h.ContributorCount != 5 {
		t.Errorf("ContributorCount = %d, want 5", h.ContributorCount)
	}
	if h.ActiveContributors != 3 {
		t.Errorf("ActiveContributors = %d, want 3", h.ActiveContributors)
	}
}

func TestHandleTaskStatus_OfflineHiveRejected(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.registry.Hives = []RegistryEntry{
		{ID: "hive-1", Name: "TestHive", Online: false},
	}

	body := `{"hive_id":"hive-1","leaderboard":[],"contributors":{"registered":1,"active":0}}`
	req := httptest.NewRequest("POST", "/api/task-status", strings.NewReader(body))
	// Post-F2 task-status auth is fail-closed AND identity-bound: it accepts ONLY
	// the PER-HIVE bearer for the hive_id in the body. Supply that so the request
	// authenticates and we exercise the payload path this test targets.
	req.Header.Set("Authorization", "Bearer "+srv.heartbeatKeyFor("hive-1"))
	w := httptest.NewRecorder()
	srv.handleTaskStatus(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for offline hive", w.Code)
	}
}

func TestHandleTaskStatus_InvalidJSON(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("POST", "/api/task-status", strings.NewReader("not json"))
	// Post-F2 task-status auth is fail-closed AND identity-bound: it accepts ONLY
	// the PER-HIVE bearer for the hive_id in the body. Supply that so the request
	// authenticates and we exercise the payload path this test targets.
	req.Header.Set("Authorization", "Bearer "+srv.heartbeatKeyFor("hive-1"))
	w := httptest.NewRecorder()
	srv.handleTaskStatus(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleTaskStatus_EmptyHiveID(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	body := `{"hive_id":"","leaderboard":[],"contributors":{}}`
	req := httptest.NewRequest("POST", "/api/task-status", strings.NewReader(body))
	// Post-F2 task-status auth is fail-closed AND identity-bound: it accepts ONLY
	// the PER-HIVE bearer for the hive_id in the body. Supply that so the request
	// authenticates and we exercise the payload path this test targets.
	req.Header.Set("Authorization", "Bearer "+srv.heartbeatKeyFor("hive-1"))
	w := httptest.NewRecorder()
	srv.handleTaskStatus(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for empty hive_id", w.Code)
	}
}

// TestHandleTaskStatus_AcceptsPerHiveKeyOnly replaces the former
// TestHandleTaskStatus_AcceptsDerivedKey, which asserted that the FLEET-WIDE
// derived sub-key must be accepted (200). That assertion encoded F2: the
// fleet-wide key is one value held by every spoke, so accepting it let any spoke
// post task-status for any hive. It is inverted rather than deleted, and paired
// with a positive control so a handler that rejected everything cannot pass.
func TestHandleTaskStatus_AcceptsPerHiveKeyOnly(t *testing.T) {
	newSrv := func() *HubServer {
		s := NewHubServer(0, slog.Default(), "test", "v2")
		s.registry.Hives = []RegistryEntry{{ID: "hive-1", Name: "TestHive", Online: true}}
		return s
	}
	const body = `{"hive_id":"hive-1","leaderboard":[],"contributors":{"registered":1,"active":1}}`

	post := func(s *HubServer, bearer string) int {
		req := httptest.NewRequest("POST", "/api/task-status", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		s.handleTaskStatus(w, req)
		return w.Code
	}

	// Positive control: the per-hive bearer for hive-1 still works.
	s := newSrv()
	if code := post(s, s.heartbeatKeyFor("hive-1")); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with the per-hive key — real spokes would break", code)
	}
	// F2: the fleet-wide derived sub-key is no longer a credential.
	s = newSrv()
	if code := post(s, s.heartbeatKey()); code == http.StatusOK {
		t.Error("F2: task-status accepted the FLEET-WIDE derived key — every spoke holds " +
			"it, so any spoke could post task-status for any hive")
	}
	// And another hive's per-hive key cannot claim hive-1 either.
	s = newSrv()
	if code := post(s, s.heartbeatKeyFor("hive-2")); code == http.StatusOK {
		t.Error("F2: task-status accepted hive-2's bearer for a body claiming hive-1")
	}
}

// --- regPath tests ---

func TestRegPath_ReturnsNonEmpty(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	p := srv.regPath()
	if p == "" {
		t.Error("regPath() returned empty string")
	}
	if !strings.HasSuffix(p, ".json") {
		t.Errorf("regPath() = %q, expected .json suffix", p)
	}
}
