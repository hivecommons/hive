package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestDashboardCodeActivityHeaderTabHiddenByDefault(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(raw)
	start := strings.Index(html, `id="oc-matrix-tab"`)
	if start < 0 {
		t.Fatal("static/index.html missing the Code activity header tab")
	}
	open := html[strings.LastIndex(html[:start], "<a "):]
	open = open[:strings.Index(open, ">")]
	for _, want := range []string{`href="?kiosk=1&amp;view=reader"`, `target="_blank"`, `rel="noopener"`, ` hidden`, `nav-chip`} {
		if !strings.Contains(open, want) {
			t.Fatalf("Code activity header tab %q missing %q", open, want)
		}
	}
	if !strings.Contains(html[start:], `>Code activity</span></a>`) {
		t.Fatal("toolbar entry must be labelled Code activity")
	}
	// One entry: the legacy oc-matrix-tab id is retained, with no side-by-side tab.
	if strings.Count(html, `id="oc-matrix-tab"`) != 1 || strings.Contains(html, `id="oc-reader-tab"`) {
		t.Fatal("toolbar must carry exactly one Code activity entry")
	}
	if strings.Contains(html, "view=matrix\"") {
		t.Fatal("toolbar still links the retired Matrix view")
	}
	if !strings.Contains(html, "data.matrixEnabled") {
		t.Fatal("static/index.html never toggles the Code activity tab from the status payload")
	}
}

func TestHandleStatus_MatrixEnabledFlag(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		s := newTestServer()
		s.MatrixObserver(config.MatrixConfig{Enabled: enabled, Repos: []string{"org/repo"}})
		s.UpdateStatus(minimalPayload())

		rec := httptest.NewRecorder()
		s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))

		var raw map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
			t.Fatalf("decode error: %v", err)
		}
		if got, ok := raw["matrixEnabled"].(bool); !ok || got != enabled {
			t.Errorf("matrixEnabled = %v (present=%v), want %v", raw["matrixEnabled"], ok, enabled)
		}
	}
}
