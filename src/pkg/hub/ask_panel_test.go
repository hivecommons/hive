package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHubDashboardCarriesThePanelOnlyWhenAskIsOn is the "off is inert"
// tripwire for the hub panel: with Ask off the dashboard document is the
// embedded bytes unchanged, the panel assets are not served, and the
// security headers are identical on and off.
func TestHubDashboardCarriesThePanelOnlyWhenAskIsOn(t *testing.T) {
	type snapshot struct {
		body    string
		headers http.Header
		asset   int
	}
	take := func(on bool) snapshot {
		s, _ := newAskTestHub(t, on, &askFakeUpstream{})
		s.mux.HandleFunc("GET /dashboard", s.handleDashboard)
		w := askDo(s, "GET", "/dashboard", "", "alice", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("dashboard %d", w.Code)
		}
		asset := askDo(s, "GET", askPanelJSPath, "", "alice", nil)
		return snapshot{body: w.Body.String(), headers: w.Header(), asset: asset.Code}
	}
	off, on := take(false), take(true)

	if off.body != dashboardHTML || strings.Contains(off.body, askPanelJSPath) || off.asset != http.StatusNotFound {
		t.Fatalf("off: document changed or panel served (asset %d)", off.asset)
	}
	if !strings.Contains(on.body, askPanelTags) || strings.Contains(on.body, askPanelMarker) || on.asset != http.StatusOK {
		t.Fatalf("on: panel tags missing (asset %d)", on.asset)
	}
	if strings.Count(dashboardHTML, askPanelMarker) != 1 || strings.Count(dashboardHTML, `id="hive-ask-slot"`) != 1 {
		t.Fatal("the dashboard must hold exactly one panel marker and one opener slot")
	}
	for _, h := range []string{"Content-Security-Policy", "Content-Security-Policy-Report-Only", "X-Frame-Options", "Referrer-Policy"} {
		if off.headers.Get(h) != on.headers.Get(h) {
			t.Errorf("%s differs with Ask on: %q vs %q", h, off.headers.Get(h), on.headers.Get(h))
		}
	}
	if off.headers.Get("ETag") == on.headers.Get("ETag") {
		t.Error("the two documents must not share an ETag")
	}
	// The only additions are same-origin: no third-party host, no inline script.
	added := strings.Replace(on.body, askPanelTags, "", 1)
	if added != strings.Replace(dashboardHTML, askPanelMarker, "", 1) {
		t.Fatal("Ask on changed the document beyond the panel tags")
	}
	for _, bad := range []string{"http://", "https://", "<script>", "onclick", "kapa"} {
		if strings.Contains(askPanelTags, bad) {
			t.Fatalf("panel tags carry %q", bad)
		}
	}
}

func TestHubAskPanelAssetsAndOperatorState(t *testing.T) {
	s, _ := newAskTestHub(t, true, &askFakeUpstream{})
	js := askDo(s, "GET", askPanelJSPath, "", "alice", nil)
	css := askDo(s, "GET", askPanelCSSPath, "", "alice", nil)
	if !strings.HasPrefix(js.Header().Get("Content-Type"), "text/javascript") || !strings.Contains(js.Body.String(), "HiveAsk") {
		t.Fatalf("js %d %q", js.Code, js.Header().Get("Content-Type"))
	}
	if !strings.HasPrefix(css.Header().Get("Content-Type"), "text/css") || !strings.Contains(css.Body.String(), ".hive-ask-panel") {
		t.Fatalf("css %d %q", css.Code, css.Header().Get("Content-Type"))
	}

	for _, on := range []bool{false, true} {
		hub, _ := newAskTestHub(t, on, &askFakeUpstream{})
		helperRedirectScaleSettings(t)
		rec := httptest.NewRecorder()
		hub.handleGetScaleSettings(rec, httptest.NewRequest(http.MethodGet, "/api/admin/scale-settings", nil))
		var view struct {
			Effective map[string]any `json:"effective"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if got, ok := view.Effective["ask_enabled"].(bool); !ok || got != on {
			t.Fatalf("scale settings ask_enabled = %v, want %v", view.Effective["ask_enabled"], on)
		}
	}
	if !strings.Contains(dashboardHTML, `id="ask-effective"`) || !strings.Contains(dashboardHTML, "eff.ask_enabled") ||
		!strings.Contains(dashboardHTML, "hub-deployment.md#ask-ai-help") {
		t.Fatal("the operator card must show the effective Ask switch with a help mark")
	}
}
