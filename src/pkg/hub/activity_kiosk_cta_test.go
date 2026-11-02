package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The public Fleet activity shell carries the kiosk call-to-action badge
// (#16236) with the environment's URL and copy, and the page script only
// turns it on for kiosk=1 without cta=off.
func TestActivityKioskCTA_ShellCarriesBadgeAndScriptGatesIt(t *testing.T) {
	t.Setenv("HUB_DATA_DIR", t.TempDir())
	t.Setenv("HIVE_HUB_KIOSK_CTA_URL", "https://hub.example/get-started")
	t.Setenv("HIVE_HUB_KIOSK_CTA_TEXT", "Get <yours>")
	srv := newHubServerForTest(t, withHubIdentity("test", "v5"))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, activityPublicPagePath+"?kiosk=1", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`<aside id="kiosk-cta" class="kiosk-cta"`,
		`class="kiosk-cta-url">hub.example/get-started<`,
		`class="kiosk-cta-text">Get &lt;yours&gt;<`,
		`src="data:image/png;base64,`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("public shell missing %q", want)
		}
	}
	if strings.Contains(body, "<!-- hive-kiosk-cta -->") {
		t.Error("placeholder left in the served shell")
	}
	if rec.Header().Get("Content-Security-Policy") != activityPageCSP {
		t.Errorf("CSP changed: %q", rec.Header().Get("Content-Security-Policy"))
	}

	src := activityDisplayHarness(t) + "\n" + `result = [
  activityKioskCTAOn('https://hub.example/activity/public'),
  activityKioskCTAOn('https://hub.example/activity/public?kiosk=1'),
  activityKioskCTAOn('https://hub.example/activity/public?kiosk=1&cta=off'),
  activityKioskCTAOn('https://hub.example/activity/public?display=1'),
  activityKioskCTAOn('nope'),
];`
	var got []bool
	runEvidenceJS(t, src, &got)
	want := []bool{false, true, false, false, false}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("case %d = %v, want %v", i, got[i], want[i])
		}
	}
	js := readActivityScript(t)
	if !strings.Contains(js, "if (activityKioskCTAOn(window.location.href)) document.body.classList.add('kiosk-cta-on');") {
		t.Error("activity.js does not set body.kiosk-cta-on from the URL")
	}
}
