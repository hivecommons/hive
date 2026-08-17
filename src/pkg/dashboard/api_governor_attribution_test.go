package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"
)

// governorAttributionValue reads the effective attribution.attributionTrailer
// boolean out of GET /api/config/governor.
func governorAttributionValue(t *testing.T, s *Server) bool {
	t.Helper()
	rec := doGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor config: %d", rec.Code)
	}
	var resp struct {
		Attribution struct {
			AttributionTrailer bool `json:"attributionTrailer"`
		} `json:"attribution"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Attribution.AttributionTrailer
}

func TestGovAttribution_DefaultOnAndToggle(t *testing.T) {
	s := govServer(t)

	// Untouched config → default ON reflected to the UI.
	if !governorAttributionValue(t, s) {
		t.Fatal("attributionTrailer must default to on")
	}
	if !s.deps.Config.Governor.AttributionTrailerEnabled() {
		t.Fatal("config accessor must default to on")
	}

	// Bad body → 400.
	if rec := doPutRaw(s, "/api/config/governor/attribution", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: expected 400, got %d", rec.Code)
	}

	// Toggle OFF: gates ONLY the trailer; persisted as an explicit false.
	if rec := doPut(s, "/api/config/governor/attribution", map[string]any{"attributionTrailer": false}); rec.Code != http.StatusOK {
		t.Fatalf("put false: %d", rec.Code)
	}
	if s.deps.Config.Governor.AttributionTrailer == nil || *s.deps.Config.Governor.AttributionTrailer {
		t.Fatal("explicit false must be stored as an explicit false pointer")
	}
	if governorAttributionValue(t, s) {
		t.Fatal("GET must reflect the trailer off")
	}

	// Absent field in the body → no change (partial-update convention).
	if rec := doPut(s, "/api/config/governor/attribution", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("put empty: %d", rec.Code)
	}
	if s.deps.Config.Governor.AttributionTrailerEnabled() {
		t.Fatal("empty body must not flip the stored value")
	}

	// Toggle back ON.
	if rec := doPut(s, "/api/config/governor/attribution", map[string]any{"attributionTrailer": true}); rec.Code != http.StatusOK {
		t.Fatalf("put true: %d", rec.Code)
	}
	if !governorAttributionValue(t, s) {
		t.Fatal("GET must reflect the trailer back on")
	}
}
