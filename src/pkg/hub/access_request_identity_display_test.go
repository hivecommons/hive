package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPendingAccessRequestsCarryDisplayLabels(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const hiveID = "label-hive"
	const owner = "owner"
	const requester = "ibmid:6620042TJ8"

	if err := saveSaaSHive(&SaaSHive{ID: hiveID, Owner: owner}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: owner, Hives: map[string]string{hiveID: "owner"}}); err != nil {
		t.Fatalf("save owner: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: requester, DisplayName: "Ada Lovelace", Provider: "ibmid"}); err != nil {
		t.Fatalf("save requester: %v", err)
	}
	saveAccessRequests(hiveID, []AccessRequest{{Username: requester, RequestedAt: "2026-09-03T11:00:00Z", Status: "pending", Note: "need access"}})

	s := newHandlerHub()
	rec := httptest.NewRecorder()
	s.handleGetRequests(rec, setPathValue(reqWithUser(http.MethodGet, "/api/saas/hives/"+hiveID+"/requests", "", owner), "id", hiveID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Requests []PendingAccessRequest `json:"requests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(resp.Requests))
	}
	got := resp.Requests[0]
	if got.Username != requester {
		t.Fatalf("username = %q, want raw key %q", got.Username, requester)
	}
	if got.DisplayLabel != "Ada Lovelace" {
		t.Fatalf("display_label = %q, want Ada Lovelace", got.DisplayLabel)
	}
	if got.Provider != "ibmid" {
		t.Fatalf("provider = %q, want ibmid", got.Provider)
	}
}

func TestPendingAccessRequestRowsPreferDisplayLabel(t *testing.T) {
	checks := []string{
		"var userLabel = String(pr.display_label || rawUser);",
		"esc(userLabel || rawUser)",
		"title=\"Auth key\"",
		"var userLabel = String(r.display_label || rawUser);",
	}
	for _, snippet := range checks {
		if !strings.Contains(dashboardHTML, snippet) {
			t.Errorf("dashboardHTML missing pending access label snippet %q", snippet)
		}
	}
}

func TestPendingAccessPanelExpansionSurvivesFleetRefresh(t *testing.T) {
	checks := []string{
		"var _expandedPendingRows = new Set();",
		"function pruneExpandedPendingRows(allHives) {",
		"pruneExpandedPendingRows(allHives);",
		"var pendingRowStyle = _expandedPendingRows.has(String(h.id || '')) ? '' : 'display:none';",
		"style=\"' + pendingRowStyle + '\"",
		"if (opening) _expandedPendingRows.add(key);",
		"else _expandedPendingRows.delete(key);",
	}
	for _, snippet := range checks {
		if !strings.Contains(dashboardHTML, snippet) {
			t.Errorf("dashboardHTML missing pending access expansion snippet %q", snippet)
		}
	}
}
