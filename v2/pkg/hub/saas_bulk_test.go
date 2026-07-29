package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// bulkTestHub returns a hub with the bulk route registered and the SaaS store
// pointed at a temp dir, plus a cleanup func.
func bulkTestHub(t *testing.T) *HubServer {
	t.Helper()
	cleanup := helperSetupTempDirs(t)
	t.Cleanup(cleanup)
	// NewHubServer -> registerSaaSRoutes already wires the bulk route.
	return NewHubServer(0, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), "test", "v2")
}

// bulkSeedHive writes a hosted hive owned by owner into the temp SaaS store,
// along with the owner's user record — getAuthUser only honours a session
// cookie for a user that actually exists.
func bulkSeedHive(t *testing.T, id, owner string) {
	t.Helper()
	bulkSeedUser(t, owner)
	if err := saveSaaSHive(&SaaSHive{ID: id, Owner: owner}); err != nil {
		t.Fatalf("seed hive %s: %v", id, err)
	}
}

func bulkSeedUser(t *testing.T, username string) {
	t.Helper()
	if username == "" || loadSaaSUser(username) != nil {
		return
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: username}); err != nil {
		t.Fatalf("seed user %s: %v", username, err)
	}
}

func bulkPost(t *testing.T, srv *HubServer, user string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest("POST", "/api/saas/hives/bulk", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.AddCookie(testAuthCookie(user))
	}
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

// TestAuthorizeBulkHivePerHive covers the core security property: authorization
// is re-derived per hive from the hub's own store, never from the client list.
func TestAuthorizeBulkHivePerHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	bulkSeedHive(t, "hosted-alice", "alice")
	bulkSeedHive(t, "hosted-bob", "bob")
	bulkSeedUser(t, hubAdminUsername)

	tests := []struct {
		name    string
		hiveID  string
		caller  string
		wantOK  bool
		wantWhy string
	}{
		{"owner may act on own hive", "hosted-alice", "alice", true, ""},
		{"owner may not act on another user's hive", "hosted-bob", "alice", false, "not authorized for this hive"},
		{"hub admin may act on any hive", "hosted-bob", hubAdminUsername, true, ""},
		{"unknown hive is refused", "hosted-ghost", "alice", false, "hive not found"},
		{"unauthenticated caller is refused", "hosted-alice", "", false, "not authenticated"},
		{"path traversal id is refused", "../../etc", "alice", false, "hive not found"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, why := authorizeBulkHive(tc.hiveID, tc.caller)
			gotOK := h != nil
			if gotOK != tc.wantOK {
				t.Fatalf("authorizeBulkHive(%q,%q) ok=%v, want %v (reason %q)",
					tc.hiveID, tc.caller, gotOK, tc.wantOK, why)
			}
			if !tc.wantOK && why != tc.wantWhy {
				t.Errorf("reason = %q, want %q", why, tc.wantWhy)
			}
		})
	}
}

// TestBulkHandlerRejectsNonOwnerPerHive proves an attacker cannot widen their
// blast radius by putting someone else's hive IDs in the list: the request as a
// whole succeeds, but each unauthorized hive comes back as a per-hive error and
// is never acted on.
func TestBulkHandlerRejectsNonOwnerPerHive(t *testing.T) {
	srv := bulkTestHub(t)
	bulkSeedHive(t, "hosted-mine", "mallory")
	bulkSeedHive(t, "hosted-yours", "victim")

	w, out := bulkPost(t, srv, "mallory", BulkHiveRequest{
		Action:  bulkActionDisableAutoUpgrade,
		HiveIDs: []string{"hosted-mine", "hosted-yours"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	results := bulkResults(t, out)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	byID := map[string]BulkHiveResult{}
	for _, r := range results {
		byID[r.HiveID] = r
	}
	if !byID["hosted-mine"].Ok {
		t.Errorf("own hive should succeed, got %+v", byID["hosted-mine"])
	}
	if byID["hosted-yours"].Ok {
		t.Error("another user's hive must NOT be acted on")
	}
	if !strings.Contains(byID["hosted-yours"].Error, "not authorized") {
		t.Errorf("want authorization error, got %q", byID["hosted-yours"].Error)
	}
	// The victim's stored preference must be untouched.
	victim := loadSaaSHive("hosted-yours")
	if victim == nil || victim.AutoUpgrade {
		t.Error("victim hive was mutated by an unauthorized bulk request")
	}
}

// TestBulkHandlerUnauthenticated confirms a caller with no session is rejected
// outright rather than reaching the per-hive fan-out.
func TestBulkHandlerUnauthenticated(t *testing.T) {
	srv := bulkTestHub(t)
	bulkSeedHive(t, "hosted-a", "alice")

	w, _ := bulkPost(t, srv, "", BulkHiveRequest{
		Action:  bulkActionEnableAutoUpgrade,
		HiveIDs: []string{"hosted-a"},
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	h := loadSaaSHive("hosted-a")
	if h == nil || h.AutoUpgrade {
		t.Error("unauthenticated request must not mutate any hive")
	}
}

// TestBulkPartialFailureAggregation checks that a mixed batch reports an
// accurate per-hive breakdown plus correct succeeded/failed counts, rather
// than collapsing to a single boolean.
func TestBulkPartialFailureAggregation(t *testing.T) {
	srv := bulkTestHub(t)
	bulkSeedHive(t, "hosted-ok1", "alice")
	bulkSeedHive(t, "hosted-ok2", "alice")
	bulkSeedHive(t, "hosted-other", "bob")

	tests := []struct {
		name          string
		ids           []string
		wantSucceeded int
		wantFailed    int
	}{
		{"all succeed", []string{"hosted-ok1", "hosted-ok2"}, 2, 0},
		{"one unauthorized", []string{"hosted-ok1", "hosted-other"}, 1, 1},
		{"one missing", []string{"hosted-ok1", "hosted-nope"}, 1, 1},
		{"all fail", []string{"hosted-other", "hosted-nope"}, 0, 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, out := bulkPost(t, srv, "alice", BulkHiveRequest{
				Action:  bulkActionEnableAutoUpgrade,
				HiveIDs: tc.ids,
			})
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			if got := int(out["succeeded"].(float64)); got != tc.wantSucceeded {
				t.Errorf("succeeded = %d, want %d", got, tc.wantSucceeded)
			}
			if got := int(out["failed"].(float64)); got != tc.wantFailed {
				t.Errorf("failed = %d, want %d", got, tc.wantFailed)
			}
			results := bulkResults(t, out)
			if len(results) != len(tc.ids) {
				t.Fatalf("got %d results, want one per requested id (%d)", len(results), len(tc.ids))
			}
			// Every failure must carry a reason the UI can display.
			for _, r := range results {
				if !r.Ok && r.Error == "" {
					t.Errorf("hive %s failed with no reason", r.HiveID)
				}
			}
		})
	}
}

// TestBulkRequestCap covers the blast-radius bound.
func TestBulkRequestCap(t *testing.T) {
	srv := bulkTestHub(t)
	bulkSeedHive(t, "hosted-a", "alice")

	tooMany := make([]string, maxBulkHivesPerRequest+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("hosted-%d", i)
	}
	atCap := make([]string, maxBulkHivesPerRequest)
	for i := range atCap {
		atCap[i] = fmt.Sprintf("hosted-%d", i)
	}

	tests := []struct {
		name     string
		ids      []string
		wantCode int
	}{
		{"empty selection rejected", nil, http.StatusBadRequest},
		{"single hive accepted", []string{"hosted-a"}, http.StatusOK},
		{"exactly at the cap accepted", atCap, http.StatusOK},
		{"one over the cap rejected", tooMany, http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := bulkPost(t, srv, "alice", BulkHiveRequest{
				Action:  bulkActionDisableAutoUpgrade,
				HiveIDs: tc.ids,
			})
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.wantCode, w.Body.String())
			}
		})
	}
}

// TestBulkDeduplicatesIDs ensures a repeated id is acted on once, so a buggy
// client cannot restart the same hive N times in one request — and cannot use
// duplicates to slip past the cap.
func TestBulkDeduplicatesIDs(t *testing.T) {
	srv := bulkTestHub(t)
	bulkSeedHive(t, "hosted-dup", "alice")

	w, out := bulkPost(t, srv, "alice", BulkHiveRequest{
		Action:  bulkActionEnableAutoUpgrade,
		HiveIDs: []string{"hosted-dup", "hosted-dup", "hosted-dup"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := int(out["requested"].(float64)); got != 1 {
		t.Errorf("requested = %d, want 1 after de-duplication", got)
	}
	if got := len(bulkResults(t, out)); got != 1 {
		t.Errorf("got %d results, want 1", got)
	}
}

// TestBulkActionValidation rejects unknown actions and a branch switch with no
// branch before any hive is touched.
func TestBulkActionValidation(t *testing.T) {
	srv := bulkTestHub(t)
	bulkSeedHive(t, "hosted-a", "alice")

	tests := []struct {
		name     string
		action   string
		branch   string
		wantCode int
	}{
		{"unknown action rejected", "delete-everything", "", http.StatusBadRequest},
		{"empty action rejected", "", "", http.StatusBadRequest},
		{"switch-branch without branch rejected", bulkActionSwitchBranch, "", http.StatusBadRequest},
		{"known action accepted", bulkActionDisableAutoUpgrade, "", http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := bulkPost(t, srv, "alice", BulkHiveRequest{
				Action:  tc.action,
				Branch:  tc.branch,
				HiveIDs: []string{"hosted-a"},
			})
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.wantCode, w.Body.String())
			}
		})
	}
}

// TestBulkAutoUpgradePersists confirms the enable/disable actions actually
// write through to the SaaS store — the same persistence handleToggleAutoUpgrade
// relies on, which is what the spoke reads on its next heartbeat.
func TestBulkAutoUpgradePersists(t *testing.T) {
	srv := bulkTestHub(t)
	bulkSeedHive(t, "hosted-p1", "alice")
	bulkSeedHive(t, "hosted-p2", "alice")
	ids := []string{"hosted-p1", "hosted-p2"}

	for _, tc := range []struct {
		action string
		want   bool
	}{
		{bulkActionEnableAutoUpgrade, true},
		{bulkActionDisableAutoUpgrade, false},
	} {
		t.Run(tc.action, func(t *testing.T) {
			w, _ := bulkPost(t, srv, "alice", BulkHiveRequest{Action: tc.action, HiveIDs: ids})
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			for _, id := range ids {
				h := loadSaaSHive(id)
				if h == nil {
					t.Fatalf("hive %s disappeared", id)
				}
				if h.AutoUpgrade != tc.want {
					t.Errorf("%s AutoUpgrade = %v, want %v", id, h.AutoUpgrade, tc.want)
				}
			}
		})
	}
}

// TestSortedBulkResults verifies failures sort first so the UI can lead with
// the actionable rows.
func TestSortedBulkResults(t *testing.T) {
	in := []BulkHiveResult{
		{HiveID: "b", Ok: true},
		{HiveID: "c", Ok: false, Error: "boom"},
		{HiveID: "a", Ok: true},
		{HiveID: "a2", Ok: false, Error: "boom"},
	}
	got := sortedBulkResults(in)
	wantOrder := []string{"a2", "c", "a", "b"}
	for i, want := range wantOrder {
		if got[i].HiveID != want {
			t.Errorf("position %d = %q, want %q (full: %+v)", i, got[i].HiveID, want, got)
		}
	}
	// Input must not be mutated.
	if in[0].HiveID != "b" {
		t.Error("sortedBulkResults mutated its input")
	}
}

func bulkResults(t *testing.T, out map[string]any) []BulkHiveResult {
	t.Helper()
	raw, err := json.Marshal(out["results"])
	if err != nil {
		t.Fatalf("marshal results: %v", err)
	}
	var results []BulkHiveResult
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatalf("unmarshal results: %v", err)
	}
	return results
}
