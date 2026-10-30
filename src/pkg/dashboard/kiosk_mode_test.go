package dashboard

import (
	"strings"
	"testing"
)

// TestKioskModeWiring guards the `?kiosk=1` display mode the headless Twitch
// streamer (src/deploy/stream) loads: the flag is read before first paint, the
// chrome and every non-tracker section are removed from flow, and the two
// dialogs that would otherwise park on an unattended screen are suppressed.
func TestKioskModeWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`searchParams.get('kiosk')==='1'`,
		`window.hiveKioskMode=function(){ return !!window.HIVE_KIOSK; };`,
		`document.body.classList.add('kiosk-mode');`,
		`#section-activity-monitor-section'`,
		`body.kiosk-mode .oc-sidebar, body.kiosk-mode .oc-sidebar-resize, body.kiosk-mode .oc-topbar,`,
		// Must out-specify theme.go's layoutGuardCSS, which forces the topbar visible with id specificity.
		`html body.kiosk-mode #oc-topbar[id] { display: none !important; }`,
		`body.kiosk-mode #hive-dashboard-root > [data-dashboard-section]:not(#activity-monitor-section) { display: none !important; }`,
		`if (!_welcomeState.dismissed && !(window.hiveKioskMode && window.hiveKioskMode())) openWelcomeDialog();`,
		`if (window.hiveKioskMode && window.hiveKioskMode()) return;`,
		`if (!el._countdownWired && !(window.hiveKioskMode && window.hiveKioskMode())) {`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing kiosk-mode wiring %q", want)
		}
	}
	if strings.Contains(html, `localStorage.setItem('hive-kiosk`) {
		t.Error("kiosk mode must not persist state in localStorage")
	}
}
