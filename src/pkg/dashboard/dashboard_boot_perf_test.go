package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServerTimingHeaderIsEmitted(t *testing.T) {
	s := &Server{}
	h := s.withServerTiming(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	got := rec.Header().Get("Server-Timing")
	if !strings.HasPrefix(got, "app;dur=") {
		t.Fatalf("Server-Timing = %q, want app duration", got)
	}
}

func TestBootPerfSummaryIsOptIn(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading static index: %v", err)
	}
	html := string(b)
	for _, want := range []string{"initBootPerfSummary", "params.get('perf') !== '1'", "window.__hiveBootPerf", "[hive perf] boot fetch summary"} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard missing perf summary snippet %q", want)
		}
	}
}
