package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/hubactivity"
)

func activityUpdateBody(t *testing.T, hiveID string, d *hubactivity.Digest) string {
	t.Helper()
	body, err := json.Marshal(spoke.ActivityUpdatePayload{Update: &spoke.ActivityUpdate{HiveID: hiveID, Activity: d}})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func postActivityUpdate(s *HubServer, body, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/heartbeat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(spoke.ActivityUpdateHeader, "1")
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// An activity-only update replaces the held digest and touches nothing else:
// the registry entry keeps what the last full heartbeat reported.
func TestActivityUpdateRecordsOnlyTheDigest(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newActivityHub()
	postActivityHeartbeat(t, s, "hive-a", activityDigest("o/a", 1))
	s.mu.RLock()
	before := s.registry.Hives[0]
	s.mu.RUnlock()

	next := activityDigest("o/a", 1, 2, 3)
	rec := postActivityUpdate(s, activityUpdateBody(t, "hive-a", next), heartbeatBearer(testHubSecret, "hive-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("activity update: status %d body=%s", rec.Code, rec.Body.String())
	}
	var resp HeartbeatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.ActivityETag != next.ETag {
		t.Fatalf("response = %+v, want ok and the new etag acknowledged", resp)
	}
	held := s.activity.snapshot([]string{"hive-a"})["hive-a"]
	if len(held.digest.Items) != 3 {
		t.Fatalf("held digest has %d items, want the update's 3", len(held.digest.Items))
	}
	s.mu.RLock()
	after := s.registry.Hives[0]
	s.mu.RUnlock()
	if len(s.registry.Hives) != 1 || after.Org != before.Org || after.PrimaryRepo != before.PrimaryRepo ||
		len(after.Repos) != len(before.Repos) || after.LastHeartbeat != before.LastHeartbeat {
		t.Fatalf("activity update changed the registry entry: before=%+v after=%+v", before, after)
	}

	// An unchanged marker for the held etag is acknowledged too.
	marker := &hubactivity.Digest{ETag: next.ETag, GeneratedAt: next.GeneratedAt}
	rec = postActivityUpdate(s, activityUpdateBody(t, "hive-a", marker), heartbeatBearer(testHubSecret, "hive-a"))
	var markerResp HeartbeatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &markerResp); err != nil || rec.Code != http.StatusOK || markerResp.ActivityETag != next.ETag {
		t.Fatalf("marker update: status %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestActivityUpdateRejections(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newActivityHub()
	postActivityHeartbeat(t, s, "hive-a", activityDigest("o/a", 1))
	d := activityDigest("o/a", 1, 2)
	good := heartbeatBearer(testHubSecret, "hive-a")

	for _, tc := range []struct {
		name   string
		body   string
		bearer string
		want   int
	}{
		{"missing bearer", activityUpdateBody(t, "hive-a", d), "", http.StatusUnauthorized},
		{"another hive's bearer", activityUpdateBody(t, "hive-a", d), heartbeatBearer(testHubSecret, "hive-b"), http.StatusUnauthorized},
		{"malformed body", "{", good, http.StatusBadRequest},
		{"no activity_update", `{"hive_id":"hive-a"}`, good, http.StatusBadRequest},
		{"invalid hive id", activityUpdateBody(t, "", d), good, http.StatusBadRequest},
		{"no activity block", activityUpdateBody(t, "hive-a", nil), good, http.StatusBadRequest},
		{"hive not in the registry", activityUpdateBody(t, "hive-new", d), heartbeatBearer(testHubSecret, "hive-new"), http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := postActivityUpdate(s, tc.body, tc.bearer); rec.Code != tc.want {
				t.Fatalf("status %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	if held := s.activity.snapshot([]string{"hive-a", "hive-new"}); len(held) != 1 || len(held["hive-a"].digest.Items) != 1 {
		t.Fatalf("a rejected update changed the store: %+v", held)
	}
}

// Only a marked POST to the heartbeat path is diverted; everything else,
// including an ordinary heartbeat, reaches the mux.
func TestActivityUpdateGuardPassesOtherRequestsThrough(t *testing.T) {
	s := newActivityHub()
	reached := 0
	h := s.activityUpdateGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached++ }))
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/api/heartbeat", strings.NewReader("{}")),
		httptest.NewRequest(http.MethodGet, "/api/heartbeat", nil),
		httptest.NewRequest(http.MethodPost, "/api/task-status", strings.NewReader("{}")),
	} {
		if req.URL.Path != "/api/heartbeat" || req.Method != http.MethodPost {
			req.Header.Set(spoke.ActivityUpdateHeader, "1")
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if reached != 3 {
		t.Fatalf("%d of 3 unmarked or off-path requests reached the mux", reached)
	}
}

// A hub without the update path decodes the body as a heartbeat with no
// top-level hive_id and rejects it, so an older hub can never record an
// update as an empty beat; the spoke backs off on that 400.
func TestActivityUpdateBodyIsRejectedByThePlainHeartbeatHandler(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newActivityHub()
	req := httptest.NewRequest(http.MethodPost, "/api/heartbeat", strings.NewReader(activityUpdateBody(t, "hive-a", activityDigest("o/a", 1))))
	req.Header.Set("Authorization", heartbeatBearer(testHubSecret, "hive-a"))
	rec := httptest.NewRecorder()
	s.handleHeartbeat(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("plain heartbeat handler answered %d, want 400", rec.Code)
	}
	if len(s.registry.Hives) != 0 {
		t.Fatal("an activity update body created a registry entry")
	}
}
