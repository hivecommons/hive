package hub

// Tests for the Manage Access permission-change audit log (#4148):
// handleAccessAdd must record a distinguishable timeline entry for a fresh
// grant vs a role change (with actor), and handleAccessLog must expose only
// access/ownership events to hive owners.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newAccessLogTestServer seeds a hive "h1" owned by "creator" with one
// read-role member "member", backed by temp dirs and an in-memory timeline.
func newAccessLogTestServer(t *testing.T) *HubServer {
	t.Helper()
	cleanup := helperSetupTempDirs(t)
	t.Cleanup(cleanup)

	origDir := timelineDir
	timelineDir = t.TempDir()
	t.Cleanup(func() { timelineDir = origDir })

	for user, hives := range map[string]map[string]string{
		"creator":        {"h1": "owner"},
		"member":         {"h1": "read"},
		hubAdminUsername: {},
	} {
		if err := saveSaaSUser(&SaaSUser{GitHubUsername: user, Hives: hives}); err != nil {
			t.Fatalf("save user %q: %v", user, err)
		}
	}
	if err := saveSaaSHive(&SaaSHive{ID: "h1", Owner: "creator"}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	return &HubServer{
		logger:    slog.Default(),
		hubSecret: testHubSecret,
		// Mirror newHandlerHub: resolveIdentity's impersonation verifier fails
		// closed on hub-admin cookies without a generation set.
		keyGenerations: legacyGenerationSet(testHubSecret),
		timeline:       newTimelineStore(),
	}
}

func accessLogEvents(t *testing.T, s *HubServer, username string) ([]TimelineEvent, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodGet, "/access-log", "", username), "id", "h1")
	s.handleAccessLog(rec, req)
	if rec.Code != http.StatusOK {
		return nil, rec.Code
	}
	var body struct {
		Events []TimelineEvent `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode access-log body: %v", err)
	}
	return body.Events, rec.Code
}

func doAccessAdd(t *testing.T, s *HubServer, actor, body string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/access", body, actor), "id", "h1")
	s.handleAccessAdd(rec, req)
	return rec.Code
}

func TestAccessAddRecordsGrantAndRoleChangeAudit(t *testing.T) {
	s := newAccessLogTestServer(t)

	// Fresh grant.
	if code := doAccessAdd(t, s, "creator", `{"username":"newbie","role":"read-write"}`); code != http.StatusOK {
		t.Fatalf("grant status = %d, want 200", code)
	}
	// Role change for an existing member.
	if code := doAccessAdd(t, s, "creator", `{"username":"member","role":"merger"}`); code != http.StatusOK {
		t.Fatalf("role-change status = %d, want 200", code)
	}
	// Re-granting the identical role must NOT append a new entry.
	if code := doAccessAdd(t, s, "creator", `{"username":"member","role":"merger"}`); code != http.StatusOK {
		t.Fatalf("noop re-grant status = %d, want 200", code)
	}

	events, code := accessLogEvents(t, s, "creator")
	if code != http.StatusOK {
		t.Fatalf("access-log status = %d, want 200", code)
	}
	if len(events) != 2 {
		t.Fatalf("access-log events = %d, want 2 (grant + role change, no noop entry): %+v", len(events), events)
	}
	// Newest first: role change, then grant.
	change, grant := events[0], events[1]
	if change.Detail != "role for member changed: read → merger" {
		t.Errorf("role-change detail = %q", change.Detail)
	}
	if grant.Detail != "access granted to newbie as read-write" {
		t.Errorf("grant detail = %q", grant.Detail)
	}
	for _, ev := range events {
		if ev.Actor != "creator" {
			t.Errorf("event actor = %q, want creator (detail %q)", ev.Actor, ev.Detail)
		}
		if ev.TS == "" {
			t.Errorf("event missing timestamp (detail %q)", ev.Detail)
		}
		if ev.Kind != TimelineAccess {
			t.Errorf("event kind = %q, want %q", ev.Kind, TimelineAccess)
		}
	}
}

func TestAccessRemoveRecordsRevocationAudit(t *testing.T) {
	s := newAccessLogTestServer(t)

	rec := httptest.NewRecorder()
	req := setPathValues(reqWithUser(http.MethodDelete, "/access", "", "creator"),
		map[string]string{"id": "h1", "username": "member"})
	s.handleAccessRemove(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove status = %d, want 200", rec.Code)
	}

	events, _ := accessLogEvents(t, s, "creator")
	if len(events) != 1 {
		t.Fatalf("access-log events = %d, want 1: %+v", len(events), events)
	}
	if events[0].Detail != "access revoked from member" || events[0].Actor != "creator" {
		t.Errorf("revocation event = %+v", events[0])
	}
}

func TestAccessLogFiltersToPermissionEvents(t *testing.T) {
	s := newAccessLogTestServer(t)
	s.recordTimeline("h1", TimelineVersionChanged, "version abc → def", "")
	s.recordTimeline("h1", TimelineAccess, "access granted to x as read", "creator")
	s.recordTimeline("h1", TimelineOwnership, "ownership transferred to y", "creator")
	s.recordTimeline("h1", TimelineHealthChanged, "health ok → degraded", "")

	events, code := accessLogEvents(t, s, "creator")
	if code != http.StatusOK {
		t.Fatalf("access-log status = %d, want 200", code)
	}
	if len(events) != 2 {
		t.Fatalf("access-log events = %d, want 2 (access + ownership only): %+v", len(events), events)
	}
	if events[0].Kind != TimelineOwnership || events[1].Kind != TimelineAccess {
		t.Errorf("filtered kinds = %q, %q; want ownership then access (newest first)", events[0].Kind, events[1].Kind)
	}
}

func TestAccessLogAuthorization(t *testing.T) {
	s := newAccessLogTestServer(t)
	s.recordTimeline("h1", TimelineAccess, "access granted to x as read", "creator")

	cases := []struct {
		name     string
		username string
		wantCode int
	}{
		{name: "creator", username: "creator", wantCode: http.StatusOK},
		{name: "hub admin", username: hubAdminUsername, wantCode: http.StatusOK},
		{name: "non-owner member", username: "member", wantCode: http.StatusForbidden},
		{name: "unknown user", username: "stranger", wantCode: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, code := accessLogEvents(t, s, tc.username)
			if code != tc.wantCode {
				t.Errorf("access-log status = %d, want %d", code, tc.wantCode)
			}
		})
	}

	t.Run("unknown hive", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := setPathValue(reqWithUser(http.MethodGet, "/access-log", "", "creator"), "id", "nope")
		s.handleAccessLog(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("unknown-hive status = %d, want 404", rec.Code)
		}
	})
}
