package dashboard

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGovernorHubTabHasCooldownControl pins the "Task cooldown" control (enable
// toggle + period number input) in the Governor Config Hub tab (embedded
// static/index.html), mirroring the existing suspend/skip controls. This is the
// canonical editor; the Management-tab mirror is pinned separately below.
func TestGovernorHubTabHasCooldownControl(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		// Enable toggle wired to the same section/key the PUT round-trips.
		`data-key="contribute_cooldown_enabled"`,
		`Task Cooldown`,
		`toggleCooldownEnabled(this)`,
		`function toggleCooldownEnabled(`,
		// Period input, marks the hub section dirty so Save PUTs it.
		`id="hub-cooldown-hours"`,
		`markDirty('hub','contribute_cooldown_hours'`,
		`contribute_cooldown_hours`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("Governor Hub tab missing cooldown control markup %q", want)
		}
	}
}

// TestManagementTabHasCooldownControl pins the mirrored "Task cooldown" control
// in the clanker Management tab (/contribute page), next to the suspend/skip
// controls. It writes through the SAME /api/config/governor/hub endpoint, so a
// change on either surface shows on the other on next poll.
func TestManagementTabHasCooldownControl(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		// Enable toggle — immediate-apply like suspend/skip.
		`id="admin-cooldown-switch"`,
		`data-key="contribute_cooldown_enabled"`,
		`bindImmediateToggle('admin-cooldown-switch')`,
		// Period input + its immediate-save binding.
		`id="admin-cooldown-hours"`,
		`function bindCooldownHoursInput(`,
		`bindCooldownHoursInput()`,
		`contribute_cooldown_hours`,
		// Persists through the SAME endpoint the Governor dialog uses.
		`/api/config/governor/hub`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Management tab missing cooldown control markup %q", want)
		}
	}
}
