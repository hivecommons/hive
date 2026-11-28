package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleMyHivesReportsReleaseTrackingDisabled (#20188): the dashboard can
// only stop waiting for a latest version that will never come if the hub
// tells it tracking is off.
func TestHandleMyHivesReportsReleaseTrackingDisabled(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(hubReleaseTrackingDisableEnv, "true")
	saveSaaSUser(&SaaSUser{GitHubUsername: "alice"})

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	rec := httptest.NewRecorder()
	s.handleMyHives(rec, reqWithUser(http.MethodGet, "/api/saas/my-hives", "", "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("my-hives status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ReleaseTrackingDisabled *bool `json:"release_tracking_disabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal my-hives: %v", err)
	}
	if resp.ReleaseTrackingDisabled == nil || !*resp.ReleaseTrackingDisabled {
		t.Fatalf("release_tracking_disabled = %v, want true", resp.ReleaseTrackingDisabled)
	}
}

// TestDashboardReleaseTrackingOff (#20188): with tracking off the dashboard
// used to render a disabled "Waiting for latest version…" Upgrade button, a
// spinner that never stopped, the hub auto checkbox, an all-"unknown" channel
// block and a "collecting…" image-pulls line. Every one of those must be
// gated on the server's flag, and the replacement line must exist.
func TestDashboardReleaseTrackingOff(t *testing.T) {
	html := dashScript(t)
	for _, want := range []string{
		// The flag is read from the payload.
		"_releaseTrackingDisabled = !!data.release_tracking_disabled;",
		// The replacement line.
		">Release tracking is off</span>",
		// Latest-image and channel rows are replaced.
		"if (_releaseTrackingDisabled) shaEl.innerHTML = '<div>' + RELEASE_TRACKING_OFF_HTML + '</div>';",
		// Hub header: no waiting spinner or disabled Upgrade button.
		"var hubLatestUnknown = !hubBranchLatest && !_releaseTrackingDisabled;",
		"var hubStatusIcon = (_releaseTrackingDisabled && !hubBranchLatest)",
		// Hub header: no auto checkbox.
		"if (!_releaseTrackingDisabled) hubAutoCheck = ' <label",
		// Per-hive rows: no spinner and no dimmed Upgrade.
		"var status = (_releaseTrackingDisabled && !branchLatest)",
		"} else if (latestUnknown && !_releaseTrackingDisabled && isHosted && h.role === 'owner') {",
		// Image-pulls line hidden.
		"if (_releaseTrackingDisabled) { host.style.display = 'none'; return; }",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboardHTML is missing %q", want)
		}
	}
	// The only remaining "Waiting for latest version…" renders must sit behind
	// the tracking flag: count them against the guarded branches above.
	if n := strings.Count(html, `Waiting for latest version…`); n != 2 {
		t.Errorf("found %d \"Waiting for latest version…\" renders, want 2 (hub header and hive row, both gated)", n)
	}
}
