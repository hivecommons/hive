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

func TestBootModelDiscoveryIsDeferred(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading static index: %v", err)
	}
	html := string(b)
	fn := strings.Index(html, "function refreshBackendDiscovery")
	deferred := strings.Index(html, "const deferred = [")
	call := strings.Index(html, "() => Promise.resolve(refreshBackendDiscovery()).then(() => fetchBackendsConfig())")
	if fn < 0 || deferred < 0 || call < 0 {
		t.Fatalf("backend discovery must be defined and scheduled in deferred init")
	}
	if fn > deferred {
		t.Fatalf("refreshBackendDiscovery definition must precede deferred init")
	}
	if call < deferred {
		t.Fatalf("backend discovery is not deferred")
	}
	immediate := html[fn:deferred]
	if strings.Contains(immediate, "fetchBackendsConfig();") || strings.Contains(immediate, "INFERENCE_BACKENDS.forEach(b => fetchInferenceModels(b));") {
		t.Fatalf("model discovery still starts on the parser path before deferred init")
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
