package hub

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestHubTokensCSSMatchesDashboardSource(t *testing.T) {
	dashboard, err := os.ReadFile("../dashboard/static/tokens.css")
	if err != nil {
		t.Fatalf("reading dashboard token source: %v", err)
	}
	hub, err := os.ReadFile("static/tokens.css")
	if err != nil {
		t.Fatalf("reading hub token copy: %v", err)
	}
	if !bytes.Equal(hub, dashboard) {
		t.Fatal("pkg/hub/static/tokens.css drifted from pkg/dashboard/static/tokens.css")
	}
}

func TestHubTokensCSSServedAsStylesheet(t *testing.T) {
	s := &HubServer{}
	w := httptest.NewRecorder()
	s.serveStatic("static/tokens.css")(w, httptest.NewRequest(http.MethodGet, "/tokens.css", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/css") {
		t.Fatalf("Content-Type = %q, want text/css", got)
	}
}
