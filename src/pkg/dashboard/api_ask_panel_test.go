package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/askai"
	"github.com/hivecommons/hive/pkg/config"
)

// TestHiveAskPanelAssetsOnlyWhenOn: the panel component is served from
// 'self' only while ask is on, and /api/config tells the page whether to load
// it at all.
func TestHiveAskPanelAssetsOnlyWhenOn(t *testing.T) {
	get := func(s *Server, path string, data []byte, ct string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handleAskPanelAsset(ct, data)(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	off, _ := askTestServer(t, config.AskConfig{}, config.HubConfig{}, "")
	if rec := get(off, "/ask-panel.js", askai.PanelJS, "text/javascript"); rec.Code != http.StatusNotFound || askOn(off.deps.Config) {
		t.Fatalf("off: %d", rec.Code)
	}
	on, _ := askTestServer(t, askDirect, config.HubConfig{}, "")
	js := get(on, "/ask-panel.js", askai.PanelJS, "text/javascript; charset=utf-8")
	css := get(on, "/ask-panel.css", askai.PanelCSS, "text/css; charset=utf-8")
	if js.Code != http.StatusOK || !strings.Contains(js.Body.String(), "HiveAsk") || !strings.HasPrefix(js.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("js %d", js.Code)
	}
	if css.Code != http.StatusOK || !strings.Contains(css.Body.String(), ".hive-ask-panel") || !askOn(on.deps.Config) {
		t.Fatalf("css %d", css.Code)
	}
	src, err := os.ReadFile("api_config.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `"ask_enabled": askOn(cfg)`) {
		t.Fatal("/api/config must report ask_enabled from the effective settings")
	}
}

// TestHiveAskPanelLoaderIsGatedAndSameOrigin pins the page side: one opener
// slot, the loader only runs on cfg.ask_enabled === true, it loads the
// component from this origin, and it adds no third-party host.
func TestHiveAskPanelLoaderIsGatedAndSameOrigin(t *testing.T) {
	loader, err := os.ReadFile("static-src/js/ask.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(loader)
	for _, want := range []string{"cfg.ask_enabled !== true", "'/ask-panel.js'", "'/ask-panel.css'", "window.HiveAsk.mount(", "DASHBOARD_SECTION_HELP", "dashboardDocsHref("} {
		if !strings.Contains(js, want) {
			t.Errorf("loader lacks %q", want)
		}
	}
	for _, banned := range []string{"http://", "https://", "kapa", "innerHTML"} {
		if strings.Contains(js, banned) {
			t.Errorf("loader carries %q", banned)
		}
	}
	index, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(index), `id="hive-ask-slot"`) != 1 || !strings.Contains(string(index), "loadAskPanel(cfg)") {
		t.Fatal("the dashboard must hold one Ask slot and call the gated loader from the config bootstrap")
	}
}
