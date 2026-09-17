package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// #7451: hub.dashboard_url is owned by the hub while the hub is enabled. The
// heartbeat's project-config push (cmd/hive/main.go, ProjectConfigCallback)
// adopts the hub's value whenever it differs and persists it, so a local edit
// is reverted on the next beat — and until it is, a bad value is not inert:
// this field is the second-choice source for the OAuth callback origin
// (oauthPublicOrigin), so a typo can lock the operator out of the dashboard
// they are editing from. These tests pin both halves of the fix: the Hub tab
// renders the field read-only with its provenance, and the server refuses the
// write so the read-only input is not merely cosmetic.

// TestHubDashboardURLRenderedReadOnlyWhenHubEnabled pins the Hub tab markup:
// the hub-enabled branch is a read-only input with the "Policy source: the hub
// (heartbeat)" provenance line and no markDirty wiring; the hub-disabled branch
// (the unmanaged-spoke escape hatch) keeps the editable input.
func TestHubDashboardURLRenderedReadOnlyWhenHubEnabled(t *testing.T) {
	html := indexHTML(t)

	const marker = "const dashboardUrlRow = hub.enabled ?"
	start := strings.Index(html, marker)
	if start < 0 {
		t.Fatalf("Hub tab no longer branches the Dashboard URL row on hub.enabled (%q not found) — a hub-managed spoke would render it editable again", marker)
	}
	// The ternary's two arms are template literals; the `: ` between them
	// separates the hub-enabled arm from the hub-disabled arm.
	rest := html[start:]
	sep := strings.Index(rest, "</div>` : `")
	if sep < 0 {
		t.Fatal("could not find the boundary between the hub-enabled and hub-disabled Dashboard URL arms")
	}
	enabledArm := rest[:sep]
	disabledArm := rest[sep:]
	if end := strings.Index(disabledArm, "`;"); end > 0 {
		disabledArm = disabledArm[:end]
	}

	if !strings.Contains(enabledArm, "readonly disabled") {
		t.Error("hub-enabled Dashboard URL input is not readonly+disabled — an operator can still type a value the heartbeat will revert (and that can break OAuth first)")
	}
	if strings.Contains(enabledArm, `data-arg1="dashboard_url"`) || strings.Contains(enabledArm, `data-change-action="markDirty"`) {
		t.Error("hub-enabled Dashboard URL input is still wired to markDirty — an edit would be PUT to /api/config/governor/hub")
	}
	if !strings.Contains(enabledArm, "Policy source: the hub (heartbeat)") {
		t.Error("hub-enabled Dashboard URL row is missing the provenance line used elsewhere on this tab (\"Policy source: the hub (heartbeat)\")")
	}
	if !strings.Contains(enabledArm, "dashboard.public_url") {
		t.Error("hub-enabled Dashboard URL row does not point operators at dashboard.public_url, the override that actually wins for OAuth")
	}

	if !strings.Contains(disabledArm, `data-change-action="markDirty"`) || !strings.Contains(disabledArm, `data-arg1="dashboard_url"`) {
		t.Error("hub-disabled Dashboard URL row lost its editable wiring — an unmanaged spoke can no longer set its own dashboard URL")
	}
	// No inline event handlers: the CI inline-JS guard forbids them.
	for _, bad := range []string{"onclick=", "onchange=", "oninput="} {
		if strings.Contains(enabledArm, bad) || strings.Contains(disabledArm, bad) {
			t.Errorf("Dashboard URL row uses inline handler %q — use data-action/data-change-action", bad)
		}
	}
}

func putGovernorHub(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/hub", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	s.handleGovernorHub(w, req)
	return w
}

// TestGovernorHubRejectsDashboardURLWriteWhileHubEnabled is the server-side half:
// a read-only input can be bypassed with curl, so the handler must refuse.
func TestGovernorHubRejectsDashboardURLWriteWhileHubEnabled(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Hub.Enabled = true
	deps.Config.Hub.DashboardURL = "https://real.hive.hivecommons.dev"
	deps.Config.Dashboard.PublicURL = ""

	w := putGovernorHub(t, s, `{"dashboard_url":"https://typo.example.invalid"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("hub-owned dashboard_url write got %d, want 409 (body: %s)", w.Code, w.Body.String())
	}
	if got := deps.Config.Hub.DashboardURL; got != "https://real.hive.hivecommons.dev" {
		t.Fatalf("hub.dashboard_url was overwritten to %q despite the hub owning it", got)
	}

	// The lockout this prevents: with dashboard.public_url unset, hub.dashboard_url
	// is what the OAuth callback origin resolves to.
	origin := oauthPublicOrigin(deps.Config, httptest.NewRequest(http.MethodGet, "/", nil))
	if origin != "https://real.hive.hivecommons.dev" {
		t.Fatalf("OAuth callback origin became %q — a rejected edit still moved where an authorization code is redeemed", origin)
	}
}

// TestGovernorHubAllowsUnchangedDashboardURLResubmit: the Hub tab PUTs the whole
// form, so a resubmit that carries the current value must not 409.
func TestGovernorHubAllowsUnchangedDashboardURLResubmit(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Hub.Enabled = true
	deps.Config.Hub.DashboardURL = "https://real.hive.hivecommons.dev"

	w := putGovernorHub(t, s, `{"dashboard_url":"https://real.hive.hivecommons.dev","is_public":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("same-value resubmit got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if !deps.Config.Hub.IsPublic {
		t.Error("the rest of the form did not apply on a same-value dashboard_url resubmit")
	}
}

// TestGovernorHubAllowsDashboardURLWhenHubDisabled keeps the escape hatch for an
// unmanaged spoke, where no heartbeat overwrites the value.
func TestGovernorHubAllowsDashboardURLWhenHubDisabled(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Hub.Enabled = false
	deps.Config.Hub.DashboardURL = "https://old.example.com"

	w := putGovernorHub(t, s, `{"dashboard_url":"https://new.example.com"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("unmanaged-spoke dashboard_url write got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if got := deps.Config.Hub.DashboardURL; got != "https://new.example.com" {
		t.Fatalf("hub.dashboard_url = %q, want the operator's value on an unmanaged spoke", got)
	}
}

// TestGovernorHubDashboardURLFollowsEnabledInSamePUT: disabling the hub and
// setting the URL in one request leaves an unmanaged spoke, so the write applies.
func TestGovernorHubDashboardURLFollowsEnabledInSamePUT(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Hub.Enabled = true
	deps.Config.Hub.DashboardURL = "https://old.example.com"

	w := putGovernorHub(t, s, `{"enabled":false,"dashboard_url":"https://new.example.com"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("disable-hub-and-set-url PUT got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if got := deps.Config.Hub.DashboardURL; got != "https://new.example.com" {
		t.Fatalf("hub.dashboard_url = %q after the hub was disabled in the same PUT", got)
	}
}
