package dashboard

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContributePage_NoPrintfArtifacts(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("code %d", w.Code)
	}
	if strings.Contains(body, "%!") {
		t.Error("printf artifact %! found")
	}
	for _, want := range []string{"me-callsign", "me-heraldry", "mePathProgress", "The path does not expire", "lb-title", "/heraldry", "me-dossier-form"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}
