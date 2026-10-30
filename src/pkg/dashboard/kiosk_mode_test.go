package dashboard

import (
	"strings"
	"testing"
)

// TestKioskModeWiring pins the ?kiosk=1 unattended display mode: boot flags
// <html>, the kiosk stylesheet hides the chrome with enough specificity to
// beat the !important topbar rules in /api/theme.css, and the auto-opened
// dialogs bail out.
func TestKioskModeWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`new URLSearchParams(window.location.search).get('kiosk') === '1'`,
		`document.documentElement.classList.add('kiosk');`,
		`html.kiosk body #oc-topbar.oc-topbar,`,
		`html.kiosk body #oc-sidebar,`,
		`html.kiosk body #dash-notices,`,
		`html.kiosk body #nps-survey-card,`,
		`html.kiosk body [data-dashboard-section]:not(#activity-monitor-section) { display: none !important; }`,
		`html.kiosk body #activity-monitor-section {`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing kiosk wiring %q", want)
		}
	}
	guard := `if (document.documentElement.classList.contains('kiosk')) return;`
	for _, fn := range []string{"async function initWelcomeDialog() {", "async function initNPSSurvey() {"} {
		i := strings.Index(html, fn)
		if i < 0 {
			t.Fatalf("index.html missing %q", fn)
		}
		if !strings.HasPrefix(strings.TrimLeft(html[i+len(fn):], " \n"), guard) {
			t.Errorf("%s must return early in kiosk mode", fn)
		}
	}
}
