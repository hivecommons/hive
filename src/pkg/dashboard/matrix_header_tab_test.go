package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestDashboardMatrixHeaderTabHiddenByDefault(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(raw)
	start := strings.Index(html, `id="oc-matrix-tab"`)
	if start < 0 {
		t.Fatal("static/index.html missing the Matrix header tab")
	}
	open := html[strings.LastIndex(html[:start], "<a "):]
	open = open[:strings.Index(open, ">")]
	for _, want := range []string{`href="?kiosk=1&amp;view=matrix"`, ` hidden`, `nav-chip`} {
		if !strings.Contains(open, want) {
			t.Fatalf("Matrix header tab %q missing %q", open, want)
		}
	}
	if !strings.Contains(html, "data.matrixEnabled") {
		t.Fatal("static/index.html never toggles the Matrix tab from the status payload")
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
