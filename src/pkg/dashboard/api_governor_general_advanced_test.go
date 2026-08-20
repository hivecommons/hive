package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"
)

// generalAdvancedValue reads the general-advanced payload from its GET
// endpoint.
func generalAdvancedValue(t *testing.T, s *Server) (int, bool) {
	t.Helper()
	rec := doOwnerGet(s, "/api/config/governor/general-advanced")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET general-advanced: %d", rec.Code)
	}
	var resp struct {
		EvalIntervalS      int  `json:"eval_interval_s"`
		AttributionTrailer bool `json:"attribution_trailer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.EvalIntervalS, resp.AttributionTrailer
}

func TestGovGeneralAdvanced_GetAndPut(t *testing.T) {
	s := govServer(t)

	// Untouched config → attribution defaults ON.
	if _, on := generalAdvancedValue(t, s); !on {
		t.Fatal("attribution_trailer must default to on")
	}

	// Non-owner GET → 403 (owner-only surface).
	if rec := doGet(s, "/api/config/governor/general-advanced"); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner GET: expected 403, got %d", rec.Code)
	}

	// Bad body → 400.
	if rec := doPutRaw(s, "/api/config/governor/general-advanced", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: expected 400, got %d", rec.Code)
	}

	// Out-of-range eval interval → 400, nothing stored.
	if rec := doPut(s, "/api/config/governor/general-advanced", map[string]any{"eval_interval_s": 5}); rec.Code != http.StatusBadRequest {
		t.Fatalf("interval 5: expected 400, got %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/governor/general-advanced", map[string]any{"eval_interval_s": 90000}); rec.Code != http.StatusBadRequest {
		t.Fatalf("interval 90000: expected 400, got %d", rec.Code)
	}

	// Valid interval persists.
	if rec := doPut(s, "/api/config/governor/general-advanced", map[string]any{"eval_interval_s": 120}); rec.Code != http.StatusOK {
		t.Fatalf("put interval: %d", rec.Code)
	}
	if s.deps.Config.Governor.EvalIntervalS != 120 {
		t.Fatalf("EvalIntervalS = %d, want 120", s.deps.Config.Governor.EvalIntervalS)
	}
	if got, _ := generalAdvancedValue(t, s); got != 120 {
		t.Fatalf("GET eval_interval_s = %d, want 120", got)
	}

	// Toggle attribution OFF: stored as an explicit false pointer.
	if rec := doPut(s, "/api/config/governor/general-advanced", map[string]any{"attribution_trailer": false}); rec.Code != http.StatusOK {
		t.Fatalf("put false: %d", rec.Code)
	}
	if s.deps.Config.Governor.AttributionTrailer == nil || *s.deps.Config.Governor.AttributionTrailer {
		t.Fatal("explicit false must be stored as an explicit false pointer")
	}
	if _, on := generalAdvancedValue(t, s); on {
		t.Fatal("GET must reflect the trailer off")
	}

	// Absent fields → no change (partial-update convention).
	if rec := doPut(s, "/api/config/governor/general-advanced", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("put empty: %d", rec.Code)
	}
	if got, on := generalAdvancedValue(t, s); got != 120 || on {
		t.Fatalf("empty body must not change stored values: got %d/%v", got, on)
	}

	// Toggle back ON.
	if rec := doPut(s, "/api/config/governor/general-advanced", map[string]any{"attribution_trailer": true}); rec.Code != http.StatusOK {
		t.Fatalf("put true: %d", rec.Code)
	}
	if _, on := generalAdvancedValue(t, s); !on {
		t.Fatal("GET must reflect the trailer back on")
	}
}
