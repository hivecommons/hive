package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ============================================================
// saas.go — Manage Access CSV export (#4152)
// ============================================================

// TestManageAccessDialogHasCSVExport pins the client-side export affordance:
// an Export button in the Manage Access dialog and a downloader that builds
// the CSV entirely in the browser (Blob + synthetic anchor), so no access
// list is ever written server-side.
func TestManageAccessDialogHasCSVExport(t *testing.T) {
	for _, want := range []string{
		`id="access-export-btn"`,
		`onclick="exportAccessCSV()"`,
		`function exportAccessCSV()`,
		`function csvField(v)`,
		// The audit columns from #4152.
		`username,role,granted_at,last_active`,
		// Client-side download, no server round trip.
		`new Blob([lines.join(`,
		`URL.createObjectURL(blob)`,
	} {
		if !strings.Contains(dashboardHTML, want) {
			t.Errorf("dashboardHTML missing %q", want)
		}
	}
}

// TestAccessListCarriesLastActive verifies the owner-visible access list rows
// carry the user's coarse last-active timestamp (see latestUserActivity),
// which feeds the export's last-active column. Unlike the admin-only
// engagement stats, last_active rides for any owner — it answers the same
// audit question the grant itself does.
func TestAccessListCarriesLastActive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}

	saveSaaSUser(&SaaSUser{GitHubUsername: "owner", Hives: map[string]string{"h1": "owner"}})
	saveSaaSUser(&SaaSUser{
		GitHubUsername: "reader",
		LastLogin:      "2026-08-18T09:00:00Z",
		Hives:          map[string]string{"h1": "read"},
	})
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "owner"})

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodGet, "/access", "", "owner"), "id", "h1")
	s.handleAccessList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("access list status = %d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Access []HiveAccessEntry `json:"access"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, e := range resp.Access {
		if e.Username == "reader" {
			found = true
			if e.LastActive != "2026-08-18T09:00:00Z" {
				t.Errorf("reader last_active = %q, want 2026-08-18T09:00:00Z", e.LastActive)
			}
		}
	}
	if !found {
		t.Fatal("reader missing from access list")
	}
}
